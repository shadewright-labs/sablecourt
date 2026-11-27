package mcpgate

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const runtimeHandlesNamespace = "system:runtime-handles"

// PushBranchInput is the approved value of git.push. CommitSHA is required:
// the dialog shows it as a target, and the push happens only while the task
// branch still points at it. An empty Remote means origin.
type PushBranchInput struct {
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	Remote    string `json:"remote,omitempty"`
	Repo      string `json:"repo,omitempty"`
}

type PushBranchRequest struct {
	RepoDir string
	Remote  string
	Branch  string
	Commit  string
}

type PushBranchResult struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Status string `json:"status"`
}

// BranchPusher pushes one verified commit on a task branch using gate-owned credentials.
type BranchPusher interface {
	PushBranch(ctx context.Context, req PushBranchRequest) (PushBranchResult, error)
}

// GitBranchPusher pushes a verified commit to a configured git remote without
// exposing upstream credentials to workloads or process arguments.
type GitBranchPusher struct {
	Secrets secret.SecretStore
}

func (p *GitBranchPusher) PushBranch(ctx context.Context, req PushBranchRequest) (PushBranchResult, error) {
	if err := validateSafeBranch(req.Branch); err != nil {
		return PushBranchResult{}, err
	}
	if !isCommitID(req.Commit) {
		return PushBranchResult{}, errors.New("commit must be a full hex commit SHA")
	}
	if err := validateSafeRemote(req.Remote); err != nil {
		return PushBranchResult{}, err
	}
	if strings.TrimSpace(req.RepoDir) == "" {
		return PushBranchResult{}, errors.New("repository directory is required")
	}

	headSHA, err := resolveBranchCommit(ctx, req.RepoDir, req.Branch)
	if err != nil {
		return PushBranchResult{}, err
	}
	if headSHA != req.Commit {
		return PushBranchResult{}, newActionError(http.StatusConflict, "commit_mismatch", "branch HEAD does not match the approved commit_sha")
	}

	remoteURL, err := getRemoteURL(ctx, req.RepoDir, req.Remote)
	if err != nil {
		return PushBranchResult{}, newActionError(http.StatusForbidden, "unknown_target", "unknown remote target")
	}

	refspec := req.Commit + ":refs/heads/" + req.Branch
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.ext.allow=never",
		"-C", req.RepoDir,
		"push", "--no-verify", "--", req.Remote, refspec,
	)

	env := []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"GIT_TERMINAL_PROMPT=0",
	}
	var rawSecrets []string
	if isGitHubHTTPSURL(remoteURL) && p != nil && p.Secrets != nil {
		token, err := p.Secrets.Get(ctx, "github_token")
		if err == nil && strings.TrimSpace(token) != "" {
			basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
			rawSecrets = append(rawSecrets, token, basic)
			env = append(env,
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
				"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic,
			)
		}
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		clean := sanitizeSecrets(strings.TrimSpace(string(out)), rawSecrets)
		return PushBranchResult{}, fmt.Errorf("git push failed: %w (%s)", err, redact.String(clean))
	}
	return PushBranchResult{
		Remote: req.Remote,
		Branch: req.Branch,
		Commit: req.Commit,
		Status: "pushed",
	}, nil
}

// validatePushInput checks the git.push value on its own, before policy
// effects and live state.
func validatePushInput(in PushBranchInput) error {
	if err := validateSafeBranch(in.Branch); err != nil {
		return newActionError(http.StatusForbidden, "wrong_branch", err.Error())
	}
	if !isCommitID(in.CommitSHA) {
		return errors.New("commit_sha must be a full 40- or 64-character lowercase hex SHA")
	}
	if in.Remote != "" {
		if err := validateSafeRemote(in.Remote); err != nil {
			return newActionError(http.StatusForbidden, "unknown_target", err.Error())
		}
	}
	return nil
}

// resolvePushRequest checks a git.push value against live state: the branch
// is this task's branch, the repository and remote are granted, and the
// branch head is the approved commit_sha. Any drift refuses the push.
func resolvePushRequest(ctx context.Context, g *Gate, defaultRepoRoot string, wl domain.Workload, taskID string, in PushBranchInput) (PushBranchRequest, error) {
	remote := in.Remote
	if remote == "" {
		remote = "origin"
	}
	expectedBranch := expectedTaskBranch(ctx, g.store, taskID)
	if in.Branch != expectedBranch {
		return PushBranchRequest{}, newActionError(http.StatusForbidden, "wrong_branch", fmt.Sprintf("branch %q is not this task's branch", in.Branch))
	}
	repoDir, err := resolveGrantedRepoDir(ctx, defaultRepoRoot, wl, remote, in.Repo)
	if err != nil {
		return PushBranchRequest{}, err
	}
	headSHA, err := resolveBranchCommit(ctx, repoDir, in.Branch)
	if err != nil {
		return PushBranchRequest{}, err
	}
	if headSHA != in.CommitSHA {
		return PushBranchRequest{}, newActionError(http.StatusConflict, "commit_mismatch", "branch HEAD does not match the approved commit_sha")
	}
	return PushBranchRequest{
		RepoDir: repoDir,
		Remote:  remote,
		Branch:  in.Branch,
		Commit:  in.CommitSHA,
	}, nil
}

