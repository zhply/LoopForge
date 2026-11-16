package tracker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// confluenceClient publishes product docs to a Confluence Server/Data Center
// instance (REST v1; the API paths live directly under /rest/api, no /wiki
// prefix) using the same credentials model as the Jira client: Basic auth
// where the account accepts a legacy Server/Data Center username + password
// or a Cloud email + API token. Bearer (PAT) is used only when the account is
// empty, which legacy Confluence Server does not support.
type confluenceClient struct {
	base, email, token string
	http               *http.Client
}

func (s *Service) confluenceClient() (*confluenceClient, error) {
	cfg := s.cfg.Get()
	base := strings.TrimRight(strings.TrimSpace(cfg.ConfluenceBaseURL), "/")
	if base == "" || strings.TrimSpace(cfg.ConfluenceToken) == "" {
		return nil, fmt.Errorf("请先在同步设置中配置 Confluence 地址与账号、密码（或 API Token）")
	}
	return &confluenceClient{base: base, email: strings.TrimSpace(cfg.ConfluenceEmail), token: strings.TrimSpace(cfg.ConfluenceToken), http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (k *confluenceClient) request(method, path string, query url.Values, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	endpoint := k.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	r, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/json")
	if k.email != "" {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(k.email+":"+k.token)))
	} else {
		r.Header.Set("Authorization", "Bearer "+k.token)
	}
	res, err := k.http.Do(r)
	if err != nil {
		return fmt.Errorf("Confluence request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("Confluence returned HTTP %d; check the Confluence account, password/token and space permissions", res.StatusCode)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(output)
}

type confluenceVersion struct {
	Number int `json:"number"`
}

type confluencePage struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Version confluenceVersion `json:"version"`
	Body    struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

// confluencePageURL resolves the browse URL; Confluence Server returns a
// relative webui path under _links.base.
func confluencePageURL(base string, page confluencePage) string {
	if strings.HasPrefix(page.Links.WebUI, "http://") || strings.HasPrefix(page.Links.WebUI, "https://") {
		return page.Links.WebUI
	}
	if page.Links.WebUI != "" {
		return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(page.Links.WebUI, "/")
	}
	return strings.TrimRight(base, "/") + "/pages/viewpage.action?pageId=" + page.ID
}

func (k *confluenceClient) getPage(pageID string) (confluencePage, error) {
	var page confluencePage
	err := k.request("GET", "/rest/api/content/"+url.PathEscape(pageID), url.Values{"expand": {"version,body.storage"}}, nil, &page)
	if err != nil {
		return confluencePage{}, err
	}
	if page.ID == "" {
		return confluencePage{}, fmt.Errorf("Confluence 页面 %s 不存在或无权限", pageID)
	}
	return page, nil
}

// searchPageByTitle finds one page by exact title inside a space via CQL.
func (k *confluenceClient) searchPageByTitle(spaceKey, title string) (confluencePage, bool, error) {
	cql := fmt.Sprintf(`type=page AND space=%q AND title=%q`, cqlEscape(spaceKey), cqlEscape(title))
	q := url.Values{"cql": {cql}, "expand": {"version"}, "limit": {"1"}}
	var res struct {
		Results []confluencePage `json:"results"`
	}
	if err := k.request("GET", "/rest/api/content/search", q, nil, &res); err != nil {
		return confluencePage{}, false, err
	}
	if len(res.Results) == 0 || res.Results[0].ID == "" {
		return confluencePage{}, false, nil
	}
	return res.Results[0], true, nil
}

func (k *confluenceClient) createPage(spaceKey, title, storage string) (confluencePage, error) {
	var created confluencePage
	payload := map[string]any{
		"type":  "page",
		"title": title,
		"space": map[string]any{"key": spaceKey},
		"body":  map[string]any{"storage": map[string]any{"value": storage, "representation": "storage"}},
	}
	if err := k.request("POST", "/rest/api/content", nil, payload, &created); err != nil {
		return confluencePage{}, err
	}
	if created.ID == "" {
		return confluencePage{}, fmt.Errorf("Confluence 创建页面未返回页面 ID")
	}
	return created, nil
}

func (k *confluenceClient) updatePage(pageID string, version int, title, storage string) (confluencePage, error) {
	var updated confluencePage
	payload := map[string]any{
		"id":      pageID,
		"type":    "page",
		"title":   title,
		"version": map[string]any{"number": version + 1},
		"body":    map[string]any{"storage": map[string]any{"value": storage, "representation": "storage"}},
	}
	if err := k.request("PUT", "/rest/api/content/"+url.PathEscape(pageID), nil, payload, &updated); err != nil {
		return confluencePage{}, err
	}
	return updated, nil
}

// cqlEscape makes a literal safe inside a CQL double-quoted string.
func cqlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// PublishConfluencePage creates or updates the product-doc page for one
// requirement. A known pageID (or an exact-title hit in the space) updates the
// existing page with version+1; otherwise a page is created. It returns the
// resolved page id and browse URL.
func (s *Service) PublishConfluencePage(pageID, spaceKey, title, markdown string) (string, string, error) {
	started := time.Now().UTC()
	pageID = strings.TrimSpace(pageID)
	spaceKey = strings.TrimSpace(spaceKey)
	title = strings.TrimSpace(title)
	if title == "" {
		return "", "", fmt.Errorf("页面标题不能为空")
	}
	if spaceKey == "" {
		return "", "", fmt.Errorf("请配置 Confluence Space Key")
	}
	client, err := s.confluenceClient()
	if err != nil {
		return "", "", err
	}
	storage := ConfluenceStorageFromMarkdown(markdown)
	var page confluencePage
	if pageID != "" {
		if page, err = client.getPage(pageID); err != nil {
			return "", "", err
		}
	} else if existing, ok, searchErr := client.searchPageByTitle(spaceKey, title); searchErr != nil {
		return "", "", searchErr
	} else if ok {
		pageID = existing.ID
	}
	if pageID == "" {
		page, err = client.createPage(spaceKey, title, storage)
	} else {
		if page.ID == "" {
			if page, err = client.getPage(pageID); err != nil {
				return "", "", err
			}
		}
		page, err = client.updatePage(page.ID, page.Version.Number, title, storage)
	}
	if err != nil {
		s.addLog(domain.SyncLog{ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "push", Source: "confluence", Action: "publish_doc", Status: "failed", Message: err.Error(), StartedAt: started, FinishedAt: time.Now().UTC()})
		return "", "", err
	}
	pageURL := confluencePageURL(client.base, page)
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "push", Source: "confluence",
		Action: "publish_doc", Status: "success", Count: 1,
		Message:   fmt.Sprintf("已发布 Confluence 页面：%s", title),
		StartedAt: started, FinishedAt: time.Now().UTC(),
	})
	return page.ID, pageURL, nil
}

