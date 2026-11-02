package gdocs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sigma/okf-tools/internal/publish/backend/request"
)

// Scopes are the OAuth scopes the publisher needs: Drive to find, create and
// update the destination file and its sidecar, Docs to write tab content.
//
// They must be requested explicitly. gcloud's --impersonate-service-account
// silently IGNORES --scopes and hands back a cloud-platform token, which Drive
// rejects with ACCESS_TOKEN_SCOPE_INSUFFICIENT (sigma/okf-tools#154) — the same
// trap in library form is passing no scopes here.
var Scopes = []string{
	"https://www.googleapis.com/auth/drive",
	"https://www.googleapis.com/auth/documents",
}

// tokenSource builds the credentials the publisher writes with.
//
// There is deliberately no key-file path. Newer Google organizations enforce
// constraints/iam.managed.disableServiceAccountKeyCreation, so a service-account
// JSON key cannot even be minted (#154); the identity is instead reached by
// IMPERSONATION from whatever Application Default Credentials the environment
// already has — a developer's gcloud login, or a Workload Identity Federation
// credential in CI. Both are short-lived, and neither is a secret to leak.
//
// impersonate is empty for the degenerate case where ADC already IS the publish
// identity, in which case the ambient credentials are used unchanged.
//
// onRetry is called once per retried token mint, so the run's stats see them.
func tokenSource(ctx context.Context, impersonate, iamEndpoint string, onRetry func()) (oauth2.TokenSource, error) {
	// The source credentials only ever mint an impersonation token, so they need
	// cloud-platform rather than the Drive/Docs scopes the impersonated token carries.
	scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}
	if impersonate == "" {
		scopes = Scopes
	}
	creds, err := google.FindDefaultCredentials(ctx, scopes...)
	if err != nil {
		return nil, fmt.Errorf("gdocs: application default credentials: %w", err)
	}
	return wrapSource(ctx, creds.TokenSource, impersonate, iamEndpoint, onRetry, nil), nil
}

// wrapSource layers impersonation, when asked for, and then retry over the
// ambient credentials. sleep is the clock seam; nil waits in real time.
func wrapSource(ctx context.Context, src oauth2.TokenSource, impersonate, iamEndpoint string,
	onRetry func(), sleep func(context.Context, time.Duration) error) oauth2.TokenSource {
	if impersonate != "" {
		src = &impersonatedSource{ctx: ctx, source: src, target: impersonate, endpoint: iamEndpoint}
	}
	return &retryingSource{ctx: ctx, source: src, onRetry: onRetry, sleep: sleep}
}

// tokenRetryBudget is how long minting one token may spend paused between
// retries. Shorter than RetryBudget: a token endpoint is not quota-windowed, and
// a credential that is wrong rather than briefly unavailable should fail soon.
const tokenRetryBudget = 20 * time.Second

// retryingSource retries a token mint that failed transiently.
//
// The mint happens inside the HTTP transport, BEFORE the request it authorizes
// goes out, so the request's own retry never sees it: a CI run's 504 from the
// external-account credential source failed a whole selection on the spot
// (#218).
//
// The library reports that failure as untyped text, so a transient error cannot
// be told from a permanent one without parsing its message. Instead every
// failure is retried within a short budget, EXCEPT the ones that are known
// terminal: an OAuth 4xx (other than 429) and an impersonation refusal.
type retryingSource struct {
	ctx     context.Context
	source  oauth2.TokenSource
	onRetry func()
	sleep   func(context.Context, time.Duration) error
}

func (s *retryingSource) Token() (*oauth2.Token, error) {
	var paused time.Duration
	for n := 1; ; n++ {
		tok, err := s.source.Token()
		if err == nil || terminalTokenError(err) || paused >= tokenRetryBudget {
			return tok, err
		}
		d := request.Backoff(n)
		sleep := s.sleep
		if sleep == nil {
			sleep = request.Sleep
		}
		if serr := sleep(s.ctx, d); serr != nil {
			return nil, err
		}
		paused += d
		if s.onRetry != nil {
			s.onRetry()
		}
	}
}

// terminalTokenError reports a mint failure that retrying cannot fix.
func terminalTokenError(err error) bool {
	var refused *refusedError
	if errors.As(err, &refused) {
		return true
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		code := re.Response.StatusCode
		return code >= 400 && code < 500 && code != http.StatusTooManyRequests
	}
	return false
}

// refusedError is an impersonation the IAM Credentials API refused outright,
// as opposed to one it failed to answer.
type refusedError struct{ error }

func (e *refusedError) Unwrap() error { return e.error }

// impersonatedSource exchanges a source token for a short-lived token belonging
// to the target service account, via the IAM Credentials API. It is the one call
// google.golang.org/api/impersonate would make; doing it here keeps this module's
// dependency set to golang.org/x/oauth2 rather than pulling in grpc and protobuf
// for a single HTTP POST.
type impersonatedSource struct {
	ctx      context.Context
	source   oauth2.TokenSource
	target   string
	endpoint string
}

func (s *impersonatedSource) Token() (*oauth2.Token, error) {
	src, err := s.source.Token()
	if err != nil {
		return nil, fmt.Errorf("gdocs: source credentials: %w", err)
	}
	body, err := json.Marshal(map[string]any{"scope": Scopes, "lifetime": "3600s"})
	if err != nil {
		return nil, err
	}
	// The target is interpolated into the path; a stray slash would silently
	// address a different resource, so refuse rather than build a wrong URL.
	if strings.ContainsAny(s.target, "/?#") {
		return nil, fmt.Errorf("gdocs: invalid impersonation target %q", s.target)
	}
	url := fmt.Sprintf("%s/v1/projects/-/serviceAccounts/%s:generateAccessToken", s.endpoint, s.target)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+src.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gdocs: impersonate %s: %w", s.target, err)
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"`
		Error       struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gdocs: impersonate %s: %w", s.target, err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		msg := out.Error.Message
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("gdocs: impersonate %s: %s", s.target, msg)
		}
		return nil, &refusedError{fmt.Errorf("gdocs: impersonate %s: %s "+
			"(the caller needs roles/iam.serviceAccountTokenCreator on that account, "+
			"and iamcredentials.googleapis.com must be enabled)", s.target, msg)}
	}
	exp, err := time.Parse(time.RFC3339, out.ExpireTime)
	if err != nil {
		exp = time.Now().Add(55 * time.Minute)
	}
	return &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer", Expiry: exp}, nil
}
