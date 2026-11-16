package httpapi

import (
	"net/http"
	"strings"

	"agentflow/internal/domain"

	"github.com/gin-gonic/gin"
)

// publishConfluence publishes a requirement's product doc to a Confluence page
// (create or version-bump) and records the page pointer on the issue. The doc
// body comes from the request, else the newest doc artifact in the
// requirement's conversation space, else the issue description.
func (h issuesHandlers) publishConfluence(c *gin.Context, issue domain.Issue) {
	var req struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		SpaceKey string `json:"spaceKey"`
	}
	if !decode(c, &req) {
		return
	}
	if h.sync == nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "Confluence 同步未配置"})
		return
	}
	cfg := h.repo.SyncConfig()
	if strings.TrimSpace(cfg.ConfluenceBaseURL) == "" || strings.TrimSpace(cfg.ConfluenceToken) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "请先在同步设置中配置 Confluence 地址与 API Token"})
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(issue.Title)
	}
	markdown := strings.TrimSpace(req.Content)
	if markdown == "" {
		markdown = h.confluenceDocFromSpace(issue)
	}
	if markdown == "" {
		markdown = strings.TrimSpace(issue.Description)
	}
	if markdown == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "没有可发布的方案内容：请先在需求空间生成 doc 产物，或直接传入 content"})
		return
	}
	spaceKey := strings.TrimSpace(req.SpaceKey)
	if spaceKey == "" {
		spaceKey = strings.TrimSpace(cfg.ConfluenceSpaceKey)
	}
	h.configureSync(cfg)
	pageID, pageURL, err := h.sync.PublishConfluencePage(issue.ConfluencePageID, spaceKey, title, markdown)
	if err != nil {
		respond(c, err)
		return
	}
	issue.ConfluencePageID = pageID
	issue.ConfluencePageURL = pageURL
	if err := h.repo.PutIssue(issue); err != nil {
		respond(c, err)
		return
	}
	saved, err := h.appendStoredOperation(c, issue.ID, "publish-confluence", "已发布产品方案到 Confluence："+pageURL)
	if err != nil {
		respond(c, err)
		return
	}
	h.writeIssueView(c, http.StatusOK, saved)
}

// confluenceDocFromSpace returns the newest publishable doc artifact body in
// the conversation space carrying the issue, if any.
func (h issuesHandlers) confluenceDocFromSpace(issue domain.Issue) string {
	space, ok := h.repo.SpaceByIssue(issue.ID)
	if !ok {
		return ""
	}
	var best domain.SpaceArtifact
	for _, a := range h.repo.SpaceArtifacts(space.ID) {
		if a.Kind != "doc" || strings.TrimSpace(a.Body) == "" {
			continue
		}
		if best.ID == "" || a.UpdatedAt.After(best.UpdatedAt) {
			best = a
		}
	}
	return strings.TrimSpace(best.Body)
}
