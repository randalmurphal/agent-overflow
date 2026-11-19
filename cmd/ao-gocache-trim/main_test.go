package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
