package sessionfork

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/transferfiles"
)

// TransferFiles collects the native transcript and its complete opaque sidecar
// subtree. The injected projects root may itself be a relocated-home symlink;
// links inside a session are refused. The caller closes the provider process
// and holds the thread action lock before taking this snapshot.
//
// Keeping the original project slug is intentional. Supported Claude versions
// search every projects directory for --resume UUID, even with a different cwd.
// On installation the app also relocates into the destination workspace's slug
// through RelocateSession, preserving compatibility with older native clients.
func TransferFiles(ctx context.Context, projectsDir, sessionID, workspace string) ([]transferfiles.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := LocateSessionFile(projectsDir, sessionID, workspace)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(projectsDir, file)
	if err != nil || !fs.ValidPath(filepath.ToSlash(relative)) {
		return nil, errors.New("claude transfer: transcript is outside the projects directory")
	}
	relative = filepath.ToSlash(relative)
	if !transferfiles.ValidName(relative) {
		return nil, errors.New("claude transfer: transcript path is not portable")
	}
	root, err := os.OpenRoot(projectsDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.FromSlash(relative))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("claude transfer: transcript is not a regular file")
	}
	result := []transferfiles.Source{{Root: projectsDir, Path: relative, Name: "native/claude/" + relative}}
	sidecars := strings.TrimSuffix(relative, ".jsonl")
	info, err = root.Lstat(filepath.FromSlash(sidecars))
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("claude transfer: sidecar directory is not a regular directory")
	}
	err = fs.WalkDir(root.FS(), sidecars, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !transferfiles.ValidName(name) {
			return fmt.Errorf("claude transfer: unsupported sidecar %q", name)
		}
		if len(result) >= transferfiles.MaxFiles {
			return errors.New("claude transfer: too many sidecar files")
		}
		result = append(result, transferfiles.Source{Root: projectsDir, Path: name, Name: "native/claude/" + name})
		return nil
	})
	return result, err
}
