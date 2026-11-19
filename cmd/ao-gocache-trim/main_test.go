package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/filelock"
)

var testNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

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
	dir := t.TempDir()
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
	dir := t.TempDir()
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

// TestProtectedFilesSurvive: the cache's top-level files and entries used
// within the hour stay even when the cap cannot be met without them.
func TestProtectedFilesSurvive(t *testing.T) {
	dir := t.TempDir()
	readme := writeEntry(t, dir, "README", 500, 365*24*time.Hour)
	trimTxt := writeEntry(t, dir, "trim.txt", 500, 365*24*time.Hour)
	other := writeEntry(t, dir, "notes/old", 500, 365*24*time.Hour)
	stale := writeEntry(t, dir, "00/stale-a", 500, 7*24*time.Hour)
	recent := writeEntry(t, dir, "01/recent-d", 2000, 30*time.Minute)
	result, err := trim(dir, 1000, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.removed != 1 || exists(t, stale) {
		t.Fatalf("result = %+v, want only the stale entry removed", result)
	}
	for _, path := range []string{readme, trimTxt, other, recent} {
		if !exists(t, path) {
			t.Fatalf("%s was removed", path)
		}
	}
}

func TestRecentStampSkipsTheWalk(t *testing.T) {
	dir := t.TempDir()
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

func TestCapFromEnv(t *testing.T) {
	for value, want := range map[string]int64{"": 20 << 30, "0": 0, "5": 5 << 30, "0.5": 1 << 29} {
		if got, err := capFromEnv(value); err != nil || got != want {
			t.Fatalf("capFromEnv(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"-1", "lots"} {
		if _, err := capFromEnv(value); err == nil {
			t.Fatalf("capFromEnv(%q) accepted", value)
		}
	}
}

func TestRunIsOffInCIAndReportsRemovals(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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

	dir = t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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

// TestGoCacheDirAgreesWithTheGoCommand: the cache directory resolved
// without starting the go command is the one it names, from the
// environment, the Go environment file or the user cache directory.
func TestGoCacheDirAgreesWithTheGoCommand(t *testing.T) {
	root := t.TempDir()
	withCache := filepath.Join(root, "with-cache")
	if err := os.WriteFile(withCache, []byte("GOCACHE="+filepath.Join(root, "from-file")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	without := filepath.Join(root, "without")
	if err := os.WriteFile(without, []byte("GOPROXY=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg"))
	t.Setenv("HOME", root)
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, gocache, goenv, want string }{
		{"environment", filepath.Join(root, "from-env"), withCache, filepath.Join(root, "from-env")},
		{"Go environment file", "", withCache, filepath.Join(root, "from-file")},
		{"user cache directory", "", without, filepath.Join(userCache, "go-build")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOCACHE", tc.gocache)
			t.Setenv("GOENV", tc.goenv)
			got, err := goCacheDir()
			if err != nil || got != tc.want {
				t.Fatalf("goCacheDir = %q, %v; want %q", got, err, tc.want)
			}
			out, err := exec.Command("go", "env", "GOCACHE").Output()
			if err != nil {
				t.Fatal(err)
			}
			if goSays := strings.TrimSpace(string(out)); goSays != got {
				t.Fatalf("go env GOCACHE = %q, goCacheDir = %q", goSays, got)
			}
		})
	}
	t.Setenv("GOCACHE", "off")
	if dir, err := goCacheDir(); err == nil {
		t.Fatalf("goCacheDir = %q with GOCACHE=off", dir)
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
