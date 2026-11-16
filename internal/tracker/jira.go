package tracker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// jiraClient is a read-only Jira REST v2 client. P1 only imports: issues are
// pulled per project (Project.JiraProjectKey) and status changes never write
// back. Auth is Basic; the account field accepts a Jira Server/Data Center
// username (e.g. 8.13.9, which has no PAT support and signs in with
// username+password) or a Jira Cloud email + API token. Bearer (PAT) is used
// only when the account is empty, which legacy Jira Server does not support.
type jiraClient struct {
	base, email, token string
	http               *http.Client
}

func (s *Service) jiraClient() (*jiraClient, error) {
	cfg := s.cfg.Get()
	base := strings.TrimRight(strings.TrimSpace(cfg.JiraBaseURL), "/")
	if base == "" || strings.TrimSpace(cfg.JiraToken) == "" {
		return nil, fmt.Errorf("请先在同步设置中配置 Jira 地址与账号、密码（或 API Token）")
	}
	return &jiraClient{base: base, email: strings.TrimSpace(cfg.JiraEmail), token: strings.TrimSpace(cfg.JiraToken), http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (j *jiraClient) request(method, path string, query url.Values, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	endpoint := j.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	r, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/json")
	if j.email != "" {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(j.email+":"+j.token)))
	} else {
		r.Header.Set("Authorization", "Bearer "+j.token)
	}
	res, err := j.http.Do(r)
	if err != nil {
		return fmt.Errorf("Jira request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Jira returned HTTP %d; check the Jira account, password/token and permissions", res.StatusCode)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(output)
}

type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Status      struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"statusCategory"`
		} `json:"status"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Created string `json:"created"`
		Updated string `json:"updated"`
	} `json:"fields"`
}

// jiraIssueURL builds the browse URL for one issue key.
func jiraIssueURL(base, key string) string {
	return strings.TrimRight(base, "/") + "/browse/" + url.PathEscape(key)
}

func jiraPriority(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "highest", "blocker":
		return "urgent"
	case "high", "高":
		return "high"
	case "medium", "middle", "中":
		return "medium"
	case "low", "lowest", "低":
		return "low"
	default:
		return "medium"
	}
}

// jiraStatus maps a Jira status onto the local lifecycle. Done-category
// statuses close the item with an external close source; everything else
// lands as todo so the platform lifecycle stays authoritative.
func jiraStatus(issue jiraIssue) string {
	category := strings.ToLower(strings.TrimSpace(issue.Fields.Status.StatusCategory.Key))
	if category == "done" {
		return "closed"
	}
	// Older Jira versions omit statusCategory inside status; fall back to the
	// status name so completed items still close locally.
	switch strings.ToLower(strings.TrimSpace(issue.Fields.Status.Name)) {
	case "done", "closed", "resolved", "已完成", "关闭":
		return "closed"
	}
	return "todo"
}

func jiraJQL(projectKey, extra string) string {
	jql := "project = \"" + projectKey + "\""
	if trimmed := strings.TrimSpace(extra); trimmed != "" {
		jql += " AND (" + trimmed + ")"
	}
	return jql
}

// pullJiraIssues imports every project bound to a Jira project key.
func (s *Service) pullJiraIssues(store IssueStoreAdapter) (int, error) {
	cfg := s.cfg.Get()
	if strings.TrimSpace(cfg.JiraBaseURL) == "" || strings.TrimSpace(cfg.JiraToken) == "" {
		return 0, nil
	}
	client, err := s.jiraClient()
	if err != nil {
		return 0, err
	}
	total := 0
	var lastErr error
	for _, project := range store.Projects() {
		key := strings.TrimSpace(project.JiraProjectKey)
		if key == "" {
			continue
		}
		n, err := s.pullJiraProject(client, store, project.ID, key, cfg.JiraJQL)
		total += n
		if err != nil {
			lastErr = err
		}
	}
	return total, lastErr
}

func (s *Service) pullJiraProject(client *jiraClient, store IssueStoreAdapter, projectID, projectKey, extraJQL string) (int, error) {
	started := time.Now().UTC()
	const pageSize = 50
	count := 0
	for start := 0; start < 1000; start += pageSize {
		var page struct {
			Issues     []jiraIssue `json:"issues"`
			Total      int         `json:"total"`
			StartAt    int         `json:"startAt"`
			MaxResults int         `json:"maxResults"`
		}
		q := url.Values{
			"jql":        {jiraJQL(projectKey, extraJQL)},
			"maxResults": {strconv.Itoa(pageSize)},
			"startAt":    {strconv.Itoa(start)},
			"fields":     {"summary,description,status,priority,created,updated"},
		}
		if err := client.request("GET", "/rest/api/2/search", q, nil, &page); err != nil {
			s.addLog(domain.SyncLog{ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "jira", Action: "sync_issues", Status: "failed", Message: err.Error(), ProjectID: projectID, StartedAt: started, FinishedAt: time.Now().UTC()})
			return count, err
		}
		for _, item := range page.Issues {
			if err := s.upsertJiraIssue(store, projectID, item, false); err != nil {
				return count, err
			}
			count++
		}
		if len(page.Issues) < pageSize {
			break
		}
	}
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "jira",
		Action: "sync_issues", Status: "success", Count: count,
		Message:   fmt.Sprintf("同步 %d 条 Jira issue（%s）", count, projectKey),
		ProjectID: projectID,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	})
	return count, nil
}

