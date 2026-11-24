package coding

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// Git records a submodule as a commit ID. Nothing in the parent snapshot
// preserves its index, working files, or commits outside that history.
func inspectNested(ctx context.Context, checkout gitRepo, revision, base, modules string) ([]string, error) {
	tree, err := checkout.run(ctx, "ls-tree", "-rz", "--end-of-options", revision)
	if err != nil {
		return nil, err
	}
	links := make(map[string]string)
	for record := range strings.SplitSeq(tree, "\x00") {
		if strings.HasPrefix(record, "160000 ") {
			header, path, _ := strings.Cut(record, "\t")
			_, id, _ := strings.Cut(header, "160000 commit ")
			if !isCommitID(id) || !filepath.IsLocal(path) {
				return nil, errors.New("invalid snapshot gitlink")
			}
			links[path] = id
		}
	}
	index, err := checkout.run(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	indexed := make(map[string]bool)
	for record := range strings.SplitSeq(index, "\x00") {
		if strings.HasPrefix(record, "160000 ") {
			header, path, _ := strings.Cut(record, "\t")
			indexed[path] = true
			if header != "160000 "+links[path]+" 0" {
				return nil, fmt.Errorf("nested Git repository %q is staged as a gitlink", redact.String(path))
			}
		}
	}

	var exclude []string
	inspected := make(map[string]bool)
	err = filepath.WalkDir(checkout.workTree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == checkout.workTree {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			if entry.Name() == "HEAD" {
				objects, err := os.Lstat(filepath.Join(filepath.Dir(path), "objects"))
				if err == nil && objects.IsDir() {
					refs, err := os.Lstat(filepath.Join(filepath.Dir(path), "refs"))
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}
					if err != nil {
						return err
					}
					if !refs.IsDir() {
						return nil
					}
					rel, _ := filepath.Rel(checkout.workTree, filepath.Dir(path))
					return fmt.Errorf("cannot safely remove possible bare Git repository %q", redact.String(rel))
				}
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return nil
		}
		rel, err := filepath.Rel(checkout.workTree, path)
		if err != nil {
			return err
		}
		id := links[rel]
		dot := filepath.Join(path, ".git")
		info, err := os.Lstat(dot)
		if errors.Is(err, os.ErrNotExist) {
			if id != "" {
				children, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				if len(children) > 0 {
					return fmt.Errorf("submodule %q has files without a checkout", redact.String(rel))
				}
				return fs.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		}
		if id == "" {
			if err := verifyEmptyNestedRepo(path); err != nil {
				return fmt.Errorf("nested Git repository %q: %w", redact.String(rel), err)
			}
		} else {
			if !indexed[rel] {
				return fmt.Errorf("submodule %q is staged for removal", redact.String(rel))
			}
			gitdir, err := verifyModuleCheckout(ctx, checkout, rel, modules, revision, base, id, info.Mode())
			if err != nil {
				return err
			}
			inspected[gitdir] = true
		}
		exclude = append(exclude, ":(exclude,literal)"+rel)
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}
	// A deinitialized or deleted checkout can leave its only object store
	// in the registration that worktree remove is about to delete.
	if _, err := os.Lstat(modules); !errors.Is(err, os.ErrNotExist) {
		err = filepath.WalkDir(modules, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if inspected[path] {
				return fs.SkipDir
			}
			if !entry.IsDir() {
				return fmt.Errorf("unverified submodule storage at %q", redact.String(path))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return exclude, nil
}

func verifyModuleCheckout(ctx context.Context, checkout gitRepo, rel, modules, revision, base, id string, entryMode fs.FileMode) (string, error) {
	// A gitlink records an ID but does not store its objects. A local
	// commit first referenced by this task may exist only in this clone.
	if revision != base {
		original, err := checkout.run(ctx, "ls-tree", "-z", "--end-of-options", base, "--", ":(literal)"+rel)
		if err != nil {
			return "", err
		}
		if original != "160000 commit "+id+"\t"+rel+"\x00" {
			return "", fmt.Errorf("submodule %q commit is absent from the launch snapshot", redact.String(rel))
		}
	}
	path := filepath.Join(checkout.workTree, rel)
	gitdir := filepath.Join(path, ".git")
	var err error
	if !entryMode.IsDir() {
		if !entryMode.IsRegular() {
			return "", fmt.Errorf("submodule %q has an unsupported .git entry", redact.String(rel))
		}
		gitdir, err = submoduleGitdir(path, modules)
		if err != nil {
			return "", fmt.Errorf("submodule %q: %w", redact.String(rel), err)
		}
	}
	if err := inspectSubmodule(ctx, path, gitdir, id, checkout.beforeRun); err != nil {
		return "", fmt.Errorf("submodule %q: %w", redact.String(rel), err)
	}
	return gitdir, nil
}

func submoduleGitdir(dir, modules string) (string, error) {
	data, _, err := readMetadata(filepath.Join(dir, ".git"))
	if err != nil {
		return "", err
	}
	path, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), "gitdir: ")
	if !ok {
		return "", errors.New("invalid gitdir pointer")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != path || !strings.HasPrefix(resolved, modules+string(os.PathSeparator)) {
		return "", errors.New("gitdir is outside the registered submodule store")
	}
	return resolved, nil
}

func inspectSubmodule(ctx context.Context, dir, gitdir, id string, beforeGit func() error) error {
	// Read objects and index entries through a temporary Git directory. Git
	// must never discover config, hooks, or filters in a workload's module.
	scratch, err := os.MkdirTemp("", "coding-module-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	g := gitRepo{beforeRun: beforeGit, dir: scratch, gitDir: scratch, workTree: dir}
	format := "sha1"
	if len(id) == 64 {
		format = "sha256"
	}
	if _, err := g.run(ctx, "init", "--quiet", "--template=", "--object-format="+format); err != nil {
		return err
	}
	for _, name := range []string{"HEAD", "objects", "refs", "packed-refs", "index"} {
		src := filepath.Join(gitdir, name)
		info, err := os.Lstat(src)
		if name == "packed-refs" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		dirExpected := name == "objects" || name == "refs"
		if (dirExpected && !info.IsDir()) || (!dirExpected && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsupported module %s", name)
		}
		dst := filepath.Join(scratch, name)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if name == "HEAD" {
			data, _, err := readMetadata(src)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o600); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return err
		}
	}
	head, err := g.commitID(ctx, "HEAD")
	if err != nil {
		return err
	}
	if head != id {
		return errors.New("checked out commit differs from the snapshot")
	}
	staged, err := g.run(ctx, "ls-files", "-z", "--stage")
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(scratch, "index")); err != nil {
		return err
	}
	if _, err := g.run(ctx, "read-tree", "--end-of-options", id); err != nil {
		return err
	}
	expected, err := g.run(ctx, "ls-files", "-z", "--stage")
	if err != nil {
		return err
	}
	if staged != expected {
		return errors.New("index contains staged changes")
	}
	// Rebuild the index so assume-unchanged and skip-worktree flags cannot
	// hide edits. Include ignored files, which removal would also destroy.
	exclude, err := inspectNested(ctx, g, id, id, filepath.Join(gitdir, "modules"))
	if err != nil {
		return err
	}
	status, err := g.run(ctx, append([]string{"status", "--porcelain=v1", "--untracked-files=all", "--ignored", "--ignore-submodules=all", "--"}, exclude...)...)
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("worktree contains uncommitted changes")
	}
	// Inspect all stored commits, including those with no branch or reflog.
	history, err := g.run(ctx, "rev-list", "--end-of-options", id, "--")
	if err != nil {
		return err
	}
	reachable := make(map[string]bool)
	for commit := range strings.SplitSeq(history, "\n") {
		reachable[commit] = true
	}
	objects, err := g.run(ctx, "cat-file", "--batch-all-objects", "--unordered", "--batch-check=%(objecttype) %(objectname)")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(objects, "\n"), "\n") {
		kind, oid, _ := strings.Cut(line, " ")
		if !isCommitID(oid) {
			return errors.New("invalid module object listing")
		}
		switch kind {
		case "blob", "tree", "tag":
			continue
		case "commit":
		default:
			return errors.New("unknown module object type")
		}
		if !reachable[oid] {
			return errors.New("object store contains a commit unreachable from the snapshot")
		}
	}
	return nil
}

func verifyEmptyNestedRepo(path string) error {
	checkout, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer checkout.Close()
	entries, err := fs.ReadDir(checkout.FS(), ".")
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != ".git" {
		return errors.New("worktree contains files outside the saved snapshot")
	}
	if !entries[0].IsDir() {
		return errors.New("cannot verify external gitdir")
	}
	repo, err := checkout.OpenRoot(".git")
	if err != nil {
		return err
	}
	defer repo.Close()
	// Inspect the stores without loading config or following a nested gitdir.
	for _, file := range []string{"commondir", "index", "packed-refs", "reftable"} {
		if _, err := repo.Lstat(file); err == nil {
			return fmt.Errorf("%s is outside the saved snapshot", file)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, store := range []string{"refs", "objects"} {
		err := fs.WalkDir(repo.FS(), store, func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				return fmt.Errorf("%s contains data outside the saved snapshot", store)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
