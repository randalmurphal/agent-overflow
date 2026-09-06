package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Git exposes intent-to-add only through add, which runs clean/encoding
// filters even for an empty placeholder. Run it against an empty temporary
// repository/worktree while pointing at the destination's LOCAL index and
// object directory. Git still writes its own index format; no index crosses
// computers and no destination checkout/global attributes can execute here.
func (c *Core) restoreTransferIntentIndex(ctx context.Context, cwd string, entries []TransferIndexEntry, sha256 bool) error {
	if len(entries) == 0 {
		return nil
	}
	gitDir, found, err := c.revParsePath(cwd, "--absolute-git-dir")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("transfer: prepared worktree has no Git directory")
	}
	resolve := func(name string) (string, error) {
		path, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"rev-parse", "--path-format=absolute", "--git-path", name}})
		if err != nil {
			return "", err
		}
		path = strings.TrimSpace(path)
		if !filepath.IsAbs(path) {
			return "", errors.New("transfer: Git returned a relative preparation path")
		}
		return filepath.Clean(path), nil
	}
	index, err := resolve("index")
	if err != nil {
		return err
	}
	objects, err := resolve("objects")
	if err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(gitDir, "ao-intent-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	args := []string{"init", "--quiet", "--template="}
	if sha256 {
		args = append(args, "--object-format=sha256")
	}
	args = append(args, "--", temporary)
	isolated := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_COUNT=0", "GIT_CONFIG_PARAMETERS="}
	privateGit := filepath.Join(temporary, ".git")
	// Pin init too: inherited Git environment must never turn this temporary
	// repository setup into reinitializing a user's repository.
	initEnv := append(append([]string(nil), isolated...), "GIT_DIR="+privateGit, "GIT_WORK_TREE="+temporary, "GIT_INDEX_FILE="+filepath.Join(privateGit, "index"), "GIT_OBJECT_DIRECTORY="+filepath.Join(privateGit, "objects"))
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: temporary, ctx: ctx, args: args, extraEnv: initEnv}); err != nil {
		return err
	}
	root, err := os.OpenRoot(temporary)
	if err != nil {
		return err
	}
	defer root.Close()
	var paths strings.Builder
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ensureTransferParents(root, entry.Path); err != nil {
			return err
		}
		if entry.Mode == "120000" {
			if err := root.Symlink("transfer-intent-placeholder", filepath.FromSlash(entry.Path)); err != nil {
				return err
			}
		} else {
			mode := fs.FileMode(0o600)
			if entry.Mode == "100755" {
				mode = 0o700
			}
			file, err := root.OpenFile(filepath.FromSlash(entry.Path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
		paths.WriteString(entry.Path)
		paths.WriteByte(0)
	}
	isolated = append(isolated, "GIT_DIR="+privateGit, "GIT_WORK_TREE="+temporary, "GIT_INDEX_FILE="+index, "GIT_OBJECT_DIRECTORY="+objects)
	_, _, err = c.executeSpec(commandSpec{binary: "git", cwd: temporary, ctx: ctx, extraEnv: isolated, stdin: paths.String(), args: []string{
		"-c", "core.attributesFile=" + os.DevNull, "--literal-pathspecs", "add", "--intent-to-add", "--pathspec-from-file=-", "--pathspec-file-nul"}})
	return err
}
