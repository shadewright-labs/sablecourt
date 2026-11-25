package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type CreateBranchInput struct {
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
}

type OpenPRInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

type PromoteSandboxInput struct {
	Workload string `json:"workload"`
	Branch   string `json:"branch"`
	Reason   string `json:"reason"`
}

// PROpener creates a GitHub pull request using gate-owned credentials.
type PROpener interface {
	OpenPR(ctx context.Context, in OpenPRInput) (string, error)
}

type GitHubPROpener struct {
	Secrets secret.SecretStore
	APIURL  string
	Client  *http.Client
	// Timeout bounds one API call; zero means defaultGitHubTimeout.
	Timeout time.Duration
}

const defaultGitHubTimeout = 30 * time.Second

// httpClient returns a client that never follows redirects, so the gate's token
// cannot be replayed to another URL, and that gives up after the timeout.
func (gh *GitHubPROpener) httpClient() *http.Client {
	var cli http.Client
	if gh.Client != nil {
		cli = *gh.Client
	}
	cli.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if gh.Timeout > 0 {
		cli.Timeout = gh.Timeout
	}
	if cli.Timeout <= 0 {
		cli.Timeout = defaultGitHubTimeout
	}
	return &cli
}

func (gh *GitHubPROpener) OpenPR(ctx context.Context, in OpenPRInput) (string, error) {
	if err := validateRepoName("owner", in.Owner); err != nil {
		return "", err
	}
	if err := validateRepoName("repo", in.Repo); err != nil {
		return "", err
	}
	if gh.Secrets == nil {
		return "", errors.New("secret store unavailable for github_token")
	}
	token, err := gh.Secrets.Get(ctx, "github_token")
	if err != nil {
		return "", fmt.Errorf("load github_token: %w", err)
	}
	apiBase := gh.APIURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	cli := gh.httpClient()

	payload, _ := json.Marshal(map[string]string{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
	})
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", strings.TrimRight(apiBase, "/"), url.PathEscape(in.Owner), url.PathEscape(in.Repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github API returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// RegisterBuiltinGitOps registers the closed catalog of GitOps typed actions and a read-only MCP tool.
func RegisterBuiltinGitOps(g *Gate, repoRoot string, prOpener PROpener) {
	g.RegisterTool(ToolDef{
		Name:        "sablecourt.list_workloads",
		Description: "List declared workloads in the current Sablecourt bundle",
		Handler: func(ctx context.Context, _ domain.Workload, _ string, _ json.RawMessage) (string, error) {
			items, err := g.store.ListWorkloads(ctx)
			if err != nil {
				return "", err
			}
			raw, err := json.Marshal(items)
			return string(raw), err
		},
	})

	g.RegisterAction("git.create_branch", func(ctx context.Context, _ domain.Workload, _ string, raw json.RawMessage) (any, error) {
		var in CreateBranchInput
		if err := decodeStrict(raw, &in); err != nil {
			return nil, err
		}
		if err := validateSafeBranch(in.Branch); err != nil {
			return nil, err
		}
		if repoRoot == "" {
			return nil, errors.New("repo root not configured for git.create_branch")
		}
		base := strings.TrimSpace(in.Base)
		if strings.HasPrefix(base, "-") || strings.Contains(base, "..") || strings.ContainsAny(base, " \t\r\n") {
			return nil, errors.New("invalid base ref for git.create_branch")
		}
		args := []string{"-C", repoRoot, "branch", "--", in.Branch}
		if base != "" {
			args = append(args, base)
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git branch failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return map[string]string{"branch": in.Branch, "status": "created"}, nil
	})

	g.RegisterAction("git.open_pr", func(ctx context.Context, _ domain.Workload, _ string, raw json.RawMessage) (any, error) {
		var in OpenPRInput
		if err := decodeStrict(raw, &in); err != nil {
			return nil, err
		}
		if err := validateSafeBranch(in.Head); err != nil {
			return nil, fmt.Errorf("invalid head branch: %w", err)
		}
		if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Repo) == "" || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Base) == "" {
			return nil, errors.New("owner, repo, base, and title are required")
		}
		if err := checkOpenPRTarget(ctx, repoRoot, in.Owner, in.Repo); err != nil {
			return nil, fmt.Errorf("%w: %v", errToolRefused, err)
		}
		if prOpener == nil {
			return nil, errors.New("PR opener not configured")
		}
		prURL, err := prOpener.OpenPR(ctx, in)
		if err != nil {
			return nil, err
		}
		return map[string]string{"pr_url": prURL, "head": in.Head, "base": in.Base}, nil
	})

	g.RegisterAction("sandbox.propose_promotion", func(ctx context.Context, _ domain.Workload, _ string, raw json.RawMessage) (any, error) {
		var in PromoteSandboxInput
		if err := decodeStrict(raw, &in); err != nil {
			return nil, err
		}
		if err := validateSafeBranch(in.Branch); err != nil {
			return nil, err
		}
		snap := g.snapshotFromContext(ctx)
		if snap.bundle == nil {
			return nil, errors.New("no manifest bundle loaded")
		}
		target, ok := snap.bundle.Workloads[in.Workload]
		if !ok {
			return nil, fmt.Errorf("workload %q not found", in.Workload)
		}
		if target.Metadata.Zone != domain.ZoneSandbox {
			return nil, fmt.Errorf("workload %q is already in zone %q", in.Workload, target.Metadata.Zone)
		}
		return map[string]string{
			"status":   "proposal_ready",
			"workload": in.Workload,
			"branch":   in.Branch,
			"next":     "open PR to change metadata.zone from sandbox to runtime",
		}, nil
	})
}

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// validateRepoName accepts one GitHub owner or repository name: letters, digits,
// '_', '.', '-', and not made of dots alone, so it is a single path segment.
func validateRepoName(field, name string) error {
	if !repoNamePattern.MatchString(name) || strings.Trim(name, ".") == "" {
		return fmt.Errorf("invalid %s for git.open_pr", field)
	}
	return nil
}

