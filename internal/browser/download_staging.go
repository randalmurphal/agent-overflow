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
	"regexp"
	"strings"
	"sync"
	"syscall"
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
// in.
//
// Each process stages in a run directory of its own inside the staging
// directory (stagingRun), which it opens once and holds: every removal it
// makes there goes through that handle (os.Root), so a run directory moved
// aside or replaced by another never has a file of the replacement removed,
// and a publish takes the staged file only while its path still names the
// file in the held directory. A run directory and the staging directory
// are removed only as the directories they were, and only empty
// (removeHeldDir), never a file put at their names. The run directory holds
// an exclusive lock (runLockName) for as long as it exists. A publish gives
// the staged file its private name there too (placeNoReplace), so every
// name a download has before its public one is the run's. A process
// removes another run's staged files and private names only on positive
// evidence that the run ended: it takes that run's lock
// (pruneStagingLocked). A run directory without its lock file, and the
// files directly in the staging directory that releases staging without
// run directories left there, carry no such evidence and stay.

// stagingDirName is the app's staging directory in the Downloads folder.
const stagingDirName = ".agent-overflow-downloads"

// runLockName is the lock file in a run directory, held locked by the
// process staging there for as long as the directory exists.
const runLockName = ".lock"

// stagingRun is one process's run directory in one staging directory, held
// open from its first staged file to the release of its last. Guarded by
// staging.mu.
type stagingRun struct {
	// dir is the run directory's path as it was made, which may name
	// another directory since; root is the directory itself.
	dir  string
	root *os.Root
	lock *os.File
	// files is the staged names of its running downloads.
	files map[string]bool
	// staging is the staging directory the run was made in, as it was
	// then, which the run's end removes only while its path still names it.
	staging os.FileInfo
}

// staging is the process's record of its run directories: the current run
// of each staging directory, the run of each staged file a running download
// owns, and the staging directories already pruned in this process.
var staging = struct {
	mu     sync.Mutex
	token  string
	seq    uint64
	runs   map[string]*stagingRun
	owned  map[string]*stagingRun
	pruned map[string]bool
}{runs: map[string]*stagingRun{}, owned: map[string]*stagingRun{}, pruned: map[string]bool{}}

// stagingToken is the part of a run directory's name unique to this
// process. The caller holds staging.mu.
func stagingToken() string {
	if staging.token == "" {
		var b [6]byte
		// crypto/rand.Read never answers an error: it ends the process.
		_, _ = rand.Read(b[:])
		staging.token = hex.EncodeToString(b[:])
	}
	return staging.token
}

// stageDownload claims a staged file for a new download: in this process's
// run directory of the staging directory of the current Downloads folder,
// each created (mode 0700, with a missing Downloads folder) and verified to
// be a real directory, the staging directory pruned on its first use in
// this process, and a name in it no other download uses. The engine writes
// the download there and nowhere else.
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
	stagingInfo, err := checkStagingDir(dir)
	if err != nil {
		return "", err
	}
	if !staging.pruned[dir] {
		staging.pruned[dir] = true
		pruneStagingLocked(dir)
	}
	run := staging.runs[dir]
	if run != nil && !run.inPlace() {
		// Moved aside or replaced: its downloads finish in it, and new ones
		// stage in a run directory of their own.
		delete(staging.runs, dir)
		run = nil
	}
	if run == nil {
		if run, err = startStagingRun(dir, stagingInfo); err != nil {
			return "", err
		}
		staging.runs[dir] = run
	}
	staging.seq++
	name := fmt.Sprintf("dl-%d", staging.seq)
	path := filepath.Join(run.dir, name)
	run.files[name] = true
	staging.owned[path] = run
	return path, nil
}

