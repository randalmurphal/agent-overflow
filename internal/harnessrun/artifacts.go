package harnessrun

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func createArtifactRoot(plan RunPlan) error {
	root := ArtifactRoot(plan)
	if err := rejectRealAppRoot(root); err != nil {
		return fmt.Errorf("artifact root: %w", err)
	}
	if err := rejectSymlinkedParents(root); err != nil {
		return fmt.Errorf("artifact root: %w", err)
	}
	if st, err := os.Lstat(root); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("artifact root %s is not an owned directory", root)
		}
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			return fmt.Errorf("inspect artifact root: %w", readErr)
		}
		if len(entries) != 0 {
			return fmt.Errorf("artifact root %s already contains files", root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect artifact root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create artifact root: %w", err)
	}
	if st, err := os.Lstat(root); err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		if err != nil {
			return fmt.Errorf("verify artifact root: %w", err)
		}
		return fmt.Errorf("artifact root %s became a symlink or non-directory", root)
	}
	for _, path := range []string{plan.Output} {
		if path == "" {
			continue
		}
		if err := prepareArtifactPath(path, root); err != nil {
			return err
		}
	}
	for _, artifact := range plan.Artifacts {
		if artifact.Destination == "" {
			continue
		}
		if err := prepareArtifactPath(artifact.Destination, root); err != nil {
			return err
		}
	}
	return nil
}

func prepareArtifactPath(path, root string) error {
	path = filepath.Clean(path)
	if !underPath(path, root) {
		return fmt.Errorf("artifact path %s is outside supervisor artifact root %s", path, root)
	}
	parent := filepath.Dir(path)
	if err := rejectSymlinkedParents(parent); err != nil {
		return fmt.Errorf("artifact path %s: %w", path, err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create artifact parent %s: %w", parent, err)
	}
	if err := rejectSymlinkedParents(parent); err != nil {
		return fmt.Errorf("artifact path %s: %w", path, err)
	}
	return nil
}

// RecordArtifact verifies source bytes and, when configured, writes a durable
// external copy before marking the record durable.
func (s *Supervisor) RecordArtifact(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("run supervisor is closed")
	}
	if s.manifest.State == StateSucceeded || s.manifest.State == StateQuarantined {
		return fmt.Errorf("cannot record artifact on terminal run %q", s.manifest.State)
	}
	idx := -1
	for i := range s.manifest.Plan.Artifacts {
		if s.manifest.Plan.Artifacts[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("artifact %q is not declared", name)
	}
	pl := s.manifest.Plan.Artifacts[idx]
	src, err := confinedPath(s.root, pl.Path)
	rec := ArtifactRecord{Name: pl.Name, Path: pl.Path, Destination: pl.Destination, Status: ArtifactFailed, RecordedAt: time.Now().UTC()}
	if err == nil {
		rec.SHA256, rec.Bytes, err = digestFile(src)
	}
	if err == nil && pl.Destination == "" && s.manifest.Plan.Ownership == OwnershipFresh && !s.manifest.Plan.PreserveRoot {
		err = errors.New("artifact has no durable destination before the fresh root is removed")
	}
	if err == nil && pl.Destination != "" {
		err = copyDurable(src, pl.Destination, rec.SHA256)
	}
	if err != nil {
		rec.Error = err.Error()
		rec.Status = ArtifactMissing
	} else {
		rec.Status = ArtifactDurable
	}
	oldArtifacts := append([]ArtifactRecord(nil), s.manifest.Artifacts...)
	s.manifest.Artifacts = appendOrReplaceArtifact(s.manifest.Artifacts, rec)
	if persistErr := s.persistLocked(); persistErr != nil {
		s.manifest.Artifacts = oldArtifacts
		return errors.Join(err, persistErr)
	}
	if err != nil {
		return fmt.Errorf("artifact %q: %w", name, err)
	}
	return nil
}

func appendOrReplaceArtifact(all []ArtifactRecord, rec ArtifactRecord) []ArtifactRecord {
	for i := range all {
		if all[i].Name == rec.Name {
			all[i] = rec
			return all
		}
	}
	return append(all, rec)
}

func confinedPath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", errors.New("artifact path must be relative")
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	p, err := filepath.Abs(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes data root")
	}
	// Lexical confinement is insufficient when a workload replaces a parent
	// directory with a symlink. Evaluate the existing portion and check the
	// resolved path before opening it.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if resolved != resolvedBase && !strings.HasPrefix(resolved, resolvedBase+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes data root through symlink")
	}
	return p, nil
}

func digestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("artifact is not a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyDurable(src, dst, expectedSHA string) error {
	if dst == "" {
		return nil
	}
	parent := filepath.Dir(dst)
	if err := rejectSymlinkedParents(parent); err != nil {
		return fmt.Errorf("external artifact parent: %w", err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := rejectSymlinkedParents(parent); err != nil {
		return fmt.Errorf("external artifact parent changed: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(parent, ".run-artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err = io.Copy(tmp, in); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, dst)
	}
	if err != nil {
		return err
	}
	if err := syncDir(parent); err != nil {
		return err
	}
	got, _, err := digestFile(dst)
	if err != nil {
		return err
	}
	if got != expectedSHA {
		return errors.New("external artifact changed while copying")
	}
	return nil
}

// Complete validates required artifacts and records success. Root deletion is
// intentionally separate and only happens after this method returns nil.
func (s *Supervisor) Complete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifest.State != StateStopping {
		return fmt.Errorf("cannot complete from %q", s.manifest.State)
	}
	for _, p := range s.manifest.Plan.Artifacts {
		var found *ArtifactRecord
		for i := range s.manifest.Artifacts {
			if s.manifest.Artifacts[i].Name == p.Name {
				found = &s.manifest.Artifacts[i]
				break
			}
		}
		if p.Required && (found == nil || found.Status != ArtifactDurable) {
			return fmt.Errorf("required artifact %q is not durable", p.Name)
		}
	}
	now := time.Now().UTC()
	old := s.manifest
	s.manifest.State, s.manifest.Phase, s.manifest.FinishedAt = StateSucceeded, PhaseFinalize, &now
	if err := s.persistLocked(); err != nil {
		s.manifest = old
		return err
	}
	return nil
}
