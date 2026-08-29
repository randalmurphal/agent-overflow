package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type attachmentCopy struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"`
}

// copyAttachments carries the on-disk half of the attachments table. Symlinks
// are skipped rather than followed, since the source is a real user directory.
func copyAttachments(sourceDir, targetDir string) (attachmentCopy, error) {
	var out attachmentCopy
	sourceFiles := make(map[string]string)
	sourceDirs := map[string]struct{}{".": {}}
	info, err := os.Lstat(sourceDir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return out, fmt.Errorf("attachments source %s is a symlink", sourceDir)
		}
		if !info.IsDir() {
			return out, fmt.Errorf("%s is not a directory", sourceDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("inspect %s: %w", sourceDir, err)
	}
	if err == nil {
		walkErr := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(sourceDir, path)
			if relErr != nil {
				return relErr
			}
			if rel == "." {
				return nil
			}
			if entry.IsDir() {
				sourceDirs[rel] = struct{}{}
				return nil
			}
			if !entry.Type().IsRegular() {
				out.Skipped++
				return nil
			}
			sourceFiles[rel] = path
			return nil
		})
		if walkErr != nil {
			return out, fmt.Errorf("inspect attachments from %s: %w", sourceDir, walkErr)
		}
	}

	targetInfo, err := os.Lstat(targetDir)
	if errors.Is(err, os.ErrNotExist) {
		targetInfo = nil
	} else if err != nil {
		return out, fmt.Errorf("inspect %s: %w", targetDir, err)
	} else if targetInfo.Mode()&os.ModeSymlink != 0 {
		return out, fmt.Errorf("attachments target %s is a symlink", targetDir)
	} else if !targetInfo.IsDir() {
		return out, fmt.Errorf("%s is not a directory", targetDir)
	}

	if targetInfo != nil {
		if err := reconcileAttachmentTarget(targetDir, sourceFiles, sourceDirs); err != nil {
			return out, err
		}
	}
	if len(sourceDirs) > 1 {
		dirs := make([]string, 0, len(sourceDirs)-1)
		for rel := range sourceDirs {
			if rel != "." {
				dirs = append(dirs, rel)
			}
		}
		sort.Slice(dirs, func(i, j int) bool {
			return strings.Count(dirs[i], string(filepath.Separator)) < strings.Count(dirs[j], string(filepath.Separator))
		})
		for _, rel := range dirs {
			if err := os.MkdirAll(filepath.Join(targetDir, rel), 0o700); err != nil {
				return out, fmt.Errorf("create attachment directory %s: %w", rel, err)
			}
		}
	}
	for rel, source := range sourceFiles {
		destination := filepath.Join(targetDir, rel)
		written, err := copyFile(source, destination)
		if err != nil {
			return out, fmt.Errorf("copy attachment %s: %w", rel, err)
		}
		out.Files++
		out.Bytes += written
	}
	return out, nil
}

func reconcileAttachmentTarget(targetDir string, sourceFiles map[string]string, sourceDirs map[string]struct{}) error {
	type targetEntry struct {
		path string
		rel  string
		dir  bool
		mode os.FileMode
	}
	entries := make([]targetEntry, 0)
	walkErr := filepath.WalkDir(targetDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(targetDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entries = append(entries, targetEntry{path: path, rel: rel, dir: entry.IsDir(), mode: entry.Type()})
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("inspect target attachments %s: %w", targetDir, walkErr)
	}
	for _, entry := range entries {
		if entry.dir {
			continue
		}
		if _, wanted := sourceFiles[entry.rel]; wanted {
			if entry.mode&os.ModeSymlink != 0 {
				return fmt.Errorf("target attachment %s is a symlink", entry.path)
			}
			if !entry.mode.IsRegular() {
				return fmt.Errorf("target attachment %s is not a regular file", entry.path)
			}
			continue
		}
		if err := os.Remove(entry.path); err != nil {
			return fmt.Errorf("remove stale attachment %s: %w", entry.path, err)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.Count(entries[i].rel, string(filepath.Separator)) > strings.Count(entries[j].rel, string(filepath.Separator))
	})
	for _, entry := range entries {
		if !entry.dir {
			continue
		}
		if _, wanted := sourceDirs[entry.rel]; wanted {
			continue
		}
		if err := os.Remove(entry.path); err != nil {
			return fmt.Errorf("remove stale attachment directory %s: %w", entry.path, err)
		}
	}
	return nil
}

func copyFile(source, destination string) (int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return written, copyErr
	}
	return written, closeErr
}
