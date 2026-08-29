package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// copyOne copies one regular file without following a link in either path.
// The source and destination directory handles keep the operation anchored if
// a caller replaces a path concurrently. The destination is published with a
// same-directory rename, so readers see either the old file or the complete
// new file.
func copyOne(source, dest string, mode os.FileMode) (resultErr error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := refuseSymlinkPath(source); err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	if err := ensureCopyParent(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("copy destination parent: %w", err)
	}
	if err := refuseSymlinkPath(dest); err != nil {
		return fmt.Errorf("copy destination: %w", err)
	}

	sourceParent, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, sourceParent.Close()) }()
	sourceName := filepath.Base(source)
	sourceInfo, err := sourceParent.Lstat(sourceName)
	if err != nil {
		return err
	}
	if !sourceInfo.Mode().IsRegular() {
		return errors.New("copy source is not a regular file")
	}
	in, err := sourceParent.Open(sourceName)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, in.Close()) }()
	openedInfo, err := in.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(sourceInfo, openedInfo) {
		return errors.New("copy source changed while opening")
	}

	destinationParent, err := os.OpenRoot(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, destinationParent.Close()) }()
	destinationName := filepath.Base(dest)
	if info, statErr := destinationParent.Lstat(destinationName); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("copy destination is a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("copy destination is not a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	var tempName string
	var out *os.File
	for attempt := 0; attempt < 8; attempt++ {
		tempName = fmt.Sprintf(".compare-copy-%s-%d", uuid.NewString(), time.Now().UnixNano())
		out, err = destinationParent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("create copy temporary file: %w", err)
	}
	defer func() { _ = destinationParent.Remove(tempName) }()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Re-check the final component immediately before publication. Rename
	// replaces a link rather than following it on supported platforms, but a
	// link is still an unsafe caller-visible destination and must be refused.
	if info, statErr := destinationParent.Lstat(destinationName); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("copy destination became a symlink")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := destinationParent.Rename(tempName, destinationName); err != nil {
		return fmt.Errorf("publish copied file: %w", err)
	}
	return nil
}

func writeReport(path string, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'))
}

func atomicWrite(path string, data []byte) (resultErr error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := ensureCopyParent(filepath.Dir(path)); err != nil {
		return err
	}
	if err := refuseSymlinkPath(path); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	name := filepath.Base(path)
	if info, statErr := parent.Lstat(name); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("atomic destination is a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("atomic destination is not a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	tempName := fmt.Sprintf(".compare-write-%s-%d", uuid.NewString(), time.Now().UnixNano())
	temp, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Remove(tempName) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if info, statErr := parent.Lstat(name); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("atomic destination became a symlink")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return parent.Rename(tempName, name)
}

func ensureCopyParent(parent string) error {
	parent, err := filepath.Abs(parent)
	if err != nil {
		return err
	}
	if err := refuseSymlinkPath(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(parent); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("destination parent is not an owned directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ancestor := parent
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("destination ancestor is not an owned directory")
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return errors.New("destination parent has no existing ancestor")
		}
		ancestor = next
	}
	relative, err := filepath.Rel(ancestor, parent)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return errors.New("destination parent escapes its existing ancestor")
	}
	root, err := os.OpenRoot(ancestor)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(relative, 0o700); err != nil {
		_ = root.Close()
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	return refuseSymlinkPath(parent)
}
