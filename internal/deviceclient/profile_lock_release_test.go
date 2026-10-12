package deviceclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A release that cannot unlock or close reports it: the lock is the only
// thing standing between two processes writing one profile file, so a
// release failure is the caller's error, not a silent one.
func TestReleaseProfileReportsAFailedUnlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.lock")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = releaseProfile(f)
	if err == nil {
		t.Fatal("releasing a closed descriptor reported no error")
	}
	if !strings.Contains(err.Error(), "unlock "+path) || !strings.Contains(err.Error(), "close "+path) {
		t.Fatalf("error does not name both failures: %v", err)
	}
}

var errReleaseRefused = errors.New("release refused")

// failProfileLockRelease makes the release of every profile lock whose
// file name contains match fail with errReleaseRefused after releasing it,
// for the rest of the test. Tests that use it must not be parallel.
func failProfileLockRelease(t *testing.T, match string) {
	t.Helper()
	saved := releaseProfileLock
	releaseProfileLock = func(f *os.File) error {
		err := releaseProfile(f)
		if strings.Contains(filepath.Base(f.Name()), match) {
			return errors.Join(err, errReleaseRefused)
		}
		return err
	}
	t.Cleanup(func() { releaseProfileLock = saved })
}

// Every operation that takes a profile lock reports a failed release as
// its own error, including the ones whose work succeeded.
func TestProfileLockCallersReportAFailedRelease(t *testing.T) {
	t.Run("WithProfileLock", func(t *testing.T) {
		dir := t.TempDir()
		failProfileLockRelease(t, "catalog")
		ran := false
		err := WithProfileLock(t.Context(), dir, "catalog", func() error { ran = true; return nil })
		if !ran || !errors.Is(err, errReleaseRefused) {
			t.Fatalf("WithProfileLock = %v, ran %v", err, ran)
		}
	})
	t.Run("EnrollDeviceKey", func(t *testing.T) {
		dir := t.TempDir()
		failProfileLockRelease(t, KeyFileName)
		key, err := EnrollDeviceKey(dir)
		if key != nil || !errors.Is(err, errReleaseRefused) {
			t.Fatalf("EnrollDeviceKey = %v, %v", key, err)
		}
		if _, err := DeviceKey(dir); err != nil {
			t.Fatalf("the key was not persisted: %v", err)
		}
	})
	t.Run("SaveSession", func(t *testing.T) {
		dir := t.TempDir()
		failProfileLockRelease(t, "backend-a")
		err := SaveSession(dir, Session{BackendID: "backend-a", SessionID: "s", Credential: "c"})
		if !errors.Is(err, errReleaseRefused) {
			t.Fatalf("SaveSession = %v", err)
		}
	})
	t.Run("ForgetSession", func(t *testing.T) {
		dir := t.TempDir()
		failProfileLockRelease(t, "backend-a")
		if err := ForgetSession(dir, "backend-a"); !errors.Is(err, errReleaseRefused) {
			t.Fatalf("ForgetSession = %v", err)
		}
	})
}
