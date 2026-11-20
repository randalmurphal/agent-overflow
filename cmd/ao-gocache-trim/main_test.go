package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/filelock"
)

var testNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// newCache makes a directory the trim accepts as a Go build cache: it
// holds the README the go command writes.
func newCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	readme := goCacheMarker + "Run \"go clean -cache\" if the directory is getting too large.\n"
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeEntry writes a size-byte file at rel under dir, last modified age
// before testNow.
func writeEntry(t *testing.T, dir, rel string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := testNow.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestUnderCapDeletesNothing(t *testing.T) {
	dir := newCache(t)
	a := writeEntry(t, dir, "00/a-a", 400, 30*24*time.Hour)
	b := writeEntry(t, dir, "ff/b-d", 500, 10*24*time.Hour)
	result, err := trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !result.walked || result.removed != 0 || result.before != 900 {
		t.Fatalf("result = %+v, want a walk of 900 bytes that removes nothing", result)
	}
	if !exists(t, a) || !exists(t, b) {
		t.Fatal("an entry was removed from a cache within its cap")
	}
}

// TestOverCapDeletesOldestFirstToTheTarget: 1500 bytes over a 1000-byte cap
// lose the oldest entries until at most 800 (trimTarget) remain.
func TestOverCapDeletesOldestFirstToTheTarget(t *testing.T) {
	dir := newCache(t)
	oldest := writeEntry(t, dir, "0a/oldest-d", 300, 40*24*time.Hour)
	older := writeEntry(t, dir, "1b/older-a", 300, 30*24*time.Hour)
	old := writeEntry(t, dir, "2c/old-d", 300, 20*24*time.Hour)
	newer := writeEntry(t, dir, "3d/newer-a", 300, 10*24*time.Hour)
	newest := writeEntry(t, dir, "4e/newest-d", 300, 2*time.Hour)
	result, err := trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.removed != 3 || result.after != 600 {
		t.Fatalf("result = %+v, want 3 removed leaving 600 bytes", result)
	}
	for path, want := range map[string]bool{oldest: false, older: false, old: false, newer: true, newest: true} {
		if exists(t, path) != want {
			t.Fatalf("%s exists = %v, want %v", filepath.Base(path), !want, want)
		}
	}
}

// TestProtectedFilesSurvive: the cache's top-level files, names in its
// subdirectories that are not entries (Go's trim leaves them too) and
// entries used within the hour stay even when the cap cannot be met
// without them.
func TestProtectedFilesSurvive(t *testing.T) {
	dir := newCache(t)
	readme := filepath.Join(dir, "README")
	trimTxt := writeEntry(t, dir, "trim.txt", 500, 365*24*time.Hour)
	other := writeEntry(t, dir, "notes/old", 500, 365*24*time.Hour)
	notEntry := writeEntry(t, dir, "00/scratch", 500, 365*24*time.Hour)
	stale := writeEntry(t, dir, "00/stale-a", 500, 7*24*time.Hour)
	recent := writeEntry(t, dir, "01/recent-d", 2000, 30*time.Minute)
	result, err := trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.removed != 1 || exists(t, stale) || result.before != 2500 {
		t.Fatalf("result = %+v, want 2500 bytes of entries and only the stale one removed", result)
	}
	for _, path := range []string{readme, trimTxt, other, notEntry, recent} {
		if !exists(t, path) {
			t.Fatalf("%s was removed", path)
		}
	}
}

func TestRecentStampSkipsTheWalk(t *testing.T) {
	dir := newCache(t)
	victim := writeEntry(t, dir, "00/old-a", 5000, 30*24*time.Hour)
	writeEntry(t, dir, stampName, 0, checkInterval-time.Minute)
	result, err := trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.walked || !exists(t, victim) {
		t.Fatalf("result = %+v, want no walk within the interval", result)
	}

	// Once the interval has passed the walk runs and restamps.
	stamp := writeEntry(t, dir, stampName, 0, checkInterval+time.Minute)
	result, err = trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !result.walked || exists(t, victim) {
		t.Fatalf("result = %+v, want a walk that removes the old entry", result)
	}
	info, err := os.Stat(stamp)
	if err != nil || !info.ModTime().Equal(testNow) {
		t.Fatalf("stamp after the walk: %v %v, want mtime %v", info, err, testNow)
	}
}

func TestMissingCacheIsNotAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-cache")
	result, err := trim(dir, 1000, testNow)
	if err != nil || result.walked {
		t.Fatalf("trim of a missing cache = %+v, %v", result, err)
	}
	if exists(t, dir) {
		t.Fatal("trim created the cache directory")
	}
}

