package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	openedPRsNamespace = "system:opened-prs"
	prChecksNamespace  = "system:pr-checks"
)

// OpenedPRRecord tracks a pull request opened by Sablecourt so later check reads
// and merge requests can verify provenance before acting.
type OpenedPRRecord struct {
	Owner    string    `json:"owner"`
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	URL      string    `json:"url"`
	Head     string    `json:"head"`
	Base     string    `json:"base"`
	TaskID   string    `json:"task_id"`
	Workload string    `json:"workload"`
	OpenedAt time.Time `json:"opened_at"`
}

type PRChecksInput struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
}

type CheckRunState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Required   bool   `json:"required,omitempty"`
	// CompletedAt is when GitHub recorded this result: completed_at of a
	// check run, updated_at of a commit status. Zero while it runs.
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// PRCheckReport is the timestamped evidence of check state on a pull request.
type PRCheckReport struct {
	Owner           string          `json:"owner"`
	Repo            string          `json:"repo"`
	PRNumber        int             `json:"pr_number"`
	HeadSHA         string          `json:"head_sha"`
	HeadBranch      string          `json:"head_branch"`
	BaseBranch      string          `json:"base_branch"`
	PRState         string          `json:"pr_state"`
	Draft           bool            `json:"draft,omitempty"`
	MergeableState  string          `json:"mergeable_state"`
	RequiredChecks  []string        `json:"required_checks,omitempty"`
	Checks          []CheckRunState `json:"checks"`
	AllRequiredPass bool            `json:"all_required_pass"`
	Summary         string          `json:"summary"`
	// LastResultAt is the latest CompletedAt of the checks, zero when no
	// check has a result time.
	LastResultAt time.Time `json:"last_result_at,omitzero"`
	ObservedAt   time.Time `json:"observed_at"`
}

// MergePRInput is the approved value of git.merge_pr. CommitSHA is required:
// the dialog shows it as a target, and the merge happens only while the pull
// request head is still that commit (GitHub also refuses the merge otherwise).
// An empty MergeMethod means squash.
type MergePRInput struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	PRNumber    int    `json:"pr_number"`
	CommitSHA   string `json:"commit_sha"`
	MergeMethod string `json:"merge_method,omitempty"`
}

type MergePRRequest struct {
	Owner       string
	Repo        string
	PRNumber    int
	CommitSHA   string
	MergeMethod string
}

type MergePRResult struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	PRNumber    int    `json:"pr_number"`
	CommitSHA   string `json:"commit_sha"`
	MergeSHA    string `json:"merge_sha"`
	MergeMethod string `json:"merge_method"`
	Status      string `json:"status"`
}

// PRChecker reads check runs, commit statuses, and branch mergeability for a pull request.
type PRChecker interface {
	CheckPR(ctx context.Context, in PRChecksInput) (PRCheckReport, error)
}

// PRMerger merges a pull request pinned to a reviewed commit SHA without bypassing branch rules.
type PRMerger interface {
	MergePR(ctx context.Context, req MergePRRequest) (MergePRResult, error)
}

