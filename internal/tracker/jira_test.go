package tracker

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/domain"
)

func jiraIssueJSON(key, summary, statusName, category string) string {
	return `{"key":"` + key + `","fields":{"summary":"` + summary + `","description":"desc","status":{"name":"` + statusName + `","statusCategory":{"key":"` + category + `","name":"` + statusName + `"}},"priority":{"name":"High"},"created":"2026-01-02T03:04:05.000+0000","updated":"2026-01-03T03:04:05.000+0000"}}`
}

func newJiraTestService(t *testing.T, handler http.HandlerFunc) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	svc := NewService()
	svc.Configure(domain.SyncConfig{JiraBaseURL: server.URL, JiraEmail: "user@example.com", JiraToken: "token"})
	return svc, server
}

func TestParseJiraIssueRef(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://jira.example.com/browse/PROJ-123", "PROJ-123"},
		{"看看这个 https://jira.example.com/browse/PROJ-123?foo=1 谢谢", "PROJ-123"},
		{"https://jira.example.com/projects/PROJ/issues/PROJ-45", "PROJ-45"},
		{"PROJ-9", "PROJ-9"},
		{"proj-9", "proj-9"},
	}
	for _, c := range cases {
		got, err := ParseJiraIssueRef(c.raw)
		if err != nil || got != c.want {
			t.Fatalf("ParseJiraIssueRef(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
	for _, raw := range []string{"", "PROJ", "PROJ-", "-1", "https://jira.example.com/other", "PROJ-X"} {
		if _, err := ParseJiraIssueRef(raw); err == nil {
			t.Fatalf("ParseJiraIssueRef(%q) should fail", raw)
		}
	}
}

func TestImportJiraIssue(t *testing.T) {
	var mu sync.Mutex
	var sawAuth, sawPath string
	svc, _ := newJiraTestService(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawAuth, sawPath = r.Header.Get("Authorization"), r.URL.Path
		mu.Unlock()
		_, _ = w.Write([]byte(jiraIssueJSON("PROJ-123", "Fix login flow", "In Progress", "indeterminate")))
	})
	store := &memIssueStore{projects: map[string]domain.Project{"p1": {ID: "p1", JiraProjectKey: "PROJ"}}}
	issue, err := svc.ImportJiraIssue(store, "p1", "https://jira.example.com/browse/PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	auth, path := sawAuth, sawPath
	mu.Unlock()
	if want := "Basic " + basicAuth("user@example.com:token"); auth != want {
		t.Fatalf("auth header = %q; want %q", auth, want)
	}
	if !strings.HasPrefix(path, "/rest/api/2/issue/") {
		t.Fatalf("request path = %q", path)
	}
	if issue.ID != "jira-PROJ-123" {
		t.Fatalf("issue ID = %q", issue.ID)
	}
	if issue.Source != "jira" || issue.Status != "todo" || issue.Priority != "high" {
		t.Fatalf("issue = %+v", issue)
	}
	if issue.ExternalRef == nil || issue.ExternalRef.ExternalID != "PROJ-123" || !strings.HasSuffix(issue.ExternalRef.ExternalURL, "/browse/PROJ-123") {
		t.Fatalf("external ref = %+v", issue.ExternalRef)
	}
	if _, ok := store.Issue("jira-PROJ-123"); !ok {
		t.Fatal("imported issue not stored")
	}
}

func TestImportJiraIssueClosedStartsClosed(t *testing.T) {
	svc, _ := newJiraTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(jiraIssueJSON("PROJ-7", "Done item", "Done", "done")))
	})
	store := &memIssueStore{projects: map[string]domain.Project{"p1": {ID: "p1"}}}
	issue, err := svc.ImportJiraIssue(store, "p1", "PROJ-7")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != "closed" || issue.CloseSource != "external" {
		t.Fatalf("status=%q closeSource=%q", issue.Status, issue.CloseSource)
	}
}

func TestPullJiraIssues(t *testing.T) {
	var mu sync.Mutex
	var sawJQL string
	svc, _ := newJiraTestService(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawJQL = r.URL.Query().Get("jql")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"issues":[` + jiraIssueJSON("PROJ-1", "One", "To Do", "todo") + `,` + jiraIssueJSON("PROJ-2", "Two", "Done", "done") + `],"total":2,"startAt":0,"maxResults":50}`))
	})
	store := &memIssueStore{
		projects: map[string]domain.Project{
			"p1": {ID: "p1", JiraProjectKey: "PROJ"},
			"p2": {ID: "p2"},
		},
	}
	count, err := svc.pullJiraIssues(store)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d", count)
	}
	mu.Lock()
	jql := sawJQL
	mu.Unlock()
	if !strings.Contains(jql, `project = "PROJ"`) {
		t.Fatalf("jql = %q", jql)
	}
	if issue, ok := store.Issue("jira-PROJ-2"); !ok || issue.Status != "closed" {
		t.Fatalf("PROJ-2 = %+v ok=%v", issue, ok)
	}
}

func TestPullJiraIssuesNeedsConfig(t *testing.T) {
	svc := NewService()
	store := &memIssueStore{projects: map[string]domain.Project{"p1": {ID: "p1", JiraProjectKey: "PROJ"}}}
	count, err := svc.pullJiraIssues(store)
	if err != nil || count != 0 {
		t.Fatalf("unconfigured jira should be a no-op, got %d, %v", count, err)
	}
}

func TestPullJiraIssuesJQLFilter(t *testing.T) {
	var mu sync.Mutex
	var sawJQL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawJQL = r.URL.Query().Get("jql")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer server.Close()
	svc := NewService()
	svc.Configure(domain.SyncConfig{JiraBaseURL: server.URL, JiraToken: "pat", JiraJQL: "status != Done"})
	store := &memIssueStore{projects: map[string]domain.Project{"p1": {ID: "p1", JiraProjectKey: "PROJ"}}}
	if _, err := svc.pullJiraIssues(store); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	jql := sawJQL
	mu.Unlock()
	if !strings.Contains(jql, "status != Done") {
		t.Fatalf("jql = %q", jql)
	}
}

func basicAuth(userPass string) string {
	// Matches the encoding used by jiraClient (base64 of user:token).
	return base64.StdEncoding.EncodeToString([]byte(userPass))
}
