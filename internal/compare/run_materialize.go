package compare

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

func materialize(c Capsule, base, leg string, pair int) (string, directoryIdentity, error) {
	root, err := os.MkdirTemp(base, fmt.Sprintf("compare-%s%d-", leg, pair))
	if err != nil {
		return "", directoryIdentity{}, fmt.Errorf("create fresh %s%d root: %w", leg, pair, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".compare-root-identity"), []byte(uuid.NewString()), 0o600); err != nil {
		return "", directoryIdentity{}, fmt.Errorf("publish fresh %s%d root identity: %w", leg, pair, err)
	}
	rootIdentity, err := ownedDirectoryIdentity(root)
	if err != nil {
		return "", directoryIdentity{}, errors.Join(fmt.Errorf("capture fresh %s%d root identity: %w", leg, pair, err), removeDisposableRoot(root, directoryIdentity{}))
	}
	cleanup := func(cause error) (string, directoryIdentity, error) {
		return "", directoryIdentity{}, errors.Join(cause, removeDisposableRoot(root, rootIdentity))
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return cleanup(err)
	}
	dataDir := filepath.Join(root, "agent-overflow")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return cleanup(err)
	}
	if err := copyOne(filepath.Join(filepath.Dir(cPath(c)), c.Database.Path), filepath.Join(dataDir, "agent-overflow.db"), 0o600); err != nil {
		return cleanup(fmt.Errorf("materialize database: %w", err))
	}
	// Capsule content is addressed relative to its manifest directory. No
	// database rewrite, seed, or identifier map occurs here.
	capsuleDir := filepath.Dir(cPath(c))
	for _, asset := range c.Attachments {
		if err := copyOne(filepath.Join(capsuleDir, filepath.FromSlash(asset.Path)), filepath.Join(dataDir, filepath.FromSlash(asset.Path)), 0o600); err != nil {
			return cleanup(fmt.Errorf("materialize attachment %s: %w", asset.Path, err))
		}
	}
	for _, asset := range c.Fixtures {
		if err := copyOne(filepath.Join(capsuleDir, filepath.FromSlash(asset.Path)), filepath.Join(dataDir, filepath.FromSlash(asset.Path)), 0o600); err != nil {
			return cleanup(fmt.Errorf("materialize fixture %s: %w", asset.Path, err))
		}
	}
	if err := copyOne(filepath.Join(capsuleDir, c.Events.Path), filepath.Join(dataDir, "events.jsonl"), 0o600); err != nil {
		return cleanup(fmt.Errorf("materialize event stream: %w", err))
	}
	if err := os.MkdirAll(filepath.Join(root, "browser"), 0o700); err != nil {
		return cleanup(err)
	}
	return root, rootIdentity, nil
}

// cPath is the path retained by Load in the private registry. The registry
// avoids adding an untrusted filesystem path to the public capsule schema.
func cPath(c Capsule) string { return c.manifestPath }

type directoryIdentity struct {
	info  os.FileInfo
	token string
}

func ownedDirectoryIdentity(path string) (directoryIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return directoryIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return directoryIdentity{}, errors.New("path is not an owned directory")
	}
	if err := refuseSymlinkPath(path); err != nil {
		return directoryIdentity{}, err
	}
	marker := filepath.Join(path, ".compare-root-identity")
	markerInfo, err := os.Lstat(marker)
	if err != nil {
		return directoryIdentity{}, fmt.Errorf("inspect root identity: %w", err)
	}
	if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return directoryIdentity{}, errors.New("root identity is not a regular file")
	}
	token, err := os.ReadFile(marker)
	if err != nil {
		return directoryIdentity{}, fmt.Errorf("read root identity: %w", err)
	}
	if len(token) == 0 {
		return directoryIdentity{}, errors.New("root identity is empty")
	}
	return directoryIdentity{info: info, token: string(token)}, nil
}

func removeDisposableRoot(path string, expected directoryIdentity) error {
	if expected.info == nil || expected.token == "" {
		return errors.New("disposable root identity is unavailable")
	}
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		return nil
	} else if statErr != nil {
		return statErr
	}
	actual, err := ownedDirectoryIdentity(path)
	if err != nil {
		return err
	}
	if !os.SameFile(expected.info, actual.info) || expected.token != actual.token {
		return errors.New("disposable root identity changed; refusing removal")
	}
	return os.RemoveAll(path)
}
