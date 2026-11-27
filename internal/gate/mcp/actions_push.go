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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const runtimeHandlesNamespace = "system:runtime-handles"

// PushBranchInput is the approved value of git.push. CommitSHA and
// Destination are required and the dialog shows both as targets. The push
// happens only while the task branch still points at CommitSHA and the push
// URL of Remote, without user information, is still Destination. An empty
// Remote means origin.
type PushBranchInput struct {
	Branch      string `json:"branch"`
	CommitSHA   string `json:"commit_sha"`
	Destination string `json:"destination"`
	Remote      string `json:"remote,omitempty"`
	Repo        string `json:"repo,omitempty"`
}

// maxDestinationLen bounds the destination of a git.push value.
const maxDestinationLen = 2048

// PushBranchRequest is a push the gate has checked. URL is the push URL of
// Remote as git resolved it when the request was checked (pushurl and
// pushInsteadOf applied); the push goes to that URL, not to the remote name,
// so a later change of the repository's configuration cannot redirect it.
type PushBranchRequest struct {
	RepoDir string
	Remote  string
	URL     string
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

// GitBranchPusher pushes a verified commit object to a checked push URL
// without exposing upstream credentials to workloads or process arguments.
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

	pushURL := strings.TrimSpace(req.URL)
	if pushURL == "" || strings.HasPrefix(pushURL, "-") || strings.ContainsAny(pushURL, "\r\n") {
		return PushBranchResult{}, newActionError(http.StatusForbidden, "unknown_target", "no checked push URL")
	}
	var token string
	if p != nil && p.Secrets != nil {
		if tok, err := p.Secrets.Get(ctx, "github_token"); err == nil {
			token = strings.TrimSpace(tok)
		}
	}
	pushURL, authEnv := githubPushAuth(pushURL, token)

	objectsDir, err := gitObjectsDir(ctx, req.RepoDir)
	if err != nil {
		return PushBranchResult{}, err
	}
	// The push runs from an empty bare repository of the gate's own that
	// borrows the task repository's objects. It has no refs, so the commit_sha
	// in the refspec can only be read as that object, and no configuration of
	// the task repository (remotes, pushurl, insteadOf, hooks) applies to it.
	pushDir, err := os.MkdirTemp("", "sablecourt-push-")
	if err != nil {
		return PushBranchResult{}, err
	}
	defer func() { _ = os.RemoveAll(pushDir) }()
	env := []string{
		"PATH=/Library/Developer/CommandLineTools/usr/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"GIT_TERMINAL_PROMPT=0",
	}
	initCmd := exec.CommandContext(ctx, "git", "init", "--quiet", "--bare", "--template=", "--", pushDir)
	initCmd.Env = env
	if out, err := initCmd.CombinedOutput(); err != nil {
		return PushBranchResult{}, fmt.Errorf("prepare push repository: %w (%s)", err, redact.String(strings.TrimSpace(string(out))))
	}
	if err := os.MkdirAll(filepath.Join(pushDir, "objects", "info"), 0o700); err != nil {
		return PushBranchResult{}, err
	}
	if err := os.WriteFile(filepath.Join(pushDir, "objects", "info", "alternates"), []byte(objectsDir+"\n"), 0o600); err != nil {
		return PushBranchResult{}, err
	}

	refspec := req.Commit + ":refs/heads/" + req.Branch
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.ext.allow=never",
		"-c", "http.followRedirects=false",
		"-C", pushDir,
		"push", "--no-verify", "--", pushURL, refspec,
	)
	cmd.Env = append(slices.Clone(env), authEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		var rawSecrets []string
		if token != "" {
			rawSecrets = append(rawSecrets, token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
		}
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
	if err := validateDestination(in.Destination); err != nil {
		return err
	}
	if in.Remote != "" {
		if err := validateSafeRemote(in.Remote); err != nil {
			return newActionError(http.StatusForbidden, "unknown_target", err.Error())
		}
	}
	return nil
}

// validateDestination checks the destination of a git.push value on its own:
// present, bounded, no whitespace or control characters, not an option, and
// with no user information, so a credential never enters the approved value.
func validateDestination(dest string) error {
	if dest == "" {
		return errors.New("destination is required: the push URL of the remote, without user information")
	}
	if len(dest) > maxDestinationLen {
		return fmt.Errorf("destination is longer than %d bytes", maxDestinationLen)
	}
	if strings.HasPrefix(dest, "-") {
		return errors.New("destination must not start with -")
	}
	for _, r := range dest {
		if r <= ' ' || r == 0x7f {
			return errors.New("destination must not contain whitespace or control characters")
		}
	}
	got, hadUser, err := stripUserinfo(dest)
	if hadUser || errors.Is(err, errURLPassword) {
		return errors.New("destination must not carry user information")
	}
	if err != nil {
		return fmt.Errorf("destination: %v", err)
	}
	if got != dest {
		return fmt.Errorf("destination is not in canonical form, expected %q", got)
	}
	return nil
}

// errURLPassword refuses a URL that carries a password.
var errURLPassword = errors.New("push URL carries a password")

// pushDestination returns the destination a push URL names: the URL with any
// user name removed. A URL with a password, or a URL that does not parse, is
// refused. A local path is returned as it is. The error text never quotes
// the URL, which may hold a credential.
func pushDestination(pushURL string) (string, error) {
	dest, _, err := stripUserinfo(pushURL)
	return dest, err
}

// stripUserinfo removes the user name from a URL (scheme://user@host/... or
// the scp-like user@host:path) and reports whether it had one.
func stripUserinfo(raw string) (string, bool, error) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false, errors.New("push URL does not parse")
		}
		if u.User == nil {
			return u.String(), false, nil
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", true, errURLPassword
		}
		u.User = nil
		return u.String(), true, nil
	}
	if !isRemoteURL(raw) {
		return raw, false, nil
	}
	colon := strings.Index(raw, ":")
	hostPart, path := raw[:colon], raw[colon:]
	at := strings.LastIndex(hostPart, "@")
	if at < 0 {
		return raw, false, nil
	}
	if strings.Contains(hostPart[:at], ":") {
		return "", true, errURLPassword
	}
	if at == len(hostPart)-1 {
		return "", true, errors.New("push URL has no host")
	}
	return hostPart[at+1:] + path, true, nil
}

