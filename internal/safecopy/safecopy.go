package safecopy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// dirPerm is the app's private-directory mode. What this package copies is
// project-local material (a `.env`, a run's deliverable), so a directory it
// creates is never group- or world-readable.
const dirPerm os.FileMode = 0o700

// TempPrefix names the in-flight copies this package leaves behind when it is
// interrupted. Callers that list a destination directory skip them — a crashed
// copy is not a file anyone asked for.
const TempPrefix = ".ao-copy-"

// File copies one regular file from sourceRelative under sourceRootPath to
// destinationRelative under destinationRootPath.
//
// Both sides go through os.OpenRoot, so no component of either path can leave
// its root even by way of a symbolic link planted mid-copy. The write lands on
// a unique temp name, is fsynced, and is renamed into place: an interrupted
// copy leaves a TempPrefix file, never a half-written destination.
func File(sourceRootPath, sourceRelative, destinationRootPath, destinationRelative string, mode fs.FileMode) (resultErr error) {
	destination := filepath.Join(destinationRootPath, destinationRelative)
	if err := ValidateDestination(destinationRootPath, destination); err != nil {
		return err
	}
	sourceRoot, err := os.OpenRoot(sourceRootPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, sourceRoot.Close()) }()
	input, err := sourceRoot.Open(sourceRelative)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	destinationRoot, err := os.OpenRoot(destinationRootPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, destinationRoot.Close()) }()
	if err := destinationRoot.MkdirAll(filepath.Dir(destinationRelative), dirPerm); err != nil {
		return err
	}
	tempRelative := TempPrefix + uuid.NewString()
	temp, err := destinationRoot.OpenFile(tempRelative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = destinationRoot.Remove(tempRelative) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := io.Copy(temp, input); err != nil {
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
	return destinationRoot.Rename(tempRelative, destinationRelative)
}

// ValidateDestination refuses a destination whose parent chain leaves the
// managed root or passes through a symbolic link. os.OpenRoot already refuses
// the traversal; this refuses it with a diagnosis a human can act on, and it
// covers the directory-creation paths where no file handle is opened.
func ValidateDestination(root, destination string) error {
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination escapes its managed root")
	}
	current := root
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect destination parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination parent %q is a symbolic link", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("destination parent %q is not a directory", current)
		}
	}
	return nil
}
