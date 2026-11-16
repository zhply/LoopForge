package tracker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// gitlabClient talks to a standard (self-hosted or gitlab.com) GitLab REST v4
// API with a project owner's personal access token. Repository ids carry the
// "gitlab:<host>/<path>" remote form so one platform can reach many instances.
type gitlabClient struct {
	base, path, token string
	http              *http.Client
}

func (s *Service) gitlabClient(project string) (*gitlabClient, error) {
	_, path, ok := domain.SplitGitLabProject(project)
	if !ok {
		return nil, fmt.Errorf("invalid GitLab repository %q", project)
	}
	var token string
	if s.projectAuth != nil {
		for _, repo := range s.projectAuth.Repositories() {
			if strings.TrimSpace(repo.GongfengProjectID) != project {
				continue
			}
			if p, found := s.projectAuth.Project(repo.ProjectID); found {
				if u, exists := s.projectAuth.User(p.OwnerID); exists {
					token = u.GitToken
				}
			}
			break
		}
	}
	base := s.gitlabAPIBase
	if base == "" {
		host, _, _ := domain.SplitGitLabProject(project)
		base = "https://" + host + "/api/v4"
	}
	return &gitlabClient{base: strings.TrimRight(base, "/"), path: path, token: token, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (g *gitlabClient) request(method, suffix string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, g.base+"/projects/"+url.QueryEscape(g.path)+suffix, body)
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		r.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.http.Do(r)
	if err != nil {
		return fmt.Errorf("GitLab request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("GitLab returned HTTP %d; check the project owner's Git token and repository permissions", res.StatusCode)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(output)
}

// gitlabListMRs searches merge requests; the JSON shape matches
// gongfengListedMR so all MR parsing helpers are shared.
func (g *gitlabClient) gitlabListMRs(query url.Values) ([]gongfengListedMR, error) {
	var list []gongfengListedMR
	if err := g.request("GET", "/merge_requests?"+query.Encode(), nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// EnsureAndMerge finds or creates an MR source→target and merges it. An empty
// target only merges an already-open MR for the source branch.
func (g *gitlabClient) EnsureAndMerge(sourceBranch, targetBranch, title, description string) (string, error) {
	find := func(state string) (gongfengListedMR, bool, error) {
		q := url.Values{"source_branch": {sourceBranch}, "state": {state}, "per_page": {"20"}, "order_by": {"updated_at"}, "sort": {"desc"}}
		if targetBranch != "" {
			q.Set("target_branch", targetBranch)
		}
		list, err := g.gitlabListMRs(q)
		if err != nil {
			return gongfengListedMR{}, false, fmt.Errorf("search MR for %s: %w", sourceBranch, err)
		}
		item, ok := pickBranchMR(list, sourceBranch)
		return item, ok, nil
	}
	item, found, err := find("opened")
	if err != nil {
		return "", err
	}
	if found && normalizeMRState(item.State) == "merged" {
		return "!" + strconv.Itoa(item.IID), nil
	}
	if !found {
		if targetBranch == "" {
			return "", ErrNoOpenMR
		}
		createTitle := strings.TrimSpace(title)
		if createTitle == "" {
			createTitle = "LoopForge: " + sourceBranch + " → " + targetBranch
		}
		createDesc := strings.TrimSpace(description)
		if createDesc == "" {
			createDesc = fmt.Sprintf("定时 SOP 将 `%s` 合入 `%s`。", sourceBranch, targetBranch)
		}
		var created gongfengListedMR
		createErr := g.request("POST", "/merge_requests", map[string]any{
			"source_branch": sourceBranch,
			"target_branch": targetBranch,
			"title":         createTitle,
			"description":   createDesc,
		}, &created)
		if createErr == nil {
			item = created
		} else {
			// Another instance may have won the create race; re-check before
			// failing, keeping the create error as the fallback cause.
			item, found, err = find("opened")
			if err != nil {
				return "", err
			}
			if !found {
				return "", fmt.Errorf("create MR %s → %s: %w", sourceBranch, targetBranch, createErr)
			}
		}
	}
	mergeArgs := map[string]any{}
	if msg := strings.TrimSpace(title); msg != "" {
		mergeArgs["merge_commit_message"] = msg
	}
	var merged gongfengListedMR
	if err := g.request("PUT", "/merge_requests/"+strconv.Itoa(item.IID)+"/merge", mergeArgs, &merged); err != nil {
		return "", fmt.Errorf("merge MR !%d: %w", item.IID, err)
	}
	return "!" + strconv.Itoa(item.IID), nil
}

// FindBranch returns the delivery MR whose source is sourceBranch: the newest
// non-closed one, else the newest closed one.
func (g *gitlabClient) FindBranch(sourceBranch string) (gongfengListedMR, bool, error) {
	q := url.Values{"source_branch": {sourceBranch}, "state": {"all"}, "per_page": {"20"}, "order_by": {"updated_at"}, "sort": {"desc"}}
	list, err := g.gitlabListMRs(q)
	if err != nil {
		return gongfengListedMR{}, false, fmt.Errorf("search MR for %s: %w", sourceBranch, err)
	}
	item, ok := pickBranchMR(list, sourceBranch)
	return item, ok, nil
}

func (s *Service) gitlabMergeBranch(project, sourceBranch, targetBranch, title, description string) (string, error) {
	if strings.TrimSpace(sourceBranch) == "" {
		return "", fmt.Errorf("source branch is required")
	}
	if strings.TrimSpace(targetBranch) != "" && domain.SameGitBranch(sourceBranch, targetBranch) {
		return "", nil
	}
	g, err := s.gitlabClient(project)
	if err != nil {
		return "", err
	}
	return g.EnsureAndMerge(sourceBranch, targetBranch, title, description)
}

func (s *Service) gitlabFindBranch(project, sourceBranch string) (domain.DeliveryMR, bool, error) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(sourceBranch) == "" {
		return domain.DeliveryMR{}, false, nil
	}
	g, err := s.gitlabClient(project)
	if err != nil {
		return domain.DeliveryMR{}, false, err
	}
	item, ok, err := g.FindBranch(sourceBranch)
	if err != nil || !ok {
		return domain.DeliveryMR{}, ok, err
	}
	gitlabURL := strings.TrimSpace(item.WebURL)
	if _, _, parsed, ok := domain.ParseGongfengMR(gitlabURL); ok {
		gitlabURL = parsed
	} else {
		gitlabURL = domain.GongfengMRURL(project, item.IID)
	}
	return domain.DeliveryMR{
		Project:       project,
		IID:           item.IID,
		URL:           gitlabURL,
		TargetBranch:  item.TargetBranch,
		GongfengState: normalizeMRState(item.State),
	}, true, nil
}

func (s *Service) refreshGitLabDelivery(store MRDeliveryStore, project string, iid int) error {
	g, err := s.gitlabClient(project)
	if err != nil {
		return err
	}
	var mr gongfengListedMR
	if err := g.request("GET", "/merge_requests/"+strconv.Itoa(iid), nil, &mr); err != nil {
		return err
	}
	state := normalizeMRState(mr.State)
	if state == "" {
		state = "unknown"
	}
	return store.UpdateMRDeliveryState(project, iid, state, strings.TrimSpace(mr.TargetBranch), gongfengMRMergedAt(mr), time.Now().UTC())
}

// pullGitLabIssues imports a GitLab project's issues as read-only work items.
func (s *Service) pullGitLabIssues(store IssueStoreAdapter, repo domain.Repository) (int, error) {
	started := time.Now().UTC()
	g, err := s.gitlabClient(repo.GongfengProjectID)
	if err != nil {
		return 0, err
	}
	count := 0
	for page := 1; page <= 100; page++ {
		var list []struct {
			ID          int    `json:"id"`
			IID         int    `json:"iid"`
			Title       string `json:"title"`
			Description string `json:"description"`
			State       string `json:"state"`
			WebURL      string `json:"web_url"`
			CreatedAt   string `json:"created_at"`
		}
		q := url.Values{"state": {"all"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}, "order_by": {"updated_at"}, "sort": {"desc"}}
		if err := g.request("GET", "/issues?"+q.Encode(), nil, &list); err != nil {
			s.addLog(domain.SyncLog{ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "gitlab", Action: "sync_issues", Status: "failed", Message: err.Error(), ProjectID: repo.ProjectID, StartedAt: started, FinishedAt: time.Now().UTC()})
			return count, err
		}
		for _, item := range list {
			id := fmt.Sprintf("gl-%s-%d", repo.ID, item.IID)
			status := gongfengStateToStatus(item.State)
			issue := domain.Issue{ID: id, Number: item.IID, ProjectID: repo.ProjectID, RepositoryID: repo.ID, BaseBranch: repo.DefaultBranch, Kind: "issue", Title: item.Title, Description: item.Description, Priority: "medium", Source: "gitlab", Status: status, CreatedAt: parseTime(item.CreatedAt), UpdatedAt: time.Now().UTC(), ExternalRef: &domain.ExternalRef{Source: "gitlab", ExternalID: strconv.Itoa(item.ID), ExternalIID: item.IID, ExternalURL: item.WebURL, ExternalState: item.State, ExternalType: "issue", SyncedAt: time.Now().UTC()}}
			if status == "closed" {
				issue.CloseSource = "external"
			}
			if old, ok := store.Issue(id); ok {
				applyExistingIssue(&issue, old)
			}
			if err := store.PutSyncedIssue(issue); err != nil {
				return count, err
			}
			count++
		}
		if len(list) < 100 {
			break
		}
	}
	s.addLog(domain.SyncLog{
		ID: fmt.Sprintf("sync-%d", started.UnixNano()), Direction: "pull", Source: "gitlab",
		Action: "sync_issues", Status: "success", Count: count,
		Message:   fmt.Sprintf("同步 %d 条 GitLab issue", count),
		ProjectID: repo.ProjectID,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	})
	return count, nil
}