// resolvePushRequest checks a git.push value against live state: the branch
// is this task's branch, the repository and remote are granted, the push URL
// of the remote names the approved destination, and the branch head is the
// approved commit_sha. Any drift refuses the push.
func resolvePushRequest(ctx context.Context, g *Gate, defaultRepoRoot string, wl domain.Workload, taskID string, in PushBranchInput) (PushBranchRequest, error) {
	remote := in.Remote
	if remote == "" {
		remote = "origin"
	}
	expectedBranch := expectedTaskBranch(ctx, g.store, taskID)
	if in.Branch != expectedBranch {
		return PushBranchRequest{}, newActionError(http.StatusForbidden, "wrong_branch", fmt.Sprintf("branch %q is not this task's branch", in.Branch))
	}
	repoDir, pushURL, err := resolveGrantedRepoDir(ctx, defaultRepoRoot, g.snapshotFromContext(ctx).allowedRepo, wl, remote, in.Repo)
	if err != nil {
		return PushBranchRequest{}, err
	}
	dest, err := pushDestination(pushURL)
	if err != nil {
		return PushBranchRequest{}, newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("the push URL of remote %q: %v", remote, err))
	}
	if dest != in.Destination {
		return PushBranchRequest{}, newActionError(http.StatusConflict, "destination_mismatch", fmt.Sprintf("the push URL of remote %q is %q, not the approved destination", remote, dest))
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
		URL:     pushURL,
		Branch:  in.Branch,
		Commit:  in.CommitSHA,
	}, nil
}

// resolveGrantedRepoDir returns the repository and the URL a push to remote
// goes to, refusing unless both are granted. The URL is the remote's push URL
// as git resolves it (pushurl and pushInsteadOf applied) and must be a single
// URL. It is a granted target when an entry of grants.repos names it (the
// same URL, the same absolute local path, or owner/repo of a GitHub URL with
// its scheme and host checked), or, for origin, when it is the GitHub
// repository pinned at start. A path grant names the local repository, not
// where it pushes to.
func resolveGrantedRepoDir(ctx context.Context, defaultRepoRoot, pinnedRepo string, wl domain.Workload, remote, requestedRepo string) (string, string, error) {
	hasCodingRepo := wl.Spec.Coding != nil && strings.TrimSpace(wl.Spec.Coding.RepoPath) != ""
	hasRepoGrants := len(wl.Spec.Grants.Repos) > 0
	if !hasCodingRepo && !hasRepoGrants {
		return "", "", newActionError(http.StatusForbidden, "unknown_target", "workload has no granted repository")
	}

	repoDir := strings.TrimSpace(defaultRepoRoot)
	if hasCodingRepo {
		repoDir = strings.TrimSpace(wl.Spec.Coding.RepoPath)
	}
	if repoDir == "" {
		return "", "", newActionError(http.StatusForbidden, "unknown_target", "repository root not configured for git.push")
	}

	pushURL, err := getPushURL(ctx, repoDir, remote)
	if err != nil {
		return "", "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("unknown remote %q in granted repository: %v", remote, err))
	}

	if hasRepoGrants && !matchesAnyRepoGrant(wl.Spec.Grants.Repos, repoDir, pushURL) {
		return "", "", newActionError(http.StatusForbidden, "unknown_target", "target repository is not in workload grants.repos")
	}
	pinnedTarget := remote == "origin" && pinnedRepo != "" && githubSlug(pushURL) != "" && strings.EqualFold(githubSlug(pushURL), pinnedRepo)
	if !pinnedTarget && !matchesAnyRemoteGrant(wl.Spec.Grants.Repos, pushURL) {
		return "", "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("the push URL of remote %q is not a granted push target", remote))
	}
	if requestedRepo != "" {
		if !matchesRepoTarget(requestedRepo, repoDir, pushURL) {
			return "", "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("repository %q does not match granted repository", requestedRepo))
		}
		if hasRepoGrants && !slices.ContainsFunc(wl.Spec.Grants.Repos, func(g string) bool { return sameRepoName(g, requestedRepo) }) {
			return "", "", newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("repository %q is not granted", requestedRepo))
		}
	}
	return repoDir, pushURL, nil
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

