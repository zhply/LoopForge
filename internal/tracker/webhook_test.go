package tracker

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestParseGitLabMRWebhook(t *testing.T) {
	payload := []byte(`{"object_kind":"merge_request","project":{"path_with_namespace":"group/app"},"object_attributes":{"iid":9,"state":"merged","target_branch":"main","source_branch":"work/2","merged_at":"2026-01-05T00:00:00Z","url":"https://gitlab.test/group/app/-/merge_requests/9"}}`)
	project, iid, state, target, url, mergedAt, ok, err := ParseGitLabMRWebhook(payload)
	if err != nil || !ok {
		t.Fatalf("parse = %v, ok=%v", err, ok)
	}
	if project != "gitlab:gitlab.test/group/app" || iid != 9 || state != "merged" || target != "main" {
		t.Fatalf("parsed = %q %d %q %q", project, iid, state, target)
	}
	if url != "https://gitlab.test/group/app/-/merge_requests/9" {
		t.Fatalf("url = %q", url)
	}
	if mergedAt.IsZero() {
		t.Fatal("mergedAt missing")
	}
	// Non-MR events are accepted but ignored.
	if _, _, _, _, _, _, ok, err = ParseGitLabMRWebhook([]byte(`{"object_kind":"push"}`)); err != nil || ok {
		t.Fatalf("push event should be ignored: ok=%v err=%v", ok, err)
	}
	if _, _, _, _, _, _, _, err = ParseGitLabMRWebhook([]byte(`not json`)); err == nil {
		t.Fatal("invalid payload should fail")
	}
}

func TestApplyWebhookMRStateUpdatesAndSkipsUnknown(t *testing.T) {
	store := &fakeMRDeliveryStore{}
	svc := NewService()
	if err := svc.ApplyWebhookMRState(store, "gitlab:gitlab.test/group/app", 9, "merged", "main", time.Time{}, "https://gitlab.test/group/app/-/merge_requests/9"); err != nil {
		t.Fatal(err)
	}
	if len(store.updates) != 1 {
		t.Fatalf("updates = %d", len(store.updates))
	}
	up := store.updates[0]
	if up.state != "merged" || up.targetBranch != "main" || up.mergedAt.IsZero() {
		t.Fatalf("update = %+v", up)
	}
	// A closed event still lands (it is authoritative), but unknown states
	// must be ignored so polling cannot be poisoned.
	if err := svc.ApplyWebhookMRState(store, "gitlab:gitlab.test/group/app", 9, "closed", "", time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyWebhookMRState(store, "gitlab:gitlab.test/group/app", 9, "weird", "", time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if len(store.updates) != 2 {
		t.Fatalf("unknown state must be ignored, updates = %d", len(store.updates))
	}
}

func TestApplyJiraWebhookRepullsKnownIssue(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	svc, _ := newJiraTestService(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(jiraIssueJSON("PROJ-12", "Webhook title", "Done", "done")))
	})
	store := &memIssueStore{
		projects: map[string]domain.Project{"p1": {ID: "p1", JiraProjectKey: "PROJ"}},
		issues: map[string]domain.Issue{
			"jira-PROJ-12": {ID: "jira-PROJ-12", ProjectID: "p1", Title: "old", Source: "jira", ExternalRef: &domain.ExternalRef{Source: "jira", ExternalID: "PROJ-12"}},
		},
	}
	if err := svc.ApplyJiraWebhook(store, "PROJ-12"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	fetched := false
	for _, p := range paths {
		if strings.HasPrefix(p, "GET /rest/api/2/issue/PROJ-12") {
			fetched = true
		}
	}
	mu.Unlock()
	if !fetched {
		t.Fatalf("issue not re-pulled: %v", paths)
	}
	saved, ok := store.Issue("jira-PROJ-12")
	if !ok || saved.Title != "Webhook title" {
		t.Fatalf("saved = %+v", saved)
	}
	// Unknown issues are accepted and skipped.
	if err := svc.ApplyJiraWebhook(store, "PROJ-999"); err != nil {
		t.Fatalf("unknown issue should be skipped, got %v", err)
	}
}

func TestApplyJiraWebhookToleratesBlankInput(t *testing.T) {
	svc := NewService()
	if err := svc.ApplyJiraWebhook(nil, ""); err != nil {
		t.Fatal(err)
	}
}
