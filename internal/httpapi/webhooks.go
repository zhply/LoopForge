package httpapi

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentflow/internal/tracker"

	"github.com/gin-gonic/gin"
)

const webhookBodyLimit = 4 << 20

// webhookIncoming handles POST /api/webhooks/:source (P4). The path is exempt
// from interactive login (protectedAccessPath) and authenticates itself by
// echoing the platform webhook secret in X-Webhook-Secret; GitLab may use its
// native X-Gitlab-Token header. Secret delivery events are idempotent: MR
// state writes are authoritative overwrites and Jira events re-pull.
func (s *Server) webhookIncoming(c *gin.Context) {
	if s.sync == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sync service unavailable"})
		return
	}
	secret := strings.TrimSpace(s.repo.SyncConfig().WebhookSecret)
	if secret == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "未配置 Webhook Secret，请先在同步设置中生成并保存"})
		return
	}
	got := strings.TrimSpace(c.GetHeader("X-Webhook-Secret"))
	if got == "" {
		got = strings.TrimSpace(c.GetHeader("X-Gitlab-Token"))
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "webhook secret 不匹配"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, webhookBodyLimit))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "webhook payload 读取失败"})
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.Param("source"))) {
	case "gitlab":
		project, iid, state, target, mrURL, mergedAt, ok, err := tracker.ParseGitLabMRWebhook(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusAccepted, gin.H{"status": "ignored"})
			return
		}
		if err := s.sync.ApplyWebhookMRState(s.repo, project, iid, state, target, mergedAt, mrURL); err != nil {
			respond(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "mr": fmt.Sprintf("%s!%d", project, iid), "state": state})
	case "jira":
		key, ok, err := tracker.ParseJiraWebhook(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusAccepted, gin.H{"status": "ignored"})
			return
		}
		if err := s.sync.ApplyJiraWebhook(s.repo, key); err != nil {
			respond(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "issue": key})
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown webhook source"})
	}
}
