package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/transferfiles"
)

func validateTransferWorkspace(w TransferWorkspace) error {
	if w.Version != 1 || !validTransferOID(w.Head) || len(w.Branch) > 1024 || strings.ContainsAny(w.Branch, "\x00\r\n") || len(w.Working) > transferfiles.MaxFiles || len(w.Flags) > len(w.Index) {
		return errors.New("transfer: invalid workspace metadata")
	}
	if err := validateTransferIndex(w.Index); err != nil {
		return err
	}
	indexed := make(map[string]TransferIndexEntry, len(w.Index))
	final := make(map[string]bool, len(w.Index)+len(w.Working))
	for _, entry := range w.Index {
		if !transferWorkspacePath(entry.Path) || entry.Mode == "160000" || len(entry.OID) != len(w.Head) {
			return errors.New("transfer: unsupported workspace index")
		}
		indexed[entry.Path], final[entry.Path] = entry, true
	}
	seen := make(map[string]bool, len(w.Flags))
	for _, flag := range w.Flags {
		entry, found := indexed[flag.Path]
		if !found || seen[flag.Path] || (!flag.IntentToAdd && !flag.SkipWorktree && !flag.AssumeUnchanged) {
			return errors.New("transfer: invalid workspace index flags")
		}
		if flag.IntentToAdd {
			// Intent-to-add carries an empty staged blob. Do not let a peer
			// discard staged bytes by attaching that flag to an ordinary entry.
			emptyBlob := "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
			if len(w.Head) == 64 {
				emptyBlob = fmt.Sprintf("%x", sha256.Sum256([]byte("blob 0\x00")))
			}
			if entry.OID != emptyBlob {
				return errors.New("transfer: intent-to-add contains staged content")
			}
		}
		seen[flag.Path] = true
	}
	clear(seen)
	for _, file := range w.Working {
		if !transferWorkspacePath(file.Path) || seen[strings.ToLower(file.Path)] {
			return errors.New("transfer: duplicate or invalid working path")
		}
		seen[strings.ToLower(file.Path)] = true
		switch file.Kind {
		case "deleted":
			delete(final, file.Path)
		case "file", "symlink":
			final[file.Path] = true
		default:
			return errors.New("transfer: invalid working file kind")
		}
		if (file.Kind != "symlink" && file.Link != "") || (file.Kind == "symlink" && (file.Link == "" || len(file.Link) > 4096 || strings.ContainsRune(file.Link, 0))) {
			return errors.New("transfer: invalid working symbolic link")
		}
	}
	paths := make([]string, 0, len(final))
	clear(seen)
	for path := range final {
		folded := strings.ToLower(path)
		if seen[folded] {
			return errors.New("transfer: working paths collide on this computer")
		}
		seen[folded] = true
		paths = append(paths, folded)
	}
	slices.Sort(paths)
	for _, path := range paths {
		prefix := path + "/"
		at, _ := slices.BinarySearch(paths, prefix)
		if at < len(paths) && strings.HasPrefix(paths[at], prefix) {
			return errors.New("transfer: working files overlap a file or symbolic link")
		}
	}
	return nil
}

