package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transferfiles"
)

// The private activation recipe binds actual working bytes and semantic index
// state. Ignore index stat caches: background git status may refresh those.
// Preparation hashes and flushes in one walk; activation only verifies.
func (c *Core) transferWorktreeFingerprint(ctx context.Context, directory string, flush bool) (string, error) {
	index, err := c.transferIndexFingerprint(ctx, directory)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, "ao-workspace-v1\x00"+index+"\x00")
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var directories []string
	var total int64
	count := 0
	buffer := make([]byte, 128<<10)
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > (maxTransferIndexEntries+transferfiles.MaxFiles)*2+1 {
			return errors.New("transfer: prepared workspace exceeds file limit")
		}
		if name == ".git" {
			if !entry.Type().IsRegular() {
				return errors.New("transfer: prepared workspace Git reference changed")
			}
			return nil // Bound separately to the operation's registered GitDir.
		}
		if entry.IsDir() {
			if flush {
				directories = append(directories, name)
			}
			return nil
		}
		if !transferWorkspacePath(name) {
			return errors.New("transfer: prepared workspace contains a reserved path")
		}
		working, before, err := readWorkingEntry(root, name)
		if err != nil {
			return err
		}
		if before == nil || (working.Kind != "file" && working.Kind != "symlink") {
			return errors.New("transfer: prepared workspace changed while checking it")
		}
		size := before.Size()
		if working.Kind == "symlink" {
			size = int64(len(working.Link))
		}
		total += size
		if size < 0 || size > transferfiles.MaxFileBytes || total > transferfiles.MaxTotalBytes {
			return errors.New("transfer: prepared workspace exceeds byte limit")
		}
		// Each header has an explicit body length. JSON escapes arbitrary link
		// text and filenames, so no separator can alias a different tree.
		header := struct {
			Path       string
			Kind       string
			Size       int64
			Executable bool
			Link       string
		}{name, working.Kind, size, working.Kind == "file" && before.Mode()&0o100 != 0, working.Link}
		if err := json.NewEncoder(digest).Encode(header); err != nil {
			return err
		}
		if working.Kind == "symlink" {
			current, err := root.Lstat(filepath.FromSlash(name))
			if err != nil || !sameTransferWorkingFile(before, current) {
				return errors.New("transfer: prepared symbolic link changed while checking it")
			}
			return nil
		}
		return hashTransferWorkingFile(ctx, root, name, before, digest, buffer, flush)
	})
	if err != nil {
		return "", err
	}
	if flush {
		for i := len(directories) - 1; i >= 0; i-- {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := atomicfile.SyncRootDir(root, filepath.FromSlash(directories[i])); err != nil {
				return "", err
			}
		}
	}
	currentIndex, err := c.transferIndexFingerprint(ctx, directory)
	if err != nil {
		return "", err
	}
	if currentIndex != index {
		return "", errors.New("transfer: prepared index changed while checking the workspace")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (c *Core) transferIndexFingerprint(ctx context.Context, directory string) (string, error) {
	index, err := c.ReadTransferIndex(ctx, directory)
	if err != nil {
		return "", err
	}
	flags, err := c.readTransferIndexFlags(ctx, directory)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	if err := encoder.Encode(index); err != nil {
		return "", err
	}
	if err := encoder.Encode(flags); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashTransferWorkingFile(ctx context.Context, root *os.Root, name string, before fs.FileInfo, digest hash.Hash, buffer []byte, flush bool) error {
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameTransferWorkingFile(before, opened) {
		return errors.New("transfer: prepared file was replaced while checking it")
	}
	remaining := before.Size()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(file, buffer[:min(int64(len(buffer)), remaining)])
		if err != nil {
			return fmt.Errorf("transfer: prepared file changed: %w", err)
		}
		_, _ = digest.Write(buffer[:n])
		remaining -= int64(n)
	}
	if flush {
		if err := file.Sync(); err != nil {
			return err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(filepath.FromSlash(name))
	if err != nil || !sameTransferWorkingFile(before, after) || !sameTransferWorkingFile(before, current) {
		return errors.New("transfer: prepared file changed while checking it")
	}
	return file.Close()
}

func (c *Core) verifyTransferWorktreeFingerprint(ctx context.Context, plan TransferWorktree, directory string) error {
	if decoded, err := hex.DecodeString(plan.Fingerprint); err != nil || len(decoded) != sha256.Size {
		return errors.New("transfer: workspace preparation has no valid content fingerprint")
	}
	actual, err := c.transferWorktreeFingerprint(ctx, directory, false)
	if err != nil {
		return err
	}
	if actual != plan.Fingerprint {
		return errors.New("The prepared destination workspace changed. Restore its prepared files and index before retrying this transfer.")
	}
	return nil
}
