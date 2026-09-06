package transferfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFixture(t *testing.T, staging, name, body string, executable bool) File {
	t.Helper()
	p := filepath.Join(staging, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0600)
	if executable {
		mode = 0700
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(body))
	return File{Name: name, Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:]), Executable: executable}
}

func TestPreparedInstallationResumesAcrossPartialCompletion(t *testing.T) {
	ctx := context.Background()
	staging, dest := t.TempDir(), t.TempDir()
	roots := map[string]string{"provider": dest}
	one := installFixture(t, staging, "native/one.jsonl", strings.Repeat("native history\n", 100_000), false)
	two := installFixture(t, staging, "native/two.jsonl", "replacement session", true)
	installFixture(t, dest, "sessions/two.jsonl", "retired session", false)
	targets := []InstallTarget{{File: one, Root: "provider", Path: "sessions/nested/one.jsonl"}, {File: two, Root: "provider", Path: "sessions/two.jsonl", ReplaceExisting: true}}
	prepared, err := PrepareInstallation(ctx, roots, targets)
	if err != nil {
		t.Fatal(err)
	}
	if prepared[0].PreviousSHA256 != "" || prepared[1].PreviousSHA256 == "" {
		t.Fatalf("wrong baselines: %+v", prepared)
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions/nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preparation changed filesystem")
	}
	if err := InstallPreparedFiles(ctx, staging, roots, prepared[:1]); err != nil {
		t.Fatal(err)
	}
	// The durable recipe survives process memory and its first file is already
	// installed. Retrying the complete recipe must keep it and finish the rest.
	wire, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var resumed []Installation
	if err := json.Unmarshal(wire, &resumed); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := InstallPreparedFiles(ctx, staging, roots, resumed); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range targets {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(target.Path)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(target.File.Name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("installed bytes differ")
		}
		stat, err := os.Stat(filepath.Join(dest, filepath.FromSlash(target.Path)))
		if err != nil || (stat.Mode()&0100 != 0) != target.File.Executable {
			t.Fatalf("execute permission: %v %v", stat, err)
		}
	}
	assertNoInstallTemps(t, dest)
}

func TestInstallationRefusesChangedOrCorruptFiles(t *testing.T) {
	for _, change := range []string{"new destination", "changed destination", "corrupt staging", "short staging", "canceled", "unauthorized replacement"} {
		t.Run(change, func(t *testing.T) {
			staging, dest := t.TempDir(), t.TempDir()
			roots := map[string]string{"provider": dest}
			file := installFixture(t, staging, "source.jsonl", "native content", false)
			target := InstallTarget{File: file, Root: "provider", Path: "session.jsonl"}
			if change == "changed destination" || change == "unauthorized replacement" {
				installFixture(t, dest, target.Path, "old content", false)
				target.ReplaceExisting = true
			}
			prepared, err := PrepareInstallation(context.Background(), roots, []InstallTarget{target})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "new destination", "changed destination":
				installFixture(t, dest, target.Path, "changed outside transfer", false)
			case "corrupt staging":
				installFixture(t, staging, file.Name, "corrupt bytes!", false)
			case "short staging":
				installFixture(t, staging, file.Name, "short", false)
			case "canceled":
				cancel()
			case "unauthorized replacement":
				prepared[0].ReplaceExisting = false
			}
			before, readErr := os.ReadFile(filepath.Join(dest, target.Path))
			if err := InstallPreparedFiles(ctx, staging, roots, prepared); err == nil {
				t.Fatal("accepted changed transfer")
			}
			after, afterErr := os.ReadFile(filepath.Join(dest, target.Path))
			if !bytes.Equal(before, after) || errors.Is(readErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
				t.Fatal("failed installation changed destination")
			}
			assertNoInstallTemps(t, dest)
		})
	}
}

func TestInstallationRejectsCollisionsAndLinksBeforeWriting(t *testing.T) {
	for _, name := range []string{"existing file", "parent link", "leaf link", "alias roots", "prefix overlap", "invalid path", "unknown root", "bad digest"} {
		t.Run(name, func(t *testing.T) {
			staging, dest := t.TempDir(), t.TempDir()
			file := installFixture(t, staging, "source", "content", false)
			roots := map[string]string{"provider": dest, "alias": dest}
			targets := []InstallTarget{{File: file, Root: "provider", Path: "target"}}
			switch name {
			case "existing file":
				installFixture(t, dest, "target", "different", false)
			case "parent link":
				if err := os.Symlink(staging, filepath.Join(dest, "target")); err != nil {
					t.Skip(err)
				}
				targets[0].Path = "target/new"
			case "leaf link":
				if err := os.Symlink(filepath.Join(staging, "source"), filepath.Join(dest, "target")); err != nil {
					t.Skip(err)
				}
			case "alias roots":
				targets = append(targets, InstallTarget{File: file, Root: "alias", Path: "TARGET"})
			case "prefix overlap":
				targets = append(targets, InstallTarget{File: file, Root: "provider", Path: "target-else"}, InstallTarget{File: file, Root: "provider", Path: "target/child"})
			case "invalid path":
				targets[0].Path = "../escape"
			case "unknown root":
				targets[0].Root = "unknown"
			case "bad digest":
				targets[0].File.SHA256 = "bad"
			}
			if _, err := PrepareInstallation(context.Background(), roots, targets); err == nil {
				t.Fatal("accepted invalid target set")
			}
			assertNoInstallTemps(t, dest)
		})
	}
}

func TestInstallationPreservesCompletedFilesWhenLaterFileConflicts(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	roots := map[string]string{"provider": dest}
	file := installFixture(t, staging, "source", "snapshot", false)
	prepared, err := PrepareInstallation(context.Background(), roots, []InstallTarget{{File: file, Root: "provider", Path: "first"}, {File: file, Root: "provider", Path: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallPreparedFiles(context.Background(), staging, roots, prepared[:1]); err != nil {
		t.Fatal(err)
	}
	installFixture(t, dest, "second", "unrelated write", false)
	if err := InstallPreparedFiles(context.Background(), staging, roots, prepared); err == nil {
		t.Fatal("overwrote unrelated write on recovery")
	}
	first, err := os.ReadFile(filepath.Join(dest, "first"))
	if err != nil || string(first) != "snapshot" {
		t.Fatal("lost completed file")
	}
	second, err := os.ReadFile(filepath.Join(dest, "second"))
	if err != nil || string(second) != "unrelated write" {
		t.Fatal("lost conflicting file")
	}
}

func TestInstallationDiscardsItsOwnCrashTemp(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	roots := map[string]string{"provider": dest}
	file := installFixture(t, staging, "source", "snapshot", false)
	target := InstallTarget{File: file, Root: "provider", Path: "target"}
	prepared, err := PrepareInstallation(context.Background(), roots, []InstallTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	nonce := sha256.Sum256([]byte(target.Root + "\n" + target.Path + "\n" + file.SHA256))
	installFixture(t, dest, ".ao-transfer-"+hex.EncodeToString(nonce[:16])+".tmp", "half-written", false)
	if err := InstallPreparedFiles(context.Background(), staging, roots, prepared); err != nil {
		t.Fatal(err)
	}
	assertNoInstallTemps(t, dest)
}

func assertNoInstallTemps(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".ao-transfer-") {
			t.Errorf("orphan temporary file: %s", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
