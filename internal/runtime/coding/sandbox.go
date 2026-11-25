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
	"time"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

// CheckSandbox reports whether the current platform has a working OS sandbox
// for host coding harnesses. It fails closed whenever confinement cannot be
// enforced.
func CheckSandbox(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("no working coding sandbox on %s", runtime.GOOS)
	}
	if _, err := exec.LookPath(seatbeltExecutable); err != nil {
		return fmt.Errorf("Seatbelt executable unavailable: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return checkSeatbelt(probeCtx, seatbeltProbeProfile(), "/")
}

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
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("apply Seatbelt sandbox at %q: %w (%s)", bin, err, msg)
		}
		return fmt.Errorf("apply Seatbelt sandbox at %q: %w", bin, err)
	}
	return nil
}

func seatbeltProbeProfile() string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	allowSeatbeltRuntime(&profile)
	return profile.String()
}

func allowSeatbeltRuntime(profile *strings.Builder) {
	// Dyld's libignition opens "/" before it can find the shared cache.
	// On newer macOS the cache is inside the OS cryptex.
	profile.WriteString("(allow file-read* file-test-existence (literal \"/\"))\n")
	for _, path := range []string{"/usr/lib", "/System/Library", "/System/Cryptexes/OS",
		"/System/Volumes/Preboot/Cryptexes/OS"} {
		if real, err := resolvedPath(path); err == nil {
			fmt.Fprintf(profile, "(allow file-read* file-test-existence file-map-executable (subpath %q))\n", real)
			allowParentMetadata(profile, real)
		}
	}
	for _, path := range []string{"/bin", "/usr/bin", "/usr/lib", "/usr/libexec", "/System/Library",
		"/Library/Developer/CommandLineTools/usr"} {
		if real, err := resolvedPath(path); err == nil {
			fmt.Fprintf(profile, "(allow file-read* file-test-existence process-exec (subpath %q))\n", real)
		}
	}
}

