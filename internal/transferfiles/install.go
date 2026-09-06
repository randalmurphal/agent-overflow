package transferfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/atomicfile"
)

// InstallTarget maps a verified archive member into one caller-injected root.
// Root is a logical key (claude, codex, attachments, workspace), never a path
// accepted from the archive. Replacement is reserved for a caller-authorized
// retired native session; ordinary files and uploads must not be overwritten.
type InstallTarget struct {
	File            File   `json:"file"`
	Root            string `json:"root"`
	Path            string `json:"path"`
	ReplaceExisting bool   `json:"replaceExisting,omitempty"`
}

// Installation records the exact state admitted during preparation. An empty
// PreviousSHA256 means absent. It is persisted BEFORE source retirement and
// never recomputed to excuse a conflict during activation or recovery.
type Installation struct {
	InstallTarget
	PreviousSHA256     string `json:"previousSha256,omitempty"`
	PreviousExecutable bool   `json:"previousExecutable,omitempty"`
}

// PrepareInstallation checks the whole target set without changing any files.
// The caller fences affected native sessions/workspaces until activation and
// holds their operation locks while preparing or installing. External writers
// are not coordinated by filesystem renames; observed changes are conflicts.
func PrepareInstallation(ctx context.Context, roots map[string]string, targets []InstallTarget) ([]Installation, error) {
	opened, err := openInstallRoots(roots, targets)
	if err != nil {
		return nil, err
	}
	defer closeInstallRoots(opened)
	prepared := make([]Installation, 0, len(targets))
	var previousBytes int64
	for _, target := range targets {
		current, found, err := inspectInstallFile(ctx, opened[target.Root], target.Path)
		if err != nil {
			return nil, err
		}
		previousBytes += current.Size
		if previousBytes > MaxTotalBytes {
			return nil, errors.New("transfer: destination baselines exceed total limit")
		}
		if found && !sameInstallContent(current, target.File) && !target.ReplaceExisting {
			return nil, fmt.Errorf("transfer: destination file already has different content: %s", target.Path)
		}
		entry := Installation{InstallTarget: target}
		if found {
			entry.PreviousSHA256, entry.PreviousExecutable = current.SHA256, current.Executable
		}
		prepared = append(prepared, entry)
	}
	return prepared, nil
}

// InstallPreparedFiles is restartable per file. A file is either still the
// admitted baseline or already has the exact new digest, size and executable
// bit. Unknown bytes are never treated as a partially completed installation.
// The caller authorizes activation before entering; success does not itself
// publish a conversation or change execution ownership.
func InstallPreparedFiles(ctx context.Context, staging string, roots map[string]string, prepared []Installation) error {
	targets := make([]InstallTarget, len(prepared))
	for i, entry := range prepared {
		targets[i] = entry.InstallTarget
		if entry.PreviousSHA256 != "" && !validInstallHash(entry.PreviousSHA256) {
			return errors.New("transfer: invalid installation baseline")
		}
		if !entry.ReplaceExisting && entry.PreviousSHA256 != "" && (entry.PreviousSHA256 != entry.File.SHA256 || entry.PreviousExecutable != entry.File.Executable) {
			return errors.New("transfer: replacement was not authorized")
		}
	}
	opened, err := openInstallRoots(roots, targets)
	if err != nil {
		return err
	}
	defer closeInstallRoots(opened)
	input, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer input.Close()
	// Catch a conflict before installing any member, then recheck each file
	// immediately before replacing it to catch changes during the copy.
	for _, entry := range prepared {
		if _, err := checkInstallBaseline(ctx, opened[entry.Root], entry); err != nil {
			return err
		}
	}
	for _, entry := range prepared {
		if err := installPreparedFile(ctx, input, opened[entry.Root], entry); err != nil {
			return err
		}
	}
	return nil
}

func validInstallHash(s string) bool {
	hash, err := hex.DecodeString(s)
	return err == nil && len(hash) == sha256.Size && strings.ToLower(s) == s
}

func openInstallRoots(roots map[string]string, targets []InstallTarget) (opened map[string]*os.Root, err error) {
	if len(targets) > MaxFiles {
		return nil, errors.New("transfer: too many installed files")
	}
	opened = make(map[string]*os.Root)
	defer func(handles map[string]*os.Root) {
		if err != nil {
			closeInstallRoots(handles)
		}
	}(opened)
	canonical := make(map[string]string)
	var names []string
	var total int64
	for _, target := range targets {
		if !ValidName(target.File.Name) || !ValidName(target.Path) || !ValidName(target.Root) || strings.Contains(target.Root, "/") || target.File.Size < 0 || target.File.Size > MaxFileBytes || !validInstallHash(target.File.SHA256) {
			return nil, errors.New("transfer: invalid installation target")
		}
		total += target.File.Size
		if total > MaxTotalBytes {
			return nil, errors.New("transfer: installed files exceed total limit")
		}
		if opened[target.Root] == nil {
			rootPath, ok := roots[target.Root]
			if !ok || !filepath.IsAbs(rootPath) {
				return nil, errors.New("transfer: unknown destination root")
			}
			resolved, err := filepath.EvalSymlinks(rootPath)
			if err != nil {
				return nil, err
			}
			root, err := os.OpenRoot(resolved)
			if err != nil {
				return nil, err
			}
			opened[target.Root] = root
			canonical[target.Root] = resolved
		}
		names = append(names, strings.ToLower(filepath.ToSlash(filepath.Join(canonical[target.Root], filepath.FromSlash(target.Path)))))
	}
	sort.Strings(names)
	for i, name := range names {
		child := sort.SearchStrings(names, name+"/")
		if (i > 0 && name == names[i-1]) || (child < len(names) && strings.HasPrefix(names[child], name+"/")) {
			return nil, errors.New("transfer: overlapping installation targets")
		}
	}
	return opened, nil
}

