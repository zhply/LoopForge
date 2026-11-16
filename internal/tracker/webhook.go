package tracker

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// ApplyWebhookMRState feeds one inbound GitLab/Gongfeng MR webhook into the
// MR delivery store (P4). The event is authoritative about its own state, so
// the delivery row is written directly; a merged event additionally runs the
// Jira delivery write-back scan so merged announcements do not wait for the
// next polling round. Unknown states are ignored to protect history.
func (s *Service) ApplyWebhookMRState(store MRDeliveryStore, project string, iid int, state, targetBranch string, mergedAt time.Time, mrURL string) error {
	if store == nil {
		return fmt.Errorf("MR store unavailable")
	}
	project = strings.TrimSpace(project)
	state = normalizeMRState(state)
	if project == "" || iid <= 0 || state == "" || state == "unknown" {
		return nil
	}
	if state == "merged" && mergedAt.IsZero() {
		mergedAt = time.Now().UTC()
	}
	if err := store.UpdateMRDeliveryState(project, iid, state, strings.TrimSpace(targetBranch), mergedAt, time.Now().UTC()); err != nil {
		return err
	}
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", time.Now().UTC().UnixNano()), Direction: "pull", Source: "webhook",
		Action: "mr_state", Status: "success", Count: 1,
		Message:   fmt.Sprintf("webhook 更新 MR %s!%d 为 %s", project, iid, state),
		StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	})
	if state == "merged" {
		if iss, ok := store.(IssueStoreAdapter); ok {
			if n := s.WriteBackJiraDeliveries(iss); n > 0 {
				log.Printf("jira write-back: %d work items updated (webhook-triggered)", n)
			}
		}
	}
	return nil
}

// ApplyJiraWebhook re-pulls one Jira issue whose change event arrived (P4).
// Pulling through the REST API keeps the upsert path identical to scheduled
// sync and stays idempotent for duplicate deliveries. Unknown issues (not
// imported yet) are accepted and skipped.
func (s *Service) ApplyJiraWebhook(store IssueStoreAdapter, key string) error {
	key = strings.TrimSpace(key)
	if store == nil || key == "" {
		return nil
	}
	id := "jira-" + strings.ToUpper(key)
	existing, ok := store.Issue(id)
	if !ok {
		return nil
	}
	_, err := s.ImportJiraIssue(store, existing.ProjectID, key)
	return err
}

// GitLabWebhookEvent is the subset of the GitLab merge-request webhook payload
// the platform consumes.
type GitLabWebhookEvent struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		IID          int    `json:"iid"`
		State        string `json:"state"`
		TargetBranch string `json:"target_branch"`
		SourceBranch string `json:"source_branch"`
		URL          string `json:"url"`
		MergedAt     string `json:"merged_at"`
		UpdatedAt    string `json:"updated_at"`
	} `json:"object_attributes"`
}

// ParseGitLabMRWebhook extracts (project, iid, state, target, mergedAt, url)
// from a merge_request webhook payload. ok=false for other event kinds. The
// project keeps the delivery-store form: "gitlab:<host>/<path>" when the MR
// URL reveals the host, the bare Gongfeng path otherwise.
func ParseGitLabMRWebhook(payload []byte) (project string, iid int, state, target, mrURL string, mergedAt time.Time, ok bool, err error) {
	var event GitLabWebhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return "", 0, "", "", "", time.Time{}, false, fmt.Errorf("webhook payload 无效：%v", err)
	}
	if event.ObjectKind != "merge_request" {
		return "", 0, "", "", "", time.Time{}, false, nil
	}
	attrs := event.ObjectAttributes
	path := strings.TrimPrefix(strings.TrimSpace(event.Project.PathWithNamespace), "/")
	if path == "" || attrs.IID <= 0 {
		return "", 0, "", "", "", time.Time{}, false, nil
	}
	if attrs.MergedAt != "" {
		if t, perr := time.Parse(time.RFC3339, attrs.MergedAt); perr == nil {
			mergedAt = t
		}
	}
	mrURL = strings.TrimSpace(attrs.URL)
	if p, _, _, pOK := domain.ParseGongfengMR(mrURL); pOK {
		project = p
	} else {
		project = path
	}
	if mrURL == "" {
		mrURL = domain.GongfengMRURL(project, attrs.IID)
	}
	return project, attrs.IID, attrs.State, attrs.TargetBranch, mrURL, mergedAt, true, nil
}

// JiraWebhookEvent is the subset of the Jira webhook payload the platform
// consumes.
type JiraWebhookEvent struct {
	WebhookEvent string `json:"webhookEvent"`
	Issue        struct {
		Key string `json:"key"`
	} `json:"issue"`
}

// ParseJiraWebhook extracts the issue key from a Jira change webhook payload.
func ParseJiraWebhook(payload []byte) (key string, ok bool, err error) {
	var event JiraWebhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return "", false, fmt.Errorf("webhook payload 无效：%v", err)
	}
	key = strings.TrimSpace(event.Issue.Key)
	return key, key != "", nil
}
