package selfupdate

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// stagedFileMode is the mode a staged artifact lands with. It is an
	// executable the launcher hands to the OS, so it carries the exec bit on
	// the Linux side of the boundary too.
	stagedFileMode = 0o755
	// stagingDirMode is the mode SweepStagingDir/StageCopy create the staging
	// directory with. It sits under the app-managed root, not a shared temp
	// dir, and holds nothing secret.
	stagingDirMode = 0o755
	// stagingTempPrefix marks in-progress copies. A crashed StageCopy leaves
	// one behind; SweepStagingDir clears it along with everything else.
	stagingTempPrefix = ".staging-"
)

// StageCopy copies src into dstDir under filename, verifying the streamed bytes
// against digest, and returns the final path.
//
// The copy lands on a temp file inside dstDir — not os.TempDir — so the final
// rename never crosses a filesystem and is therefore atomic; on the WSL side
// dstDir is a /mnt/c DrvFs path, where a cross-filesystem rename would degrade
// into a non-atomic copy. Nothing is renamed into place until the digest
// matches, so the destination never holds a partial or unverified file. The
// temp file is removed on every failure path too; if that removal itself fails
// (a scanner holding a DrvFs handle), the failure is joined onto the returned
// error rather than dropped — the leftover carries stagingTempPrefix, which
// SweepStagingDir clears before the next staging attempt.
func StageCopy(src, dstDir, filename string, digest []byte) (_ string, err error) {
	if src == "" {
		return "", errors.New("selfupdate: stage source path is empty")
	}
	if dstDir == "" {
		return "", errors.New("selfupdate: stage destination dir is empty")
	}
	if err := validateBareFilename(filename); err != nil {
		return "", err
	}
	if err := validateDigest(digest); err != nil {
		return "", err
	}

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("selfupdate: open %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(dstDir, stagingDirMode); err != nil {
		return "", fmt.Errorf("selfupdate: mkdir %s: %w", dstDir, err)
	}

	tmp, err := os.CreateTemp(dstDir, stagingTempPrefix+filename+"-*")
	if err != nil {
		return "", fmt.Errorf("selfupdate: create temp in %s: %w", dstDir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if committed {
			return
		}
		// Best-effort cleanup of the in-progress copy, but never silent: a
		// removal that fails joins the error the caller reports, so "the copy
		// failed AND its temp is still on disk" arrives as one message. Close
		// errors are ignored here deliberately — the happy path already
		// checks Close, so this Close only runs when the copy has failed and
		// the file's contents no longer matter.
		_ = tmp.Close()
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("selfupdate: remove temp %s: %w", tmpPath, rmErr))
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), in); err != nil {
		return "", fmt.Errorf("selfupdate: copy %s: %w", src, err)
	}
	if got := hasher.Sum(nil); subtle.ConstantTimeCompare(got, digest) != 1 {
		return "", fmt.Errorf("selfupdate: %s digest %s does not match the expected %s",
			src, hex.EncodeToString(got), hex.EncodeToString(digest))
	}
	if err := tmp.Chmod(stagedFileMode); err != nil {
		return "", fmt.Errorf("selfupdate: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("selfupdate: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("selfupdate: close %s: %w", tmpPath, err)
	}

	final := filepath.Join(dstDir, filename)
	if err := os.Rename(tmpPath, final); err != nil {
		return "", fmt.Errorf("selfupdate: rename %s -> %s: %w", tmpPath, final, err)
	}
	committed = true
	return final, nil
}

// SweepStagingDir removes every non-directory entry in dir: the staged artifacts
// themselves plus any temp file a crashed StageCopy left behind. A missing dir is
// success — there is nothing staged. Removal failures are collected and returned
// rather than dropped, so a sweep that could not clear a locked artifact is
// visible to the caller.
//
// A stray symlink is removed as the link itself; nothing here follows one.
func SweepStagingDir(dir string) error {
	if dir == "" {
		return errors.New("selfupdate: staging dir is empty")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("selfupdate: read staging dir %s: %w", dir, err)
	}
	var failures []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("selfupdate: sweep staging dir %s: %w", dir, errors.Join(failures...))
	}
	return nil
}
