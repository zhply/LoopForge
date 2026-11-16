package tracker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/domain"
)

// Legacy Confluence Server/Data Center (no PAT) signs in with username +
// password; a non-email account must ride the same Basic header as Cloud
// email + API token.
func TestPublishConfluencePageServerUsernameBasicAuth(t *testing.T) {
	var mu sync.Mutex
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawAuth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/rest/api/content" {
			_, _ = w.Write([]byte(`{"id":"300","title":"Fresh plan"}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(server.Close)
	svc := NewService()
	svc.Configure(domain.SyncConfig{ConfluenceBaseURL: server.URL, ConfluenceEmail: "ops-user", ConfluenceToken: "p@ssw0rd"})
	if _, _, err := svc.PublishConfluencePage("", "PROJ", "Fresh plan", "# plan"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	auth := sawAuth
	mu.Unlock()
	if want := "Basic " + basicAuth("ops-user:p@ssw0rd"); auth != want {
		t.Fatalf("server basic auth = %q; want %q", auth, want)
	}
}

// recordedConfluenceCall is one captured REST call from the handler goroutine.
type recordedConfluenceCall struct {
	Method string
	Path   string
	Query  string
	Body   string
}

type confluenceRecorder struct {
	mu    sync.Mutex
	calls []recordedConfluenceCall
}

func (r *confluenceRecorder) add(c recordedConfluenceCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *confluenceRecorder) snapshot() []recordedConfluenceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedConfluenceCall(nil), r.calls...)
}

func newConfluenceTestService(t *testing.T, handle func(c *confluenceRecorder, w http.ResponseWriter, r *http.Request) bool) (*Service, *confluenceRecorder) {
	t.Helper()
	rec := &confluenceRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedConfluenceCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		if handle != nil && handle(rec, w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/content/search"):
			if strings.Contains(r.URL.Query().Get("cql"), `title="Existing plan"`) {
				_, _ = w.Write([]byte(`{"results":[{"id":"100","title":"Existing plan","version":{"number":4}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/content/"):
			_, _ = w.Write([]byte(`{"id":"100","title":"Existing plan","version":{"number":4}}`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/content":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			body, _ := json.Marshal(map[string]any{"payload": payload})
			rec.add(recordedConfluenceCall{Method: "POST-PAYLOAD", Path: r.URL.Path, Body: string(body)})
			_, _ = w.Write([]byte(`{"id":"200","title":"created","_links":{"webui":"/display/PROJ/created"}}`))
			return
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rest/api/content/"):
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			body, _ := json.Marshal(map[string]any{"payload": payload})
			rec.add(recordedConfluenceCall{Method: "PUT-PAYLOAD", Path: r.URL.Path, Body: string(body)})
			_, _ = w.Write([]byte(`{"id":"100","title":"updated","version":{"number":5},"_links":{"webui":"/display/PROJ/updated"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	svc := NewService()
	svc.Configure(domain.SyncConfig{ConfluenceBaseURL: server.URL, ConfluenceEmail: "user@example.com", ConfluenceToken: "token"})
	return svc, rec
}

func payloadOf(t *testing.T, rec *confluenceRecorder, method string) map[string]any {
	t.Helper()
	for _, c := range rec.snapshot() {
		if c.Method == method {
			var wrapper struct {
				Payload map[string]any `json:"payload"`
			}
			if json.Unmarshal([]byte(c.Body), &wrapper) != nil {
				t.Fatalf("payload = %s", c.Body)
			}
			return wrapper.Payload
		}
	}
	t.Fatalf("%s payload not captured", method)
	return nil
}

func TestConfluenceStorageFromMarkdown(t *testing.T) {
	md := "# 计划\n\n第一段 with **bold** and `code`。\n\n- 条目一\n- 条目二\n\n1. 步骤 A\n2. 步骤 B\n\n```go\nfmt.Println(\"hi\")\n```\n\n[链接](https://example.com/x) 结尾 <script>alert(1)</script>"
	storage := ConfluenceStorageFromMarkdown(md)
	for _, want := range []string{
		"<h1>计划</h1>",
		"<strong>bold</strong>",
		"<code>code</code>",
		"<ul><li>条目一</li><li>条目二</li></ul>",
		"<ol><li>步骤 A</li><li>步骤 B</li></ol>",
		`ac:name="code"`,
		`<![CDATA[fmt.Println("hi")]]>`,
		`<a href="https://example.com/x">链接</a>`,
		"&lt;script&gt;",
	} {
		if !strings.Contains(storage, want) {
			t.Fatalf("storage missing %q:\n%s", want, storage)
		}
	}
	if strings.Contains(storage, "<script>") {
		t.Fatalf("storage must not contain raw script tags:\n%s", storage)
	}
	if !strings.HasPrefix(storage, "<") || !strings.HasSuffix(storage, ">") {
		t.Fatalf("storage should be XHTML:\n%s", storage)
	}
	if got := ConfluenceStorageFromMarkdown("  \n "); got != "<p></p>" {
		t.Fatalf("empty md = %q", got)
	}
}

func TestPublishConfluencePageCreates(t *testing.T) {
	svc, rec := newConfluenceTestService(t, nil)
	pageID, pageURL, err := svc.PublishConfluencePage("", "PROJ", "产品方案", "# 方案\n\n内容")
	if err != nil {
		t.Fatal(err)
	}
	if pageID != "200" {
		t.Fatalf("pageID = %q", pageID)
	}
	if !strings.Contains(pageURL, "/display/PROJ/created") {
		t.Fatalf("pageURL = %q", pageURL)
	}
	payload := payloadOf(t, rec, "POST-PAYLOAD")
	if payload["type"] != "page" {
		t.Fatalf("payload = %+v", payload)
	}
	space, _ := payload["space"].(map[string]any)
	if space == nil || space["key"] != "PROJ" {
		t.Fatalf("space = %+v", payload["space"])
	}
	body, _ := payload["body"].(map[string]any)
	storageMap, _ := body["storage"].(map[string]any)
	if storageMap["representation"] != "storage" || !strings.Contains(storageMap["value"].(string), "<h1>方案</h1>") {
		t.Fatalf("body = %+v", payload["body"])
	}
}

func TestPublishConfluencePageUpdatesByPageID(t *testing.T) {
	svc, rec := newConfluenceTestService(t, nil)
	pageID, pageURL, err := svc.PublishConfluencePage("100", "PROJ", "更新标题", "# 更新")
	if err != nil {
		t.Fatal(err)
	}
	if pageID != "100" {
		t.Fatalf("pageID = %q", pageID)
	}
	if pageURL == "" || !strings.Contains(pageURL, "display/PROJ/updated") {
		t.Fatalf("pageURL = %q", pageURL)
	}
	payload := payloadOf(t, rec, "PUT-PAYLOAD")
	version, _ := payload["version"].(map[string]any)
	if version["number"].(float64) != 5 {
		t.Fatalf("version = %+v", payload["version"])
	}
	if payload["title"] != "更新标题" {
		t.Fatalf("title = %+v", payload["title"])
	}
	// No search when the page id is known.
	for _, c := range rec.snapshot() {
		if strings.Contains(c.Path, "/search") {
			t.Fatal("search should be skipped for a known page id")
		}
	}
}

func TestPublishConfluencePageUpdatesByTitle(t *testing.T) {
	svc, rec := newConfluenceTestService(t, nil)
	pageID, _, err := svc.PublishConfluencePage("", "PROJ", "Existing plan", "# 复用")
	if err != nil {
		t.Fatal(err)
	}
	if pageID != "100" {
		t.Fatalf("pageID = %q", pageID)
	}
	payload := payloadOf(t, rec, "PUT-PAYLOAD")
	version, _ := payload["version"].(map[string]any)
	if version["number"].(float64) != 5 {
		t.Fatalf("version = %+v", payload["version"])
	}
}

func TestPublishConfluencePageNeedsConfig(t *testing.T) {
	svc := NewService()
	if _, _, err := svc.PublishConfluencePage("", "PROJ", "标题", "正文"); err == nil {
		t.Fatal("unconfigured confluence should fail")
	}
}

func TestConfluenceCQLEscaping(t *testing.T) {
	if got := cqlEscape(`a"b\c`); got != `a\"b\\c` {
		t.Fatalf("cqlEscape = %q", got)
	}
}
