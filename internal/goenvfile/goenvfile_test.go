package goenvfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookup(t *testing.T) {
	data := []byte("GOFLAGS=-mod=mod\n GOTMPDIR = /first\nGOTMPDIR=/last\nGOPROXY=\n")
	for name, want := range map[string]string{"GOTMPDIR": "/last", "GOFLAGS": "-mod=mod", "GOPROXY": "", "GOCACHE": ""} {
		if got := Lookup(data, name); got != want {
			t.Fatalf("Lookup(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "env")
	if err := os.WriteFile(file, []byte("GOCACHE=/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", file)
	if data, err := Read(); err != nil || string(data) != "GOCACHE=/x\n" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	for _, goenv := range []string{"off", filepath.Join(dir, "absent")} {
		t.Setenv("GOENV", goenv)
		if data, err := Read(); err != nil || data != nil {
			t.Fatalf("Read with GOENV=%s = %q, %v; want nothing", goenv, data, err)
		}
	}
	t.Setenv("GOENV", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(config, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "go", "env"), []byte("GOPROXY=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := Read(); err != nil || string(data) != "GOPROXY=off\n" {
		t.Fatalf("Read of the user config file = %q, %v", data, err)
	}
}
