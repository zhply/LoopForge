package tracker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/domain"
)

type recordedGitLabRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// gitlabRequests records requests from the httptest handler goroutine. A mutex
// keeps the assertions on the test goroutine race-free.
type gitlabRequests struct {
	mu    sync.Mutex
	items []recordedGitLabRequest
}

func (r *gitlabRequests) add(req recordedGitLabRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, req)
}

func (r *gitlabRequests) setLastBody(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) > 0 {
		r.items[len(r.items)-1].Body = body
	}
}

func (r *gitlabRequests) snapshot() []recordedGitLabRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedGitLabRequest(nil), r.items...)
}

// newGitLabTestServer serves a minimal GitLab REST v4 surface. mergeResponses
// maps PUT .../merge_requests/<iid>/merge to a fixed response body.
func newGitLabTestServer(t *testing.T, mergeResponses map[int]string, list func(query string) []string) (*httptest.Server, *gitlabRequests) {
	t.Helper()
	requests := &gitlabRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(recordedGitLabRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			items := list(r.URL.RawQuery)
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			body, _ := json.Marshal(readGitLabPayload(r))
			requests.setLastBody(string(body))
			_, _ = w.Write([]byte(`{"iid":7,"state":"opened","target_branch":"main","source_branch":"work/1","web_url":"https://gitlab.test/group/app/-/merge_requests/7"}`))
			return
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/merge"), "/")
			iid, _ := strconv.Atoi(parts[len(parts)-1])
			body, _ := json.Marshal(readGitLabPayload(r))
			requests.setLastBody(string(body))
			if resp, ok := mergeResponses[iid]; ok {
				_, _ = w.Write([]byte(resp))
				return
			}
			_, _ = w.Write([]byte(`{"iid":` + strconv.Itoa(iid) + `,"state":"merged"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func readGitLabPayload(r *http.Request) map[string]any {
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	return payload
}

func gitlabMRJSON(iid int, state, source, target string) string {
	return `{"iid":` + strconv.Itoa(iid) + `,"state":"` + state + `","source_branch":"` + source + `","target_branch":"` + target + `","web_url":"https://gitlab.test/group/app/-/merge_requests/` + strconv.Itoa(iid) + `"}`
}

func TestEnsureAndMergeBranchMRGitLab(t *testing.T) {
	server, requests := newGitLabTestServer(t, map[int]string{}, func(query string) []string {
		return []string{gitlabMRJSON(7, "opened", "work/1", "main")}
	})
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	ref, err := svc.EnsureAndMergeBranchMR("gitlab:gitlab.test/group/app", "work/1", "main", "merge title", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "!7" {
		t.Fatalf("ref = %q", ref)
	}
	items := requests.snapshot()
	if len(items) < 2 || !strings.Contains(items[0].Query, "source_branch=work%2F1") {
		t.Fatalf("requests = %+v", items)
	}
	merged := false
	for _, req := range items {
		if req.Method == http.MethodPut && strings.Contains(req.Path, "/merge") {
			merged = true
			if !strings.Contains(req.Body, "merge title") {
				t.Fatalf("merge payload = %s", req.Body)
			}
		}
	}
	if !merged {
		t.Fatal("merge PUT not called")
	}
}

func TestEnsureAndMergeBranchMRGitLabCreatesWhenMissing(t *testing.T) {
	server, requests := newGitLabTestServer(t, map[int]string{}, func(query string) []string { return nil })
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	ref, err := svc.EnsureAndMergeBranchMR("gitlab:gitlab.test/group/app", "work/1", "main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "!7" {
		t.Fatalf("ref = %q", ref)
	}
	created := false
	for _, req := range requests.snapshot() {
		if req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/merge_requests") {
			created = true
			if !strings.Contains(req.Body, "work/1") || !strings.Contains(req.Body, "main") {
				t.Fatalf("create payload = %s", req.Body)
			}
		}
	}
	if !created {
		t.Fatal("create POST not called")
	}
}

func TestEnsureAndMergeBranchMRGitLabNoTargetWithoutOpenMR(t *testing.T) {
	server, _ := newGitLabTestServer(t, map[int]string{}, func(query string) []string { return nil })
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	if _, err := svc.EnsureAndMergeBranchMR("gitlab:gitlab.test/group/app", "work/1", "", "", ""); err != ErrNoOpenMR {
		t.Fatalf("err = %v", err)
	}
}

func TestFindBranchMRGitLab(t *testing.T) {
	server, _ := newGitLabTestServer(t, map[int]string{}, func(query string) []string {
		return []string{gitlabMRJSON(3, "opened", "feat/x", "main"), gitlabMRJSON(2, "closed", "feat/x", "main")}
	})
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	mr, ok, err := svc.FindBranchMR("gitlab:gitlab.test/group/app", "feat/x")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if mr.IID != 3 || mr.Project != "gitlab:gitlab.test/group/app" || mr.TargetBranch != "main" || mr.GongfengState != "opened" {
		t.Fatalf("mr = %+v", mr)
	}
	if mr.URL != "https://gitlab.test/group/app/-/merge_requests/3" {
		t.Fatalf("url = %q", mr.URL)
	}
}

func TestRefreshGitLabDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"iid":9,"state":"merged","source_branch":"work/2","target_branch":"main","merged_at":"2026-01-05T00:00:00Z","web_url":"https://gitlab.test/group/app/-/merge_requests/9"}`))
	}))
	defer server.Close()
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	store := &fakeMRDeliveryStore{}
	if err := svc.refreshGitLabDelivery(store, "gitlab:gitlab.test/group/app", 9); err != nil {
		t.Fatal(err)
	}
	if len(store.updates) != 1 {
		t.Fatalf("updates = %+v", store.updates)
	}
	update := store.updates[0]
	if update.state != "merged" || update.targetBranch != "main" || update.mergedAt.IsZero() {
		t.Fatalf("update = %+v", update)
	}
}