// startStagingRun makes, locks and opens a run directory of this process
// in the staging directory dir, whose identity is stagingInfo: through a
// handle on that directory, so the run is made in it however its path is
// changed meanwhile. A run it cannot lock is removed again, and every
// failure of that cleanup is in the answer. The caller holds staging.mu.
func startStagingRun(dir string, stagingInfo os.FileInfo) (*stagingRun, error) {
	parent, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("browser: open the download staging folder %s: %w", dir, err)
	}
	defer func() {
		if err := parent.Close(); err != nil {
			log.Printf("browser: close the download staging folder %s: %v", dir, err)
		}
	}()
	if opened, err := parent.Stat("."); err != nil || !os.SameFile(opened, stagingInfo) {
		return nil, fmt.Errorf("browser: the download staging folder %s was moved or replaced as it was opened, so nothing is saved through it", dir)
	}
	base := "run-" + stagingToken()
	name := base
	for i := 1; ; i++ {
		err := parent.Mkdir(name, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i >= maxNameVariants {
			return nil, fmt.Errorf("browser: create the download staging folder %s: %w", filepath.Join(dir, name), err)
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	runDir := filepath.Join(dir, name)
	root, err := parent.OpenRoot(name)
	if err != nil {
		// The directory it made goes again, as no run's; one with anything
		// in it stays.
		failure := fmt.Errorf("browser: open the download staging folder %s: %w", runDir, err)
		if rmErr := parent.Remove(name); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return nil, errors.Join(failure, fmt.Errorf("browser: remove the download staging folder %s: %w", runDir, rmErr))
		}
		return nil, failure
	}
	lock, err := root.OpenFile(runLockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("browser: lock the download staging folder %s: %w", runDir, err), abandonRun(runDir, root))
	}
	locked, err := lockRun(lock)
	if err == nil && !locked {
		err = errors.New("another process holds its lock")
	}
	if err != nil {
		errs := []error{fmt.Errorf("browser: lock the download staging folder %s: %w", runDir, err)}
		if closeErr := lock.Close(); closeErr != nil {
			errs = append(errs, fmt.Errorf("browser: close the download staging lock in %s: %w", runDir, closeErr))
		}
		if removeErr := root.Remove(runLockName); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("browser: remove the download staging lock in %s: %w", runDir, removeErr))
		}
		return nil, errors.Join(append(errs, abandonRun(runDir, root))...)
	}
	return &stagingRun{dir: runDir, root: root, lock: lock, files: map[string]bool{}, staging: stagingInfo}, nil
}

// abandonRun removes the run directory startStagingRun made at runDir and
// could not lock, only as the directory root holds and only empty, and
// closes root, answering what failed.
func abandonRun(runDir string, root *os.Root) error {
	var errs []error
	if held, err := root.Stat("."); err != nil {
		errs = append(errs, fmt.Errorf("browser: read the download staging folder %s: %w", runDir, err))
	} else if _, err := removeHeldDir(runDir, held); err != nil {
		errs = append(errs, fmt.Errorf("browser: remove the download staging folder %s: %w", runDir, err))
	}
	if err := root.Close(); err != nil {
		errs = append(errs, fmt.Errorf("browser: close the download staging folder %s: %w", runDir, err))
	}
	return errors.Join(errs...)
}

// removeHeldDir removes the directory at path while it is still held, the
// directory as it was found, and answers whether it did: never a file,
// which rmdir refuses, never a directory with anything in it, and never
// another directory put at the name since the identity was taken, which is
// compared first. A directory swapped in between that comparison and the
// rmdir is removed only if it is empty, so nothing put there is lost.
func removeHeldDir(path string, held os.FileInfo) (bool, error) {
	current, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !current.IsDir() || !os.SameFile(held, current) {
		return false, nil
	}
	err = syscall.Rmdir(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		// Gone already, or something in it keeps it.
		return false, nil
	}
	return false, err
}

// inPlace reports whether the run's path still names its directory.
func (r *stagingRun) inPlace() bool {
	held, err := r.root.Stat(".")
	if err != nil {
		return false
	}
	current, err := os.Lstat(r.dir)
	return err == nil && os.SameFile(held, current)
}

