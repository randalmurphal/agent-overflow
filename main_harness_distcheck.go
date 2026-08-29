// main_harness_distcheck.go — the stale-embed tripwire for isolated
// boots.
//
// The binary embeds frontend/dist at BUILD time, so a `vite build`
// followed by a not-rebuilt harness binary silently serves the previous
// bundle. Nothing on screen says so, and every measurement (perf run,
// bench, profile) then describes assets the developer no longer has —
// a whole profiling round was lost to minified names from an old embed
// after an unminified build. This check compares the embedded tree
// against the on-disk frontend/dist at boot and warns loudly on
// mismatch; `ao-harness health` reads the verdict from HarnessInfo.
//
// Isolated boots only: a normal desktop run serves the bundle it
// shipped with, and there is no adjacent checkout to compare against.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// Asset-freshness verdicts carried on harnessPaths and HarnessInfo.
const (
	assetsFreshnessMatch   = "match"
	assetsFreshnessStale   = "stale"
	assetsFreshnessUnknown = "unknown" // no on-disk dist found to compare against
	assetsFreshnessDev     = "dev-server"
)

// warnIfEmbeddedDistStale runs the check against the binary's embed,
// logs the warning when there is one, and returns the verdict for
// harnessPaths.AssetsFreshness.
func warnIfEmbeddedDistStale() string {
	verdict, warning := checkEmbeddedDistFreshness(assets)
	if warning != "" {
		log.Print(warning)
	}
	return verdict
}

// embeddedAssetDigest identifies the exact frontend bundle served from the
// binary. It is computed independently of the adjacent-dist freshness check
// because the adjacent tree is not present in installed builds.
func embeddedAssetDigest() string {
	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		return ""
	}
	hash, err := hashFSTree(dist)
	if err != nil {
		return ""
	}
	return "sha256:" + hash
}

// checkEmbeddedDistFreshness compares the embedded frontend/dist against
// an on-disk one and returns (verdict, warning). The warning is
// non-empty only for a stale embed. No on-disk dist (an installed
// binary far from any checkout) is "unknown" and silent — absence of
// the comparison target is not evidence of staleness.
func checkEmbeddedDistFreshness(embedded fs.FS) (string, string) {
	if os.Getenv("FRONTEND_DEVSERVER_URL") != "" {
		// The instance serves the dev server's assets, not the embed;
		// isolatedDevAssetWarning already shouts about that. A staleness
		// verdict about a bundle nobody is serving would only mislead.
		return assetsFreshnessDev, ""
	}
	distDir := locateOnDiskDist()
	if distDir == "" {
		return assetsFreshnessUnknown, ""
	}
	return compareEmbeddedDist(embedded, distDir)
}

// compareEmbeddedDist is the pure comparison: the embed root (which
// carries the frontend/dist prefix) against one on-disk dist directory.
func compareEmbeddedDist(embedded fs.FS, distDir string) (string, string) {
	embeddedDist, err := fs.Sub(embedded, "frontend/dist")
	if err != nil {
		return assetsFreshnessUnknown, ""
	}
	embHash, err := hashFSTree(embeddedDist)
	if err != nil {
		return assetsFreshnessUnknown, ""
	}
	diskHash, err := hashFSTree(os.DirFS(distDir))
	if err != nil {
		return assetsFreshnessUnknown, ""
	}
	if embHash == diskHash {
		return assetsFreshnessMatch, ""
	}
	return assetsFreshnessStale, "WARNING: this binary's EMBEDDED frontend bundle does not match " + distDir +
		" — the binary was built before the last frontend build (or vice versa). " +
		"Every asset this instance serves, and every measurement taken against it, is of the STALE embed. " +
		"Rebuild the harness binary (make harness-build) to pick up the current dist."
}

// locateOnDiskDist finds a frontend/dist worth comparing against: next
// to the current working directory (the make-from-repo-root workflow
// where the incident happened), or beside the executable's parent (the
// binary lives in <repo>/bin). A directory qualifies only when it holds
// an index.html — a half-deleted dist is not a comparison target.
func locateOnDiskDist() string {
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "frontend", "dist"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "frontend", "dist"))
	}
	for _, dir := range candidates {
		if st, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && st.Mode().IsRegular() {
			return dir
		}
	}
	return ""
}

// hashFSTree folds every regular file (sorted path + content) into one
// hash, so any added, removed, renamed, or rewritten file changes the
// digest. dist is a few MB; this runs once per boot.
func hashFSTree(root fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		io.WriteString(h, p)
		h.Write([]byte{0})
		f, err := root.Open(p)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(h, f)
		f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