func TestSyncMRDeliveryStatesRefreshesGitLabRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"iid":9,"state":"merged","source_branch":"work/2","target_branch":"main","merged_at":"2026-01-05T00:00:00Z"}`))
	}))
	defer server.Close()
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	store := &fakeMRDeliveryStore{pending: []domain.MRDeliveryState{{MRProject: "gitlab:gitlab.test/group/app", MRIID: 9, State: "unknown"}}}
	svc.syncMRDeliveryStates(store)
	if len(store.updates) != 1 || store.updates[0].state != "merged" {
		t.Fatalf("updates = %+v", store.updates)
	}
}

func TestPullGitLabIssues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("page") == "1" || r.URL.Query().Get("page") == "" {
			_, _ = w.Write([]byte(`[{"id":11,"iid":1,"title":"Bug one","description":"d1","state":"opened","web_url":"https://gitlab.test/group/app/-/issues/1","created_at":"2026-01-02T03:04:05Z"},{"id":12,"iid":2,"title":"Bug two","state":"closed","web_url":"https://gitlab.test/group/app/-/issues/2","created_at":"2026-01-02T03:04:05Z"}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	svc := NewService()
	svc.gitlabAPIBase = server.URL + "/api/v4"
	store := &memIssueStore{
		repositories: map[string]domain.Repository{"r1": {ID: "r1", ProjectID: "p1", Provider: "gitlab", GongfengProjectID: "gitlab:gitlab.test/group/app", DefaultBranch: "main"}},
		projects:     map[string]domain.Project{"p1": {ID: "p1"}},
	}
	count, err := svc.pullGitLabIssues(store, store.repositories["r1"])
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d", count)
	}
	issue, ok := store.Issue("gl-r1-1")
	if !ok || issue.Source != "gitlab" || issue.Status != "todo" || issue.Title != "Bug one" || issue.Description != "d1" {
		t.Fatalf("issue = %+v ok=%v", issue, ok)
	}
	if issue.ExternalRef == nil || issue.ExternalRef.ExternalIID != 1 {
		t.Fatalf("external ref = %+v", issue.ExternalRef)
	}
	closed, ok := store.Issue("gl-r1-2")
	if !ok || closed.Status != "closed" {
		t.Fatalf("closed = %+v ok=%v", closed, ok)
	}
}
