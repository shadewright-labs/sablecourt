package coding

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

type gitRepo struct {
	dir          string
	gitDir       string
	workTree     string
	filterConfig []string
}

func (g gitRepo) run(ctx context.Context, args ...string) (string, error) {
	flags := []string{
		"-C", g.dir, "--no-pager", "--no-replace-objects",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.sshCommand=false",
		"-c", "protocol.file.allow=never",
		"-c", "submodule.recurse=false",
		"-c", "status.submoduleSummary=false",
		"-c", "worktree.useRelativePaths=false",
	}
	if g.gitDir != "" {
		flags = append(flags, "--git-dir="+g.gitDir)
	}
	if g.workTree != "" {
		flags = append(flags, "--work-tree="+g.workTree)
	}
	flags = append(flags, g.filterConfig...)
	cmd := exec.CommandContext(ctx, "git", append(flags, args...)...)
	cmd.Env = make([]string, 0)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var exit *exec.ExitError
		message := ""
		if errors.As(err, &exit) {
			message = redact.String(strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w (%s)", args[0], err, message)
	}
	return string(out), nil
}

func gitExitCode(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func (g gitRepo) config(ctx context.Context, args ...string) (string, error) {
	out, err := g.run(ctx, append([]string{"config"}, args...)...)
	if gitExitCode(err, 1) {
		return "", nil
	}
	return strings.TrimSuffix(out, "\n"), err
}

func (g gitRepo) withoutFilters(ctx context.Context) (gitRepo, error) {
	keys, err := g.config(ctx, "--includes", "--null", "--name-only", "--get-regexp",
		`^filter\..*\.(clean|smudge|process|required)$`)
	if err != nil {
		return gitRepo{}, err
	}
	seen := make(map[string]bool)
	for _, key := range strings.Split(strings.TrimSuffix(keys, "\x00"), "\x00") {
		if key == "" {
			continue
		}
		filter := key[:strings.LastIndexByte(key, '.')]
		if seen[filter] {
			continue
		}
		if strings.Contains(filter, "=") {
			return gitRepo{}, errors.New("cannot disable a configured Git filter")
		}
		seen[filter] = true
		// If attributes select this filter, refuse the operation instead of changing its content.
		g.filterConfig = append(g.filterConfig,
			"-c", filter+".clean=", "-c", filter+".smudge=",
			"-c", filter+".process=", "-c", filter+".required=true",
		)
	}
	return g, nil
}

func (g gitRepo) commitID(ctx context.Context, revision string) (string, error) {
	out, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(out, "\n")
	if !isCommitID(id) {
		return "", errors.New("Git returned an invalid commit ID")
	}
	return id, nil
}

func isCommitID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