func seatbeltProfile(work, scratch, bin string, state *worktreeState, readOnly bool) (string, error) {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	profile.WriteString("(allow process-fork sysctl-read network-outbound)\n")
	profile.WriteString("(allow signal (target self))\n")

	allowSeatbeltRuntime(&profile)
	for _, path := range []string{"/dev/urandom", "/dev/random", "/etc/localtime", "/etc/hosts", "/etc/resolv.conf"} {
		if real, err := resolvedPath(path); err == nil {
			fmt.Fprintf(&profile, "(allow file-read* file-test-existence (literal %q))\n", real)
		}
	}
	// /bin/sh reads the /private/var/select/sh symlink, and git probes
	// /etc/gitconfig (which traverses the /etc symlink and may not exist,
	// so Seatbelt must return ENOENT rather than EPERM).
	profile.WriteString("(allow file-read-metadata file-test-existence (literal \"/etc\"))\n")
	for _, path := range []string{"/private/var/select/sh", "/private/etc/gitconfig"} {
		fmt.Fprintf(&profile, "(allow file-read* file-test-existence (literal %q))\n", path)
		allowParentMetadata(&profile, path)
	}
	null, err := resolvedPath(os.DevNull)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&profile, "(allow file-read* file-test-existence file-write-data (literal %q))\n", null)

	work, err = resolvedPath(work)
	if err != nil {
		return "", err
	}
	bin, err = resolvedPath(bin)
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
	fmt.Fprintf(&profile, "(allow file-read* file-test-existence file-map-executable process-exec (literal %q))\n", bin)
	fmt.Fprintf(&profile, "(allow file-read* file-test-existence (subpath %q) (subpath %q))\n", work, scratch)
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
	if state.Mode == checkoutModeLive {
		if !readOnly {
			return errors.New("live checkout mode requires read-only confinement")
		}
		repoGit := filepath.Join(work, ".git")
		if resolvedRepoGit, err := resolvedPath(repoGit); err != nil || resolvedRepoGit != repoGit {
			return errors.New("coding sandbox live repository .git directory is unavailable or redirected")
		}
		if common != git || state.Branch != "" || pathWithin(work, git) || pathWithin(git, work) {
			return errors.New("invalid coding sandbox live Git registration")
		}
		if err := checkSandboxGitPath(git); err != nil {
			return err
		}
		for _, name := range []string{"objects", "refs"} {
			if path, err := resolvedPath(filepath.Join(git, name)); err != nil || path != filepath.Join(git, name) {
				return errors.New("coding sandbox Git storage is unavailable or redirected")
			}
		}
		fmt.Fprintf(profile, "(deny file-read* file-test-existence (subpath %q))\n", repoGit)
		allowParentMetadata(profile, git)
		fmt.Fprintf(profile, "(allow file-read* file-test-existence (subpath %q))\n", git)
		return nil
	}
	if state.Mode == checkoutModeWriter && state.ReadOnly {
		if !readOnly || state.Branch != "" {
			return errors.New("invalid coding sandbox reviewer Git registration")
		}
		expectedGit := filepath.Join(work, ".git")
		if common != expectedGit || git != expectedGit {
			return errors.New("invalid coding sandbox reviewer isolated Git registration")
		}
		if err := checkSandboxGitPath(common); err != nil {
			return err
		}
		for _, name := range []string{"objects", "refs"} {
			if path, err := resolvedPath(filepath.Join(common, name)); err != nil || path != filepath.Join(common, name) {
				return errors.New("coding sandbox Git storage is unavailable or redirected")
			}
		}
		return nil
	}
	if filepath.Dir(git) != filepath.Join(common, "worktrees") ||
		pathWithin(work, common) || pathWithin(common, work) {
		return errors.New("invalid coding sandbox Git registration")
	}
	switch state.Mode {
	case "":
		if state.Branch != "sablecourt/"+sanitizeSegment(state.TaskID) {
			return errors.New("invalid coding sandbox Git registration")
		}
	case checkoutModeRef:
		if !readOnly || state.Branch != "" {
			return errors.New("invalid coding sandbox ref Git registration")
		}
	case checkoutModeWriter:
		if state.ReadOnly {
			if !readOnly || state.Branch != "" {
				return errors.New("invalid coding sandbox reviewer Git registration")
			}
		} else {
			if readOnly || state.CouncilKey == "" || state.Branch != councilWriterBranch(state.CouncilKey) {
				return errors.New("invalid coding sandbox writer Git registration")
			}
		}
	default:
		return errors.New("unsupported coding sandbox checkout mode")
	}
	allowParentMetadata(profile, git)
	for _, name := range []string{"objects", "refs"} {
		if path, err := resolvedPath(filepath.Join(common, name)); err != nil || path != filepath.Join(common, name) {
			return errors.New("coding sandbox Git storage is unavailable or redirected")
		}
		fmt.Fprintf(profile, "(allow file-read* file-test-existence (subpath %q))\n", filepath.Join(common, name))
	}
	readPaths := []string{common, filepath.Join(common, "config"), filepath.Join(common, "packed-refs"),
		filepath.Join(common, "info", "exclude")}
	if state.Mode == "" {
		readPaths = append(readPaths, work+excludeConfigSuffix)
	}
	for _, path := range readPaths {
		if err := checkSandboxGitPath(path); err != nil {
			return err
		}
		fmt.Fprintf(profile, "(allow file-read* file-test-existence (literal %q))\n", path)
	}
	fmt.Fprintf(profile, "(allow file-read-metadata file-test-existence (literal %q))\n", filepath.Join(common, "info"))
	fmt.Fprintf(profile, "(allow file-read* file-test-existence (subpath %q))\n", git)
	if readOnly {
		return nil
	}

	// Git can update this index and task branch. Configuration and the
	// registration pointers remain under daemon control.
	ref := filepath.Join(common, "refs", "heads", state.Branch)
	logs := filepath.Join(common, "logs")
	log := filepath.Join(logs, "refs", "heads", state.Branch)
	for _, path := range []string{git, filepath.Join(git, "logs"), logs, filepath.Join(logs, "refs"),
		filepath.Join(logs, "refs", "heads"), filepath.Dir(log), filepath.Dir(ref)} {
		if err := checkSandboxGitPath(path); err != nil {
			return err
		}
		fmt.Fprintf(profile, "(allow file-read-metadata file-test-existence file-write-create (literal %q))\n", path)
	}
	for _, path := range []string{filepath.Join(git, "index"), filepath.Join(git, "index.lock"),
		filepath.Join(git, "HEAD.lock"), filepath.Join(git, "COMMIT_EDITMSG"),
		filepath.Join(git, "logs", "HEAD"), ref, ref + ".lock", log} {
		if err := checkSandboxGitPath(path); err != nil {
			return err
		}
		fmt.Fprintf(profile, "(allow file-read* file-test-existence file-write* (literal %q))\n", path)
	}

	// Permit loose-object creation and temporary files without opening Git
	// configuration, alternates, hooks, pack files or sibling refs for writes.
	// Seatbelt regex literals interpret single backslashes directly and do
	// not support {n} bounded quantifiers.
	objectsDir := filepath.Join(common, "objects")
	if strings.ContainsAny(objectsDir, "\"\r\n") {
		return errors.New("coding sandbox Git objects path contains unsupported characters")
	}
	if !isCommitID(state.Base) {
		return errors.New("coding sandbox has no valid base commit ID")
	}
	objects := regexp.QuoteMeta(objectsDir)
	suffix := strings.Repeat("[0-9a-f]", len(state.Base)-2)
	fmt.Fprintf(profile, "(allow file-write-create (regex #\"^%s/[0-9a-f][0-9a-f](/%s)?$\"))\n", objects, suffix)
	fmt.Fprintf(profile, "(allow file-write* (regex #\"^%s/[0-9a-f][0-9a-f]/tmp_obj_[a-zA-Z0-9]+$\"))\n", objects)
	return nil
}

// Git creates some of these paths on its first write. Check the existing
// prefix and refuse redirects instead of allowing their targets.
func checkSandboxGitPath(path string) error {
	for {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			if real, err := resolvedPath(path); err != nil || real != path {
				return fmt.Errorf("coding sandbox Git path is redirected: %q", path)
			}
			return nil
		}
		path = filepath.Dir(path)
	}
}

func allowParentMetadata(profile *strings.Builder, path string) {
	for dir := filepath.Dir(path); dir != path; dir = filepath.Dir(path) {
		fmt.Fprintf(profile, "(allow file-read-metadata file-test-existence (literal %q))\n", dir)
		path = dir
	}
}

func pathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}
