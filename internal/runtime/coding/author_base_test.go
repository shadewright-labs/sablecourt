package coding

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// TestCheckAuthorBase checks the adapter's refusal of a command step whose
// recorded author base is not an ancestor of the commit it checks (issue
// 214): the base must name a commit in the repository and lie on the pinned
// commit's history.
func TestCheckAuthorBase(t *testing.T) {
	t.Parallel()
	repo := initTempGitRepo(t)
	rev := func(r string) string { return strings.TrimSpace(testGit(t, repo, "rev-parse", r)) }
	first := rev("HEAD")
	testWrite(t, repo, "a.txt", "a\n", 0o600)
	testGit(t, repo, "add", "a.txt")
	testGit(t, repo, "commit", "-qm", "a")
	middle := rev("HEAD")
	testWrite(t, repo, "b.txt", "b\n", 0o600)
	testGit(t, repo, "add", "b.txt")
	testGit(t, repo, "commit", "-qm", "b")
	tip := rev("HEAD")
	testGit(t, repo, "checkout", "-q", "-b", "side", first)
	testWrite(t, repo, "s.txt", "s\n", 0o600)
	testGit(t, repo, "add", "s.txt")
	testGit(t, repo, "commit", "-qm", "side")
	side := rev("HEAD")

	git := gitRepo{dir: repo + "/.git", gitDir: repo + "/.git"}
	for name, tc := range map[string]struct {
		base string
		ok   bool
	}{
		"base is the pinned commit":      {base: tip, ok: true},
		"base is the parent":             {base: middle, ok: true},
		"base is an older ancestor":      {base: first, ok: true},
		"base on another branch":         {base: side, ok: false},
		"base is a descendant":           {base: tip, ok: false}, // checked against middle below
		"base names no object":           {base: strings.Repeat("e", 40), ok: false},
		"base in upper case":             {base: strings.ToUpper(first), ok: false},
		"base is a tree, not a commit":   {base: rev("HEAD^{tree}"), ok: false},
		"base is missing from the store": {base: "", ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			commit := tip
			if name == "base is a descendant" {
				commit = middle
			}
			c := &scruntime.CommandSpec{Name: "guard", AuthorBase: tc.base}
			err := checkAuthorBase(context.Background(), git, c, commit)
			if tc.ok {
				if err != nil {
					t.Fatalf("checkAuthorBase refused an ancestor: %v", err)
				}
				return
			}
			var refusal *scruntime.CommandRefusal
			if !errors.As(err, &refusal) || refusal.Reason != scruntime.CommandAuthorBaseUnavailable || refusal.Command != "guard" {
				t.Fatalf("err = %v, want refusal %s", err, scruntime.CommandAuthorBaseUnavailable)
			}
		})
	}
}

// TestHarnessGitDirNamesAuthorBase checks that a command step's Git
// metadata names the author base as scruntime.AuthorBaseRef and that a diff
// from it to HEAD runs there: its objects come with the pinned commit's.
func TestHarnessGitDirNamesAuthorBase(t *testing.T) {
	work, state, taskBase := harnessGitDirFixture(t)
	base := strings.TrimSpace(testGit(t, work, "rev-parse", "HEAD"))
	testWrite(t, work, "changed.txt", "x\n", 0o600)
	testGit(t, work, "add", "changed.txt")
	testGit(t, work, "commit", "-qm", "change")

	gitDir, err := prepareHarnessGitDir(context.Background(), work, taskBase, state, base)
	if err != nil {
		t.Fatalf("prepareHarnessGitDir: %v", err)
	}
	harnessGit := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + gitDir, "--work-tree=" + work}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in the launch's Git directory: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := harnessGit("rev-parse", "--verify", scruntime.AuthorBaseRef+"^{commit}"); got != base {
		t.Fatalf("%s = %q, want %q", scruntime.AuthorBaseRef, got, base)
	}
	if got := harnessGit("diff", "--name-only", scruntime.AuthorBaseRef, "HEAD"); got != "changed.txt" {
		t.Fatalf("diff from the author base = %q, want changed.txt", got)
	}

	// Without a base, no ref is written.
	work2, state2, taskBase2 := harnessGitDirFixture(t)
	other, err := prepareHarnessGitDir(context.Background(), work2, taskBase2, state2, "")
	if err != nil {
		t.Fatalf("prepareHarnessGitDir: %v", err)
	}
	if out, err := exec.Command("git", "--git-dir="+other, "rev-parse", "--verify", "--quiet", scruntime.AuthorBaseRef).CombinedOutput(); err == nil {
		t.Fatalf("a launch without an author base got the ref: %s", out)
	}
}
