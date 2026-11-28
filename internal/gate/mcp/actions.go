package mcpgate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// The typed inputs below are the canonical encoding of each action: the
// approval digest is the SHA-256 of json.Marshal of the value, so field names,
// their order and omitempty are part of what an approver approved. Changing
// any of them changes the digest of every payload, and approvals pending
// across the upgrade then fail at consumption and need a new approval.
// TestCanonicalEncodingOfTypedActionsIsFixed holds a golden digest per action.

type CreateBranchInput struct {
	Branch string `json:"branch"`
	Base   string `json:"base"`
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

// FoundPR is an open pull request as GitHub lists it, with the fields the
// gate checks before it adopts one.
type FoundPR struct {
	Number     int
	URL        string
	State      string
	HeadRef    string
	HeadSHA    string
	HeadRepo   string // full name, "owner/repo"
	HeadRepoID int64
	BaseRef    string
	BaseRepoID int64
}

// PRFinder lists the open pull requests of a head branch into a base branch
// in the repository, at most two, so a git.open_pr retry can recover a pull
// request whose provenance record was not written.
type PRFinder interface {
	FindOpenPR(ctx context.Context, in OpenPRInput) ([]FoundPR, error)
}

type GitHubPROpener struct {
	Secrets secret.SecretStore
	APIURL  string
	Client  *http.Client
}

// httpClient returns a client that never follows redirects, so the gate's token
// cannot be replayed to another URL.
func (gh *GitHubPROpener) httpClient() *http.Client {
	var cli http.Client
	if gh.Client != nil {
		cli = *gh.Client
	}
	cli.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	apiBase, err := gh.apiBase()
	if err != nil {
		return "", err
	}
	token, err := gh.Secrets.Get(ctx, "github_token")
	if err != nil {
		return "", fmt.Errorf("load github_token: %w", err)
	}
	if err := redact.RegisterSetting("github_token", token); err != nil {
		return "", err
	}
	cli := gh.httpClient()

	payload, _ := json.Marshal(map[string]string{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
	})
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, url.PathEscape(in.Owner), url.PathEscape(in.Repo))
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
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
		return "", &githubAPIError{Code: resp.StatusCode, Message: strings.TrimSpace(body.Message)}
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// FindOpenPR lists the open pull requests of in.Owner:in.Head into in.Base.
// It reads one page of at most two and returns what GitHub listed; the
// caller checks every field before it trusts one.
func (gh *GitHubPROpener) FindOpenPR(ctx context.Context, in OpenPRInput) ([]FoundPR, error) {
	if err := validateRepoName("owner", in.Owner); err != nil {
		return nil, err
	}
	if err := validateRepoName("repo", in.Repo); err != nil {
		return nil, err
	}
	apiBase, token, cli, err := gh.githubAPIClient(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("state", "open")
	q.Set("head", in.Owner+":"+in.Head)
	q.Set("base", in.Base)
	q.Set("per_page", "2")
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", apiBase, url.PathEscape(in.Owner), url.PathEscape(in.Repo), q.Encode())
	type repoRef struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	var pulls []struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref  string   `json:"ref"`
			SHA  string   `json:"sha"`
			Repo *repoRef `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string   `json:"ref"`
			Repo *repoRef `json:"repo"`
		} `json:"base"`
	}
	if _, err := gh.doGitHubGET(ctx, endpoint, token, cli, &pulls); err != nil {
		return nil, fmt.Errorf("list open pull requests: %w", err)
	}
	out := make([]FoundPR, 0, len(pulls))
	for _, p := range pulls {
		f := FoundPR{
			Number:  p.Number,
			URL:     p.HTMLURL,
			State:   p.State,
			HeadRef: p.Head.Ref,
			HeadSHA: strings.ToLower(p.Head.SHA),
			BaseRef: p.Base.Ref,
		}
		if p.Head.Repo != nil {
			f.HeadRepo, f.HeadRepoID = p.Head.Repo.FullName, p.Head.Repo.ID
		}
		if p.Base.Repo != nil {
			f.BaseRepoID = p.Base.Repo.ID
		}
		out = append(out, f)
	}
	return out, nil
}

// recoverOpenedPR handles a create that GitHub refused with 422, as after a
// create whose provenance record failed. It adopts the open pull request of
// the head into the base only when every check holds: exactly one is listed,
// it is open, its head repository is the pinned repository itself (same full
// name, and the same repository ID as its base, so not a fork), its head is
// the task's branch, its base is the base asked for, its URL carries its
// number, and its head SHA is the commit the gate last pushed for the task
// when one is recorded. Anything else is refused with 409 and nothing is
// recorded. The record is marked adopted; git.merge_pr still needs an
// approval for it.
func recoverOpenedPR(ctx context.Context, g *Gate, finder PRFinder, wl domain.Workload, taskID string, in OpenPRInput, createErr error) (map[string]string, error) {
	refuse := func(why string) error {
		return newActionError(http.StatusConflict, "adopt_refused", "an open pull request was not adopted: "+why)
	}
	pinned := g.snapshotFromContext(ctx).allowedRepo
	taskBranch := expectedTaskBranch(ctx, g.store, taskID)
	if in.Head != taskBranch {
		return nil, refuse(fmt.Sprintf("head %s is not the task branch %s", in.Head, taskBranch))
	}
	found, err := finder.FindOpenPR(ctx, in)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, createErr
	case 1:
	default:
		return nil, refuse("more than one open pull request for the head and base")
	}
	p := found[0]
	num, ok := parsePRNumber(p.URL)
	switch {
	case p.State != "open":
		return nil, refuse("it is not open")
	case pinned == "" || !strings.EqualFold(p.HeadRepo, pinned) || p.HeadRepoID <= 0 || p.HeadRepoID != p.BaseRepoID:
		return nil, refuse("its head repository is not the pinned repository")
	case p.HeadRef != taskBranch:
		return nil, refuse("its head is not the task branch " + taskBranch)
	case p.BaseRef != in.Base:
		return nil, refuse("its base is not " + in.Base)
	case !ok || num != p.Number:
		return nil, refuse("its URL does not carry its number")
	case !isCommitID(p.HeadSHA):
		return nil, refuse("its head SHA is invalid")
	}
	pushed, hasPush, err := lookupPushedCommit(ctx, g.store, taskID)
	if err != nil {
		return nil, err
	}
	if hasPush && pushed.Branch == taskBranch && pushed.Commit != p.HeadSHA {
		return nil, refuse("its head is not the commit the gate pushed for the task")
	}
	res := map[string]string{"pr_url": p.URL, "head": in.Head, "base": in.Base, "pr_number": strconv.Itoa(num), "status": "adopted"}
	entry, err := g.store.GetMemory(ctx, openedPRsNamespace, prKey(in.Owner, in.Repo, num))
	switch {
	case err == nil:
		var rec OpenedPRRecord
		if entry.Author != "gate" || json.Unmarshal([]byte(entry.Value), &rec) != nil ||
			rec.TaskID != taskID || rec.Head != in.Head || rec.Base != in.Base {
			return nil, refuse("it is recorded for another task or branch")
		}
		if !rec.Adopted {
			res["status"] = "existing"
		}
		return res, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if err := recordOpenedPR(ctx, g, OpenedPRRecord{
		Owner: in.Owner, Repo: in.Repo, Number: num, URL: p.URL, Head: in.Head, Base: in.Base,
		TaskID: taskID, Workload: wl.Metadata.Name, OpenedAt: g.now(),
		Adopted: true, HeadSHA: p.HeadSHA,
	}); err != nil {
		return nil, fmt.Errorf("record adopted pull request: %w", err)
	}
	return res, nil
}

// RepoBinding holds the GitHub repository the GitOps actions may target: the
// origin remote of the repo root, read once when the catalog is registered at
// daemon start. Nothing changes the pin afterwards: a reload only compares the
// origin with it, so pointing the checkout at another repository needs a
// daemon restart.
type RepoBinding struct {
	gate     *Gate
	repoRoot string

	once   sync.Once
	pin    string // "owner/repo", or "" when the start read failed
	pinErr error

	mu sync.Mutex
	// drift identifies the origin the last reload observed differing from
	// the pin: the parsed owner/repo, or for an origin that cannot be used the
	// read error plus a SHA-256 of the raw remote, so two different unusable
	// remotes count as two changes. It is "" while the origin matches. A drift
	// audit row is owed only when this changes, not on every reload. The raw
	// remote may hold credentials and is never logged or audited.
	drift string
	// owed holds drift rows not yet written, oldest first. Each reload tries
	// them in order and stops at the first failure, so a row stays owed when
	// the origin changes again before it is written. At most maxOwedDrift are
	// kept; past that the oldest is dropped, and the reload still logs the
	// drift it sees.
	owed []domain.AuditRecord
}

const maxOwedDrift = 16

// pinOnce reads the origin and pins it on the gate. Only the first call reads;
// later calls return the first result.
func (b *RepoBinding) pinOnce(ctx context.Context) error {
	b.once.Do(func() {
		owner, repo, err := readGitHubOrigin(ctx, b.repoRoot)
		if err != nil {
			b.pinErr = err
			b.gate.SetAllowedRepo("")
			return
		}
		b.pin = owner + "/" + repo
		b.gate.SetAllowedRepo(b.pin)
	})
	return b.pinErr
}

// Pinned returns the repository pinned at start, or the error that left none
// pinned (every git.open_pr is then refused until a restart).
func (b *RepoBinding) Pinned() (string, error) {
	_ = b.pinOnce(context.Background())
	return b.pin, b.pinErr
}

// Recheck runs on every reload. It never changes the pin: it reads the origin
// and, when the origin cannot be read or names another repository, returns an
// error for the daemon to log. A denied audit row records the drift once per
// change of the observed origin. git.open_pr stays refused while the origin
// differs, because each call compares the origin with the pin again.
func (b *RepoBinding) Recheck(ctx context.Context) error {
	if err := b.pinOnce(ctx); err != nil {
		return fmt.Errorf("no repository was pinned at start (%v); git.open_pr is refused until the daemon restarts", err)
	}
	detail := map[string]string{"allowed_repo": b.pin}
	var observed string
	var owner, repo string
	raw, err := readOriginURL(ctx, b.repoRoot)
	if err == nil {
		owner, repo, err = parseOrigin(raw)
	}
	switch {
	case err != nil:
		sum := sha256.Sum256([]byte(raw))
		observed = "unreadable: " + err.Error() + " " + hex.EncodeToString(sum[:])
		detail["reason"] = "repo_origin_unreadable"
		detail["error"] = err.Error()
		err = fmt.Errorf("origin of the repo root cannot be read at reload (%v); git.open_pr stays refused, pinned to %s", err, b.pin)
	case !strings.EqualFold(owner+"/"+repo, b.pin):
		observed = owner + "/" + repo
		detail["reason"] = "repo_origin_changed"
		detail["origin"] = observed
		err = fmt.Errorf("origin of the repo root is %s but %s was pinned at start; git.open_pr stays refused, restart the daemon to change the repository", observed, b.pin)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if observed != b.drift {
		b.drift = observed
		if err != nil {
			raw, _ := json.Marshal(detail)
			b.owed = append(b.owed, domain.AuditRecord{
				Category:   "data_action_gate",
				Action:     "action:git.open_pr",
				Outcome:    "denied",
				DetailJSON: string(raw),
				CreatedAt:  b.gate.now(),
			})
			if n := len(b.owed); n > maxOwedDrift {
				b.owed = append([]domain.AuditRecord(nil), b.owed[n-maxOwedDrift:]...)
			}
		}
	}
	for len(b.owed) > 0 {
		if appendErr := b.gate.store.AppendAudit(ctx, b.owed[0]); appendErr != nil {
			break // still owed, so the next reload writes it
		}
		b.owed = b.owed[1:]
	}
	return err
}

// GitOpsOptions configures the built-in GitOps actions and tools on the gate.
// PRChecker and PRMerger default to PROpener when it implements them, and
// BranchPusher defaults to a GitBranchPusher with Secrets.
type GitOpsOptions struct {
	RepoRoot     string
	PROpener     PROpener
	PRChecker    PRChecker
	PRMerger     PRMerger
	BranchPusher BranchPusher
	Secrets      secret.SecretStore
}

// RegisterBuiltinGitOps registers the closed catalog of GitOps typed actions
// and the read-only MCP tools, and pins the repository git.open_pr and
// git.merge_pr may target. The pin is final; the returned binding only checks
// the origin against it.
func RegisterBuiltinGitOps(g *Gate, repoRoot string, prOpener PROpener) *RepoBinding {
	opts := GitOpsOptions{RepoRoot: repoRoot, PROpener: prOpener}
	if gh, ok := prOpener.(*GitHubPROpener); ok && gh != nil {
		opts.Secrets = gh.Secrets
	}
	return RegisterBuiltinGitOpsWithOptions(g, opts)
}

// RegisterBuiltinGitOpsWithOptions is RegisterBuiltinGitOps with every
// backend named.
func RegisterBuiltinGitOpsWithOptions(g *Gate, opts GitOpsOptions) *RepoBinding {
	repoRoot := opts.RepoRoot
	prOpener := opts.PROpener
	prChecker := opts.PRChecker
	if prChecker == nil {
		prChecker, _ = prOpener.(PRChecker)
	}
	prMerger := opts.PRMerger
	if prMerger == nil {
		prMerger, _ = prOpener.(PRMerger)
	}
	pusher := opts.BranchPusher
	if pusher == nil {
		pusher = &GitBranchPusher{Secrets: opts.Secrets}
	}

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

	RegisterTypedAction(g, "git.create_branch", validateCreateBranch, func(ctx context.Context, _ domain.Workload, _ string, in CreateBranchInput) (any, error) {
		if repoRoot == "" {
			return nil, errors.New("repo root not configured for git.create_branch")
		}
		cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "branch", "--", in.Branch, in.Base)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git branch failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		store.NoteEffect(ctx, "branch %s was created from %s", in.Branch, in.Base)
		return map[string]string{"branch": in.Branch, "base": in.Base, "status": "created"}, nil
	})

	// The target is checked against the pin when the payload is decoded, so a
	// request for another repository is refused before an approval is
	// requested or spent. The run step reads the origin again, because the
	// checkout can change after the approval was asked for.
	validateOpenPRTarget := func(in OpenPRInput) error {
		if err := validateOpenPR(in); err != nil {
			return err
		}
		if err := checkPinnedTarget(g.snapshot().allowedRepo, in.Owner, in.Repo); err != nil {
			return fmt.Errorf("%w: %v", errToolRefused, err)
		}
		return nil
	}
	registerTypedActionDef(g, "git.open_pr", typedAction[OpenPRInput]{
		validate: validateOpenPRTarget,
		precheck: func(_ context.Context, wl domain.Workload, _ string, in OpenPRInput) error {
			return checkOwnerRepoGrant(wl, in.Owner, in.Repo)
		},
		run: func(ctx context.Context, wl domain.Workload, taskID string, in OpenPRInput) (any, error) {
			if err := checkOpenPRTarget(ctx, repoRoot, g.snapshotFromContext(ctx).allowedRepo, in.Owner, in.Repo); err != nil {
				return nil, fmt.Errorf("%w: %v", errToolRefused, err)
			}
			if err := checkOwnerRepoGrant(wl, in.Owner, in.Repo); err != nil {
				return nil, err
			}
			if prOpener == nil {
				return nil, errors.New("PR opener not configured")
			}
			prURL, err := prOpener.OpenPR(ctx, in)
			store.NoteEffectIf(ctx, err, "pull request %s was opened from %s to %s", prURL, in.Head, in.Base)
			if err != nil {
				// A retry after a failed provenance write finds the pull
				// request the first call opened instead of stranding it.
				var apiErr *githubAPIError
				if finder, ok := prOpener.(PRFinder); ok && errors.As(err, &apiErr) && apiErr.Code == http.StatusUnprocessableEntity {
					return recoverOpenedPR(ctx, g, finder, wl, taskID, in, err)
				}
				return nil, err
			}
			res := map[string]string{"pr_url": prURL, "head": in.Head, "base": in.Base}
			if num, ok := parsePRNumber(prURL); ok {
				res["pr_number"] = strconv.Itoa(num)
				if err := recordOpenedPR(ctx, g, OpenedPRRecord{
					Owner:    in.Owner,
					Repo:     in.Repo,
					Number:   num,
					URL:      prURL,
					Head:     in.Head,
					Base:     in.Base,
					TaskID:   taskID,
					Workload: wl.Metadata.Name,
					OpenedAt: g.now(),
				}); err != nil {
					return nil, fmt.Errorf("record opened pull request: %w", err)
				}
			}
			return res, nil
		},
	})

	// git.push always needs an out-of-band approval. The approved value names
	// the commit and the destination; the precheck and the run step both
	// compare them with the live head of the task branch and the live push URL
	// of the remote, and the pusher compares the head once more just before it
	// pushes to the URL the run step resolved.
	registerTypedActionDef(g, "git.push", typedAction[PushBranchInput]{
		validate:         validatePushInput,
		requiresApproval: true,
		precheck: func(ctx context.Context, wl domain.Workload, taskID string, in PushBranchInput) error {
			_, err := resolvePushRequest(ctx, g, repoRoot, wl, taskID, in)
			return err
		},
		run: func(ctx context.Context, wl domain.Workload, taskID string, in PushBranchInput) (any, error) {
			req, err := resolvePushRequest(ctx, g, repoRoot, wl, taskID, in)
			if err != nil {
				return nil, err
			}
			res, err := pusher.PushBranch(ctx, req)
			store.NoteEffectIf(ctx, err, "commit %s was pushed to branch %s", res.Commit, res.Branch)
			if err != nil {
				return nil, err
			}
			// The record lets a git.open_pr retry check the head of a pull
			// request it would adopt against the commit pushed here.
			if err := recordPushedCommit(ctx, g, taskID, res); err != nil {
				return nil, fmt.Errorf("the push of %s to %s succeeded, but recording it failed: %w", res.Commit, res.Branch, err)
			}
			return res, nil
		},
	})

	g.RegisterTool(ToolDef{
		Name:        "git.pr_checks",
		Description: "Read check state for a pull request Sablecourt opened and store timestamped evidence",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pr_number":{"type":"integer"}},"required":["owner","repo","pr_number"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			in, err := decodeTyped[PRChecksInput](args)
			if err != nil {
				return "", err
			}
			if err := validatePRChecks(in); err != nil {
				return "", err
			}
			report, err := executePRChecks(ctx, g, prChecker, wl, taskID, in)
			if err != nil {
				return "", err
			}
			out, err := json.Marshal(report)
			return string(out), err
		},
	})
	RegisterTypedAction(g, "git.pr_checks", validatePRChecks, func(ctx context.Context, wl domain.Workload, taskID string, in PRChecksInput) (any, error) {
		return executePRChecks(ctx, g, prChecker, wl, taskID, in)
	})

	// git.merge_pr always needs an out-of-band approval, and the precheck asks
	// for it only once the required checks have passed at the approved commit.
	// The run step reads the checks again, refuses when the head moved, and
	// refuses when a check result is newer than the approval.
	registerTypedActionDef(g, "git.merge_pr", typedAction[MergePRInput]{
		validate:         validateMergePRInput,
		requiresApproval: true,
		precheck: func(ctx context.Context, wl domain.Workload, taskID string, in MergePRInput) error {
			_, err := checkMergeReady(ctx, g, prChecker, wl, taskID, in)
			return err
		},
		run: func(ctx context.Context, wl domain.Workload, taskID string, in MergePRInput) (any, error) {
			report, err := checkMergeReady(ctx, g, prChecker, wl, taskID, in)
			if err != nil {
				return nil, err
			}
			if err := checkApprovalAfterChecks(ctx, g, report); err != nil {
				return nil, err
			}
			if prMerger == nil {
				return nil, errors.New("PR merger not configured")
			}
			method := in.MergeMethod
			if method == "" {
				method = "squash"
			}
			res, err := prMerger.MergePR(ctx, MergePRRequest{
				Owner:       in.Owner,
				Repo:        in.Repo,
				PRNumber:    in.PRNumber,
				CommitSHA:   in.CommitSHA,
				MergeMethod: method,
			})
			store.NoteEffectIf(ctx, err, "pull request %s/%s#%d was merged", in.Owner, in.Repo, in.PRNumber)
			return res, err
		},
	})

	RegisterTypedAction(g, "sandbox.propose_promotion", validatePromotion, func(ctx context.Context, _ domain.Workload, _ string, in PromoteSandboxInput) (any, error) {
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

	binding := &RepoBinding{gate: g, repoRoot: repoRoot}
	_ = binding.pinOnce(context.Background()) // an unresolved origin refuses every git.open_pr
	return binding
}

func validateCreateBranch(in CreateBranchInput) error {
	if err := validateSafeBranch(in.Branch); err != nil {
		return err
	}
	if err := validateRefName(in.Base); err != nil {
		return fmt.Errorf("invalid base: %w", err)
	}
	return nil
}

func validateOpenPR(in OpenPRInput) error {
	if err := validateRepoName("owner", in.Owner); err != nil {
		return err
	}
	if err := validateRepoName("repo", in.Repo); err != nil {
		return err
	}
	if err := validateSafeBranch(in.Head); err != nil {
		return fmt.Errorf("invalid head branch: %w", err)
	}
	if err := validateRefName(in.Base); err != nil {
		return fmt.Errorf("invalid base: %w", err)
	}
	if strings.TrimSpace(in.Title) == "" {
		return errors.New("title is required")
	}
	return nil
}

func validatePromotion(in PromoteSandboxInput) error {
	if strings.TrimSpace(in.Workload) == "" {
		return errors.New("workload is required")
	}
	return validateSafeBranch(in.Branch)
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
// pinned when the daemon started (allowed, "owner/repo"): the origin remote of
// repoRoot, where git.create_branch makes the head branch. A workload's
// grants.repos narrows this further, checked separately by the caller
// (checkOwnerRepoGrant). If the origin read now differs from the pin, the
// checkout changed under the daemon and the call is refused.
func checkOpenPRTarget(ctx context.Context, repoRoot, allowed, owner, repo string) error {
	if err := validateRepoName("owner", owner); err != nil {
		return err
	}
	if err := validateRepoName("repo", repo); err != nil {
		return err
	}
	if err := checkPinnedTarget(allowed, owner, repo); err != nil {
		return err
	}
	curOwner, curRepo, err := readGitHubOrigin(ctx, repoRoot)
	if err != nil {
		return err
	}
	if !strings.EqualFold(curOwner+"/"+curRepo, allowed) {
		return fmt.Errorf("origin of the repo root changed to %s/%s but %s was pinned at start; restart the daemon to change the repository", curOwner, curRepo, allowed)
	}
	return nil
}

// checkPinnedTarget allows owner/repo only when it is the repository pinned
// at start (allowed, "owner/repo"), and nothing when none was pinned.
func checkPinnedTarget(allowed, owner, repo string) error {
	if allowed == "" {
		return errors.New("no GitHub repository was pinned for git.open_pr at start")
	}
	if !strings.EqualFold(owner+"/"+repo, allowed) {
		return fmt.Errorf("git.open_pr target %s/%s is not the configured repository", owner, repo)
	}
	return nil
}

// readGitHubOrigin reads owner and repository from the origin remote of repoRoot.
func readGitHubOrigin(ctx context.Context, repoRoot string) (owner, repo string, err error) {
	raw, err := readOriginURL(ctx, repoRoot)
	if err != nil {
		return "", "", err
	}
	return parseOrigin(raw)
}

// readOriginURL returns the raw origin remote URL of repoRoot. It may carry
// credentials, so callers never log or audit it.
func readOriginURL(ctx context.Context, repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", errors.New("repo root not configured for git.open_pr")
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", errors.New("repo root has no origin remote for git.open_pr")
	}
	return strings.TrimSpace(string(out)), nil
}

// parseOrigin reads owner and repository from a raw origin remote URL.
func parseOrigin(raw string) (owner, repo string, err error) {
	owner, repo, ok := parseGitHubRemote(raw)
	if !ok {
		return "", "", errors.New("origin remote of the repo root is not a GitHub repository")
	}
	return owner, repo, nil
}

// parseGitHubRemote reads owner and repository from a github.com remote URL in
// HTTPS, SSH URL or scp-like form, or from GitHub's SSH over port 443
// (the ssh scheme with user git, host ssh.github.com and port 443).
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
		// GitHub's SSH over the HTTPS port uses its own host, accepted only
		// with ssh on port 443.
		sshOver443 := u.Scheme == "ssh" && strings.EqualFold(u.Hostname(), "ssh.github.com") && u.Port() == "443"
		if !sshOver443 && ((u.Scheme != "https" && u.Scheme != "ssh") || !strings.EqualFold(u.Hostname(), "github.com")) {
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

// refComponent is one slash-separated part of a ref name: it starts with a
// letter or digit and holds only letters, digits, '.', '_' and '-'.
var refComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateRefName accepts a branch name built from refComponent parts, at most
// 200 bytes, with no "..", no component ending in "." or ".lock", and not
// HEAD. The value checked is used as given: nothing is trimmed.
func validateRefName(ref string) error {
	if ref == "" {
		return errors.New("ref name is required")
	}
	if len(ref) > 200 || strings.Contains(ref, "..") || strings.EqualFold(ref, "HEAD") {
		return fmt.Errorf("invalid ref name %q", ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if !refComponent.MatchString(part) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid ref name %q", ref)
		}
	}
	return nil
}

// validateSafeBranch accepts a ref name under sablecourt/, the only branches
// the gate writes.
func validateSafeBranch(branch string) error {
	if err := validateRefName(branch); err != nil {
		return err
	}
	if !strings.HasPrefix(branch, "sablecourt/") {
		return errors.New("branch name must start with sablecourt/")
	}
	return nil
}
