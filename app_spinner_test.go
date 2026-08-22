package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/spinner"
)

// newSpinnerTestApp builds the narrowest App that can answer the spinner
// binding: a config dir and nothing else. The spinners surface touches no
// store, no provider, and no process — and it must never resolve the
// developer's real config dir, which is what configDir being set to
// t.TempDir() guarantees.
func newSpinnerTestApp(t *testing.T) (*App, string) {
	t.Helper()
	configDir := t.TempDir()
	return &App{configDir: configDir}, configDir
}

func TestGetSpinnerFilesListsUserSprites(t *testing.T) {
	app, configDir := newSpinnerTestApp(t)

	files, err := app.GetSpinnerFiles()
	if err != nil {
		t.Fatalf("GetSpinnerFiles: %v", err)
	}
	if files.Dir != filepath.Join(configDir, spinner.DirName) {
		t.Fatalf("dir = %q, want the spinners subdirectory of %q", files.Dir, configDir)
	}
	if len(files.Sprites) != 0 || len(files.Warnings) != 0 {
		t.Fatalf("a fresh install answered %+v, want an empty, warning-free listing", files)
	}

	if err := os.MkdirAll(files.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 'P', 'N', 'G', 0xff}
	manifest := `{"frames":8,"frameMs":100}`
	if err := os.WriteFile(filepath.Join(files.Dir, "robo-papers.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files.Dir, "robo-papers.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err = app.GetSpinnerFiles()
	if err != nil {
		t.Fatalf("GetSpinnerFiles: %v", err)
	}
	if len(files.Sprites) != 1 || files.Sprites[0].ID != "robo-papers" {
		t.Fatalf("sprites = %+v, want one under id \"robo-papers\"", files.Sprites)
	}
	if files.Sprites[0].Manifest != manifest {
		t.Fatalf("manifest = %q, want the file's bytes verbatim", files.Sprites[0].Manifest)
	}
	// The strip crosses the wire as base64 rather than a []byte, because
	// the binding generator has no []byte case and would type it
	// `number[]` while JSON actually carries a string.
	decoded, err := base64.StdEncoding.DecodeString(files.Sprites[0].PNG)
	if err != nil {
		t.Fatalf("png is not base64: %v", err)
	}
	if string(decoded) != string(png) {
		t.Fatalf("decoded strip = %v, want the file's bytes verbatim", decoded)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", files.Warnings)
	}
}

// initSpinnerDirectory is boot: it materializes the directory, seeds the
// authoring reference, and arms live reload. None of it may fail boot.
func TestInitSpinnerDirectorySeedsTheReferenceAndArmsTheWatcher(t *testing.T) {
	app, configDir := newSpinnerTestApp(t)
	app.initSpinnerDirectory()
	t.Cleanup(func() {
		if app.spinnerWatcher != nil {
			_ = app.spinnerWatcher.Close()
		}
	})

	dir := filepath.Join(configDir, spinner.DirName)
	if _, err := os.Stat(filepath.Join(dir, spinner.ReferenceFileName)); err != nil {
		t.Fatalf("%s was not seeded: %v", spinner.ReferenceFileName, err)
	}
	if app.spinnerWatcher == nil {
		t.Fatal("the spinners watcher did not start")
	}
}

// A blocked spinners directory is degraded, not broken: boot logs and
// moves on, the RPC still answers, and the seed heals from the next read
// once the blocker is gone.
func TestInitSpinnerDirectorySurvivesABlockedBoot(t *testing.T) {
	app, configDir := newSpinnerTestApp(t)
	blocker := filepath.Join(configDir, spinner.DirName)
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	app.initSpinnerDirectory()
	t.Cleanup(func() {
		if app.spinnerWatcher != nil {
			_ = app.spinnerWatcher.Close()
		}
	})
	if app.spinnerWatcher != nil {
		t.Fatal("a watcher armed over a path that is a file")
	}

	files, err := app.GetSpinnerFiles()
	if err != nil {
		t.Fatalf("GetSpinnerFiles while blocked: %v", err)
	}
	if len(files.Sprites) != 0 {
		t.Fatalf("blocked listing = %+v, want empty", files.Sprites)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if files, err = app.GetSpinnerFiles(); err != nil {
		t.Fatalf("GetSpinnerFiles after unblocking: %v", err)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("a healed listing still warns: %v", files.Warnings)
	}
	if _, err := os.Stat(filepath.Join(files.Dir, spinner.ReferenceFileName)); err != nil {
		t.Fatalf("the reference was never seeded after unblocking: %v", err)
	}
}
