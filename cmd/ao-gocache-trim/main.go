// Command ao-gocache-trim caps the size of the Go build cache. Go trims
// its cache only by age (entries unused for five days), so a cache shared
// by many worktrees and build tags grows until the disk is full. The Make
// Go targets run a pass before their build or test and another after it.
//
//	ao-gocache-trim [-warn] [cache-dir]
//
// At most once per checkInterval (a stamp file in the cache records the
// last walk) a pass sums the cache's entries and, when they exceed the cap,
// deletes the least recently used until they fit in trimTarget of it. Go
// refreshes an entry's mtime when it uses one more than an hour old and
// treats a missing entry as a miss, which its own trim relies on too, so
// only entries untouched for an hour are deleted, and only entries: the
// names Go's own trim removes, ending -a or -d in the two-hex-digit
// subdirectories. A linked executable is an entry too, a -d directory
// holding the binary; it counts as its whole tree, ages by the directory's
// mtime, which is the one Go refreshes, and goes whole. The files at the
// cache's top level, README and trim.txt among them, stay. A directory
// without the README Go writes in every cache it opens is refused before
// anything in it is touched, so a wrong GOCACHE or argument deletes nothing.
// When those recent entries alone keep the cache over its cap, the pass
// says so. One pass walks at a time: a pass that finds another holding the
// cache's trim lock leaves the work to it and exits quietly.
//
// The cap is 20 GiB; AO_GOCACHE_MAX_GIB sets another, and 0 turns the trim
// off. It is off in CI (CI set), whose caches are restored per job. The
// cache directory defaults to the one `go env GOCACHE` names.
//
// It exits 2 on an invalid cap or usage and 1 when a pass fails. With
// -warn a failed pass prints its error and exits 0, so a cache problem does
// not fail the build or test it runs beside; an invalid cap still exits 2,
// because it leaves the cache without a cap until someone fixes it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/filelock"
	"agent-overflow/internal/goenvfile"
)

const (
	defaultCapGiB = 20
	// capEnv overrides the cap in GiB; 0 turns the trim off.
	capEnv = "AO_GOCACHE_MAX_GIB"
	// trimTarget is the fraction of the cap a trim brings the cache down
	// to, so the next one is days of builds away rather than one.
	trimTarget = 0.8
	// checkInterval bounds how often the cache is walked. A walk of a
	// 20 GiB cache (about 70,000 files) takes about a second, and parallel
	// sessions add about 10 GiB an hour, so a walk every five minutes costs
	// well under 1% of one core and lets the cache pass its cap by under
	// 1 GiB between walks.
	checkInterval = 5 * time.Minute
	// stampName is the file whose mtime records the last walk.
	stampName = "ao-trim-stamp"
	// lockName is the file a pass holds locked while it walks.
	lockName = "ao-trim-lock"
	// recentAge protects entries Go may be using now: it refreshes a used
	// entry's mtime only when the mtime is over an hour old.
	recentAge = time.Hour
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now()))
}

func run(args []string, stdout, stderr io.Writer, now time.Time) int {
	flags := flag.NewFlagSet("ao-gocache-trim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	warn := flags.Bool("warn", false, "print a failed pass's error and exit 0")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if os.Getenv("CI") != "" {
		return 0
	}
	capBytes, err := capFromEnv(os.Getenv(capEnv))
	if err != nil {
		say(stderr, "ao-gocache-trim:", err)
		return 2
	}
	if capBytes == 0 {
		return 0
	}
	failed := func(err error) int {
		if *warn {
			sayf(stderr, "ao-gocache-trim: %v (the cache was not trimmed)\n", err)
			return 0
		}
		say(stderr, "ao-gocache-trim:", err)
		return 1
	}
	var dir string
	switch flags.NArg() {
	case 0:
		dir, err = goenvfile.CacheDir()
		if err != nil {
			return failed(fmt.Errorf("find the Go build cache: %w", err))
		}
	case 1:
		dir = flags.Arg(0)
	default:
		say(stderr, "usage: ao-gocache-trim [-warn] [cache-dir]")
		return 2
	}
	result, err := trim(dir, capBytes, now)
	if err != nil {
		return failed(err)
	}
	if result.removed > 0 {
		sayf(stdout, "ao-gocache-trim: removed %d least recently used entries (%s) from %s: %s -> %s, cap %s\n",
			result.removed, gib(result.before-result.after), dir, gib(result.before), gib(result.after), gib(capBytes))
	}
	if result.walked && result.after > capBytes {
		sayf(stderr, "ao-gocache-trim: %s holds %s, over its %s cap: %s of it was used within the last hour, which a trim keeps\n",
			dir, gib(result.after), gib(capBytes), gib(result.recent))
	}
	return 0
}