// checkOpenPRTarget allows a pull request only against the GitHub repository
// that the gate's GitOps actions work in: the origin remote of repoRoot, where
// git.create_branch makes the head branch. Workload grants name actions, not
// repositories, so this is the narrowest target the manifests can bind today.
func checkOpenPRTarget(ctx context.Context, repoRoot, owner, repo string) error {
	if err := validateRepoName("owner", owner); err != nil {
		return err
	}
	if err := validateRepoName("repo", repo); err != nil {
		return err
	}
	if repoRoot == "" {
		return errors.New("repo root not configured for git.open_pr")
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "remote", "get-url", "origin").Output()
	if err != nil {
		return errors.New("repo root has no origin remote for git.open_pr")
	}
	allowedOwner, allowedRepo, ok := parseGitHubRemote(strings.TrimSpace(string(out)))
	if !ok {
		return errors.New("origin remote of the repo root is not a GitHub repository")
	}
	if !strings.EqualFold(owner, allowedOwner) || !strings.EqualFold(repo, allowedRepo) {
		return fmt.Errorf("git.open_pr target %s/%s is not the configured repository", owner, repo)
	}
	return nil
}

// parseGitHubRemote reads owner and repository from a github.com remote URL in
// HTTPS, SSH URL or scp-like form.
func parseGitHubRemote(remote string) (owner, repo string, ok bool) {
	var path string
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		path = strings.TrimPrefix(remote, "git@github.com:")
	default:
		u, err := url.Parse(remote)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		if (u.Scheme != "https" && u.Scheme != "ssh") || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", "", false
		}
		if u.Port() != "" && u.Scheme == "https" {
			return "", "", false
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || validateRepoName("owner", parts[0]) != nil || validateRepoName("repo", parts[1]) != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func decodeStrict(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid action payload: %w", err)
	}
	return nil
}

func validateSafeBranch(branch string) error {
	b := strings.TrimSpace(branch)
	if b == "" || b == "main" || b == "master" {
		return errors.New("protected or empty branch is forbidden")
	}
	if !strings.HasPrefix(b, "sablecourt/") {
		return errors.New("branch name must start with sablecourt/")
	}
	if strings.Contains(b, "..") || strings.ContainsAny(b, " \t\n~^:?*[\\") {
		return errors.New("branch name contains illegal characters")
	}
	return nil
}