// ConfluenceStorageFromMarkdown converts a markdown product doc into
// Confluence storage format (XHTML). The input is HTML-escaped first so the
// converted output cannot inject markup; markdown syntax is then applied on
// the escaped text. Supported blocks: headings, fenced code, ul/ol lists,
// blockquotes and paragraphs; inline: bold, italic, code, links.
func ConfluenceStorageFromMarkdown(md string) string {
	if strings.TrimSpace(md) == "" {
		return "<p></p>"
	}
	var out strings.Builder
	paragraph := false
	inList := 0
	listTag := ""
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	closeParagraph := func() {
		if paragraph {
			out.WriteString("</p>")
			paragraph = false
		}
	}
	closeList := func() {
		if inList > 0 && listTag != "" {
			out.WriteString("</" + listTag + ">")
			inList = 0
			listTag = ""
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			closeParagraph()
			closeList()
		case strings.HasPrefix(trimmed, "```"):
			closeParagraph()
			closeList()
			lang := strings.TrimPrefix(trimmed, "```")
			var code strings.Builder
			for i+1 < len(lines) {
				i++
				if strings.TrimSpace(lines[i]) == "```" {
					break
				}
				if code.Len() > 0 {
					code.WriteString("\n")
				}
				code.WriteString(lines[i])
			}
			out.WriteString(`<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">` + html.EscapeString(strings.TrimSpace(lang)) + `</ac:parameter><ac:plain-text-body><![CDATA[` + code.String() + `]]></ac:plain-text-body></ac:structured-macro>`)
		case strings.HasPrefix(trimmed, "#"):
			closeParagraph()
			closeList()
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 6 {
				level = 6
			}
			text := confluenceInline(strings.TrimSpace(trimmed[level:]))
			tag := fmt.Sprintf("h%d", level)
			out.WriteString("<" + tag + ">" + text + "</" + tag + ">")
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* "):
			closeParagraph()
			if inList == 0 || listTag != "ul" {
				closeList()
				out.WriteString("<ul>")
				listTag = "ul"
			}
			inList = 1
			out.WriteString("<li>" + confluenceInline(strings.TrimSpace(trimmed[2:])) + "</li>")
		default:
			if m := orderedListRe.FindStringSubmatch(trimmed); m != nil {
				closeParagraph()
				if inList == 0 || listTag != "ol" {
					closeList()
					out.WriteString("<ol>")
					listTag = "ol"
				}
				inList = 1
				out.WriteString("<li>" + confluenceInline(strings.TrimSpace(m[1])) + "</li>")
			} else {
				closeList()
				if !paragraph {
					out.WriteString("<p>")
					paragraph = true
				} else {
					out.WriteString("<br/>")
				}
				out.WriteString(confluenceInline(trimmed))
			}
		}
	}
	closeParagraph()
	closeList()
	return out.String()
}

var orderedListRe = regexp.MustCompile(`^\d+[.)]\s+(.*)$`)

// confluenceInline renders markdown inline syntax onto HTML-escaped text.
func confluenceInline(text string) string {
	escaped := html.EscapeString(text)
	escaped = inlineCodeRe.ReplaceAllString(escaped, "<code>$1</code>")
	escaped = boldRe.ReplaceAllString(escaped, "<strong>$1</strong>")
	escaped = italicRe.ReplaceAllString(escaped, "$1<em>$2</em>$3")
	escaped = linkRe.ReplaceAllString(escaped, `<a href="$2">$1</a>`)
	return escaped
}

var (
	inlineCodeRe = regexp.MustCompile("`([^`]+)`")
	boldRe       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRe     = regexp.MustCompile(`(^|[^*])\*([^*\s][^*]+?)\*([^*]|$)`)
	linkRe       = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
)