// say writes a line of the tool's output. A failed write to stdout or
// stderr has nowhere better to be reported and leaves the exit status as
// the pass decided it.
func say(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// sayf is say with a format, which ends the line itself.
func sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// capFromEnv parses AO_GOCACHE_MAX_GIB into bytes. It refuses what
// ParseFloat accepts but no cap means: NaN, infinities, a size whose byte
// count does not fit in an int64, and a positive size under one byte,
// which would otherwise leave the trim nothing to keep.
func capFromEnv(value string) (int64, error) {
	if value == "" {
		return defaultCapGiB << 30, nil
	}
	invalid := func(why string) error {
		return fmt.Errorf("%s=%q: %s; want a size in GiB, or 0 to turn the trim off", capEnv, value, why)
	}
	n, err := strconv.ParseFloat(value, 64)
	// Out of a float64's range ParseFloat returns an infinity with
	// ErrRange; below it, a zero.
	if err != nil && !errors.Is(err, strconv.ErrRange) || math.IsNaN(n) {
		return 0, invalid("not a number")
	}
	// The sign of a zero comes from the exact value.
	sign := 0
	if exact, ok := new(big.Float).SetString(value); ok {
		sign = exact.Sign()
	}
	switch {
	case sign < 0 || n < 0:
		return 0, invalid("negative")
	case math.IsInf(n, 1):
		return 0, invalid("too large")
	}
	bytes := n * (1 << 30)
	// float64(math.MaxInt64) is 2^63, one past the largest int64.
	if bytes >= float64(math.MaxInt64) {
		return 0, invalid("too large")
	}
	capBytes := int64(bytes)
	if capBytes == 0 && sign > 0 {
		return 0, invalid("under one byte")
	}
	return capBytes, nil
}

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// entry is one cache entry: a file, or an executable's directory (tree).
type entry struct {
	path  string
	size  int64
	mtime time.Time
	tree  bool
}

type trimResult struct {
	walked        bool
	removed       int
	before, after int64
	// recent is the size of the entries used within recentAge, which no
	// trim removes.
	recent int64
}

// trim walks dir unless its stamp is younger than checkInterval or another
// pass holds the trim lock, and when the entries exceed capBytes deletes
// the oldest until they fit in trimTarget of it. A deletion that fails is
// reported once the walk is done.
func trim(dir string, capBytes int64, now time.Time) (trimResult, error) {
	var result trimResult
	stamp := filepath.Join(dir, stampName)
	if fresh, err := stampFresh(stamp, now); fresh || err != nil {
		return result, err
	}
	if _, err := os.Stat(dir); err != nil {
		// No cache yet: nothing to trim, and no stamp to write.
		if errors.Is(err, fs.ErrNotExist) {
			return result, nil
		}
		return result, err
	}
	if err := verifyGoCache(dir); err != nil {
		return result, err
	}
	return walkUnderLock(dir, stamp, capBytes, now)
}

// walkUnderLock is the pass of a trim that found the stamp stale: it walks
// under the trim lock unless the stamp is fresh by then, and leaves the
// work to a pass that holds the lock.
func walkUnderLock(dir, stamp string, capBytes int64, now time.Time) (trimResult, error) {
	var result trimResult
	lock, err := filelock.Open(filepath.Join(dir, lockName))
	if err != nil {
		return result, fmt.Errorf("open the trim lock: %w", err)
	}
	locked, err := filelock.TryLock(lock)
	if err != nil {
		return result, errors.Join(fmt.Errorf("take the trim lock: %w", err), lock.Close())
	}
	if !locked {
		return result, lock.Close() // another pass is walking
	}
	// A pass that walked and released between this one's read of the stamp
	// and its lock wrote the stamp under the lock.
	fresh, err := stampFresh(stamp, now)
	if err == nil && !fresh {
		err = walkAndTrim(dir, stamp, capBytes, now, &result)
	}
	return result, errors.Join(err, filelock.Release(lock))
}

// goCacheMarker opens the README the go command writes in every build
// cache it opens (cacheREADME in cmd/go/internal/cache); every Go release
// since the cache began starts it with this line.
const goCacheMarker = "This directory holds cached build artifacts from the Go build system.\n"

// verifyGoCache refuses dir unless its README starts with goCacheMarker.
func verifyGoCache(dir string) error {
	readme, err := os.Open(filepath.Join(dir, "README"))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is not a Go build cache (it has no README from the go command), so nothing was removed", dir)
	}
	if err != nil {
		return fmt.Errorf("check that %s is a Go build cache: %w", dir, err)
	}
	head := make([]byte, len(goCacheMarker))
	n, err := io.ReadFull(readme, head)
	if closeErr := readme.Close(); err == nil {
		err = closeErr
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("check that %s is a Go build cache: %w", dir, err)
	}
	if string(head[:n]) != goCacheMarker {
		return fmt.Errorf("%s is not a Go build cache (its README is not the go command's), so nothing was removed", dir)
	}
	return nil
}

