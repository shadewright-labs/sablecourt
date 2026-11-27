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
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const runtimeHandlesNamespace = "system:runtime-handles"

// pushedCommitsNamespace holds, per task, the branch and commit of the last
// git.push the gate made for it.
const pushedCommitsNamespace = "system:pushed-commits"

// PushedCommitRecord is the last push the gate made for a task.
type PushedCommitRecord struct {
	TaskID   string    `json:"task_id"`
	Branch   string    `json:"branch"`
	Commit   string    `json:"commit"`
	PushedAt time.Time `json:"pushed_at"`
}

func recordPushedCommit(ctx context.Context, g *Gate, taskID string, res PushBranchResult) error {
	if g == nil || g.store == nil || taskID == "" {
		return nil
	}
	raw, err := json.Marshal(PushedCommitRecord{TaskID: taskID, Branch: res.Branch, Commit: res.Commit, PushedAt: g.now()})
	if err != nil {
		return err
	}
	return g.store.PutMemory(ctx, domain.MemoryEntry{
		Namespace: pushedCommitsNamespace,
		Key:       taskID,
		Value:     string(raw),
		Author:    "gate",
		UpdatedAt: g.now(),
	})
}

// lookupPushedCommit returns the last push recorded for the task. A record
// that is not the gate's or does not parse is an error, not "none".
func lookupPushedCommit(ctx context.Context, st store.Store, taskID string) (PushedCommitRecord, bool, error) {
	if st == nil || taskID == "" {
		return PushedCommitRecord{}, false, nil
	}
	entry, err := st.GetMemory(ctx, pushedCommitsNamespace, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return PushedCommitRecord{}, false, nil
	}
	if err != nil {
		return PushedCommitRecord{}, false, err
	}
	var rec PushedCommitRecord
	if entry.Author != "gate" || json.Unmarshal([]byte(entry.Value), &rec) != nil || rec.TaskID != taskID || !isCommitID(rec.Commit) {
		return PushedCommitRecord{}, false, errors.New("the pushed commit record of the task is invalid")
	}
	return rec, true, nil
}

// PushBranchInput is the approved value of git.push. CommitSHA and
// Destination are required and the dialog shows both as targets. The push
// happens only while the task branch still points at CommitSHA and the push
// URL of Remote is still exactly Destination. An empty
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

	// extraEnv is added to the git environment before the gate's own
	// settings; tests use it to stand in for a system or global gitconfig.
	extraEnv []string
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

	pushURL := req.URL
	if err := checkPushTarget(pushURL); err != nil {
		return PushBranchResult{}, newActionError(http.StatusForbidden, "unknown_target", "the push URL "+err.Error())
	}
	var token string
	if p != nil && p.Secrets != nil {
		if tok, err := p.Secrets.Get(ctx, "github_token"); err == nil {
			token = strings.TrimSpace(tok)
		}
	}
	if token != "" {
		if err := redact.RegisterSetting("github_token", token); err != nil {
			return PushBranchResult{}, err
		}
	}
	pushURL, authEnv := githubPushAuth(pushURL, token)

	objectsDir, err := gitObjectsDir(ctx, req.RepoDir)
	if err != nil {
		return PushBranchResult{}, err
	}
	objectFormat, err := gitObjectFormat(ctx, req.RepoDir)
	if err != nil {
		return PushBranchResult{}, err
	}
	// The push runs from an empty bare repository of the gate's own that
	// borrows the task repository's objects and has its object format (SHA-1
	// or SHA-256), so it can read them. It has no refs, so the commit_sha
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
	if p != nil {
		env = append(env, p.extraEnv...)
	}
	// No system or global gitconfig applies to the gate's git commands, so an
	// insteadOf or pushInsteadOf rule outside the gate cannot rewrite the
	// checked URL. HOME is not set. These come last and win over any earlier
	// entry with the same name.
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	initCmd := exec.CommandContext(ctx, "git", "init", "--quiet", "--bare", "--template=", "--object-format="+objectFormat, "--", pushDir)
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

// validateDestination checks the destination of a git.push value on its own
// with the same allow-list that decides for the resolved push URL, so a
// credential or a helper URL never enters the approved value.
func validateDestination(dest string) error {
	if dest == "" {
		return errors.New("destination is required: the push URL of the remote")
	}
	if err := checkPushTarget(dest); err != nil {
		return fmt.Errorf("destination %v", err)
	}
	return nil
}

