package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteIfUnchangedRefusesAFileChangedSinceTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, before, err := ReadWithStat(path)
	if err != nil || string(data) != "v1" || before == nil {
		t.Fatalf("ReadWithStat = %q, %v, %v", data, before, err)
	}
	// Another writer replaces the file; the mtime moves even on a coarse clock.
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := before.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if ok, err := WriteIfUnchanged(path, []byte("mine"), before); err != nil || ok {
		t.Fatalf("WriteIfUnchanged over a changed file = %v, %v, want refused", ok, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2" {
		t.Fatalf("the other writer's file became %q", got)
	}

	_, fresh, err := ReadWithStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := WriteIfUnchanged(path, []byte("mine"), fresh); err != nil || !ok {
		t.Fatalf("WriteIfUnchanged over an unchanged file = %v, %v", ok, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "mine" {
		t.Fatalf("file = %q, want mine", got)
	}
}

func TestReadWithStatAnswersNothingForAMissingFile(t *testing.T) {
	data, info, err := ReadWithStat(filepath.Join(t.TempDir(), "absent"))
	if data != nil || info != nil || err != nil {
		t.Fatalf("ReadWithStat of a missing file = %q, %v, %v", data, info, err)
	}
	path := filepath.Join(t.TempDir(), "sub", "config")
	if ok, err := WriteIfUnchanged(path, []byte("new"), nil); err != nil || !ok {
		t.Fatalf("WriteIfUnchanged of an absent file = %v, %v", ok, err)
	}
}
