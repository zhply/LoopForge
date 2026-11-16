package httpapi

import (
	"net/http"
	"path/filepath"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

// The project integration tab saves Jira/Confluence/webhook settings through
// the same endpoint the sync panel uses; secrets must stay write-only and an
// empty submission must keep the stored value.
func TestSyncConfigIntegrationFieldsRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(root, "workspaces"))), tracker.NewService(), "", "")

	saved := requestJSON[domain.SyncConfig](t, h, http.MethodPut, "/api/sync/config", map[string]any{
		"jiraBaseUrl":          "https://jira.example.com",
		"jiraEmail":            "dev@example.com",
		"jiraToken":            "jira-secret",
		"jiraWriteBackEnabled": true,
		"confluenceBaseUrl":    "https://confluence.example.com",
		"confluenceEmail":      "dev@example.com",
		"confluenceToken":      "cf-secret",
		"confluenceSpaceKey":   "PROJ",
		"webhookSecret":        "hook-secret",
	}, http.StatusOK)
	if saved.JiraToken != "" || !saved.JiraTokenConfigured {
		t.Fatalf("jira token must stay write-only: %+v", saved)
	}
	if saved.ConfluenceToken != "" || !saved.ConfluenceTokenConfigured {
		t.Fatalf("confluence token must stay write-only: %+v", saved)
	}
	if saved.WebhookSecret != "" || !saved.WebhookSecretConfigured {
		t.Fatalf("webhook secret must stay write-only: %+v", saved)
	}
	if !saved.JiraWriteBackEnabled || saved.ConfluenceSpaceKey != "PROJ" || saved.JiraBaseURL != "https://jira.example.com" {
		t.Fatalf("integration settings not persisted: %+v", saved)
	}

	// A later partial save (empty secrets) keeps the stored credentials.
	again := requestJSON[domain.SyncConfig](t, h, http.MethodPut, "/api/sync/config", map[string]any{
		"jiraBaseUrl": "https://jira.example.com", "jiraToken": "", "confluenceToken": "", "webhookSecret": "",
	}, http.StatusOK)
	if !again.JiraTokenConfigured || !again.ConfluenceTokenConfigured || !again.WebhookSecretConfigured {
		t.Fatalf("empty submission wiped credentials: %+v", again)
	}
	stored := s.SyncConfig()
	if stored.JiraToken != "jira-secret" || stored.ConfluenceToken != "cf-secret" || stored.WebhookSecret != "hook-secret" {
		t.Fatalf("stored secrets = %q/%q/%q", stored.JiraToken, stored.ConfluenceToken, stored.WebhookSecret)
	}

	pub := requestEmptyJSON[domain.SyncConfig](t, h, http.MethodGet, "/api/sync/config", http.StatusOK)
	if pub.WebhookSecret != "" || pub.JiraToken != "" || !pub.WebhookSecretConfigured {
		t.Fatalf("GET must redact secrets: %+v", pub)
	}
}
