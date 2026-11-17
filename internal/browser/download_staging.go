package browser

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The engines in this process (downloadsStaged) write each download into
// the app's private staging directory inside the user's Downloads folder
// (stagingDirName), never anywhere else, and the Manager publishes the
// finished file into the Downloads folder beside it (publishDownload). The
// staging directory is in the Downloads folder so that publishing is a move
// within one filesystem that never replaces a file, and nothing partial
// ever sits under a public name.
//
// The directory is resolved for each download as it starts (stageDownload),
// from the Downloads folder as it is then: a relocated Downloads folder
// takes the next download, and one that cannot be resolved, created or
// verified fails that download's row with the reason, never a write
// elsewhere. A download publishes into the folder its staging directory is
// in. Every staged name carries a token unique to this process, so a file
// another run left behind never collides with a new one.

// stagingDirName is the app's staging directory in the Downloads folder.
const stagingDirName = ".agent-overflow-downloads"

// stagedLeftoverAge is how old a staged file no running download of this
// process owns must be before the first download of the process to use the
// directory removes it. A running download writes its file as bytes arrive,
// so its file stays newer than this; a download of another app process that
// stalls this long loses its file and fails at its publish.
const stagedLeftoverAge = 24 * time.Hour

// staging is the process's record of the staging directories its
// downloads use: the staged files running downloads own, and the
// directories already pruned in this process.
var staging = struct {
	mu     sync.Mutex
	token  string
	seq    uint64
	owned  map[string]bool
	pruned map[string]bool
}{owned: map[string]bool{}, pruned: map[string]bool{}}

// stagingToken is the part of a staged name unique to this process.
func stagingToken() string {
	if staging.token == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand does not fail on supported platforms; the pid still
			// separates live processes.
			staging.token = fmt.Sprintf("p%d", os.Getpid())
		} else {
			staging.token = hex.EncodeToString(b[:])
		}
	}
	return staging.token
}

// stageDownload claims a staged file for a new download: the staging
// directory of the current Downloads folder, created (mode 0700, with a
// missing Downloads folder) and verified to be a real directory, pruned on
// its first use in this process, and a name in it no other download uses.
// The engine writes the download there and nowhere else.
func stageDownload() (string, error) {
	downloads, err := userDownloadsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(downloads, stagingDirName)
	staging.mu.Lock()
	defer staging.mu.Unlock()
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("browser: create the download staging folder %s: %w", dir, err)
	}
	if err := checkStagingDir(dir); err != nil {
		return "", err
	}
	if !staging.pruned[dir] {
		staging.pruned[dir] = true
		pruneStagingLocked(dir, time.Now())
	}
	staging.seq++
	path := filepath.Join(dir, fmt.Sprintf("dl-%s-%d", stagingToken(), staging.seq))
	staging.owned[path] = true
	return path, nil
}

// checkStagingDir verifies dir is a real directory, not a link to one.
func checkStagingDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("browser: the download staging folder: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("browser: the download staging folder %s is not a folder (a link or a file is there), so nothing is saved through it", dir)
	}
	return nil
}

// stagingDirOf answers the staging directory a staged path is in, verified.
func stagingDirOf(staged string) (string, error) {
	dir := filepath.Dir(staged)
	if filepath.Base(dir) != stagingDirName {
		return "", fmt.Errorf("browser: %s is not in a download staging folder", staged)
	}
	if err := checkStagingDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// releaseStaged ends a download's claim on its staged file: the file and
// its companions go if they are still there (a canceled, failed or
// unpublished download's), and the staging directory goes once no download
// of this process stages in it and it is empty. Only names inside the app's
// staging directory are ever removed.
func releaseStaged(path string) {
	staging.mu.Lock()
	defer staging.mu.Unlock()
	if !staging.owned[path] {
		return
	}
	delete(staging.owned, path)
	dir, err := stagingDirOf(path)
	if err != nil {
		log.Printf("browser: leave the staged download %s: %v", path, err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the staged download %s: %v", path, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("browser: read the download staging folder %s: %v", dir, err)
	}
	for _, entry := range entries {
		if companion := filepath.Join(dir, entry.Name()); stagedFileOf(companion) == path && companion != path {
			if err := os.Remove(companion); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Printf("browser: remove the staged download %s: %v", companion, err)
			}
		}
	}
	removeStagingDirIfDoneLocked(dir)
}

// stagedFileOf answers the staged file a name in the staging directory
// belongs to: itself, or for a companion the engine writes beside it
// (WebKitGTK writes "<staged>.wkdownload" until the download finishes) the
// staged name before the first dot, as staged names have none.
func stagedFileOf(path string) string {
	base := filepath.Base(path)
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return filepath.Join(filepath.Dir(path), base)
}

// removeStagingDirIfDoneLocked removes dir when no download of this process
// stages in it and it is empty; files another run left keep it. The caller
// holds staging.mu.
func removeStagingDirIfDoneLocked(dir string) {
	for owned := range staging.owned {
		if filepath.Dir(owned) == dir {
			return
		}
	}
	// rmdir removes an empty directory alone, never a file.
	if err := os.Remove(dir); err == nil {
		delete(staging.pruned, dir)
	}
}

// pruneStagingLocked removes the regular files in the staging directory dir
// older than stagedLeftoverAge that no running download of this process
// owns, as its staged file or a companion of it. The caller holds
// staging.mu.
func pruneStagingLocked(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("browser: read the download staging folder %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if staging.owned[stagedFileOf(path)] || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < stagedLeftoverAge {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("browser: remove the stale staged download %s: %v", path, err)
		}
	}
}
