package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestResolveCheckoutRef(t *testing.T) {
	repo := initTempGitRepo(t)
	first := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testGit(t, repo, "tag", "v1")
	testGit(t, repo, "branch", "same")
	testGit(t, repo, "tag", "same")
	testGit(t, repo, "commit", "--allow-empty", "-q", "-m", "second")
	second := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testGit(t, repo, "branch", "feature")
	testGit(t, repo, "tag", "split", first)
	testGit(t, repo, "branch", "split", second)
	owner, err := sourceGit(repo)
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("0", 40)
	for _, tc := range []struct {
		ref, want, err string
	}{
		{ref: "HEAD", want: second},
		{ref: first, want: first},
		{ref: strings.ToUpper(first), want: first},
		{ref: missing, err: "git"},
		{ref: "refs/heads/feature", want: second},
		{ref: "refs/tags/v1", want: first},
		{ref: "refs/heads/absent", err: "not found"},
		{ref: "refs/tags/absent", err: "not found"},
		{ref: "feature", want: second},
		{ref: "v1", want: first},
		{ref: "same", want: first},
		{ref: "split", err: "ambiguous between branch and tag"},
		{ref: "absent", err: "is not a branch, tag, or full commit ID"},
		{ref: first[:12], err: "is not a branch, tag, or full commit ID"},
		{ref: "--all", err: "invalid coding checkoutRef"},
		{ref: "HEAD~1", err: "invalid coding checkoutRef"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			got, err := resolveCheckoutRef(context.Background(), owner, tc.ref)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("resolveCheckoutRef(%q) = %q, %v; want error %q", tc.ref, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveCheckoutRef(%q) = %q, %v; want %q", tc.ref, got, err, tc.want)
			}
		})
	}
}

func repoIdentity(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), stat.Ino)
}

func TestVerifyPrimaryRepoRefusals(t *testing.T) {
	repo, err := resolvedPath(initTempGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	identity := repoIdentity(t, repo)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	testGit(t, repo, "worktree", "add", "--quiet", "--detach", linked)
	plain, err := resolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(plain, "file")
	testWrite(t, plain, "file", "x", 0o600)
	a := NewAdapter(t.TempDir())
	for _, tc := range []struct {
		name, repo, id, err string
	}{
		{name: "matching identity", repo: repo, id: identity},
		{name: "no identity pin", repo: alias},
		{name: "missing path", repo: filepath.Join(plain, "absent"), err: "no such file"},
		{name: "redirected path with identity pin", repo: alias, id: identity, err: "path was redirected"},
		{name: "identity changed", repo: repo, id: "1:2", err: "directory identity changed"},
		{name: "pinned path is a file", repo: file, id: identity, err: "repository is unavailable"},
		{name: "directory without Git", repo: plain, err: "must have its own .git directory"},
		{name: "linked worktree", repo: linked, err: "must have its own .git directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := a.verifyPrimaryRepo(context.Background(), tc.repo, tc.id, true)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("verifyPrimaryRepo = %q, %v; want error %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != repo {
				t.Fatalf("verifyPrimaryRepo = %q, %v; want %q", got, err, repo)
			}
		})
	}
}

func TestCouncilCheckoutRefusals(t *testing.T) {
	ctx := context.Background()
	refInput := func(ref string) map[string]any {
		return map[string]any{"checkoutMode": "ref", "checkoutRef": ref, "conclaveId": "ccl-refuse", "prompt": "check"}
	}
	writerInput := func(writer, role string) map[string]any {
		return map[string]any{"checkoutMode": "writer", "checkoutWriter": writer, "conclaveId": "ccl-refuse", "role": role, "prompt": "check"}
	}
	for _, tc := range []struct {
		name   string
		first  map[string]any // nil: no first launch
		second func(branch string) map[string]any
		want   string
	}{
		{name: "ref does not resolve", second: func(string) map[string]any { return refInput("absent") },
			want: "resolve checkout ref"},
		{name: "writer ref does not resolve", second: func(string) map[string]any {
			in := writerInput("claude", "claude")
			in["checkoutRef"] = "absent"
			return in
		}, want: "resolve checkout ref"},
		{name: "ref differs from the council", first: refInput("HEAD"),
			second: func(branch string) map[string]any { return refInput(branch) },
			want:   "do not match existing council"},
		{name: "writer launch into a ref council", first: refInput("HEAD"),
			second: func(string) map[string]any { return writerInput("claude", "claude") },
			want:   "do not match existing council"},
		{name: "ref launch into a writer council", first: writerInput("claude", "claude"),
			second: func(string) map[string]any { return refInput("HEAD") },
			want:   "do not match existing council"},
		{name: "different writer role", first: writerInput("claude", "claude"),
			second: func(string) map[string]any { return writerInput("codex", "codex") },
			want:   "different writer role"},
		{name: "writer ref differs from the council", first: writerInput("claude", "claude"),
			second: func(branch string) map[string]any {
				in := writerInput("claude", "codex")
				in["checkoutRef"] = branch
				return in
			}, want: "checkout ref does not match"},
		{name: "second active writer", first: writerInput("claude", "claude"),
			second: func(string) map[string]any { return writerInput("claude", "claude") },
			want:   "already active"},
		{name: "wrong source identity", second: func(string) map[string]any {
			in := refInput("HEAD")
			in["checkoutSourceId"] = "1:2"
			return in
		}, want: "directory identity changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			repo, err := resolvedPath(spec.Workload.Spec.Coding.RepoPath)
			if err != nil {
				t.Fatal(err)
			}
			spec.Workload.Spec.Coding.RepoPath = repo
			branch := strings.TrimSpace(testGit(t, repo, "branch", "--show-current"))
			before := testGit(t, repo, "worktree", "list", "--porcelain")
			var firstState []byte
			if tc.first != nil {
				spec.TaskID = "task-first"
				spec.InputJSON = mustMarshalInput(t, tc.first)
				h := startTestTask(t, a, spec)
				t.Cleanup(func() { _ = a.Stop(ctx, h) })
				before = testGit(t, repo, "worktree", "list", "--porcelain")
				root, err := resolvedPath(a.WorktreeRoot)
				if err != nil {
					t.Fatal(err)
				}
				if firstState, err = os.ReadFile(councilStatePath(root, councilKey("ccl-refuse"))); err != nil {
					t.Fatal(err)
				}
			}
			spec.TaskID = "task-second"
			spec.InputJSON = mustMarshalInput(t, tc.second(branch))
			h, err := a.Start(ctx, spec)
			if err == nil {
				_ = a.Stop(ctx, h)
				t.Fatalf("Start accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start error = %v, want %q", err, tc.want)
			}
			// The refusal leaves no task state, worktree or council change behind.
			if after := testGit(t, repo, "worktree", "list", "--porcelain"); after != before {
				t.Errorf("worktrees changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			root, err := resolvedPath(a.WorktreeRoot)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err == nil {
				if _, err := os.Stat(filepath.Join(root, "task-second"+stateSuffix)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("refused task left a state file: %v", err)
				}
				got, readErr := os.ReadFile(councilStatePath(root, councilKey("ccl-refuse")))
				if tc.first == nil && !errors.Is(readErr, os.ErrNotExist) {
					t.Errorf("refused first launch left council state: %v", readErr)
				}
				if tc.first != nil && string(got) != string(firstState) {
					t.Errorf("refused launch changed council state:\n%s\nwant:\n%s", got, firstState)
				}
			}
		})
	}
}