func (gh *GitHubPROpener) githubAPIClient(ctx context.Context) (string, string, *http.Client, error) {
	if gh == nil || gh.Secrets == nil {
		return "", "", nil, errors.New("secret store unavailable for github_token")
	}
	token, err := gh.Secrets.Get(ctx, "github_token")
	if err != nil {
		return "", "", nil, fmt.Errorf("load github_token: %w", err)
	}
	apiBase := strings.TrimRight(gh.APIURL, "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return apiBase, token, gh.httpClient(), nil
}

func (gh *GitHubPROpener) doGitHubGET(ctx context.Context, endpoint, token string, cli *http.Client, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cli.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github API returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (gh *GitHubPROpener) CheckPR(ctx context.Context, in PRChecksInput) (PRCheckReport, error) {
	apiBase, token, cli, err := gh.githubAPIClient(ctx)
	if err != nil {
		return PRCheckReport{}, err
	}
	owner := url.PathEscape(in.Owner)
	repo := url.PathEscape(in.Repo)

	var pr struct {
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", apiBase, owner, repo, in.PRNumber)
	if _, err := gh.doGitHubGET(ctx, prURL, token, cli, &pr); err != nil {
		return PRCheckReport{}, fmt.Errorf("fetch pull request: %w", err)
	}
	headSHA := strings.ToLower(strings.TrimSpace(pr.Head.SHA))
	if !isCommitID(headSHA) {
		return PRCheckReport{}, errors.New("pull request head SHA is invalid")
	}

	var checkRuns struct {
		CheckRuns []struct {
			Name        string    `json:"name"`
			Status      string    `json:"status"`
			Conclusion  string    `json:"conclusion"`
			CompletedAt time.Time `json:"completed_at"`
		} `json:"check_runs"`
	}
	runsURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", apiBase, owner, repo, url.PathEscape(headSHA))
	if _, err := gh.doGitHubGET(ctx, runsURL, token, cli, &checkRuns); err != nil {
		return PRCheckReport{}, fmt.Errorf("fetch check runs: %w", err)
	}

	var checks []CheckRunState
	for _, r := range checkRuns.CheckRuns {
		checks = append(checks, CheckRunState{
			Name:        redact.String(strings.TrimSpace(r.Name)),
			Status:      strings.ToLower(strings.TrimSpace(r.Status)),
			Conclusion:  strings.ToLower(strings.TrimSpace(r.Conclusion)),
			CompletedAt: r.CompletedAt.UTC(),
		})
	}

	// Also read legacy commit statuses if present (best-effort on 404).
	var commitStatus struct {
		Statuses []struct {
			Context   string    `json:"context"`
			State     string    `json:"state"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"statuses"`
	}
	statusURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status", apiBase, owner, repo, url.PathEscape(headSHA))
	if code, err := gh.doGitHubGET(ctx, statusURL, token, cli, &commitStatus); err == nil {
		for _, s := range commitStatus.Statuses {
			st, concl := normalizeCommitStatus(s.State)
			cs := CheckRunState{
				Name:       redact.String(strings.TrimSpace(s.Context)),
				Status:     st,
				Conclusion: concl,
			}
			if st == "completed" {
				cs.CompletedAt = s.UpdatedAt.UTC()
			}
			checks = append(checks, cs)
		}
	} else if code != http.StatusNotFound {
		return PRCheckReport{}, fmt.Errorf("fetch commit status: %w", err)
	}

	// Read branch protection required status checks when accessible.
	var required []string
	if strings.TrimSpace(pr.Base.Ref) != "" {
		var prot struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		}
		protURL := fmt.Sprintf("%s/repos/%s/%s/branches/%s/protection/required_status_checks", apiBase, owner, repo, url.PathEscape(pr.Base.Ref))
		if code, err := gh.doGitHubGET(ctx, protURL, token, cli, &prot); err == nil {
			seen := make(map[string]bool)
			for _, c := range prot.Contexts {
				c = strings.TrimSpace(c)
				if c != "" && !seen[c] {
					seen[c] = true
					required = append(required, c)
				}
			}
			for _, c := range prot.Checks {
				name := strings.TrimSpace(c.Context)
				if name != "" && !seen[name] {
					seen[name] = true
					required = append(required, name)
				}
			}
		} else if code != http.StatusNotFound && code != http.StatusForbidden {
			return PRCheckReport{}, fmt.Errorf("fetch required status checks: %w", err)
		}
	}

	return PRCheckReport{
		Owner:          in.Owner,
		Repo:           in.Repo,
		PRNumber:       in.PRNumber,
		HeadSHA:        headSHA,
		HeadBranch:     strings.TrimSpace(pr.Head.Ref),
		BaseBranch:     strings.TrimSpace(pr.Base.Ref),
		PRState:        strings.ToLower(strings.TrimSpace(pr.State)),
		Draft:          pr.Draft,
		MergeableState: strings.ToLower(strings.TrimSpace(pr.MergeableState)),
		RequiredChecks: required,
		Checks:         checks,
	}, nil
}

func (gh *GitHubPROpener) MergePR(ctx context.Context, req MergePRRequest) (MergePRResult, error) {
	apiBase, token, cli, err := gh.githubAPIClient(ctx)
	if err != nil {
		return MergePRResult{}, err
	}
	method := req.MergeMethod
	if method == "" {
		method = "squash"
	}
	// Never pass any admin or branch-rule bypass flag; always pin merge to the reviewed SHA.
	body, err := json.Marshal(map[string]string{
		"sha":          req.CommitSHA,
		"merge_method": method,
	})
	if err != nil {
		return MergePRResult{}, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", apiBase, url.PathEscape(req.Owner), url.PathEscape(req.Repo), req.PRNumber)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return MergePRResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := cli.Do(httpReq)
	if err != nil {
		return MergePRResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MergePRResult{}, fmt.Errorf("github merge API returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Merged  bool   `json:"merged"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return MergePRResult{}, err
	}
	if !out.Merged {
		return MergePRResult{}, fmt.Errorf("pull request was not merged: %s", redact.String(out.Message))
	}
	return MergePRResult{
		Owner:       req.Owner,
		Repo:        req.Repo,
		PRNumber:    req.PRNumber,
		CommitSHA:   req.CommitSHA,
		MergeSHA:    out.SHA,
		MergeMethod: method,
		Status:      "merged",
	}, nil
}

func normalizeCommitStatus(state string) (status, conclusion string) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return "completed", "success"
	case "pending":
		return "in_progress", ""
	default:
		return "completed", "failure"
	}
}

func evaluatePRChecks(report *PRCheckReport, now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report.ObservedAt = now.UTC()
	report.PRState = strings.ToLower(strings.TrimSpace(report.PRState))
	report.MergeableState = strings.ToLower(strings.TrimSpace(report.MergeableState))
	report.HeadSHA = strings.ToLower(strings.TrimSpace(report.HeadSHA))

	report.LastResultAt = time.Time{}
	for i := range report.Checks {
		report.Checks[i].Name = redact.String(strings.TrimSpace(report.Checks[i].Name))
		report.Checks[i].Status = strings.ToLower(strings.TrimSpace(report.Checks[i].Status))
		report.Checks[i].Conclusion = strings.ToLower(strings.TrimSpace(report.Checks[i].Conclusion))
		if t := report.Checks[i].CompletedAt; t.After(report.LastResultAt) {
			report.LastResultAt = t.UTC()
		}
	}

	if report.PRState != "open" {
		report.AllRequiredPass = false
		report.Summary = "pr_not_open"
		return
	}
	if report.Draft {
		report.AllRequiredPass = false
		report.Summary = "pr_draft"
		return
	}

	var hasFailing, hasPending, hasMissing bool
	if len(report.RequiredChecks) > 0 {
		reqSet := make(map[string]bool, len(report.RequiredChecks))
		for _, name := range report.RequiredChecks {
			reqSet[name] = true
		}
		for i := range report.Checks {
			if reqSet[report.Checks[i].Name] {
				report.Checks[i].Required = true
			}
		}
		for _, reqName := range report.RequiredChecks {
			var matched []CheckRunState
			for _, c := range report.Checks {
				if c.Name == reqName {
					matched = append(matched, c)
				}
			}
			if len(matched) == 0 {
				hasMissing = true
				continue
			}
			for _, c := range matched {
				if c.Status != "completed" {
					hasPending = true
				} else if c.Conclusion != "success" {
					hasFailing = true
				}
			}
		}
	} else {
		if len(report.Checks) == 0 {
			report.AllRequiredPass = false
			report.Summary = "pending"
			return
		}
		for _, c := range report.Checks {
			if c.Status != "completed" {
				hasPending = true
			} else if c.Conclusion != "success" {
				hasFailing = true
			}
		}
	}

	switch {
	case hasFailing:
		report.AllRequiredPass = false
		report.Summary = "failing"
		return
	case hasPending:
		report.AllRequiredPass = false
		report.Summary = "pending"
		return
	case hasMissing:
		report.AllRequiredPass = false
		report.Summary = "missing_required"
		return
	}

	// Never bypass branch rules: refuse blocked, dirty, behind, unstable, draft,
	// unknown or missing mergeable states.
	if report.MergeableState != "clean" {
		report.AllRequiredPass = false
		report.Summary = "blocked_by_branch_rules"
		return
	}

	report.AllRequiredPass = true
	report.Summary = "passing"
}

func prKey(owner, repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func parsePRNumber(prURL string) (int, bool) {
	u, err := url.Parse(strings.TrimSpace(prURL))
	if err != nil {
		return 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[len(parts)-2] != "pull" {
		return 0, false
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func recordOpenedPR(ctx context.Context, g *Gate, rec OpenedPRRecord) error {
	if g == nil || g.store == nil || rec.Number <= 0 {
		return nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return g.store.PutMemory(ctx, domain.MemoryEntry{
		Namespace: openedPRsNamespace,
		Key:       prKey(rec.Owner, rec.Repo, rec.Number),
		Value:     string(raw),
		Author:    "gate",
		UpdatedAt: g.now(),
	})
}

func lookupOpenedPR(ctx context.Context, st store.Store, owner, repo string, number int) (OpenedPRRecord, error) {
	if st == nil {
		return OpenedPRRecord{}, newActionError(http.StatusForbidden, "unknown_target", "opened pull request registry unavailable")
	}
	entry, err := st.GetMemory(ctx, openedPRsNamespace, prKey(owner, repo, number))
	if errors.Is(err, store.ErrNotFound) {
		return OpenedPRRecord{}, newActionError(http.StatusForbidden, "unknown_target", "pull request was not opened by Sablecourt")
	}
	if err != nil {
		return OpenedPRRecord{}, err
	}
	if entry.Author != "gate" {
		return OpenedPRRecord{}, newActionError(http.StatusForbidden, "unknown_target", "pull request provenance record is untrusted")
	}
	var rec OpenedPRRecord
	if err := json.Unmarshal([]byte(entry.Value), &rec); err != nil || rec.Number != number {
		return OpenedPRRecord{}, newActionError(http.StatusForbidden, "unknown_target", "pull request provenance record is invalid")
	}
	return rec, nil
}

func recordPRChecksEvidence(ctx context.Context, g *Gate, taskID, workloadName string, report PRCheckReport) error {
	if g == nil || g.store == nil {
		return nil
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := g.store.PutMemory(ctx, domain.MemoryEntry{
		Namespace: prChecksNamespace,
		Key:       prKey(report.Owner, report.Repo, report.PRNumber),
		Value:     string(raw),
		Author:    "gate",
		UpdatedAt: report.ObservedAt,
	}); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{
		"owner":             report.Owner,
		"repo":              report.Repo,
		"pr_number":         report.PRNumber,
		"head_sha":          report.HeadSHA,
		"summary":           report.Summary,
		"all_required_pass": report.AllRequiredPass,
		"last_result_at":    report.LastResultAt.Format(time.RFC3339Nano),
		"observed_at":       report.ObservedAt.Format(time.RFC3339Nano),
	})
	g.audit(ctx, taskID, workloadName, "evidence:git.pr_checks", report.Summary, string(detail))
	return nil
}

func checkOwnerRepoGrant(wl domain.Workload, owner, repo string) error {
	if len(wl.Spec.Grants.Repos) == 0 {
		return nil
	}
	slug := normalizeOwnerRepo(owner + "/" + repo)
	if slug == "" {
		return newActionError(http.StatusBadRequest, "invalid_target", "invalid owner or repo")
	}
	for _, grant := range wl.Spec.Grants.Repos {
		if normalizeOwnerRepo(grant) == slug {
			return nil
		}
	}
	return newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("repository %s/%s is not granted", owner, repo))
}

func validatePRChecks(in PRChecksInput) error {
	if err := validateRepoName("owner", in.Owner); err != nil {
		return err
	}
	if err := validateRepoName("repo", in.Repo); err != nil {
		return err
	}
	if in.PRNumber <= 0 {
		return errors.New("pr_number must be positive")
	}
	return nil
}

func executePRChecks(ctx context.Context, g *Gate, checker PRChecker, wl domain.Workload, taskID string, in PRChecksInput) (PRCheckReport, error) {
	if err := checkOwnerRepoGrant(wl, in.Owner, in.Repo); err != nil {
		return PRCheckReport{}, err
	}
	if _, err := lookupOpenedPR(ctx, g.store, in.Owner, in.Repo, in.PRNumber); err != nil {
		return PRCheckReport{}, err
	}
	if checker == nil {
		return PRCheckReport{}, errors.New("PR checker not configured")
	}
	report, err := checker.CheckPR(ctx, in)
	if err != nil {
		return PRCheckReport{}, err
	}
	report.Owner = in.Owner
	report.Repo = in.Repo
	report.PRNumber = in.PRNumber
	evaluatePRChecks(&report, g.now())
	if err := recordPRChecksEvidence(ctx, g, taskID, wl.Metadata.Name, report); err != nil {
		return PRCheckReport{}, err
	}
	return report, nil
}

// validateMergePRInput checks the git.merge_pr value on its own, before
// policy effects and live state.
func validateMergePRInput(in MergePRInput) error {
	if err := validatePRChecks(PRChecksInput{Owner: in.Owner, Repo: in.Repo, PRNumber: in.PRNumber}); err != nil {
		return err
	}
	if !isCommitID(in.CommitSHA) {
		return errors.New("commit_sha must be a full 40- or 64-character lowercase hex SHA")
	}
	switch in.MergeMethod {
	case "", "merge", "squash", "rebase":
	default:
		return errors.New("merge_method must be merge, squash, or rebase")
	}
	return nil
}

// checkMergeReady reads the live check state of the pull request, records it
// as evidence, and refuses unless the pull request is one Sablecourt opened in
// the pinned and granted repository, its head is still the approved
// commit_sha, and every required check has passed with a result time.
func checkMergeReady(ctx context.Context, g *Gate, checker PRChecker, wl domain.Workload, taskID string, in MergePRInput) (PRCheckReport, error) {
	pinned := g.snapshotFromContext(ctx).allowedRepo
	if pinned == "" {
		return PRCheckReport{}, newActionError(http.StatusForbidden, "unknown_target", "no GitHub repository was pinned for git.merge_pr at start")
	}
	if !strings.EqualFold(in.Owner+"/"+in.Repo, pinned) {
		return PRCheckReport{}, newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("git.merge_pr target %s/%s is not the configured repository", in.Owner, in.Repo))
	}
	if err := checkOwnerRepoGrant(wl, in.Owner, in.Repo); err != nil {
		return PRCheckReport{}, err
	}
	opened, err := lookupOpenedPR(ctx, g.store, in.Owner, in.Repo, in.PRNumber)
	if err != nil {
		return PRCheckReport{}, err
	}
	if err := validateSafeBranch(opened.Head); err != nil {
		return PRCheckReport{}, newActionError(http.StatusForbidden, "wrong_branch", err.Error())
	}
	report, err := executePRChecks(ctx, g, checker, wl, taskID, PRChecksInput{Owner: in.Owner, Repo: in.Repo, PRNumber: in.PRNumber})
	if err != nil {
		return PRCheckReport{}, err
	}
	if report.PRState != "open" || report.Draft {
		return PRCheckReport{}, newActionError(http.StatusConflict, "pr_not_mergeable", fmt.Sprintf("pull request is not open for merge (%s)", report.Summary))
	}
	if validateSafeBranch(report.HeadBranch) != nil || report.HeadBranch != opened.Head {
		return PRCheckReport{}, newActionError(http.StatusForbidden, "wrong_branch", "pull request head branch does not match opened Sablecourt branch")
	}
	if report.HeadSHA != in.CommitSHA {
		return PRCheckReport{}, newActionError(http.StatusConflict, "commit_mismatch", "pull request head commit does not match the approved commit_sha")
	}
	if !report.AllRequiredPass || report.Summary != "passing" {
		return PRCheckReport{}, newActionError(http.StatusConflict, "checks_"+report.Summary, fmt.Sprintf("cannot merge pull request: required checks are %s", report.Summary))
	}
	if report.LastResultAt.IsZero() {
		return PRCheckReport{}, newActionError(http.StatusConflict, "checks_missing_result_time", "cannot merge pull request: the check results carry no completion time")
	}
	return report, nil
}

// checkApprovalAfterChecks refuses a merge whose approval was requested
// before the latest check result: the operator approved check results that
// are no longer the latest, so a new approval is needed. The approval is the
// one the gate consumed for this request; without one the merge is refused.
func checkApprovalAfterChecks(ctx context.Context, g *Gate, report PRCheckReport) error {
	id := approvalFromContext(ctx)
	if id == "" {
		return newActionError(http.StatusForbidden, "approval_required", "git.merge_pr runs only with a consumed approval")
	}
	apr, err := g.store.GetApproval(ctx, id)
	if err != nil {
		return fmt.Errorf("read approval: %w", err)
	}
	if apr.CreatedAt.Before(report.LastResultAt) {
		return newActionError(http.StatusConflict, "approval_predates_checks",
			fmt.Sprintf("approval was requested at %s, before the latest check result at %s; request a new approval",
				apr.CreatedAt.UTC().Format(time.RFC3339), report.LastResultAt.UTC().Format(time.RFC3339)))
	}
	return nil
}
