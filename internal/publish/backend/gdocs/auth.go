package gdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
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
func tokenSource(ctx context.Context, impersonate, iamEndpoint string) (oauth2.TokenSource, error) {
	// The source credentials only ever mint an impersonation token, so they need
	// cloud-platform rather than the Drive/Docs scopes the impersonated token carries.
	sourceScope := "https://www.googleapis.com/auth/cloud-platform"
	if impersonate == "" {
		creds, err := google.FindDefaultCredentials(ctx, Scopes...)
		if err != nil {
			return nil, fmt.Errorf("gdocs: application default credentials: %w", err)
		}
		return creds.TokenSource, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, sourceScope)
	if err != nil {
		return nil, fmt.Errorf("gdocs: application default credentials: %w", err)
	}
	return &impersonatedSource{
		ctx:      ctx,
		source:   creds.TokenSource,
		target:   impersonate,
		endpoint: iamEndpoint,
	}, nil
}

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
		return nil, fmt.Errorf("gdocs: impersonate %s: %s "+
			"(the caller needs roles/iam.serviceAccountTokenCreator on that account, "+
			"and iamcredentials.googleapis.com must be enabled)", s.target, msg)
	}
	exp, err := time.Parse(time.RFC3339, out.ExpireTime)
	if err != nil {
		exp = time.Now().Add(55 * time.Minute)
	}
	return &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer", Expiry: exp}, nil
}
