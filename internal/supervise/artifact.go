package supervise

import (
	"archive/zip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
)

const (
	appBundleName            = "agent-overflow.app"
	artifactDigestName       = "artifact.sha256"
	maxArtifactEntries       = 50_000
	maxArtifactBytes   int64 = 2 << 30
)

// Artifact holds a verified download, expanded when the release is a macOS
// bundle. Preflight runs the actual executable inside that bundle. Close drops
// temporary extraction data; a successful Stage transfers ownership to Layout.
type Artifact struct {
	Binary string
	dir    string
	digest string
	bundle bool
}

// PrepareArtifact only handles integrity-verified release bytes. The release
// source verifies the digest BEFORE this call; extraction is not provenance.
func PrepareArtifact(ctx context.Context, downloaded, assetName, digest string) (*Artifact, error) {
	if !strings.HasSuffix(assetName, ".zip") {
		return &Artifact{Binary: downloaded}, nil
	}
	if assetName != "agent-overflow-darwin-arm64.zip" && assetName != "agent-overflow-darwin-amd64.zip" {
		return nil, fmt.Errorf("supervise: unsupported release archive %q", assetName)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("supervise: a verified archive digest is required")
	}
	dir, err := os.MkdirTemp(filepath.Dir(downloaded), "artifact-*")
	if err != nil {
		return nil, err
	}
	a := &Artifact{dir: dir, digest: digest, bundle: true, Binary: filepath.Join(dir, appBundleName, "Contents", "MacOS", BinaryName)}
	if err := unpackAppBundle(ctx, downloaded, dir); err != nil {
		a.Close()
		return nil, err
	}
	info, err := os.Stat(a.Binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		a.Close()
		return nil, errors.New("supervise: release bundle has no executable agent-overflow")
	}
	// Keep the established versions/<v>/agent-overflow entry point. Older
	// supervisors can launch this version too, without understanding bundles.
	// exec preserves the channel and runs at the real signed bundle path.
	launcher := "#!/bin/sh\nexec \"${0%/*}/agent-overflow.app/Contents/MacOS/agent-overflow\" \"$@\"\n"
	if err := writeArtifactFile(filepath.Join(dir, BinaryName), []byte(launcher), 0o700); err != nil {
		a.Close()
		return nil, err
	}
	if err := writeArtifactFile(filepath.Join(dir, artifactDigestName), []byte(digest), 0o600); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *Artifact) Close() {
	if a.dir != "" {
		_ = os.RemoveAll(a.dir)
		a.dir = ""
	}
}

func (a *Artifact) Stage(layout Layout, version string) error {
	if !a.bundle {
		return StageBinary(layout, version, a.Binary)
	}
	if a.dir == "" {
		return errors.New("supervise: artifact was already staged or closed")
	}
	target, err := layout.VersionDir(version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(layout.VersionsDir(), dirPerm); err != nil {
		return err
	}
	if err := syncArtifactDirs(a.dir); err != nil {
		return err
	}
	if err := atomicfile.RenameNoReplace(a.dir, target); err != nil {
		if existing, readErr := os.ReadFile(filepath.Join(target, artifactDigestName)); readErr == nil && string(existing) == a.digest {
			return nil
		}
		// An older supervisor staged only the executable. Reuse that exact
		// known version instead of replacing a possible rollback target.
		if legacy, statErr := os.Lstat(filepath.Join(target, BinaryName)); statErr == nil && legacy.Mode().IsRegular() {
			want, wantErr := fileDigest(a.Binary)
			got, gotErr := fileDigest(filepath.Join(target, BinaryName))
			if wantErr == nil && gotErr == nil && string(want) == string(got) {
				return nil
			}
		}
		return fmt.Errorf("supervise: version %s could not be staged without replacing existing files: %w", version, err)
	}
	a.dir = ""
	a.Binary = filepath.Join(target, appBundleName, "Contents", "MacOS", BinaryName)
	return atomicfile.SyncDir(layout.VersionsDir())
}

func unpackAppBundle(ctx context.Context, archive, root string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("supervise: open release archive: %w", err)
	}
	defer z.Close()
	if len(z.File) > maxArtifactEntries {
		return errors.New("supervise: release archive has too many entries")
	}
	seen := make(map[string]bool, len(z.File))
	links := make(map[string]string)
	remaining := maxArtifactBytes
	for _, entry := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") || (name != appBundleName && !strings.HasPrefix(name, appBundleName+"/")) || seen[name] {
			return fmt.Errorf("supervise: invalid or duplicate bundle entry %q", entry.Name)
		}
		seen[name] = true
		dest := filepath.Join(root, filepath.FromSlash(name))
		mode := entry.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return fmt.Errorf("supervise: unsupported bundle entry %q", name)
		}
		if entry.UncompressedSize64 > uint64(remaining) {
			return errors.New("supervise: release archive exceeds its size limit")
		}
		r, err := entry.Open()
		if err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			data, readErr := io.ReadAll(io.LimitReader(r, 4097))
			r.Close()
			target := string(data)
			resolved := path.Clean(path.Join(path.Dir(name), target))
			if readErr != nil || len(data) > 4096 || target == "" || path.IsAbs(target) || strings.ContainsAny(target, "\\:\x00") || !strings.HasPrefix(resolved, appBundleName+"/") {
				return fmt.Errorf("supervise: invalid bundle link %q", name)
			}
			remaining -= int64(len(data))
			links[name] = target
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			r.Close()
			return err
		}
		file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600|mode.Perm()&0o111)
		if err != nil {
			r.Close()
			return err
		}
		n, copyErr := io.Copy(file, io.LimitReader(&artifactReader{ctx: ctx, Reader: r}, remaining+1))
		r.Close()
		if syncErr := file.Sync(); copyErr == nil {
			copyErr = syncErr
		}
		if closeErr := file.Close(); copyErr == nil {
			copyErr = closeErr
		}
		remaining -= n
		if copyErr != nil {
			return copyErr
		}
		if remaining < 0 {
			return errors.New("supervise: release archive exceeds its size limit")
		}
	}
	// Links go last, so no file write can traverse a link. Resolve the whole
	// graph afterwards: individually safe link strings can still chain out.
	for name, target := range links {
		dest := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(target, dest); err != nil {
			return err
		}
	}
	canonicalRoot, err := filepath.EvalSymlinks(filepath.Join(root, appBundleName))
	if err != nil {
		return err
	}
	for name := range links {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("supervise: bundle link escapes the bundle")
		}
	}
	return nil
}

func syncArtifactDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicfile.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// A single compressed entry can be large; cancellation must reach the copy,
// not wait for the next entry boundary.
type artifactReader struct {
	ctx context.Context
	io.Reader
}

func (r *artifactReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

func writeArtifactFile(name string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
