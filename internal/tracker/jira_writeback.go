package tracker

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// WriteBackJiraDeliveries announces merged delivery MRs on their source Jira
// issues (P3). For every Jira-sourced work item whose delivery MR reached the
// default branch and has not been announced yet, it posts one delivery comment
// and, when the item is still open, moves it through the first done-category
// transition. Idempotent via DeliveryMR.JiraBackAt; gated by
// SyncConfig.JiraWriteBackEnabled. It returns the number of work items whose
// delivery comment was posted.
func (s *Service) WriteBackJiraDeliveries(store IssueStoreAdapter) int {
	if s == nil || store == nil {
		return 0
	}
	cfg := s.cfg.Get()
	if !cfg.JiraWriteBackEnabled {
		return 0
	}
	client, err := s.jiraClient()
	if err != nil {
		log.Printf("jira write-back: %v", err)
		return 0
	}
	written := 0
	for _, issue := range store.Issues() {
		key := jiraIssueKeyOf(issue)
		if key == "" {
			continue
		}
		pending := pendingJiraBackMRs(issue)
		if len(pending) == 0 {
			continue
		}
		if err := s.writeBackJiraIssue(client, store, issue, key, pending); err != nil {
			log.Printf("jira write-back %s: %v", key, err)
			continue
		}
		written++
	}
	return written
}

// jiraIssueKeyOf returns the Jira key for a synced work item, empty for
// locally created or other-tracker items.
func jiraIssueKeyOf(issue domain.Issue) string {
	if strings.EqualFold(issue.Source, "jira") && issue.ExternalRef != nil {
		return strings.TrimSpace(issue.ExternalRef.ExternalID)
	}
	return ""
}

// pendingJiraBackMRs lists delivered MRs that have not been announced on Jira.
func pendingJiraBackMRs(issue domain.Issue) []domain.DeliveryMR {
	var out []domain.DeliveryMR
	for _, mr := range issue.DeliveryMRList() {
		state := strings.ToLower(strings.TrimSpace(mr.State))
		if (state == "merged" || state == "integrated") && mr.JiraBackAt.IsZero() {
			out = append(out, mr)
		}
	}
	return out
}

func (s *Service) writeBackJiraIssue(client *jiraClient, store IssueStoreAdapter, issue domain.Issue, key string, pending []domain.DeliveryMR) error {
	started := time.Now().UTC()
	if err := client.commentIssue(key, jiraDeliveryComment(pending)); err != nil {
		s.addLog(domain.SyncLog{
			ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "push", Source: "jira",
			Action: "delivery_writeback", Status: "failed", Count: len(pending),
			Message:   fmt.Sprintf("%s（工作项 %s）", err.Error(), issue.ID),
			ProjectID: issue.ProjectID,
			StartedAt: started, FinishedAt: time.Now().UTC(),
		})
		return err
	}
	if !issue.Completed() {
		if err := client.transitionIssueToDone(key); err != nil {
			// The comment is already on the issue and the write-back is
			// recorded, so a missing transition must not retry the comment.
			// A reviewer can move the issue manually instead.
			log.Printf("jira write-back %s transition: %v", key, err)
		}
	}
	now := time.Now().UTC()
	list := issue.DeliveryMRList()
	for i := range list {
		for _, p := range pending {
			if domain.SameGongfengMR(list[i].URL, p.URL) && list[i].JiraBackAt.IsZero() {
				list[i].JiraBackAt = now
			}
		}
	}
	issue.SetDeliveryMRs(list)
	issue.DeliveryUpdatedAt = now
	if err := store.PutSyncedIssue(issue); err != nil {
		return err
	}
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "push", Source: "jira",
		Action: "delivery_writeback", Status: "success", Count: len(pending),
		Message:   fmt.Sprintf("已在 Jira %s 回写 %d 个交付 MR（工作项 %s）", key, len(pending), issue.ID),
		ProjectID: issue.ProjectID,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	})
	return nil
}

// jiraDeliveryComment renders the delivery announcement posted on the Jira
// issue (plain wiki markup text).
func jiraDeliveryComment(pending []domain.DeliveryMR) string {
	var b strings.Builder
	b.WriteString("AgentFlow 交付更新：以下合并请求已合并交付。")
	for _, mr := range pending {
		b.WriteString("\n* ")
		if name := strings.TrimSpace(mr.Repository); name != "" {
			b.WriteString(name)
			b.WriteString("：")
		}
		b.WriteString(mr.URL)
	}
	return b.String()
}

// commentIssue posts a wiki-markup comment on one Jira issue.
func (j *jiraClient) commentIssue(key, body string) error {
	return j.request("POST", "/rest/api/2/issue/"+url.PathEscape(key)+"/comment", nil, map[string]any{"body": body}, nil)
}

type jiraTransition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	} `json:"to"`
}

// transitionIssueToDone moves one Jira issue through its first done-category
// transition. It fails when the workflow exposes none (e.g. closed issues).
func (j *jiraClient) transitionIssueToDone(key string) error {
	var res struct {
		Transitions []jiraTransition `json:"transitions"`
	}
	if err := j.request("GET", "/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", nil, nil, &res); err != nil {
		return err
	}
	for _, t := range res.Transitions {
		if strings.EqualFold(strings.TrimSpace(t.To.StatusCategory.Key), "done") {
			return j.request("POST", "/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", nil, map[string]any{"transition": t.ID}, nil)
		}
	}
	return fmt.Errorf("未找到完成类流转，请人工关闭 %s", key)
}
