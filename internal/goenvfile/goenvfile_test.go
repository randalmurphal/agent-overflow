package goenvfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupParsesAsTheGoCommandDoes(t *testing.T) {
	data := []byte("GOFLAGS=-mod=mod\n GOTMPDIR=/indented\nGOTMPDIR = /spaced\ngoproxy=lower\n# GOCACHE=/comment\nNOEQUALS\n\nGOPROXY=\nGOCACHE=/first\nGOCACHE=/last\r\nGOPRIVATE= padded \nGOTOOLCHAIN=local")
	for name, want := range map[string]string{
		"GOFLAGS":     "-mod=mod",
		"GOTMPDIR":    "",
		"GOTMPDIR ":   " /spaced",
		"GOPROXY":     "",
		"goproxy":     "",
		"GOCACHE":     "/last\r",
		"GOPRIVATE":   " padded ",
		"GOTOOLCHAIN": "local",
		"NOEQUALS":    "",
	} {
		if got := Lookup(data, name); got != want {
			t.Fatalf("Lookup(%q) = %q, want %q", name, got, want)
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

// TestResolutionAgreesWithTheGoCommand: for each crafted Go environment
// file and environment, Getenv("GOTMPDIR") and CacheDir give what `go env`
// prints, and CacheDir refuses exactly where `go env GOCACHE` says off.
func TestResolutionAgreesWithTheGoCommand(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	abs := func(name string) string { return filepath.Join(root, name) }
	t.Setenv("XDG_CONFIG_HOME", abs("config"))
	t.Setenv("XDG_CACHE_HOME", abs("cache"))
	t.Setenv("HOME", root)
	if err := os.MkdirAll(filepath.Join(abs("config"), "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs("config"), "go", "env"), []byte("GOCACHE="+abs("default-file-cache")+"\nGOTMPDIR="+abs("default-file-tmp")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file string
		// goenv is GOENV when the case names one other than its file.
		goenv, gocache, gotmpdir string
	}{
		{name: "plain", file: "GOCACHE=" + abs("c") + "\nGOTMPDIR=" + abs("t") + "\n"},
		{name: "leading space", file: " GOCACHE=" + abs("c") + "\n GOTMPDIR=" + abs("t") + "\n"},
		{name: "spaces around =", file: "GOCACHE = " + abs("c") + "\nGOTMPDIR = " + abs("t") + "\n"},
		{name: "space after =", file: "GOCACHE= " + abs("c") + "\nGOTMPDIR= " + abs("t") + "\n"},
		{name: "trailing space", file: "GOCACHE=" + abs("c") + " \nGOTMPDIR=" + abs("t") + " \n"},
		{name: "lowercase key", file: "gocache=" + abs("c") + "\ngotmpdir=" + abs("t") + "\n"},
		{name: "comment", file: "# GOCACHE=" + abs("c") + "\n#GOTMPDIR=" + abs("t") + "\n"},
		{name: "duplicate keys", file: "GOCACHE=" + abs("first") + "\nGOTMPDIR=" + abs("first") + "\nGOCACHE=" + abs("last") + "\nGOTMPDIR=" + abs("last") + "\n"},
		{name: "empty value after a set one", file: "GOCACHE=" + abs("c") + "\nGOTMPDIR=" + abs("t") + "\nGOCACHE=\nGOTMPDIR=\n"},
		{name: "off", file: "GOCACHE=off\nGOTMPDIR=off\n"},
		{name: "relative", file: "GOCACHE=rel\nGOTMPDIR=rel\n"},
		{name: "CRLF and no final newline", file: "GOCACHE=" + abs("c") + "\r\nGOTMPDIR=" + abs("t")},
		{name: "environment wins", file: "GOCACHE=" + abs("c") + "\nGOTMPDIR=" + abs("t") + "\n", gocache: abs("env-c"), gotmpdir: abs("env-t")},
		{name: "GOENV off", file: "GOCACHE=" + abs("c") + "\n", goenv: "off"},
		{name: "default location", goenv: "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(file, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			switch tc.goenv {
			case "":
				t.Setenv("GOENV", file)
			case "-":
				t.Setenv("GOENV", "")
			default:
				t.Setenv("GOENV", tc.goenv)
			}
			t.Setenv("GOCACHE", tc.gocache)
			t.Setenv("GOTMPDIR", tc.gotmpdir)
			out, err := exec.Command(goBin, "env", "GOCACHE", "GOTMPDIR").Output()
			if err != nil {
				t.Fatalf("go env: %v", err)
			}
			goSays := strings.SplitN(strings.TrimSuffix(string(out), "\n"), "\n", 2)
			if len(goSays) != 2 {
				t.Fatalf("go env printed %q", out)
			}
			gotTmp, err := Getenv("GOTMPDIR")
			if err != nil || gotTmp != goSays[1] {
				t.Fatalf("Getenv(GOTMPDIR) = %q, %v; go env GOTMPDIR = %q", gotTmp, err, goSays[1])
			}
			gotCache, err := CacheDir()
			if goSays[0] == "off" {
				if err == nil {
					t.Fatalf("CacheDir = %q where go env GOCACHE says off", gotCache)
				}
				return
			}
			if err != nil || gotCache != goSays[0] {
				t.Fatalf("CacheDir = %q, %v; go env GOCACHE = %q", gotCache, err, goSays[0])
			}
		})
	}
}
