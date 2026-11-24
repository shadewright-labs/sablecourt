package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

func (a *Adapter) validateSandbox(sandbox string, readOnly bool) error {
	switch sandbox {
	case "seatbelt":
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath(seatbeltExecutable); err != nil {
				return fmt.Errorf("Seatbelt executable unavailable: %w", err)
			}
			return nil
		}
	case "none":
		if a.allowUnconfined && !readOnly {
			return nil
		}
	}
	return fmt.Errorf("coding sandbox %q cannot enforce confinement on %s", sandbox, runtime.GOOS)
}

func seatbeltCommand(profile, bin string, args []string) (string, []string) {
	return seatbeltExecutable, append([]string{"-p", profile, bin}, args...)
}

func checkSeatbelt(ctx context.Context, profile, dir string) error {
	bin, args := seatbeltCommand(profile, "/usr/bin/true", nil)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, []string{"PATH=/usr/bin:/bin"}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply Seatbelt sandbox at %q: %w", bin, err)
	}
	return nil
}

func seatbeltProfile(work, scratch, bin string, state *worktreeState, readOnly bool) (string, error) {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	profile.WriteString("(allow process-fork sysctl-read network-outbound)\n")
	profile.WriteString("(allow signal (target self))\n")

	for _, path := range []string{"/bin", "/usr/bin", "/usr/lib", "/usr/libexec", "/System/Library",
		"/Library/Developer/CommandLineTools/usr"} {
		if real, err := resolvedPath(path); err == nil {
			fmt.Fprintf(&profile, "(allow file-read* process-exec (subpath %q))\n", real)
		}
	}
	for _, path := range []string{"/dev/null", "/dev/urandom", "/dev/random", "/etc/localtime", "/etc/hosts", "/etc/resolv.conf"} {
		if real, err := resolvedPath(path); err == nil {
			fmt.Fprintf(&profile, "(allow file-read* (literal %q))\n", real)
		}
	}
	fmt.Fprintf(&profile, "(allow file-read* process-exec (literal %q))\n", bin)
	fmt.Fprintf(&profile, "(allow file-write-data (literal %q))\n", os.DevNull)

	work, err := resolvedPath(work)
	if err != nil {
		return "", err
	}
	scratch, err = resolvedPath(scratch)
	if err != nil {
		return "", err
	}
	if pathWithin(work, scratch) || pathWithin(scratch, work) ||
		pathWithin(work, bin) || pathWithin(scratch, bin) {
		return "", errors.New("overlapping coding sandbox paths")
	}
	fmt.Fprintf(&profile, "(allow file-read* (subpath %q) (subpath %q))\n", work, scratch)
	fmt.Fprintf(&profile, "(allow file-write* (subpath %q))\n", scratch)
	if !readOnly {
		fmt.Fprintf(&profile, "(allow file-write* (subpath %q))\n", work)
	}
	for _, path := range []string{work, scratch, bin} {
		allowParentMetadata(&profile, path)
	}
	if err := allowGit(&profile, work, state, readOnly); err != nil {
		return "", err
	}
	return profile.String(), nil
}

func allowGit(profile *strings.Builder, work string, state *worktreeState, readOnly bool) error {
	common, err := resolvedPath(state.CommonDir)
	if err != nil {
		return err
	}
	git, err := resolvedPath(state.GitDir)
	if err != nil {
		return err
	}
	if filepath.Dir(git) != filepath.Join(common, "worktrees") ||
		pathWithin(work, common) || pathWithin(common, work) ||
		state.Branch != "sablecourt/"+sanitizeSegment(state.TaskID) {
		return errors.New("invalid coding sandbox Git registration")
	}
	allowParentMetadata(profile, git)
	for _, name := range []string{"objects", "refs"} {
		if path, err := resolvedPath(filepath.Join(common, name)); err != nil || path != filepath.Join(common, name) {
			return errors.New("coding sandbox Git storage is unavailable or redirected")
		}
		fmt.Fprintf(profile, "(allow file-read* (subpath %q))\n", filepath.Join(common, name))
	}
	fmt.Fprintf(profile, "(allow file-read* (subpath %q) (literal %q) (literal %q) (literal %q) (literal %q))\n",
		git, common, filepath.Join(common, "config"), filepath.Join(common, "packed-refs"), work+excludeConfigSuffix)
	if readOnly {
		return nil
	}

	// Git can update this index and task branch. Configuration and the
	// registration pointers remain under daemon control.
	ref := filepath.Join(common, "refs", "heads", state.Branch)
	log := filepath.Join(common, "logs", "refs", "heads", state.Branch)
	for _, path := range []string{git, filepath.Join(git, "logs"), filepath.Join(common, "logs"),
		filepath.Dir(log), filepath.Dir(filepath.Dir(log)), filepath.Dir(ref)} {
		fmt.Fprintf(profile, "(allow file-read-metadata file-write-create (literal %q))\n", path)
	}
	for _, path := range []string{filepath.Join(git, "index"), filepath.Join(git, "index.lock"),
		filepath.Join(git, "COMMIT_EDITMSG"), filepath.Join(git, "logs", "HEAD"), ref, ref + ".lock", log} {
		fmt.Fprintf(profile, "(allow file-read* file-write* (literal %q))\n", path)
	}

	// Permit loose-object creation and temporary files without opening Git
	// configuration, alternates, hooks, pack files or sibling refs for writes.
	objects := regexp.QuoteMeta(filepath.Join(common, "objects"))
	fmt.Fprintf(profile, "(allow file-write-create (regex #%q))\n", "^"+objects+"/[0-9a-f]{2}(/[0-9a-f]{38}|/[0-9a-f]{62})?$")
	fmt.Fprintf(profile, "(allow file-write* (regex #%q))\n", "^"+objects+"/[0-9a-f]{2}/tmp_obj_[a-zA-Z0-9]+$")
	return nil
}

func allowParentMetadata(profile *strings.Builder, path string) {
	for dir := filepath.Dir(path); dir != path; dir = filepath.Dir(path) {
		fmt.Fprintf(profile, "(allow file-read-metadata (literal %q))\n", dir)
		path = dir
	}
}

func pathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}
