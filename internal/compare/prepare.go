package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
)

const (
	appDataDirName = "agent-overflow"
	databaseName   = "agent-overflow.db"
)

// Prepare creates a self-contained, read-only capsule. Source bytes are
// read, never modified. A source with the harness marker is accepted as a
// harness-owned source, including a running harness. Any source resolving to
// the real app's config/data tree is refused before I/O begins.
func Prepare(opts PrepareOptions) (Capsule, error) {
	source, kind, err := resolveSource(opts.Source)
	if err != nil {
		return Capsule{}, err
	}
	output, err := filepath.Abs(opts.Output)
	if err != nil || strings.TrimSpace(opts.Output) == "" {
		return Capsule{}, fmt.Errorf("compare prepare needs --out <capsule directory>")
	}
	output = instanceinfo.NormalizeSystemPath(output)
	if samePathOrAncestor(output, source) || samePathOrAncestor(source, output) {
		return Capsule{}, fmt.Errorf("compare prepare refuses output %s inside or equal to source %s", output, source)
	}
	if err := refuseRealData(source); err != nil {
		return Capsule{}, err
	}
	if err := refuseActiveOfflineSource(source, kind); err != nil {
		return Capsule{}, err
	}
	outputExists := false
	if info, statErr := os.Lstat(output); statErr == nil {
		if !opts.Force {
			return Capsule{}, fmt.Errorf("capsule %s already exists; pass --force to replace it", output)
		}
		outputExists = true
		if info.Mode()&os.ModeSymlink != 0 {
			return Capsule{}, fmt.Errorf("capsule output %s is a symlink", output)
		}
		if !info.IsDir() {
			return Capsule{}, fmt.Errorf("capsule output %s is not a directory", output)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Capsule{}, fmt.Errorf("inspect capsule output %s: %w", output, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return Capsule{}, fmt.Errorf("create capsule parent: %w", err)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(output), ".compare-capsule-")
	if err != nil {
		return Capsule{}, fmt.Errorf("create capsule staging directory: %w", err)
	}
	defer func() {
		if tmp != "" {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return Capsule{}, err
	}

	sourceDB := filepath.Join(source, databaseName)
	dbBytes, err := snapshotAndScrub(sourceDB, filepath.Join(tmp, "db.snapshot"))
	if err != nil {
		return Capsule{}, err
	}
	panes, err := readLogicalPanes(sourceDB)
	if err != nil {
		return Capsule{}, err
	}
	attachments, err := copyAssets(filepath.Join(source, "attachments"), filepath.Join(tmp, "attachments"))
	if err != nil {
		return Capsule{}, fmt.Errorf("copy attachments: %w", err)
	}
	fixtures, err := copyAssets(filepath.Join(source, "fixtures"), filepath.Join(tmp, "fixtures"))
	if err != nil {
		return Capsule{}, fmt.Errorf("copy fixtures: %w", err)
	}
	prefixAssetPaths(attachments, "attachments/")
	prefixAssetPaths(fixtures, "fixtures/")
	events, err := collectEvents(filepath.Join(source, "replay"), filepath.Join(tmp, "events.jsonl"))
	if err != nil {
		return Capsule{}, err
	}

	assetDigest := opts.AssetDigest
	if assetDigest == "" {
		assetDigest, err = digestIfPresent(source, "asset.digest", "assets.digest")
		if err != nil {
			return Capsule{}, err
		}
	}
	if assetDigest == "" {
		assetDigest = "unknown"
	}
	buildDigest := opts.BuildDigest
	if buildDigest == "" {
		buildDigest, err = digestIfPresent(source, "build.digest", "version")
		if err != nil {
			return Capsule{}, err
		}
	}
	if buildDigest == "" {
		buildDigest = "unknown"
	}

	sourceSHA, err := fileDigest(sourceDB)
	if err != nil {
		return Capsule{}, fmt.Errorf("hash source database: %w", err)
	}
	databaseSHA, err := hashFile(filepath.Join(tmp, "db.snapshot"))
	if err != nil {
		return Capsule{}, fmt.Errorf("hash database snapshot: %w", err)
	}
	capsule := Capsule{
		Version:     CurrentVersion,
		CreatedAt:   time.Now().UTC(),
		Source:      Provenance{Kind: kind, SourceSHA: sourceSHA},
		Database:    Asset{Path: "db.snapshot", Bytes: dbBytes, SHA256: databaseSHA},
		Attachments: attachments,
		Fixtures:    fixtures,
		Panes:       panes,
		Events:      events,
		AssetDigest: assetDigest,
		BuildDigest: buildDigest,
		Workload:    opts.Workload,
	}
	if capsule.Workload.Name == "" {
		capsule.Workload.Name = "replay"
	}
	if err := validateCapsule(capsule); err != nil {
		return Capsule{}, err
	}
	manifest := filepath.Join(tmp, "manifest.json")
	if err := writeManifest(manifest, &capsule); err != nil {
		return Capsule{}, err
	}
	if err := makeReadOnly(tmp); err != nil {
		return Capsule{}, err
	}
	loaded, err := Load(manifest)
	if err != nil {
		return Capsule{}, err
	}
	if err := publishCapsule(tmp, output, outputExists); err != nil {
		return Capsule{}, err
	}
	tmp = ""
	// Rename makes the complete, already-validated tree visible at once. From
	// this point on it is immutable. Run copies from it into disposable leg
	// roots. Load validated the staging path before publication, so update the
	// private path retained by the returned capsule after the rename.
	loaded.manifestPath = filepath.Join(output, "manifest.json")
	return loaded, nil
}

// publishCapsule makes a complete staging tree visible without destroying the
// previous capsule until the replacement is ready. If publishing the new tree
// fails, the old tree is restored before returning the error.
func publishCapsule(stage, output string, replace bool) error {
	if !replace {
		if err := os.Rename(stage, output); err != nil {
			return fmt.Errorf("publish capsule %s: %w", output, err)
		}
		return nil
	}
	parent := filepath.Dir(output)
	backup, err := os.MkdirTemp(parent, ".compare-capsule-old-")
	if err != nil {
		return fmt.Errorf("stage previous capsule %s: %w", output, err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("prepare previous capsule replacement: %w", err)
	}
	if err := os.Rename(output, backup); err != nil {
		return fmt.Errorf("stage previous capsule %s: %w", output, err)
	}
	if err := os.Rename(stage, output); err != nil {
		restoreErr := os.Rename(backup, output)
		return fmt.Errorf("publish capsule %s: %w", output, errors.Join(err, restoreErr))
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous capsule backup %s: %w", backup, err)
	}
	return nil
}

func resolveSource(raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", fmt.Errorf("compare prepare needs --from <offline copy or harness source>")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", "", fmt.Errorf("resolve source: %w", err)
	}
	abs = instanceinfo.NormalizeSystemPath(abs)
	candidates := []string{abs, filepath.Join(abs, appDataDirName)}
	for _, candidate := range candidates {
		info, statErr := os.Stat(filepath.Join(candidate, databaseName))
		if statErr != nil || info.IsDir() {
			continue
		}
		if err := refuseSymlinkPath(candidate); err != nil {
			return "", "", err
		}
		dbInfo, err := os.Lstat(filepath.Join(candidate, databaseName))
		if err != nil || !dbInfo.Mode().IsRegular() {
			return "", "", fmt.Errorf("source database %s is not a regular file", filepath.Join(candidate, databaseName))
		}
		kind := "offline-copy"
		marker := filepath.Join(candidate, "harness-instance.json")
		if markerInfo, markerErr := os.Lstat(marker); markerErr == nil {
			if !markerInfo.Mode().IsRegular() {
				return "", "", fmt.Errorf("source marker %s is not a regular file", marker)
			}
			data, readErr := os.ReadFile(marker)
			if readErr != nil {
				return "", "", fmt.Errorf("read harness source marker: %w", readErr)
			}
			var identity struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(data, &identity); err != nil {
				return "", "", fmt.Errorf("parse harness source marker: %w", err)
			}
			switch identity.Mode {
			case "harness", "soak", "perf":
				kind = "harness-owned"
			default:
				return "", "", fmt.Errorf("source marker %s does not identify a harness-owned source", marker)
			}
		}
		return filepath.Clean(candidate), kind, nil
	}
	return "", "", fmt.Errorf("source %s has no %s", abs, databaseName)
}

func refuseRealData(source string) error {
	root, err := appdirs.Root()
	if err != nil {
		return fmt.Errorf("resolve real app data root before compare prepare: %w", err)
	}
	configRoot := filepath.Dir(root)
	if pathWithin(source, root) || pathWithin(root, source) || pathWithin(source, configRoot) {
		return fmt.Errorf("compare prepare refuses the real app data root %s; use an offline copy or harness source", source)
	}
	// A hard link is an alias without a path component to resolve. Refuse it
	// before snapshotting so an apparent offline copy cannot read the live DB.
	sourceInfo, sourceErr := os.Stat(filepath.Join(source, databaseName))
	realInfo, realErr := os.Stat(filepath.Join(root, databaseName))
	if sourceErr == nil && realErr == nil && os.SameFile(sourceInfo, realInfo) {
		return fmt.Errorf("compare prepare refuses source %s: its database is the real app database", source)
	}
	return nil
}

func refuseActiveOfflineSource(source, kind string) error {
	if kind == "harness-owned" {
		return nil
	}
	// Offline copies must not carry a live harness lock. A stale lock file is
	// ambiguous and is refused instead of guessed stale.
	if _, err := os.Stat(filepath.Join(source, "harness.lock")); err == nil {
		return fmt.Errorf("compare prepare refuses source %s: harness.lock is present without a harness marker", source)
	}
	return nil
}