func stampFresh(stamp string, now time.Time) (bool, error) {
	info, err := os.Stat(stamp)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the trim stamp: %w", err)
	}
	return now.Sub(info.ModTime()) < checkInterval, nil
}

func walkAndTrim(dir, stamp string, capBytes int64, now time.Time, result *trimResult) error {
	if err := touch(stamp, now); err != nil {
		return fmt.Errorf("write the trim stamp: %w", err)
	}
	entries, total, err := cacheEntries(dir)
	if err != nil {
		return err
	}
	result.walked = true
	result.before, result.after = total, total
	for _, e := range entries {
		if now.Sub(e.mtime) < recentAge {
			result.recent += e.size
		}
	}
	failures := 0
	var firstFailure error
	for _, victim := range selectVictims(entries, total, capBytes, now) {
		remove := os.Remove
		if victim.tree {
			remove = os.RemoveAll
		}
		if err := remove(victim.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures++
			if firstFailure == nil {
				firstFailure = err
			}
			continue
		}
		result.removed++
		result.after -= victim.size
	}
	if failures > 0 {
		return fmt.Errorf("%d entries could not be removed, the first: %w", failures, firstFailure)
	}
	return nil
}

func touch(path string, now time.Time) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chtimes(path, now, now)
}

// cacheEntries lists the entries in the cache's two-hex-digit
// subdirectories, where Go keeps every one, and their total size.
func cacheEntries(dir string) ([]entry, int64, error) {
	subdirs, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var entries []entry
	var total int64
	for _, sub := range subdirs {
		if !sub.IsDir() || !isEntryDir(sub.Name()) {
			continue
		}
		subdir := filepath.Join(dir, sub.Name())
		files, err := os.ReadDir(subdir)
		if err != nil {
			return nil, 0, err
		}
		for _, file := range files {
			name := file.Name()
			if !strings.HasSuffix(name, "-a") && !strings.HasSuffix(name, "-d") {
				continue
			}
			e, ok, err := cacheEntry(filepath.Join(subdir, name), file)
			if err != nil {
				return nil, 0, err
			}
			if ok {
				entries = append(entries, e)
				total += e.size
			}
		}
	}
	return entries, total, nil
}

// cacheEntry describes the entry at path: a regular file, or a directory
// whose files make up its size. ok is false for anything else and for an
// entry trimmed by Go or another run since the listing.
func cacheEntry(path string, file fs.DirEntry) (e entry, ok bool, err error) {
	if !file.Type().IsRegular() && !file.IsDir() {
		return entry{}, false, nil
	}
	info, err := file.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return entry{}, false, nil
	}
	if err != nil {
		return entry{}, false, err
	}
	e = entry{path: path, size: info.Size(), mtime: info.ModTime(), tree: info.IsDir()}
	if !e.tree {
		return e, true, nil
	}
	e.size = 0
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		e.size += info.Size()
		return nil
	})
	if err != nil {
		return entry{}, false, err
	}
	return e, true, nil
}

func isEntryDir(name string) bool {
	if len(name) != 2 {
		return false
	}
	_, err := strconv.ParseUint(name, 16, 8)
	return err == nil
}

// selectVictims returns the entries to delete, oldest first, to bring total
// to trimTarget of capBytes: none while total is within the cap, and never
// one used within recentAge.
func selectVictims(entries []entry, total, capBytes int64, now time.Time) []entry {
	if total <= capBytes {
		return nil
	}
	target := int64(float64(capBytes) * trimTarget)
	sorted := append([]entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].mtime.Before(sorted[j].mtime) })
	var victims []entry
	for _, e := range sorted {
		if total <= target || now.Sub(e.mtime) < recentAge {
			break
		}
		victims = append(victims, e)
		total -= e.size
	}
	return victims
}