// acceptedPushForms names the forms checkPushTarget accepts, for error text.
const acceptedPushForms = "https://host[:port]/path, ssh://[git@]host[:port]/path, git@host:path or an absolute local path"

// checkPushTarget is the one decision on where a push may go, used for the
// destination of a git.push value and for the push URL git resolves. It
// accepts only these forms, read as they are and never rewritten:
//
//   - https://host[:port]/path with no user information
//   - ssh://host[:port]/path or ssh://git@host[:port]/path
//   - the scp-like git@host:path, with the user git and nothing else
//   - an absolute local path with no colon
//
// Everything else is refused: remote helper syntax (any "::"), other
// schemes (file, http, git, ext...), opaque URLs, any user other than plain
// git, any password, queries, fragments, percent escapes and characters
// outside a small set. The error text never quotes the target, which may
// hold a credential.
func checkPushTarget(raw string) error {
	if raw == "" {
		return errors.New("is empty")
	}
	if len(raw) > maxDestinationLen {
		return fmt.Errorf("is longer than %d bytes", maxDestinationLen)
	}
	for _, r := range raw {
		if r <= ' ' || r >= 0x7f {
			return errors.New("must not contain whitespace, control or non-ASCII characters")
		}
	}
	if strings.Contains(raw, "::") {
		return errors.New("must not use remote helper syntax (::)")
	}
	if strings.HasPrefix(raw, "/") {
		if strings.ContainsAny(raw, ":\\") {
			return errors.New("is a local path with a colon or backslash")
		}
		return nil
	}
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		if scheme != "https" && scheme != "ssh" {
			return fmt.Errorf("is not one of the accepted forms (%s)", acceptedPushForms)
		}
		authority, path, ok := strings.Cut(rest, "/")
		if !ok {
			return errors.New("has no repository path")
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			if scheme != "ssh" || authority[:at] != "git" {
				return errors.New("must not carry user information other than the ssh user git")
			}
			authority = authority[at+1:]
		}
		host, port, hasPort := strings.Cut(authority, ":")
		if err := checkPushHost(host); err != nil {
			return err
		}
		if hasPort && !isPushPort(port) {
			return errors.New("has an invalid port")
		}
		return checkPushPath(path)
	}
	at, colon := strings.Index(raw, "@"), strings.Index(raw, ":")
	if colon < 0 {
		return fmt.Errorf("is not one of the accepted forms (%s)", acceptedPushForms)
	}
	if at < 0 || at > colon || raw[:at] != "git" {
		return errors.New("in the scp-like form must name the user git and carry no other user information")
	}
	if err := checkPushHost(raw[at+1 : colon]); err != nil {
		return err
	}
	return checkPushPath(raw[colon+1:])
}

// checkPushHost accepts a DNS name: letters, digits, dots and hyphens, not
// starting with a dot or hyphen. IP literals in brackets are refused.
func checkPushHost(host string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.HasPrefix(host, ".") {
		return errors.New("has no valid host")
	}
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			continue
		}
		return errors.New("has no valid host")
	}
	return nil
}

func isPushPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkPushPath accepts a repository path of letters, digits and . _ ~ / -,
// not starting with a hyphen.
func checkPushPath(path string) error {
	if path == "" || strings.HasPrefix(path, "-") {
		return errors.New("has no valid repository path")
	}
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '~' || r == '/' || r == '-' {
			continue
		}
		return errors.New("has a repository path with characters outside letters, digits and . _ ~ / -")
	}
	return nil
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
	if err := checkPushTarget(pushURL); err != nil {
		return PushBranchRequest{}, newActionError(http.StatusForbidden, "unknown_target", fmt.Sprintf("the push URL of remote %q %v", remote, err))
	}
	if pushURL != in.Destination {
		return PushBranchRequest{}, newActionError(http.StatusConflict, "destination_mismatch", fmt.Sprintf("the push URL of remote %q is %q, not the approved destination", remote, pushURL))
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

// gitObjectFormat returns the object format of the repository, "sha1" or
// "sha256"; anything else is an error.
func gitObjectFormat(ctx context.Context, repoDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-C", repoDir,
		"rev-parse", "--show-object-format",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("read the repository's object format")
	}
	switch f := strings.TrimSpace(string(out)); f {
	case "sha1", "sha256":
		return f, nil
	default:
		return "", errors.New("repository object format is neither sha1 nor sha256")
	}
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
