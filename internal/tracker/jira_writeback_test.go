package tracker

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// jiraWritebackRecorder captures write-back REST calls from the handler goroutine.
type jiraWritebackRecorder struct {
	mu    sync.Mutex
	calls []string // "METHOD path" entries, plus captured payloads
}

func (r *jiraWritebackRecorder) add(entry string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, entry)
}

func (r *jiraWritebackRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newJiraWritebackFixture(t *testing.T, enable bool) (*Service, *memIssueStore, *jiraWritebackRecorder) {
	t.Helper()
	rec := &jiraWritebackRecorder{}
	svc, _ := newJiraTestService(t, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Method + " " + r.URL.Path)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comment"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			rec.add("COMMENT: " + payload.Body)
			_, _ = w.Write([]byte(`{"id":"10001"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transitions"):
			_, _ = w.Write([]byte(`{"transitions":[{"id":"31","name":"Done","to":{"name":"Done","statusCategory":{"key":"done"}}},{"id":"21","name":"Start","to":{"name":"In Progress","statusCategory":{"key":"indeterminate"}}}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transitions"):
			var payload struct {
				Transition string `json:"transition"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			rec.add("TRANSITION: " + payload.Transition)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	if enable {
		cfg := svc.cfg.Get()
		cfg.JiraWriteBackEnabled = true
		svc.Configure(cfg)
	}
	store := &memIssueStore{projects: map[string]domain.Project{"p1": {ID: "p1", JiraProjectKey: "PROJ"}}}
	return svc, store, rec
}

func jiraDeliveryIssue(state string, backAt time.Time) domain.Issue {
	issue := domain.Issue{
		ID: "jira-PROJ-12", ProjectID: "p1", Source: "jira", Status: "todo",
		ExternalRef: &domain.ExternalRef{Source: "jira", ExternalID: "PROJ-12"},
	}
	issue.SetDeliveryMRs([]domain.DeliveryMR{
		{URL: "https://gitlab.test/group/app/-/merge_requests/9", State: state, Repository: "group/app", JiraBackAt: backAt},
	})
	return issue
}

func TestWriteBackJiraDeliveriesCommentsAndCloses(t *testing.T) {
	svc, store, rec := newJiraWritebackFixture(t, true)
	store.issues = map[string]domain.Issue{"jira-PROJ-12": jiraDeliveryIssue("merged", time.Time{})}
	if n := svc.WriteBackJiraDeliveries(store); n != 1 {
		t.Fatalf("written = %d", n)
	}
	calls := rec.snapshot()
	if !containsEntry(calls, "POST /rest/api/2/issue/PROJ-12/comment") {
		t.Fatalf("no comment call: %v", calls)
	}
	if !containsEntry(calls, "TRANSITION: 31") {
		t.Fatalf("no done transition: %v", calls)
	}
	if !strings.Contains(commentBody(calls), "https://gitlab.test/group/app/-/merge_requests/9") {
		t.Fatalf("comment missing MR URL: %v", commentBody(calls))
	}
	saved, ok := store.Issue("jira-PROJ-12")
	if !ok {
		t.Fatal("issue missing after write-back")
	}
	mrs := saved.DeliveryMRList()
	if len(mrs) != 1 || mrs[0].JiraBackAt.IsZero() {
		t.Fatalf("JiraBackAt not recorded: %+v", mrs)
	}
}

func TestWriteBackJiraDeliveriesIdempotent(t *testing.T) {
	svc, store, rec := newJiraWritebackFixture(t, true)
	backAt := time.Now().UTC().Add(-time.Hour)
	store.issues = map[string]domain.Issue{"jira-PROJ-12": jiraDeliveryIssue("merged", backAt)}
	if n := svc.WriteBackJiraDeliveries(store); n != 0 {
		t.Fatalf("already-written MR should be skipped, written = %d", n)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("unexpected calls: %v", rec.snapshot())
	}
}

func TestWriteBackJiraDeliveriesSkipsOpenMRsAndOtherSources(t *testing.T) {
	svc, store, rec := newJiraWritebackFixture(t, true)
	open := jiraDeliveryIssue("opened", time.Time{})
	local := domain.Issue{ID: "local-1", ProjectID: "p1", Source: "local"}
	local.SetDeliveryMRs([]domain.DeliveryMR{{URL: "https://gitlab.test/g/a/-/merge_requests/1", State: "merged"}})
	store.issues = map[string]domain.Issue{"jira-PROJ-12": open, "local-1": local}
	if n := svc.WriteBackJiraDeliveries(store); n != 0 {
		t.Fatalf("written = %d", n)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("unexpected calls: %v", rec.snapshot())
	}
}

func TestWriteBackJiraDeliveriesRequiresSwitch(t *testing.T) {
	svc, store, rec := newJiraWritebackFixture(t, false)
	store.issues = map[string]domain.Issue{"jira-PROJ-12": jiraDeliveryIssue("merged", time.Time{})}
	if n := svc.WriteBackJiraDeliveries(store); n != 0 {
		t.Fatalf("disabled switch should write nothing, written = %d", n)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("unexpected calls: %v", rec.snapshot())
	}
}

func TestWriteBackJiraDeliveriesSkipsClosedIssues(t *testing.T) {
	svc, store, rec := newJiraWritebackFixture(t, true)
	issue := jiraDeliveryIssue("merged", time.Time{})
	issue.Status = "done"
	store.issues = map[string]domain.Issue{"jira-PROJ-12": issue}
	if n := svc.WriteBackJiraDeliveries(store); n != 1 {
		t.Fatalf("written = %d", n)
	}
	calls := rec.snapshot()
	if containsEntry(calls, "TRANSITION: 31") {
		t.Fatal("closed issue must not transition")
	}
	if !containsEntry(calls, "POST /rest/api/2/issue/PROJ-12/comment") {
		t.Fatal("closed issue should still get the delivery comment")
	}
}

func containsEntry(entries []string, want string) bool {
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}

func commentBody(entries []string) string {
	for _, e := range entries {
		if strings.HasPrefix(e, "COMMENT: ") {
			return strings.TrimPrefix(e, "COMMENT: ")
		}
	}
	return ""
}