// end removes the run's lock file and its directory, through its handle and
// only while its path names it, and lets go of the lock. The caller holds
// staging.mu and the run has no staged file left.
func (r *stagingRun) end() {
	held, heldErr := r.root.Stat(".")
	if err := r.root.Remove(runLockName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the download staging lock in %s: %v", r.dir, err)
	}
	if heldErr != nil {
		log.Printf("browser: read the download staging folder %s: %v", r.dir, heldErr)
	} else if _, err := removeHeldDir(r.dir, held); err != nil {
		log.Printf("browser: remove the download staging folder %s: %v", r.dir, err)
	}
	if err := unlockRun(r.lock); err != nil {
		log.Printf("browser: unlock the download staging folder %s: %v", r.dir, err)
	}
	if err := r.lock.Close(); err != nil {
		log.Printf("browser: close the download staging lock in %s: %v", r.dir, err)
	}
	if err := r.root.Close(); err != nil {
		log.Printf("browser: close the download staging folder %s: %v", r.dir, err)
	}
}

// checkStagingDir verifies dir is a real directory, not a link to one,
// and answers it.
func checkStagingDir(dir string) (os.FileInfo, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("browser: the download staging folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("browser: the download staging folder %s is not a folder (a link or a file is there), so nothing is saved through it", dir)
	}
	return info, nil
}

// stagedFileOf answers the staged file of a running download to place: its
// identity as it is in its held run directory, and that directory, held
// until the download's release; and the Downloads folder the run's staging
// directory is in. It fails when path is no staged file of this process's,
// its path no longer names that file (the run directory was moved or
// replaced), or it is not a regular file.
func stagedFileOf(path string) (placeSource, string, error) {
	staging.mu.Lock()
	run := staging.owned[path]
	staging.mu.Unlock()
	if run == nil {
		return placeSource{}, "", fmt.Errorf("browser: %s is not a staged download of this app", path)
	}
	name := filepath.Base(path)
	held, err := run.root.Lstat(name)
	if err != nil {
		return placeSource{}, "", fmt.Errorf("browser: the downloaded file is gone from %s, which was moved or removed: %w", run.dir, err)
	}
	if !held.Mode().IsRegular() {
		return placeSource{}, "", fmt.Errorf("browser: the download's file %s is not a regular file", path)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(held, current) {
		return placeSource{}, "", fmt.Errorf("browser: the download staging folder %s was moved or replaced, so the download is not saved from it", run.dir)
	}
	return placeSource{path: path, info: held, dir: run.root}, filepath.Dir(filepath.Dir(run.dir)), nil
}

// releaseStaged ends a download's claim on its staged file: the file and
// its sidecars (stagedSidecars) go if they are still there (a canceled,
// failed or unpublished download's), removed through the run directory's
// handle, and the run directory, and the staging directory with it once
// empty, go once no download of this process stages in the run.
func releaseStaged(path string) {
	staging.mu.Lock()
	defer staging.mu.Unlock()
	run := staging.owned[path]
	if run == nil {
		return
	}
	delete(staging.owned, path)
	name := filepath.Base(path)
	delete(run.files, name)
	for _, staged := range stagedNames(name) {
		if err := run.root.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("browser: remove the staged download %s: %v", filepath.Join(run.dir, staged), err)
		}
	}
	if len(run.files) > 0 {
		return
	}
	stagingDir := filepath.Dir(run.dir)
	if staging.runs[stagingDir] == run {
		delete(staging.runs, stagingDir)
	}
	run.end()
	if staging.runs[stagingDir] == nil {
		// The staging directory the run was made in, only while its path
		// still names it, and only empty: another run's directory keeps it.
		removed, err := removeHeldDir(stagingDir, run.staging)
		if err != nil {
			log.Printf("browser: remove the download staging folder %s: %v", stagingDir, err)
		}
		if removed {
			delete(staging.pruned, stagingDir)
		}
	}
}

