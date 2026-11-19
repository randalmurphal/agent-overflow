// Command ao-gocache-trim caps the size of the Go build cache. Go trims
// its cache only by age (entries unused for five days), so a cache shared
// by many worktrees and build tags grows until the disk is full. The Make
// Go targets run this tool first.
//
//	ao-gocache-trim [cache-dir]
//
// At most once per checkInterval (a stamp file in the cache records the
// last walk) it sums the cache's entries and, when they exceed the cap,
// deletes the least recently used until they fit in trimTarget of it. Go
// refreshes an entry's mtime when it uses one more than an hour old and
// treats a missing entry as a miss, which its own trim relies on too, so
// only entries untouched for an hour are deleted, and only entries: the
// files at the cache's top level, README and trim.txt among them, stay.
//
// The cap is 20 GiB; AO_GOCACHE_MAX_GIB sets another, and 0 turns the trim
// off. It is off in CI (CI set), whose caches are restored per job. The
// cache directory defaults to `go env GOCACHE`.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCapGiB = 20
	// capEnv overrides the cap in GiB; 0 turns the trim off.
	capEnv = "AO_GOCACHE_MAX_GIB"
	// trimTarget is the fraction of the cap a trim brings the cache down
	// to, so the next one is days of builds away rather than one.
	trimTarget = 0.8
	// checkInterval bounds how often the cache is walked.
	checkInterval = 4 * time.Hour
	// stampName is the file whose mtime records the last walk.
	stampName = "ao-trim-stamp"
	// recentAge protects entries Go may be using now: it refreshes a used
	// entry's mtime only when the mtime is over an hour old.
	recentAge = time.Hour
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now()))
}

func run(args []string, stdout, stderr io.Writer, now time.Time) int {
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
	var dir string
	switch len(args) {
	case 0:
		dir, err = goCacheDir()
		if err != nil {
			fmt.Fprintln(stderr, "ao-gocache-trim: find the Go build cache:", err)
			return 1
		}
	case 1:
		dir = args[0]
	default:
		fmt.Fprintln(stderr, "usage: ao-gocache-trim [cache-dir]")
		return 2
	}
	result, err := trim(dir, capBytes, now)
	if err != nil {
		fmt.Fprintln(stderr, "ao-gocache-trim:", err)
		return 1
	}
	if result.removed > 0 {
		fmt.Fprintf(stdout, "ao-gocache-trim: removed %d least recently used entries (%s) from %s: %s -> %s, cap %s\n",
			result.removed, gib(result.before-result.after), dir, gib(result.before), gib(result.after), gib(capBytes))
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

func goCacheDir() (string, error) {
	out, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || dir == "off" {
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
	walked          bool
	removed         int
	before, after   int64
	removeFailures  int
	firstRemoveFail error
}

// trim walks dir unless its stamp is younger than checkInterval, and when
// the entries exceed capBytes deletes the oldest until they fit in
// trimTarget of it. A deletion that fails is reported once the walk is done.
func trim(dir string, capBytes int64, now time.Time) (trimResult, error) {
	var result trimResult
	stamp := filepath.Join(dir, stampName)
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < checkInterval {
		return result, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result, fmt.Errorf("read the trim stamp: %w", err)
	}
	if _, err := os.Stat(dir); err != nil {
		// No cache yet: nothing to trim, and no stamp to write.
		if errors.Is(err, fs.ErrNotExist) {
			return result, nil
		}
		return result, err
	}
	// Stamp first, so a concurrent run skips the walk this one makes.
	if err := touch(stamp, now); err != nil {
		return result, fmt.Errorf("write the trim stamp: %w", err)
	}
	entries, total, err := cacheEntries(dir)
	if err != nil {
		return result, err
	}
	result.walked = true
	result.before, result.after = total, total
	for _, victim := range selectVictims(entries, total, capBytes, now) {
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result.removeFailures++
			if result.firstRemoveFail == nil {
				result.firstRemoveFail = err
			}
			continue
		}
		result.removed++
		result.after -= victim.size
	}
	if result.removeFailures > 0 {
		return result, fmt.Errorf("%d entries could not be removed, the first: %w", result.removeFailures, result.firstRemoveFail)
	}
	return result, nil
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
