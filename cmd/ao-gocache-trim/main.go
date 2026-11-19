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
// files at the cache's top level, README and trim.txt among them, stay.
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
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
		fmt.Fprintln(stderr, "ao-gocache-trim:", err)
		return 2
	}
	if capBytes == 0 {
		return 0
	}
	failed := func(err error) int {
		if *warn {
			fmt.Fprintf(stderr, "ao-gocache-trim: %v (the cache was not trimmed)\n", err)
			return 0
		}
		fmt.Fprintln(stderr, "ao-gocache-trim:", err)
		return 1
	}
	var dir string
	switch flags.NArg() {
	case 0:
		dir, err = goCacheDir()
		if err != nil {
			return failed(fmt.Errorf("find the Go build cache: %w", err))
		}
	case 1:
		dir = flags.Arg(0)
	default:
		fmt.Fprintln(stderr, "usage: ao-gocache-trim [-warn] [cache-dir]")
		return 2
	}
	result, err := trim(dir, capBytes, now)
	if err != nil {
		return failed(err)
	}
	if result.removed > 0 {
		fmt.Fprintf(stdout, "ao-gocache-trim: removed %d least recently used entries (%s) from %s: %s -> %s, cap %s\n",
			result.removed, gib(result.before-result.after), dir, gib(result.before), gib(result.after), gib(capBytes))
	}
	if result.walked && result.after > capBytes {
		fmt.Fprintf(stderr, "ao-gocache-trim: %s holds %s, over its %s cap: %s of it was used within the last hour, which a trim keeps\n",
			dir, gib(result.after), gib(capBytes), gib(result.recent))
	}
	return 0
}

func capFromEnv(value string) (int64, error) {
	if value == "" {
		return defaultCapGiB << 30, nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: want a size in GiB, or 0 to turn the trim off", capEnv, value)
	}
	return int64(n * (1 << 30)), nil
}

// goCacheDir resolves GOCACHE as the go command does, without the quarter
// second that starting it costs every pass: the environment, then the Go
// environment file, then go-build in the user cache directory.
func goCacheDir() (string, error) {
	dir := os.Getenv("GOCACHE")
	if dir == "" {
		data, err := goenvfile.Read()
		if err != nil {
			return "", err
		}
		dir = goenvfile.Lookup(data, "GOCACHE")
	}
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("GOCACHE is not set and %w", err)
		}
		return filepath.Join(cache, "go-build"), nil
	}
	if dir == "off" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("GOCACHE is %q", dir)
	}
	return dir, nil
}

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

type entry struct {
	path  string
	size  int64
	mtime time.Time
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
	err = walkAndTrim(dir, stamp, capBytes, now, &result)
	return result, errors.Join(err, filelock.Release(lock))
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
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
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

// cacheEntries lists the files in the cache's two-hex-digit subdirectories,
// where Go keeps every entry, and their total size.
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
			if !file.Type().IsRegular() {
				continue
			}
			info, err := file.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // trimmed by Go or another run since the listing
			}
			if err != nil {
				return nil, 0, err
			}
			entries = append(entries, entry{path: filepath.Join(subdir, file.Name()), size: info.Size(), mtime: info.ModTime()})
			total += info.Size()
		}
	}
	return entries, total, nil
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