// TestCapFromEnv: a cap is a finite, non-negative size whose byte count
// fits an int64 and, when positive, is at least one byte; anything else
// is refused with the reason rather than turned into a cap that removes
// every entry.
func TestCapFromEnv(t *testing.T) {
	oneByte := strconv.FormatFloat(1.0/(1<<30), 'g', -1, 64)
	for _, tc := range []struct {
		value string
		want  int64
		why   string // the refusal's reason; empty when the value is valid
	}{
		{value: "", want: 20 << 30},
		{value: "0", want: 0},
		{value: "-0", want: 0},
		{value: "5", want: 5 << 30},
		{value: "0.5", want: 1 << 29},
		{value: oneByte, want: 1},
		{value: "8589934591", want: math.MaxInt64 - (1<<30 - 1)},
		{value: "-1", why: "negative"},
		{value: "-1e-400", why: "negative"},
		{value: "-Inf", why: "negative"},
		{value: "-1e400", why: "negative"},
		{value: "lots", why: "not a number"},
		{value: "NaN", why: "not a number"},
		{value: "nan", why: "not a number"},
		{value: "+Inf", why: "too large"},
		{value: "Infinity", why: "too large"},
		{value: "1e400", why: "too large"},
		{value: "1e300", why: "too large"},
		{value: "8589934592", why: "too large"},
		{value: "1e-10", why: "under one byte"},
		{value: "1e-400", why: "under one byte"},
	} {
		got, err := capFromEnv(tc.value)
		if tc.why == "" {
			if err != nil || got != tc.want {
				t.Fatalf("capFromEnv(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.why) || !strings.Contains(err.Error(), capEnv) {
			t.Fatalf("capFromEnv(%q) = %d, %v; want a refusal naming %s and %q", tc.value, got, err, capEnv, tc.why)
		}
	}
}

// TestDirectoryWithoutTheGoMarkerIsRefused: a directory shaped like a
// cache, whose old entries a trim would remove, is refused and left as it
// was when the go command's README is missing or says something else.
func TestDirectoryWithoutTheGoMarkerIsRefused(t *testing.T) {
	for name, readme := range map[string]string{
		"no README":    "",
		"other README": "This directory holds something else.\n",
		"short README": goCacheMarker[:20],
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if readme != "" {
				if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readme), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var files []string
			for i := range 4 {
				files = append(files, writeEntry(t, dir, fmt.Sprintf("%02x/old%d-a", i, i), 1000, 30*24*time.Hour))
			}
			files = append(files, writeEntry(t, dir, "0a/exe-d/tool", 1000, 30*24*time.Hour))
			result, err := trim(dir, 100, testNow)
			if err == nil || !strings.Contains(err.Error(), "not a Go build cache") || result.walked {
				t.Fatalf("trim = %+v, %v; want a refusal", result, err)
			}
			for _, file := range files {
				if !exists(t, file) {
					t.Fatalf("%s was removed", file)
				}
			}
			for _, name := range []string{stampName, lockName} {
				if exists(t, filepath.Join(dir, name)) {
					t.Fatalf("a refused pass wrote %s", name)
				}
			}
		})
	}
}

