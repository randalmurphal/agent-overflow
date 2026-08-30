package main

import (
	"reflect"
	"testing"
)

func TestParseArgsKeepsPlaywrightFlags(t *testing.T) {
	limit, mode, args, err := parseArgs([]string{"--memory-limit-bytes=1234", "--", "tests/harness.spec.ts", "--workers=1"})
	if err != nil {
		t.Fatal(err)
	}
	if limit != 1234 {
		t.Fatalf("limit = %d, want 1234", limit)
	}
	if mode != modeTests {
		t.Fatalf("mode = %v, want test mode", mode)
	}
	if want := []string{"tests/harness.spec.ts", "--workers=1"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("Playwright args = %#v, want %#v", args, want)
	}
}

func TestParseArgsRejectsZeroMemoryLimit(t *testing.T) {
	if _, _, _, err := parseArgs([]string{"--memory-limit-bytes", "0"}); err == nil {
		t.Fatal("zero memory limit succeeded")
	}
}

func TestParseArgsSelectsManagedModes(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want runMode
	}{
		{"--flow", modeFlow},
		{"--freeze-repro", modeFreeze},
	} {
		_, mode, args, err := parseArgs([]string{tc.arg, "--headed"})
		if err != nil {
			t.Fatal(err)
		}
		if mode != tc.want || len(args) != 1 || args[0] != "--headed" {
			t.Fatalf("parse %s = limit/mode/args, %v, %v", tc.arg, mode, args)
		}
	}
}

func TestParseArgsRejectsConflictingModes(t *testing.T) {
	for _, args := range [][]string{{"--test", "--flow"}, {"--flow", "--flow"}} {
		if _, _, _, err := parseArgs(args); err == nil {
			t.Fatalf("parse %v accepted conflicting modes", args)
		}
	}
}
