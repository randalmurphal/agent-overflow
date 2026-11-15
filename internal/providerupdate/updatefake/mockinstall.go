package updatefake

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// MockInstall is the provider CLI install ao-mockprovider simulates. Its
// state is two files under the home directory, which every mock process
// inherits, including the app's `--version` probe that carries no harness
// environment:
//
//   - installed-version: the version an update installed. Absent means
//     Baseline.
//   - update-target: the version an update installs, set by the harness
//     with the version it advertises. Absent means none is advertised and
//     an update changes nothing.
//
// Every change of the installed version moves the executable's mtime
// forward, so the app's binary watch sees a new binary and reads its
// version again.
type MockInstall struct {
	Home       string
	Executable string
}

const (
	mockStateDir      = ".ao-mockprovider"
	mockInstalledFile = "installed-version"
	mockTargetFile    = "update-target"
)

// Installed is the installed version. A record it cannot read or that is
// not a catalogue version is returned as an error beside Baseline.
func (m MockInstall) Installed() (string, error) {
	version, err := m.read(mockInstalledFile)
	if version == "" {
		return Baseline, err
	}
	return version, nil
}

// Target is the version an update installs, "" when none is advertised.
func (m MockInstall) Target() (string, error) {
	return m.read(mockTargetFile)
}

// SetTarget makes version, one of Stable, what an update installs. ""
// advertises none.
func (m MockInstall) SetTarget(version string) error {
	if version == "" {
		_, err := m.remove(mockTargetFile)
		return err
	}
	if !slices.Contains(Stable, version) {
		return fmt.Errorf("updatefake: update target must be one of %v, got %q", Stable, version)
	}
	return m.write(mockTargetFile, version)
}

// Install records version as installed. The executable's mtime moves
// before the record is published by rename, so a failure leaves no record
// claiming a version whose binary identity never changed.
func (m MockInstall) Install(version string) error {
	if !slices.Contains(Stable, version) {
		return fmt.Errorf("updatefake: installed version must be one of %v, got %q", Stable, version)
	}
	return m.write(mockInstalledFile, version)
}

// Reset advertises no update and restores Baseline as installed, moving
// the executable's mtime when an install was recorded. changed reports
// whether either was set.
func (m MockInstall) Reset() (changed bool, err error) {
	hadTarget, err := m.remove(mockTargetFile)
	if err != nil {
		return false, err
	}
	hadInstall, err := m.remove(mockInstalledFile)
	if err != nil {
		return hadTarget, err
	}
	if hadInstall {
		if err := m.touch(); err != nil {
			return true, err
		}
	}
	return hadTarget || hadInstall, nil
}

func (m MockInstall) path(name string) (string, error) {
	if m.Home == "" {
		return "", errors.New("updatefake: the mock install has no home directory")
	}
	return filepath.Join(m.Home, mockStateDir, name), nil
}

// read returns the version in the named file, "" when it does not exist.
func (m MockInstall) read(name string) (string, error) {
	path, err := m.path(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	version := strings.TrimSpace(string(data))
	if !slices.Contains(Stable, version) {
		return "", fmt.Errorf("%s holds %q, not a version of the catalogue %v", path, version, Stable)
	}
	return version, nil
}

// write publishes version to the named file by rename. The installed
// version moves the executable's mtime first.
func (m MockInstall) write(name, version string) error {
	path, err := m.path(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.WriteString(version + "\n")
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", tmpPath, err), removeTemp(tmpPath))
	}
	if name == mockInstalledFile {
		if err := m.touch(); err != nil {
			return errors.Join(err, removeTemp(tmpPath))
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.Join(fmt.Errorf("publish %s: %w", path, err), removeTemp(tmpPath))
	}
	return nil
}

// remove deletes the named file and reports whether it existed.
func (m MockInstall) remove(name string) (bool, error) {
	path, err := m.path(name)
	if err != nil {
		return false, err
	}
	err = os.Remove(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("remove %s: %w", path, err)
	}
	return true, nil
}

func removeTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// touch moves the resolved executable's mtime strictly forward, even for
// a file stamped ahead of the clock.
func (m MockInstall) touch() error {
	if m.Executable == "" {
		return errors.New("updatefake: the mock install has no executable")
	}
	exe, err := filepath.EvalSymlinks(m.Executable)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	now := time.Now()
	mtime := now
	if !mtime.After(info.ModTime()) {
		mtime = info.ModTime().Add(time.Second)
	}
	if err := os.Chtimes(exe, now, mtime); err != nil {
		return fmt.Errorf("change executable mtime: %w", err)
	}
	return nil
}