// getPushURL returns the single URL git would push to for remote, with
// pushurl and pushInsteadOf applied. A remote with several push URLs is
// refused.
func getPushURL(ctx context.Context, repoDir, remote string) (string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.ext.allow=never",
		"-C", repoDir,
		"remote", "get-url", "--push", "--all", "--", remote,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("remote not found")
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		return "", errors.New("remote must have exactly one push URL")
	}
	return strings.TrimSpace(lines[0]), nil
}

// gitObjectsDir returns the absolute object directory of the repository,
// shared by all its worktrees.
func gitObjectsDir(ctx context.Context, repoDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-C", repoDir,
		"rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("read the repository's git directory")
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "objects")
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\r\n") {
		return "", errors.New("repository object directory is not an absolute path")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", errors.New("repository object directory not found")
	}
	return dir, nil
}

// githubPushAuth returns the URL to push to and the git configuration that
// carries the token. Only an https URL of a github.com repository gets the
// token, and only for that repository: the URL is rewritten to its canonical
// form and the header is scoped to exactly it.
func githubPushAuth(pushURL, token string) (string, []string) {
	if token == "" || !isGitHubHTTPSURL(pushURL) {
		return pushURL, nil
	}
	owner, repo, ok := parseGitHubRemote(pushURL)
	if !ok {
		return pushURL, nil
	}
	canonical := "https://github.com/" + owner + "/" + repo + ".git"
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return canonical, []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + canonical + ".extraHeader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic,
	}
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

// githubSlug returns the lower-case owner/repo of a GitHub remote URL, with
// its scheme and host checked, or "" for any other URL.
func githubSlug(raw string) string {
	owner, repo, ok := parseGitHubRemote(strings.TrimSpace(raw))
	if !ok {
		return ""
	}
	return strings.ToLower(owner + "/" + repo)
}

// isPlainOwnerRepo reports whether s is written as owner/repo, not as a URL.
func isPlainOwnerRepo(s string) bool {
	return !strings.ContainsAny(s, ":@") && !strings.HasPrefix(s, "/") && normalizeOwnerRepo(s) != ""
}

// isRemoteURL reports whether git reads s as a URL (scheme://, or the
// scp-like host:path form) rather than as a local path.
func isRemoteURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	colon := strings.Index(s, ":")
	return colon > 0 && !strings.Contains(s[:colon], "/")
}

// localPath returns the absolute local path s names (a path or a file://
// URL), or "".
func localPath(s string) string {
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil || u.Host != "" {
			return ""
		}
		s = u.Path
	} else if isRemoteURL(s) {
		return ""
	}
	if !filepath.IsAbs(s) {
		return ""
	}
	return s
}

// sameRepoTarget reports whether a grant (or requested repository) names the
// push URL target: owner/repo of a GitHub URL, a GitHub URL of the same
// repository, the same non-local URL, or the same absolute local path.
func sameRepoTarget(name, target string) bool {
	name, target = strings.TrimSpace(name), strings.TrimSpace(target)
	if name == "" || target == "" {
		return false
	}
	if slug := githubSlug(target); slug != "" {
		if isPlainOwnerRepo(name) {
			return normalizeOwnerRepo(name) == slug
		}
		return githubSlug(name) == slug
	}
	if isRemoteURL(target) {
		return name == target
	}
	a, b := localPath(name), localPath(target)
	return a != "" && b != "" && sameCanonicalPath(a, b)
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

// sameRepoName reports whether two repository names (owner/repo, URL or
// absolute path) name the same repository.
func sameRepoName(a, b string) bool {
	if isPlainOwnerRepo(a) && isPlainOwnerRepo(b) {
		return normalizeOwnerRepo(a) == normalizeOwnerRepo(b)
	}
	return sameRepoTarget(a, b) || sameRepoTarget(b, a)
}

func matchesRepoTarget(candidate, repoDir, pushURL string) bool {
	cand := strings.TrimSpace(candidate)
	if cand == "" {
		return false
	}
	if sameRepoTarget(cand, pushURL) {
		return true
	}
	return localPath(cand) != "" && sameCanonicalPath(cand, repoDir)
}

func matchesAnyRepoGrant(grants []string, repoDir, pushURL string) bool {
	for _, grant := range grants {
		if matchesRepoTarget(grant, repoDir, pushURL) {
			return true
		}
	}
	return false
}

func matchesAnyRemoteGrant(grants []string, pushURL string) bool {
	for _, grant := range grants {
		if sameRepoTarget(grant, pushURL) {
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