func resolveGrantedRepoDir(ctx context.Context, defaultRepoRoot string, wl domain.Workload, remote, requestedRepo string) (string, error) {
	hasCodingRepo := wl.Spec.Coding != nil && strings.TrimSpace(wl.Spec.Coding.RepoPath) != ""
	hasRepoGrants := len(wl.Spec.Grants.Repos) > 0
	if !hasCodingRepo && !hasRepoGrants {
		return "", newActionError(http.StatusForbidden, "unknown_target", "workload has no granted repository")
	}

	repoDir := strings.TrimSpace(defaultRepoRoot)
	if hasCodingRepo {
		repoDir = strings.TrimSpace(wl.Spec.Coding.RepoPath)
	}
	if repoDir == "" {
		return "", newActionError(http.StatusForbidden, "unknown_target", "repository root not configured for git.push")
	}

	remoteURL, err := getRemoteURL(ctx, repoDir, remote)
	if err != nil {
		return "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("unknown remote %q in granted repository", remote))
	}
	remoteSlug := parseRepoSlug(remoteURL)

	if hasRepoGrants && !matchesAnyRepoGrant(wl.Spec.Grants.Repos, repoDir, remoteURL, remoteSlug) {
		return "", newActionError(http.StatusForbidden, "unknown_target", "target repository is not in workload grants.repos")
	}
	// The granted repository's own remote is origin. Any other remote must be
	// named by URL or owner/repo in grants.repos; a path grant names the local
	// repository, not where it pushes to.
	if remote != "origin" && !matchesAnyRemoteGrant(wl.Spec.Grants.Repos, remoteURL, remoteSlug) {
		return "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("remote %q is not a granted push target", remote))
	}
	if requestedRepo != "" {
		if !matchesRepoTarget(requestedRepo, repoDir, remoteURL, remoteSlug) {
			return "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("repository %q does not match granted repository", requestedRepo))
		}
		if hasRepoGrants && !matchesAnyRepoGrant(wl.Spec.Grants.Repos, requestedRepo, requestedRepo, parseRepoSlug(requestedRepo)) {
			return "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("repository %q is not granted", requestedRepo))
		}
	}
	return repoDir, nil
}

func expectedTaskBranch(ctx context.Context, st store.Store, taskID string) string {
	if st != nil && taskID != "" {
		entry, err := st.GetMemory(ctx, runtimeHandlesNamespace, taskID)
		if err == nil && entry.Author == "controlplane" {
			var rec struct {
				TaskID string `json:"task_id"`
				Branch string `json:"branch"`
			}
			if json.Unmarshal([]byte(entry.Value), &rec) == nil && rec.TaskID == taskID && validateSafeBranch(rec.Branch) == nil {
				return rec.Branch
			}
		}
	}
	return "sablecourt/" + sanitizeTaskSegment(taskID)
}

func sanitizeTaskSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validateSafeRemote(remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.Contains(remote, "..") {
		return errors.New("invalid git remote name")
	}
	for _, r := range remote {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return errors.New("git remote name contains illegal characters")
	}
	return nil
}

func getRemoteURL(ctx context.Context, repoDir, remote string) (string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.ext.allow=never",
		"-C", repoDir,
		"remote", "get-url", "--", remote,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func resolveBranchCommit(ctx context.Context, repoDir, branch string) (string, error) {
	ref := "refs/heads/" + branch + "^{commit}"
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.ext.allow=never",
		"-C", repoDir,
		"rev-parse", "--verify", "--end-of-options", ref,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", newActionError(http.StatusConflict, "branch_not_found", fmt.Sprintf("local branch %q not found", branch))
	}
	sha := strings.ToLower(strings.TrimSpace(string(out)))
	if !isCommitID(sha) {
		return "", newActionError(http.StatusConflict, "branch_not_found", "local branch did not resolve to a commit")
	}
	return sha, nil
}

func parseRepoSlug(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// SSH form: git@github.com:owner/repo.git
	if idx := strings.Index(s, ":"); idx > 0 && !strings.Contains(s[:idx], "/") && strings.Contains(s[:idx], "@") {
		path := strings.TrimPrefix(s[idx+1:], "/")
		return normalizeOwnerRepo(path)
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return normalizeOwnerRepo(strings.TrimPrefix(u.Path, "/"))
	}
	return normalizeOwnerRepo(s)
}

func normalizeOwnerRepo(path string) string {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	if !isSafeGitHubIdent(parts[0]) || !isSafeGitHubIdent(parts[1]) {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

func isSafeGitHubIdent(s string) bool {
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isGitHubHTTPSURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "github.com")
}

func sameCanonicalPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if absA == absB {
		return true
	}
	realA, errA := filepath.EvalSymlinks(absA)
	realB, errB := filepath.EvalSymlinks(absB)
	return errA == nil && errB == nil && realA == realB
}

func matchesRepoTarget(candidate, repoDir, remoteURL, remoteSlug string) bool {
	cand := strings.TrimSpace(candidate)
	if cand == "" {
		return false
	}
	if candSlug := normalizeOwnerRepo(cand); candSlug != "" && remoteSlug != "" && candSlug == remoteSlug {
		return true
	}
	if cand == remoteURL {
		return true
	}
	if sameCanonicalPath(cand, repoDir) || sameCanonicalPath(cand, remoteURL) {
		return true
	}
	return false
}

func matchesAnyRepoGrant(grants []string, repoDir, remoteURL, remoteSlug string) bool {
	for _, grant := range grants {
		if matchesRepoTarget(grant, repoDir, remoteURL, remoteSlug) {
			return true
		}
	}
	return false
}

func matchesAnyRemoteGrant(grants []string, remoteURL, remoteSlug string) bool {
	for _, grant := range grants {
		g := strings.TrimSpace(grant)
		if g == "" {
			continue
		}
		if g == remoteURL {
			return true
		}
		if slug := normalizeOwnerRepo(g); slug != "" && remoteSlug != "" && slug == remoteSlug {
			return true
		}
	}
	return false
}

func sanitizeSecrets(text string, secrets []string) string {
	out := text
	for _, sec := range secrets {
		if sec != "" {
			out = strings.ReplaceAll(out, sec, "[REDACTED:secret]")
		}
	}
	return out
}
