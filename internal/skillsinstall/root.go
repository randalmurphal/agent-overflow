package skillsinstall

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Every read, write and delete inside `<home>/skills` goes through an
// os.Root opened on it, so no path, however a later change builds it, can
// reach through a link to outside the skills directory. A root does follow a
// link that stays inside it; blockedAt finds those, so an install refuses
// them and a removal deletes the link, never through it.

// openSkills opens home's skills directory as a root, creating it when
// create is set. found is false when it does not exist and create is not
// set. A `skills` link is followed: it is the user's choice of where skills
// live.
func openSkills(home string, create bool) (root *os.Root, found bool, err error) {
	dir := skillsDir(home)
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	root, err = os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil {
		if info, statErr := os.Stat(dir); statErr == nil && !info.IsDir() {
			return nil, false, fmt.Errorf("%s is not a directory", dir)
		}
		return nil, false, err
	}
	return root, true, nil
}

// rootPath is name inside root as a path to show the user.
func rootPath(root *os.Root, name string) string {
	return filepath.Join(root.Name(), filepath.FromSlash(name))
}

// blockedAt returns the first directory above name, from the skill
// directory down, that exists and is not a real directory (a link, a
// file), or "" when there is none. A missing one ends the walk: nothing
// below it exists.
func blockedAt(root *os.Root, name string) (string, error) {
	dirs := parentDirs(name)
	for i := len(dirs) - 1; i >= 0; i-- {
		info, err := root.Lstat(filepath.FromSlash(dirs[i]))
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return dirs[i], nil
		}
	}
	return "", nil
}

func copyFile(root *os.Root, fsys fs.FS, file treeFile) error {
	if err := root.MkdirAll(filepath.FromSlash(path.Dir(file.path)), 0o755); err != nil {
		return err
	}
	source, err := fsys.Open(file.path)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if file.executable {
		mode = 0o755
	}
	return errors.Join(writeAtomic(root, file.path, source, mode), source.Close())
}

// writeAtomic writes beside name and renames into place, so a reader never
// sees half a file and a link at name is replaced, not written through.
func writeAtomic(root *os.Root, name string, source io.Reader, mode fs.FileMode) error {
	temp, tempName, err := createTemp(root, path.Dir(name))
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(temp, source)
	chmodErr := temp.Chmod(mode)
	closeErr := temp.Close()
	if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
		return errors.Join(err, root.Remove(tempName))
	}
	if err := root.Rename(tempName, filepath.FromSlash(name)); err != nil {
		return errors.Join(err, root.Remove(tempName))
	}
	return nil
}

func createTemp(root *os.Root, dir string) (*os.File, string, error) {
	for {
		name := filepath.FromSlash(path.Join(dir, ".ao-skill-"+strconv.FormatUint(rand.Uint64(), 36)))
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			return file, name, err
		}
	}
}

// removeFiles deletes each named file, returning the ones that could not
// be deleted. A file already gone is not a failure, nor is one under a link
// or file where its directory was: it is not there, and the link is pruneDirs'.
func removeFiles(root *os.Root, names []string) ([]string, error) {
	var failed []string
	var errs []error
	for _, name := range names {
		blocked, err := blockedAt(root, name)
		if err == nil && blocked == "" {
			err = root.Remove(filepath.FromSlash(name))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, name)
			errs = append(errs, err)
		}
	}
	return failed, errors.Join(errs...)
}

// pruneDirs removes, deepest first, the directories the named files sat in
// and the named skill directories, each only when it is empty. A link where
// one was is removed itself; nothing below a link is touched.
func pruneDirs(root *os.Root, skills, files []string) {
	seen := map[string]bool{}
	var dirs []string
	for _, name := range files {
		for _, parent := range parentDirs(name) {
			if !seen[parent] {
				seen[parent] = true
				dirs = append(dirs, parent)
			}
		}
	}
	for _, skill := range skills {
		if !seen[skill] {
			seen[skill] = true
			dirs = append(dirs, skill)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})
	for _, name := range dirs {
		// A directory that is not empty holds something the manifest does
		// not name, and stays; one already gone needs nothing.
		if blocked, err := blockedAt(root, name); err == nil && blocked == "" {
			_ = root.Remove(filepath.FromSlash(name))
		}
	}
}