// TestExecutableEntriesCountAndGoWhole: an executable entry, a -d
// directory holding the binary, counts as its whole tree, ages by the
// directory's mtime (the one Go refreshes when it uses the entry) and is
// removed whole beside file entries.
func TestExecutableEntriesCountAndGoWhole(t *testing.T) {
	dir := newCache(t)
	// writeExe makes an executable entry of size bytes in two files, the
	// files written long ago and the directory last used age ago.
	writeExe := func(rel string, size int, age time.Duration) string {
		t.Helper()
		writeEntry(t, dir, rel+"/tool", size-100, 90*24*time.Hour)
		writeEntry(t, dir, rel+"/nested/extra", 100, 90*24*time.Hour)
		path := filepath.Join(dir, rel)
		mtime := testNow.Add(-age)
		for _, p := range []string{filepath.Join(path, "nested"), path} {
			if err := os.Chtimes(p, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	oldExe := writeExe("0a/oldexe-d", 3000, 40*24*time.Hour)
	oldFile := writeEntry(t, dir, "1b/oldfile-a", 1000, 30*24*time.Hour)
	// Its files are old but Go used the entry two days ago.
	usedExe := writeExe("2c/usedexe-d", 3000, 2*24*time.Hour)
	recentExe := writeExe("3d/recentexe-d", 2000, 10*time.Minute)
	newFile := writeEntry(t, dir, "4e/newfile-d", 1000, 24*time.Hour)

	result, err := trim(dir, 8000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// 10000 bytes over an 8000-byte cap come down to at most 6400: the old
	// executable and the old file go (6000 left); the executable used two
	// days ago stays though its files are older than both.
	if result.before != 10000 || result.removed != 2 || result.after != 6000 || result.recent != 2000 {
		t.Fatalf("result = %+v, want 10000 bytes, 2 removed leaving 6000, 2000 recent", result)
	}
	for path, want := range map[string]bool{oldExe: false, oldFile: false, usedExe: true, recentExe: true, newFile: true} {
		if exists(t, path) != want {
			t.Fatalf("%s exists = %v, want %v", filepath.Base(path), !want, want)
		}
	}
	if !exists(t, filepath.Join(usedExe, "nested", "extra")) {
		t.Fatal("a kept executable entry lost a file")
	}
}

func TestRunIsOffInCIAndReportsRemovals(t *testing.T) {
	dir := newCache(t)
	victim := writeEntry(t, dir, "00/old-a", 2<<20, 30*24*time.Hour)
	t.Setenv(capEnv, "0.001") // about 1 MiB
	t.Setenv("CI", "true")
	var stdout, stderr bytes.Buffer
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 || !exists(t, victim) {
		t.Fatalf("CI run: exit %d, entry kept %v, stderr %q", code, exists(t, victim), stderr.String())
	}
	t.Setenv("CI", "")
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 || exists(t, victim) {
		t.Fatalf("run: exit %d, entry kept %v, stderr %q", code, exists(t, victim), stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed 1 least recently used") {
		t.Fatalf("stdout = %q, want the removal reported", stdout.String())
	}
}

// TestPassSaysWhenRecentEntriesKeepTheCacheOverItsCap: entries used within
// the hour stay, so a pass can end over the cap; it says so with the size
// and how much of it was protected.
func TestPassSaysWhenRecentEntriesKeepTheCacheOverItsCap(t *testing.T) {
	dir := newCache(t)
	stale := writeEntry(t, dir, "00/stale-a", 1<<20, 7*24*time.Hour)
	writeEntry(t, dir, "01/recent-d", 3<<20, 30*time.Minute)
	writeEntry(t, dir, "02/recent-a", 2<<20, 10*time.Minute)
	result, err := trim(dir, 4<<20, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if exists(t, stale) || result.after != 5<<20 || result.recent != 5<<20 {
		t.Fatalf("result = %+v, want the stale entry removed and 5 MiB of recent entries kept", result)
	}

	dir = newCache(t)
	writeEntry(t, dir, "00/stale-a", 1<<20, 7*24*time.Hour)
	writeEntry(t, dir, "01/recent-d", 300<<20, 30*time.Minute)
	t.Setenv("CI", "")
	t.Setenv(capEnv, "0.2") // 204.8 MiB
	var stdout, stderr bytes.Buffer
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "ao-gocache-trim: " + dir + " holds 0.3 GiB, over its 0.2 GiB cap: 0.3 GiB of it was used within the last hour, which a trim keeps\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	// Within the cap after the trim: nothing to say.
	stderr.Reset()
	if err := os.Remove(filepath.Join(dir, stampName)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(capEnv, "1")
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 || stderr.Len() > 0 {
		t.Fatalf("exit %d, stderr %q; want a quiet pass under the cap", code, stderr.String())
	}
}

// TestAPassLeavesTheWorkToOneHoldingTheLock: a pass that cannot take the
// trim lock exits quietly without walking or stamping.
func TestAPassLeavesTheWorkToOneHoldingTheLock(t *testing.T) {
	dir := newCache(t)
	victim := writeEntry(t, dir, "00/old-a", 5000, 30*24*time.Hour)
	held, err := filelock.Open(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := filelock.TryLock(held); err != nil || !locked {
		t.Fatalf("take the lock: %v %v", locked, err)
	}
	t.Setenv("CI", "")
	t.Setenv(capEnv, "0.000001")
	var stdout, stderr bytes.Buffer
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 || stdout.Len()+stderr.Len() > 0 {
		t.Fatalf("exit %d, output %q %q; want a quiet exit while another pass holds the lock", code, stdout.String(), stderr.String())
	}
	if !exists(t, victim) || exists(t, filepath.Join(dir, stampName)) {
		t.Fatal("a pass without the lock trimmed or stamped the cache")
	}
	if err := filelock.Release(held); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{dir}, &stdout, &stderr, testNow); code != 0 || exists(t, victim) {
		t.Fatalf("exit %d, stderr %q; want the pass after the lock is free to trim", code, stderr.String())
	}
}

// TestConcurrentPassesWalkOnce: of passes started together, as parallel
// sessions start them, one walks; the rest find the stamp it wrote first
// or the lock it holds.
func TestConcurrentPassesWalkOnce(t *testing.T) {
	dir := newCache(t)
	for i := range 200 {
		writeEntry(t, dir, fmt.Sprintf("%02x/e%d-a", i%256, i), 1000, 30*24*time.Hour)
	}
	const passes = 8
	results := make(chan trimResult, passes)
	errs := make(chan error, passes)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range passes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := trim(dir, 100_000, testNow)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	walks, removed := 0, 0
	for result := range results {
		if result.walked {
			walks++
		}
		removed += result.removed
	}
	if walks != 1 || removed != 120 {
		t.Fatalf("%d passes walked and removed %d entries; want one walk removing 120 (200 kB down to 80 kB)", walks, removed)
	}
}

// TestRunTrimsTheCacheTheGoCommandUses: without an argument a pass trims
// the directory GOCACHE resolves to (goenvfile.CacheDir), and fails where
// the go command has no cache.
func TestRunTrimsTheCacheTheGoCommandUses(t *testing.T) {
	dir := newCache(t)
	victim := writeEntry(t, dir, "00/old-a", 2<<20, 30*24*time.Hour)
	t.Setenv("CI", "")
	t.Setenv(capEnv, "0.001")
	t.Setenv("GOENV", "off")
	t.Setenv("GOCACHE", dir)
	var stderr bytes.Buffer
	if code := run(nil, io.Discard, &stderr, testNow); code != 0 || exists(t, victim) {
		t.Fatalf("exit %d, entry kept %v, stderr %q; want the entry trimmed", code, exists(t, victim), stderr.String())
	}
	t.Setenv("GOCACHE", "off")
	stderr.Reset()
	if code := run(nil, io.Discard, &stderr, testNow); code != 1 || !strings.Contains(stderr.String(), "GOCACHE=off") {
		t.Fatalf("exit %d, stderr %q; want a failed pass naming GOCACHE=off", code, stderr.String())
	}
}

// TestWarnKeepsAFailedPassFromFailingTheCaller: with -warn a failed pass
// prints its error and exits 0; an invalid cap still fails.
func TestWarnKeepsAFailedPassFromFailingTheCaller(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CI", "")
	t.Setenv(capEnv, "")
	var stderr bytes.Buffer
	if code := run([]string{notADir}, io.Discard, &stderr, testNow); code != 1 || !strings.Contains(stderr.String(), "trim stamp") {
		t.Fatalf("failed pass: exit %d, stderr %q; want 1 and the error", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-warn", notADir}, io.Discard, &stderr, testNow); code != 0 || !strings.Contains(stderr.String(), "(the cache was not trimmed)") {
		t.Fatalf("failed pass with -warn: exit %d, stderr %q; want 0 and the error", code, stderr.String())
	}
	t.Setenv(capEnv, "bad")
	stderr.Reset()
	if code := run([]string{"-warn", t.TempDir()}, io.Discard, &stderr, testNow); code != 2 || !strings.Contains(stderr.String(), capEnv) {
		t.Fatalf("invalid cap with -warn: exit %d, stderr %q; want 2 and the reason", code, stderr.String())
	}
}
