package main

import (
	"reflect"
	"testing"
)

func TestParseArgsKeepsPlaywrightFlags(t *testing.T) {
	opts, err := parseArgs([]string{"--memory-limit-bytes=1234", "--", "tests/harness.spec.ts", "--workers=1"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.limit != 1234 {
		t.Fatalf("limit = %d, want 1234", opts.limit)
	}
	if opts.mode != modeTests {
		t.Fatalf("mode = %v, want test mode", opts.mode)
	}
	if opts.hostNetwork {
		t.Fatal("host network enabled without --host-network")
	}
	if want := []string{"tests/harness.spec.ts", "--workers=1"}; !reflect.DeepEqual(opts.args, want) {
		t.Fatalf("Playwright args = %#v, want %#v", opts.args, want)
	}
}

func TestParseArgsHostNetworkIsLauncherOnly(t *testing.T) {
	opts, err := parseArgs([]string{"--host-network", "--config=playwright.android.config.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.hostNetwork {
		t.Fatal("--host-network did not select the host network")
	}
	if want := []string{"--config=playwright.android.config.ts"}; !reflect.DeepEqual(opts.args, want) {
		t.Fatalf("Playwright args = %#v, want %#v", opts.args, want)
	}
}

func TestParseArgsRejectsZeroMemoryLimit(t *testing.T) {
	if _, err := parseArgs([]string{"--memory-limit-bytes", "0"}); err == nil {
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
		opts, err := parseArgs([]string{tc.arg, "--headed"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.mode != tc.want || len(opts.args) != 1 || opts.args[0] != "--headed" {
			t.Fatalf("parse %s = mode %v, args %v", tc.arg, opts.mode, opts.args)
		}
	}
}

func TestParseArgsRejectsConflictingModes(t *testing.T) {
	for _, args := range [][]string{{"--test", "--flow"}, {"--flow", "--flow"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("parse %v accepted conflicting modes", args)
		}
	}
}
