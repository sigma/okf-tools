package gdocs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// fakeTokenPath answers the three hops a CI publish takes to a token: the
// workload identity's OIDC token (the external-account credential source), the
// STS exchange, and the IAM impersonation. fail names the hop that answers 504
// once before succeeding.
func fakeTokenPath(t *testing.T, fail string) (*httptest.Server, *int) {
	failed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hop := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasSuffix(hop, ":generateAccessToken") {
			hop = "iam"
		}
		if hop == fail && failed == 0 {
			failed++
			http.Error(w, "upstream request timeout", http.StatusGatewayTimeout)
			return
		}
		switch hop {
		case "oidc":
			w.Write([]byte("subject-token"))
		case "sts":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"federated","issued_token_type":` +
				`"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
		case "iam":
			fmt.Fprintf(w, `{"accessToken":"impersonated","expireTime":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))
		default:
			t.Errorf("unexpected token request %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &failed
}

// TestTokenMintRetriesTransient5xx is #218's third failure: a 504 from the
// external-account credential source failed a selection, because the mint runs
// inside the transport, before the request whose retry would have covered it.
// It drives the real external-account code path, not a stand-in.
func TestTokenMintRetriesTransient5xx(t *testing.T) {
	for _, hop := range []string{"oidc", "sts", "iam"} {
		t.Run(hop, func(t *testing.T) {
			srv, failed := fakeTokenPath(t, hop)
			ctx := context.Background()
			creds, err := google.CredentialsFromJSON(ctx, []byte(fmt.Sprintf(`{
				"type": "external_account",
				"audience": "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/gh",
				"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
				"token_url": %q,
				"credential_source": {"url": %q}
			}`, srv.URL+"/sts", srv.URL+"/oidc")), "https://www.googleapis.com/auth/cloud-platform")
			if err != nil {
				t.Fatalf("credentials: %v", err)
			}

			var retries int
			var pauses []time.Duration
			ts := wrapSource(ctx, creds.TokenSource, "publisher@p.iam.gserviceaccount.com", srv.URL,
				func() { retries++ },
				func(ctx context.Context, d time.Duration) error { pauses = append(pauses, d); return nil })
			tok, err := ts.Token()
			if err != nil {
				t.Fatalf("a transient 504 from the %s hop failed the mint: %v", hop, err)
			}
			if tok.AccessToken != "impersonated" || *failed != 1 || retries != 1 || len(pauses) != 1 {
				t.Errorf("token=%q failed=%d retries=%d pauses=%v; want one retry to an impersonated token",
					tok.AccessToken, *failed, retries, pauses)
			}
		})
	}
}

// TestTokenMintRefusalIsTerminal: an impersonation the IAM API refuses is a
// missing grant, which no retry fixes; it must fail at once.
func TestTokenMintRefusalIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"Permission 'iam.serviceAccounts.getAccessToken' denied"}}`))
	}))
	defer srv.Close()

	var retries int
	ts := wrapSource(context.Background(), staticSource{}, "publisher@p.iam.gserviceaccount.com", srv.URL,
		func() { retries++ }, func(context.Context, time.Duration) error { return nil })
	if _, err := ts.Token(); err == nil || retries != 0 {
		t.Errorf("a refused impersonation must fail without retrying: err=%v retries=%d", err, retries)
	}
}

type staticSource struct{}

func (staticSource) Token() (*oauth2.Token, error) { return &oauth2.Token{AccessToken: "source"}, nil }