func closeInstallRoots(roots map[string]*os.Root) {
	for _, root := range roots {
		root.Close()
	}
}

func inspectInstallFile(ctx context.Context, root *os.Root, name string) (File, bool, error) {
	if err := ctx.Err(); err != nil {
		return File{}, false, err
	}
	if err := regularPath(root, name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, false, nil
		}
		return File{}, false, err
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return File{}, false, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return File{}, false, err
	}
	if !before.Mode().IsRegular() || before.Size() > MaxFileBytes {
		return File{}, false, errors.New("transfer: destination file exceeds limit or is not regular")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, reader: f}, MaxFileBytes+1))
	if err != nil {
		return File{}, false, err
	}
	after, err := f.Stat()
	if err != nil {
		return File{}, false, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return File{}, false, errors.New("transfer: file changed while checking its content")
	}
	return File{Name: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil)), Executable: before.Mode()&0100 != 0}, true, nil
}

func sameInstallContent(a, b File) bool {
	return a.SHA256 == b.SHA256 && a.Size == b.Size && a.Executable == b.Executable
}

func checkInstallBaseline(ctx context.Context, root *os.Root, entry Installation) (done bool, err error) {
	current, found, err := inspectInstallFile(ctx, root, entry.Path)
	if err != nil {
		return false, err
	}
	if found && sameInstallContent(current, entry.File) {
		return true, nil
	}
	if (!found && entry.PreviousSHA256 == "") || (found && current.SHA256 == entry.PreviousSHA256 && current.Executable == entry.PreviousExecutable) {
		return false, nil
	}
	return false, fmt.Errorf("transfer: destination changed since preparation: %s", entry.Path)
}

func installPreparedFile(ctx context.Context, input, output *os.Root, entry Installation) error {
	done, err := checkInstallBaseline(ctx, output, entry)
	if err != nil {
		return err
	}
	if done {
		return syncInstalledFile(output, entry.Path)
	}
	if err := regularPath(input, entry.File.Name); err != nil {
		return err
	}
	source, err := input.Open(filepath.FromSlash(entry.File.Name))
	if err != nil {
		return err
	}
	defer source.Close()
	dir := path.Dir(entry.Path)
	if err := makeInstallDirs(output, dir); err != nil {
		return err
	}
	// A deterministic, content-bound private name lets a restart discard its
	// unfinished copy without accumulating one orphan per failed attempt.
	nonce := sha256.Sum256([]byte(entry.Root + "\n" + entry.Path + "\n" + entry.File.SHA256))
	tempSlash := path.Join(dir, ".ao-transfer-"+hex.EncodeToString(nonce[:16])+".tmp")
	tempName := filepath.FromSlash(tempSlash)
	if err := regularPath(output, tempSlash); err == nil {
		if err := output.Remove(tempName); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0600)
	if entry.File.Executable {
		mode = 0700
	}
	temp, err := output.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { temp.Close(); output.Remove(tempName) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(&contextReader{ctx: ctx, reader: source}, entry.File.Size+1))
	if err != nil {
		return err
	}
	if n != entry.File.Size || hex.EncodeToString(hash.Sum(nil)) != entry.File.SHA256 {
		return errors.New("transfer: staged file no longer matches its verified content")
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	done, err = checkInstallBaseline(ctx, output, entry)
	if err != nil {
		return err
	}
	if !done {
		target := filepath.FromSlash(entry.Path)
		if entry.PreviousSHA256 == "" {
			// Atomic no-clobber publication; an unexpected new file wins.
			err = output.Link(tempName, target)
		} else {
			err = output.Rename(tempName, target)
		}
		if err != nil {
			return err
		}
	}
	if err := output.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicfile.SyncRootDir(output, filepath.FromSlash(dir))
}

func syncInstalledFile(root *os.Root, name string) error {
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err := makeInstallDirs(root, path.Dir(name)); err != nil {
		return err
	}
	return atomicfile.SyncRootDir(root, filepath.FromSlash(path.Dir(name)))
}

func makeInstallDirs(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(filepath.FromSlash(name))
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(filepath.FromSlash(name), 0700); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("transfer: destination parent is not a regular directory")
		}
		if err := atomicfile.SyncRootDir(root, filepath.FromSlash(path.Dir(name))); err != nil {
			return err
		}
	}
	return nil
}
