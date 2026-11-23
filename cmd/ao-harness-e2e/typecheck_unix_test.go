//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// The check runs tsc with its incremental state unless AO_E2E_TYPECHECKED
// is exactly "1", and says so when it skips.
func TestTheTypecheckRunsUnlessTheVariableIsOne(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "bin")
	mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
echo "$*" >> "$AO_TEST_CALLS"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_TEST_CALLS", calls)
	for _, tc := range []struct {
		name, value string
		set, checks bool
	}{
		{name: "unset", checks: true},
		{name: "one", value: "1", set: true},
		{name: "true", value: "true", set: true, checks: true},
		{name: "zero", value: "0", set: true, checks: true},
		{name: "empty", value: "", set: true, checks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(typecheckedEnv, tc.value)
			} else {
				t.Setenv(typecheckedEnv, "")
				if err := os.Unsetenv(typecheckedEnv); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var out strings.Builder
			if code, err := ensureTypechecked(dir, &out, &out); code != 0 || err != nil {
				t.Fatalf("ensureTypechecked = %d, %v:\n%s", code, err, out.String())
			}
			ran, _ := os.ReadFile(calls)
			if !tc.checks {
				if len(ran) != 0 || !strings.Contains(out.String(), "AO_E2E_TYPECHECKED=1") {
					t.Fatalf("with %s=1 the check ran (%q) or the skip was not named: %q", typecheckedEnv, ran, out.String())
				}
				return
			}
			want := "exec tsc --noEmit --incremental --tsBuildInfoFile " + tsBuildInfo + " -p .\n"
			if string(ran) != want || strings.Contains(out.String(), "skipping") {
				t.Fatalf("with %s=%q ran %q, want %q; output %q", typecheckedEnv, tc.value, ran, want, out.String())
			}
		})
	}
}

// A failed check returns tsc's exit code. typecheck, which --typecheck
// runs, ignores the variable: the harness build is what makes it true.
func TestAFailedTypecheckFailsTheLauncher(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "playwright.config.ts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
echo "tests/x.spec.ts(1,1): error TS2339"
exit 2
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(typecheckedEnv, "1")
	var out strings.Builder
	code, err := typecheck(dir, &out, &out)
	if code != 2 || err == nil || !strings.Contains(out.String(), "TS2339") {
		t.Fatalf("typecheck with a type error = %d, %v:\n%s", code, err, out.String())
	}
}
