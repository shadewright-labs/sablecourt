package config_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// guardArgv returns the invariant path guard of the example coding court
// (issue 214) and its path list, so the test runs the script operators copy.
func guardArgv(t *testing.T) []string {
	t.Helper()
	bundle, err := config.LoadPath(filepath.Join("..", "..", "examples", "coding-court", "coding-court.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	wl := bundle.Workloads["developer-go"]
	cmd, ok := wl.Spec.Coding.Command("invariant-paths")
	if !ok {
		t.Fatal("developer-go has no invariant-paths command")
	}
	if len(cmd.Argv) < 4 || cmd.Argv[0] != "sh" || cmd.Argv[1] != "-c" || cmd.Argv[3] != "guard" {
		t.Fatalf("guard argv = %q, want sh -c <script> guard <paths>", cmd.Argv)
	}
	if !strings.Contains(cmd.FailureReason, "coding-invariant") {
		t.Fatalf("failure reason %q does not name coding-invariant", cmd.FailureReason)
	}
	steps := bundle.Workflows["coding"].Spec.Steps
	if len(steps) != 3 || steps[0].Role != "author" || steps[1].Command != "invariant-paths" || steps[2].Role != "verifier" {
		t.Fatalf("coding workflow steps = %+v, want author, guard, reviewer", steps)
	}
	if _, ok := bundle.Workflows["coding-invariant"]; !ok {
		t.Fatal("no coding-invariant workflow")
	}
	if !slices.Contains(cmd.Argv[4:], "internal/redact/") {
		t.Fatalf("path list %q lacks internal/redact/, which the cases below use", cmd.Argv[4:])
	}
	return cmd.Argv
}

type guardRepo struct {
	t   *testing.T
	dir string
}

func (r guardRepo) git(args ...string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r guardRepo) write(name, body string) {
	r.t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r guardRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// TestInvariantPathGuard runs the example guard against each case of the
// decision of 2026-11-28 on issue 214: the diff spans the run's first
// author base to the pinned commit, both sides of a rename count, and a
// missing or unrelated base, like any Git error, fails the step.
func TestInvariantPathGuard(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	argv := guardArgv(t)
	const (
		pass  = 0
		match = 1
		fail  = 2
	)
	for name, tc := range map[string]struct {
		// change runs after the base commit and returns the base to record
		// ("" records none).
		change func(r guardRepo, base string) string
		paths  []string // nil keeps the example list
		want   int
		output string
	}{
		"match": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // changed\n")
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"no match": {change: func(r guardRepo, base string) string {
			r.write("cmd/tool/main.go", "package main // changed\n")
			r.commit("agent")
			return base
		}, want: pass, output: "no changed path is on the list"},
		"rename into a listed path": {change: func(r guardRepo, base string) string {
			r.git("mv", "cmd/tool/main.go", "internal/redact/main.go")
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact/main.go"},
		"rename out of a listed path": {change: func(r guardRepo, base string) string {
			r.git("mv", "internal/redact/rules.go", "cmd/tool/rules.go")
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"empty diff": {change: func(_ guardRepo, base string) string {
			return base
		}, want: pass},
		"empty commits only": {change: func(r guardRepo, base string) string {
			r.commit("agent")
			return base
		}, want: pass},
		"prefix-only name": {change: func(r guardRepo, base string) string {
			r.write("internal/redactor/x.go", "package redactor\n")
			r.commit("agent")
			return base
		}, want: pass},
		"listed file without a slash is not a prefix": {change: func(r guardRepo, base string) string {
			r.write("internal/redactor/x.go", "package redactor\n")
			r.commit("agent")
			return base
		}, paths: []string{"internal/redact"}, want: pass},
		"listed directory without a slash": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/new.go", "package redact\n")
			r.commit("agent")
			return base
		}, paths: []string{"internal/redact"}, want: match},
		// An entry ending in / matches the path itself as well as what is
		// below it: a regular file of that name added by the author counts.
		"listed directory entry, file of the same name added": {change: func(r guardRepo, base string) string {
			r.write("policy", "x\n")
			r.commit("agent")
			return base
		}, paths: []string{"policy/"}, want: match, output: "policy"},
		"listed directory entry, directory of the same name added": {change: func(r guardRepo, base string) string {
			r.write("policy/x.go", "package policy\n")
			r.commit("agent")
			return base
		}, paths: []string{"policy/"}, want: match, output: "policy/x.go"},
		"listed directory entry replaced by a file": {change: func(r guardRepo, base string) string {
			r.git("rm", "-q", "-r", "internal/redact")
			r.write("internal/redact", "x\n")
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact"},
		"listed directory entry, prefix-only file name": {change: func(r guardRepo, base string) string {
			r.write("policyx", "x\n")
			r.commit("agent")
			return base
		}, paths: []string{"policy/"}, want: pass},
		"listed entry of only a slash is invalid": {change: func(r guardRepo, base string) string {
			r.write("cmd/tool/main.go", "package main // changed\n")
			r.commit("agent")
			return base
		}, paths: []string{"/"}, want: fail},
		"several agent commits": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // first\n")
			r.commit("agent one")
			r.write("cmd/tool/main.go", "package main // second\n")
			r.commit("agent two")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"split snapshot, staged and unstaged": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // staged\n")
			r.commit("chore(coding): preserve staged task edits")
			r.write("cmd/tool/main.go", "package main // unstaged\n")
			r.commit("chore(coding): preserve task edits")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"author retry after a failure": {change: func(r guardRepo, base string) string {
			// The failed attempt touched the listed path; the retry is pinned to
			// its commit and touches only an unlisted one. The base stays the first.
			r.write("internal/redact/rules.go", "package redact // failed attempt\n")
			r.commit("chore(coding): preserve task edits")
			r.write("cmd/tool/main.go", "package main // retry\n")
			r.commit("retry")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"file name with a newline and spaces": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/a b\nc.go", "package redact\n")
			r.commit("agent")
			return base
		}, want: match},
		"file name with non-ASCII characters": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/zażółć.go", "package redact\n")
			r.commit("agent")
			return base
		}, want: match},
		"deleted listed file": {change: func(r guardRepo, base string) string {
			r.git("rm", "-q", "internal/redact/rules.go")
			r.commit("agent")
			return base
		}, want: match},
		"empty path list checks nothing": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // changed\n")
			r.commit("agent")
			return base
		}, paths: []string{}, want: pass, output: "no path was checked"},
		"invalid path list entry": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // changed\n")
			r.commit("agent")
			return base
		}, paths: []string{"./internal/redact/"}, want: fail},
		// Repository config and agent-written files must not change what the
		// guard sees: a gitlink change in a listed directory counts whatever
		// .gitmodules or the repository config say.
		"gitlink change": {change: func(r guardRepo, base string) string {
			r.git("update-index", "--add", "--cacheinfo", "160000,"+base+",internal/redact/sub")
			r.git("-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "-m", "agent")
			return base
		}, want: match, output: "internal/redact/sub"},
		"gitlink change hidden by .gitmodules and repository config": {change: func(r guardRepo, base string) string {
			r.write(".gitmodules", "[submodule \"sub\"]\n\tpath = internal/redact/sub\n\turl = ./sub\n\tignore = all\n")
			r.git("update-index", "--add", "--cacheinfo", "160000,"+base+",internal/redact/sub")
			r.git("config", "diff.ignoreSubmodules", "all")
			r.git("config", "submodule.sub.ignore", "all")
			r.git("config", "diff.relative", "true")
			r.git("config", "diff.external", "/bin/true")
			r.git("add", ".gitmodules")
			r.git("-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "-m", "agent")
			return base
		}, want: match, output: "internal/redact/sub"},
		"replace ref hiding the change": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // changed\n")
			head := r.commit("agent")
			// A commit with the base's tree and the base as parent stands in
			// for the agent's commit, so the replaced history shows no change.
			stand := r.git("-c", "user.name=t", "-c", "user.email=t", "commit-tree", base+"^{tree}", "-p", base, "-m", "agent")
			r.git("replace", head, stand)
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"listed exact name with a newline": {change: func(r guardRepo, base string) string {
			r.write("docs/a\nb.md", "x\n")
			r.commit("agent")
			return base
		}, paths: []string{"docs/a\nb.md"}, want: match},
		"listed file named like a status letter, unlisted change": {change: func(r guardRepo, base string) string {
			r.write("cmd/tool/main.go", "package main // changed\n")
			r.commit("agent")
			return base
		}, paths: []string{"M"}, want: pass},
		"listed file named like a status letter, changed": {change: func(r guardRepo, base string) string {
			r.write("M", "x\n")
			r.commit("agent")
			return base
		}, paths: []string{"M"}, want: match},
		"listed path starting with a dash": {change: func(r guardRepo, base string) string {
			r.write("-x/f.go", "package x\n")
			r.commit("agent")
			return base
		}, paths: []string{"-x/"}, want: match},
		"listed entry with glob characters is literal": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/new.go", "package redact\n")
			r.commit("agent")
			return base
		}, paths: []string{"internal/*"}, want: pass},
		"mode-only change": {change: func(r guardRepo, base string) string {
			r.git("update-index", "--chmod=+x", "internal/redact/rules.go")
			r.git("-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "-m", "agent")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"listed file replaced by a symlink": {change: func(r guardRepo, base string) string {
			if err := os.Remove(filepath.Join(r.dir, "internal", "redact", "rules.go")); err != nil {
				r.t.Fatal(err)
			}
			if err := os.Symlink("../../cmd/tool/main.go", filepath.Join(r.dir, "internal", "redact", "rules.go")); err != nil {
				r.t.Fatal(err)
			}
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact/rules.go"},
		"copy out of a listed path": {change: func(r guardRepo, base string) string {
			r.write("cmd/tool/rules.go", "package redact\n")
			r.commit("agent")
			return base
		}, want: pass},
		"copy into a listed path": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/main.go", "package main\n")
			r.commit("agent")
			return base
		}, want: match, output: "internal/redact/main.go"},
		"git diff error": {change: func(r guardRepo, base string) string {
			r.write("internal/redact/rules.go", "package redact // changed\n")
			r.commit("agent")
			tree := r.git("rev-parse", "HEAD^{tree}")
			if err := os.Remove(filepath.Join(r.dir, ".git", "objects", tree[:2], tree[2:])); err != nil {
				r.t.Fatal(err)
			}
			return base
		}, want: fail},
		"base missing": {change: func(r guardRepo, _ string) string {
			r.write("cmd/tool/main.go", "package main // changed\n")
			r.commit("agent")
			return ""
		}, want: fail, output: "no author base"},
		"base not an ancestor": {change: func(r guardRepo, base string) string {
			r.git("checkout", "-q", "-b", "side", base)
			r.write("cmd/tool/side.go", "package main\n")
			side := r.commit("side")
			r.git("checkout", "-q", "-")
			r.write("internal/redact/rules.go", "package redact // changed\n")
			r.commit("agent")
			return side
		}, want: fail, output: "not an ancestor"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := guardRepo{t: t, dir: t.TempDir()}
			r.git("init", "-q", "-b", "main")
			r.write("internal/redact/rules.go", "package redact\n")
			r.write("internal/redactor/keep.go", "package redactor\n")
			r.write("cmd/tool/main.go", "package main\n")
			base := r.commit("base")
			if recorded := tc.change(r, base); recorded != "" {
				r.git("update-ref", scruntime.AuthorBaseRef, recorded)
			}
			args := argv
			if tc.paths != nil {
				args = append(slices.Clone(argv[:4]), tc.paths...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			cmd.Dir = r.dir
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			out, err := cmd.CombinedOutput()
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.want || !strings.Contains(string(out), tc.output) {
				t.Fatalf("guard exit %d, output %q; want exit %d with %q", code, out, tc.want, tc.output)
			}
		})
	}
}