// upsertJiraIssue stores one Jira issue as a read-only work item. New items
// adopt the Jira key as their stable id; existing items keep platform-owned
// fields via applyExistingIssue.
func (s *Service) upsertJiraIssue(store IssueStoreAdapter, projectID string, item jiraIssue, manualImport bool) error {
	key := strings.TrimSpace(item.Key)
	if key == "" {
		return fmt.Errorf("jira issue key is empty")
	}
	cfg := s.cfg.Get()
	id := "jira-" + key
	existing, found := store.Issue(id)
	status := jiraStatus(item)
	issue := domain.Issue{
		ID:          id,
		ProjectID:   projectID,
		Kind:        "issue",
		Title:       item.Fields.Summary,
		Description: item.Fields.Description,
		Priority:    jiraPriority(item.Fields.Priority.Name),
		Status:      status,
		Source:      "jira",
		ExternalRef: &domain.ExternalRef{
			Source:        "jira",
			ExternalID:    key,
			ExternalURL:   jiraIssueURL(cfg.JiraBaseURL, key),
			ExternalState: item.Fields.Status.Name,
			ExternalType:  "issue",
			SyncedAt:      time.Now().UTC(),
		},
	}
	if status == "closed" {
		issue.CloseSource = "external"
	}
	if found {
		applyExistingIssue(&issue, existing)
	} else {
		issue.CreatedAt = parseTime(item.Fields.Created)
	}
	issue.UpdatedAt = time.Now().UTC()
	if manualImport {
		return store.PutIssue(issue)
	}
	return store.PutSyncedIssue(issue)
}

// ImportJiraIssue fetches one Jira issue by pasted browse URL or bare key
// (e.g. "PROJ-123") and upserts it into the given project.
func (s *Service) ImportJiraIssue(store IssueStoreAdapter, projectID, raw string) (domain.Issue, error) {
	started := time.Now().UTC()
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.Issue{}, fmt.Errorf("projectId is required")
	}
	if _, ok := store.Project(projectID); !ok {
		return domain.Issue{}, fmt.Errorf("项目 %s 不存在", projectID)
	}
	key, err := ParseJiraIssueRef(raw)
	if err != nil {
		return domain.Issue{}, err
	}
	client, err := s.jiraClient()
	if err != nil {
		s.addLog(domain.SyncLog{ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "jira", Action: "import_issue", Status: "failed", Message: err.Error(), ProjectID: projectID, StartedAt: started, FinishedAt: time.Now().UTC()})
		return domain.Issue{}, err
	}
	var item jiraIssue
	if err := client.request("GET", "/rest/api/2/issue/"+url.PathEscape(key), nil, nil, &item); err != nil {
		s.addLog(domain.SyncLog{ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "jira", Action: "import_issue", Status: "failed", Message: err.Error(), ProjectID: projectID, StartedAt: started, FinishedAt: time.Now().UTC()})
		return domain.Issue{}, err
	}
	if err := s.upsertJiraIssue(store, projectID, item, true); err != nil {
		return domain.Issue{}, err
	}
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "jira",
		Action: "import_issue", Status: "success", Count: 1,
		Message:   fmt.Sprintf("导入 Jira issue %s: %s", key, item.Fields.Summary),
		ProjectID: projectID,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	})
	stored, _ := store.Issue("jira-" + key)
	return stored, nil
}

// ParseJiraIssueRef extracts an issue key from a Jira browse URL or a bare
// "PROJ-123" key. Any host is accepted because self-hosted instances vary;
// free text with a pasted link also works.
func ParseJiraIssueRef(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请粘贴 Jira issue 链接或直接输入 issue key")
	}
	if candidate, ok := jiraURLInText(raw); ok {
		return jiraKeyFromURL(candidate)
	}
	return validateJiraKey(raw)
}

// jiraURLInText finds the first http(s) URL inside free text.
func jiraURLInText(raw string) (string, bool) {
	if _, err := url.Parse(raw); err == nil && (strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")) {
		return raw, true
	}
	match := jiraURLRe.FindString(raw)
	if match == "" {
		return "", false
	}
	return match, true
}

var jiraURLRe = regexp.MustCompile(`https?://[^\s"'<>，。；！？、（）【】]+`)

func jiraKeyFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("链接里找不到 Jira issue key，请使用 issue 详情页地址或直接输入 key（如 PROJ-123）")
	}
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	for i, part := range parts {
		if strings.EqualFold(part, "browse") && i+1 < len(parts) {
			// https://jira.example.com/browse/PROJ-123
			return validateJiraKey(parts[i+1])
		}
		if strings.EqualFold(part, "projects") && i+3 < len(parts) && strings.EqualFold(parts[i+2], "issues") {
			// https://jira.example.com/projects/PROJ/issues/PROJ-123
			return validateJiraKey(parts[i+3])
		}
	}
	return "", fmt.Errorf("链接里找不到 Jira issue key，请使用 issue 详情页地址或直接输入 key（如 PROJ-123）")
}

// jiraKeyRe matches a bare "PROJ-123" issue key.
var jiraKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

func validateJiraKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("Jira issue key 不能为空")
	}
	if !jiraKeyRe.MatchString(key) {
		return "", fmt.Errorf("无法识别 Jira issue key %q，请使用 PROJ-123 形式", key)
	}
	return key, nil
}