// stagedSidecars are the suffixes of the files an engine writes beside a
// staged file while it downloads into it: WebKitGTK's "<staged>.wkdownload".
var stagedSidecars = []string{".wkdownload"}

// stagedNames answers a staged name and its sidecars' names.
func stagedNames(staged string) []string {
	names := []string{staged}
	for _, suffix := range stagedSidecars {
		names = append(names, staged+suffix)
	}
	return names
}

// stagedName reports whether name is a staged name in a run directory
// (stageDownload) or one of its sidecars', exactly.
func stagedName(name string) bool {
	if stagedNamePattern.MatchString(name) {
		return true
	}
	for _, suffix := range stagedSidecars {
		if staged, ok := strings.CutSuffix(name, suffix); ok && stagedNamePattern.MatchString(staged) {
			return true
		}
	}
	return false
}

// stagedNamePattern matches the staged names a run directory holds.
var stagedNamePattern = regexp.MustCompile(`^dl-[0-9]+$`)

// runDirPattern matches the run directories processes stage in.
var runDirPattern = regexp.MustCompile(`^run-[0-9a-f]{12}(?:-[0-9]+)?$`)

// pruneStagingLocked removes what ended runs left in the staging directory
// dir: in each run directory, not this process's, whose lock it takes,
// the staged files and their sidecars (stagedName) and the private names
// of publishes the run did not finish (placingNamePattern), then the lock
// file and the directory if nothing else is in it. A run whose lock
// another process holds, one with no lock file, and anything not a run
// directory stay. The caller holds staging.mu.
func pruneStagingLocked(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("browser: read the download staging folder %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !runDirPattern.MatchString(entry.Name()) {
			continue
		}
		runDir := filepath.Join(dir, entry.Name())
		if run := staging.runs[dir]; run != nil && run.dir == runDir {
			continue
		}
		pruneEndedRun(runDir)
	}
}

// pruneEndedRun is pruneStagingLocked for one run directory. A run
// directory gone meanwhile, one with no lock file and one whose lock
// another process holds are expected and left; every other failure is
// logged, and the run stays as far as the prune got.
func pruneEndedRun(runDir string) {
	root, err := os.OpenRoot(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("browser: open the ended download staging folder %s: %v", runDir, err)
		return
	}
	defer func() {
		if err := root.Close(); err != nil {
			log.Printf("browser: close the ended download staging folder %s: %v", runDir, err)
		}
	}()
	lock, err := root.OpenFile(runLockName, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		// No lock file: no evidence the run ended.
		return
	}
	if err != nil {
		log.Printf("browser: open the ended run's lock in %s: %v", runDir, err)
		return
	}
	defer func() {
		if err := lock.Close(); err != nil {
			log.Printf("browser: close the ended run's lock in %s: %v", runDir, err)
		}
	}()
	locked, err := lockRun(lock)
	if err != nil {
		log.Printf("browser: lock the ended download staging folder %s: %v", runDir, err)
		return
	}
	if !locked {
		// Its process still runs.
		return
	}
	defer func() {
		if err := unlockRun(lock); err != nil {
			log.Printf("browser: unlock the ended download staging folder %s: %v", runDir, err)
		}
	}()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		log.Printf("browser: read the ended download staging folder %s: %v", runDir, err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !(stagedName(name) || placingNamePattern.MatchString(name)) {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("browser: remove the ended run's staged download %s: %v", filepath.Join(runDir, name), err)
		}
	}
	if err := root.Remove(runLockName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the ended run's lock in %s: %v", runDir, err)
		return
	}
	// The run directory it opened, only empty: a file not AO's keeps it.
	held, err := root.Stat(".")
	if err == nil {
		_, err = removeHeldDir(runDir, held)
	}
	if err != nil {
		log.Printf("browser: remove the ended download staging folder %s: %v", runDir, err)
	}
}
