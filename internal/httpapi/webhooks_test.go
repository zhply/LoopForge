package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func cvBWebhookFixture(t *testing.T, secret string) cvBHTTPFixture {
	t.Helper()
	f := cvBNewHTTPFixture(t)
	if secret != "" {
		if err := f.s.PutSyncConfig(domain.SyncConfig{WebhookSecret: secret}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// cvBWebhookRequest posts a signed webhook payload: no interactive login,
// authentication happens via the webhook secret header.
func cvBWebhookRequest(f cvBHTTPFixture, source, secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/"+source, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookIncomingAuth(t *testing.T) {
	// No secret configured: the platform refuses delivery instead of
	// accepting unsigned events.
	f := cvBWebhookFixture(t, "")
	res := cvBWebhookRequest(f, "gitlab", "", `{"object_kind":"push"}`)
	if res.Code != http.StatusForbidden {
		t.Fatalf("unconfigured secret = %d", res.Code)
	}
	f = cvBWebhookFixture(t, "s3cret")
	if res = cvBWebhookRequest(f, "gitlab", "", `{"object_kind":"push"}`); res.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret = %d", res.Code)
	}
	if res = cvBWebhookRequest(f, "gitlab", "wrong", `{"object_kind":"push"}`); res.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret = %d", res.Code)
	}
}

func TestWebhookIncomingGitlabEvents(t *testing.T) {
	f := cvBWebhookFixture(t, "s3cret")
	res := cvBWebhookRequest(f, "gitlab", "s3cret", `{"object_kind":"push"}`)
	if res.Code != http.StatusAccepted {
		t.Fatalf("push event = %d", res.Code)
	}
	res = cvBWebhookRequest(f, "gitlab", "s3cret", `{"object_kind":"merge_request","project":{"path_with_namespace":"group/app"},"object_attributes":{"iid":9,"state":"merged","target_branch":"main","url":"https://gitlab.test/group/app/-/merge_requests/9"}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("merged MR = %d: %s", res.Code, res.Body.String())
	}
}

func TestWebhookIncomingJiraEvents(t *testing.T) {
	f := cvBWebhookFixture(t, "s3cret")
	// Unknown issue: accepted and skipped.
	res := cvBWebhookRequest(f, "jira", "s3cret", `{"webhookEvent":"jira:issue_updated","issue":{"key":"PROJ-1"}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("jira unknown issue = %d: %s", res.Code, res.Body.String())
	}
	// Payload without an issue key is ignored.
	res = cvBWebhookRequest(f, "jira", "s3cret", `{"webhookEvent":"jira:issue_updated"}`)
	if res.Code != http.StatusAccepted {
		t.Fatalf("jira missing key = %d", res.Code)
	}
}

func TestWebhookIncomingUnknownSource(t *testing.T) {
	f := cvBWebhookFixture(t, "s3cret")
	// The secret check runs before source dispatch, so an unknown source with
	// a valid secret reaches the 404.
	res := cvBWebhookRequest(f, "github", "s3cret", `{}`)
	if res.Code != http.StatusNotFound || !strings.Contains(res.Body.String(), "unknown webhook source") {
		t.Fatalf("unknown source = %d %s", res.Code, res.Body.String())
	}
}
