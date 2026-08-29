package harnessrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
)

var errRetentionProtected = errors.New("retention protected")

func verifyRetainable(entry ArtifactEntry) error {
	if entry.Pinned {
		return errRetentionProtected
	}
	for _, leasePath := range []string{filepath.Join(entry.DataRoot, LeaseFileName), filepath.Join(entry.Root, LeaseFileName)} {
		if _, err := os.Stat(leasePath); err == nil {
			return errRetentionProtected
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect lease %s: %w", leasePath, err)
		}
	}
	root, err := canonicalOwnedRoot(entry.Root)
	if err != nil {
		return err
	}
	if root != entry.Root {
		return fmt.Errorf("retained root is not canonical")
	}
	if filepath.Clean(entry.ManifestPath) != filepath.Join(root, ManifestFileName) {
		return fmt.Errorf("manifest path does not belong to retained root")
	}
	m, err := ReadManifest(root)
	if err != nil {
		return err
	}
	switch m.State {
	case StateCreated, StatePreparing, StateReady, StateRunning, StateStopping:
		return errRetentionProtected
	}
	if m.State != StateQuarantined || m.Plan.Ownership != OwnershipFresh || m.Plan.RunID != entry.RunID {
		return fmt.Errorf("manifest identity does not match run %q", entry.RunID)
	}
	if err := verifyManifestIdentity(m, root); err != nil {
		return err
	}
	sha, _, err := digestFile(filepath.Join(root, ManifestFileName))
	if err != nil {
		return err
	}
	if sha != entry.ManifestSHA256 {
		return fmt.Errorf("manifest checksum changed")
	}
	return nil
}

func verifyManifestIdentity(m Manifest, root string) error {
	if filepath.Clean(m.Quarantine) != root {
		return fmt.Errorf("manifest quarantine path does not match retained root")
	}
	expected := filepath.Clean(filepath.Clean(m.Plan.DataRoot) + QuarantineSuffix + string(filepath.Separator) + m.Plan.RunID)
	if expected != root {
		return fmt.Errorf("retained root is not the run's canonical quarantine path")
	}
	return rejectRealAppRoot(m.Plan.DataRoot)
}

func canonicalOwnedRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return "", errors.New("retained root must be an absolute path")
	}
	st, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("retained root is not an owned directory")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != filepath.Clean(root) {
		return "", errors.New("retained root is not canonical")
	}
	if err := rejectSymlinkedParents(root); err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func rejectSymlinkedParents(path string) error {
	for p := filepath.Dir(path); p != filepath.Dir(p); p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("retained root has a symlinked parent")
		}
	}
	return nil
}

func rejectRealAppRoot(root string) error {
	appRoot, err := appdirs.Root()
	if err != nil {
		return fmt.Errorf("retention cannot establish the real app data root: %w", err)
	}
	a, errA := canonicalSafetyPath(root)
	b, errB := canonicalSafetyPath(appRoot)
	if errA != nil || errB != nil {
		return errors.New("retention cannot canonicalize the real app data root")
	}
	if a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator)) {
		return errors.New("retention refuses a root overlapping the real app data")
	}
	return nil
}

func canonicalSafetyPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return abs, nil
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func directoryBytes(root string) (int64, error) {
	return directoryBytesContext(context.Background(), root)
}

func directoryBytesContext(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in retained root: %s", path)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