func (c *Core) materializeTransferWorkspace(ctx context.Context, cwd string, root, archive *os.Root, w TransferWorkspace) error {
	sizes, err := c.transferBlobSizes(ctx, cwd, w.Index, math.MaxInt64)
	if err != nil {
		return err
	}
	intent := make(map[string]bool)
	for _, flag := range w.Flags {
		if flag.IntentToAdd {
			intent[flag.Path] = true
		}
	}
	overridden := make(map[string]bool, len(w.Working))
	var expanded int64
	for i, file := range w.Working {
		overridden[file.Path] = true
		if file.Kind == "file" {
			info, err := archive.Lstat(filepath.FromSlash(TransferWorkingSourceName(i)))
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() > transferfiles.MaxFileBytes {
				return errors.New("transfer: invalid working file size or type")
			}
			expanded += info.Size()
		} else if file.Kind == "symlink" {
			expanded += int64(len(file.Link))
		}
	}
	entries := make([]TransferIndexEntry, 0, len(w.Index))
	var checkout, intents []TransferIndexEntry
	var checkoutSizes []int64
	for i, entry := range w.Index {
		if !intent[entry.Path] {
			entries = append(entries, entry)
			if !overridden[entry.Path] {
				checkout = append(checkout, entry)
				checkoutSizes = append(checkoutSizes, sizes[i])
				expanded += sizes[i]
			}
		} else {
			intents = append(intents, entry)
		}
		if expanded > transferfiles.MaxTotalBytes {
			return errors.New("The expanded workspace exceeds the transfer size limit.")
		}
	}
	if expanded > transferfiles.MaxTotalBytes {
		return errors.New("The expanded workspace exceeds the transfer size limit.")
	}
	if err := c.RestoreTransferIndex(ctx, cwd, entries); err != nil {
		return err
	}
	if err := c.checkoutTransferBlobs(ctx, cwd, root, checkout, checkoutSizes); err != nil {
		return err
	}
	if err := c.restoreTransferIntentIndex(ctx, cwd, intents, len(w.Head) == 64); err != nil {
		return err
	}
	// Deletions run first. A former file/link can then become a directory,
	// and a directory emptied by deleted children can become a new file.
	for _, file := range w.Working {
		if file.Kind != "deleted" {
			continue
		}
		if err := removeTransferWorkingFile(root, file.Path); err != nil {
			return err
		}
	}
	var total int64
	buffer := make([]byte, 128<<10)
	for i, file := range w.Working {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Kind == "deleted" {
			continue
		}
		if err := removeTransferWorkingFile(root, file.Path); err != nil {
			return err
		}
		if err := ensureTransferParents(root, file.Path); err != nil {
			return err
		}
		if file.Kind == "symlink" {
			if err := root.Symlink(file.Link, filepath.FromSlash(file.Path)); err != nil {
				return err
			}
			continue
		}
		size, err := copyTransferWorkingFile(ctx, archive, root, TransferWorkingSourceName(i), file.Path, transferfiles.MaxTotalBytes-total, buffer)
		if err != nil {
			return err
		}
		total += size
	}
	for _, option := range []string{"--skip-worktree", "--assume-unchanged"} {
		var paths strings.Builder
		for _, flag := range w.Flags {
			if (option == "--skip-worktree" && flag.SkipWorktree) || (option == "--assume-unchanged" && flag.AssumeUnchanged) {
				paths.WriteString(flag.Path + "\x00")
			}
		}
		if paths.Len() > 0 {
			if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: paths.String(), args: []string{"update-index", option, "-z", "--stdin"}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureTransferParents(root *os.Root, path string) error {
	parts := strings.Split(path, "/")
	for i := range len(parts) - 1 {
		parent := filepath.FromSlash(strings.Join(parts[:i+1], "/"))
		info, err := root.Lstat(parent)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(parent, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("transfer: working file traverses a symbolic link or file")
		}
	}
	return nil
}

func removeTransferWorkingFile(root *os.Root, path string) error {
	parts := strings.Split(path, "/")
	for i := range len(parts) - 1 {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		} // Already absent, and never follow an old link.
	}
	if err := root.Remove(filepath.FromSlash(path)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// Git tracks files, not empty directories. Pruning empty ancestors lets
	// a later working delta replace an indexed directory with a file/link.
	for i := len(parts) - 1; i > 0; i-- {
		if err := root.Remove(filepath.FromSlash(strings.Join(parts[:i], "/"))); err != nil {
			break
		}
	}
	return nil
}

func copyTransferWorkingFile(ctx context.Context, archive, workspace *os.Root, source, destination string, remaining int64, buffer []byte) (int64, error) {
	info, err := archive.Lstat(filepath.FromSlash(source))
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > transferfiles.MaxFileBytes || info.Size() > remaining {
		return 0, errors.New("transfer: invalid working file size or type")
	}
	in, err := archive.Open(filepath.FromSlash(source))
	if err != nil {
		return 0, err
	}
	defer in.Close()
	mode := fs.FileMode(0o600)
	if info.Mode()&0o100 != 0 {
		mode = 0o700
	}
	out, err := workspace.OpenFile(filepath.FromSlash(destination), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	var copied int64
	for copied < info.Size() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, readErr := in.Read(buffer[:min(int64(len(buffer)), info.Size()-copied)])
		if n > 0 {
			if _, err := out.Write(buffer[:n]); err != nil {
				return 0, err
			}
			copied += int64(n)
		}
		if readErr != nil {
			return 0, fmt.Errorf("transfer: working file truncated: %w", readErr)
		}
	}
	var extra [1]byte
	if n, err := in.Read(extra[:]); n != 0 || err != io.EOF {
		return 0, errors.New("transfer: working file changed after verification")
	}
	return copied, out.Close()
}
