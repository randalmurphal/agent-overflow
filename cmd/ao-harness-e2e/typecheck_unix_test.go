//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// The checks run, tsc with its incremental state and then the wait rules,
// unless AO_E2E_TYPECHECKED is exactly "1", and the launcher says so when it
// skips them.
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
			if code, err := ensureSuiteChecked(dir, &out, &out); code != 0 || err != nil {
				t.Fatalf("ensureSuiteChecked = %d, %v:\n%s", code, err, out.String())
			}
			ran, _ := os.ReadFile(calls)
			if !tc.checks {
				if len(ran) != 0 || !strings.Contains(out.String(), "AO_E2E_TYPECHECKED=1") {
					t.Fatalf("with %s=1 the check ran (%q) or the skip was not named: %q", typecheckedEnv, ran, out.String())
				}
				return
			}
			want := "exec tsc --noEmit --incremental --tsBuildInfoFile " + tsBuildInfo + " -p .\nexec node scripts/check-waits.ts\n"
			if string(ran) != want || strings.Contains(out.String(), "skipping") {
				t.Fatalf("with %s=%q ran %q, want %q; output %q", typecheckedEnv, tc.value, ran, want, out.String())
			}
		})
	}
}

// Each check stops the run with its own exit code and names itself, and a
// check after a failed one does not run. checkSuite, which --typecheck runs,
// ignores the variable: the harness build is what makes it true.
func TestCheckSuiteStopsAtTheFirstFailingCheck(t *testing.T) {
	for _, tc := range []struct {
		failing string
		code    int
		says    string
		ran     string
	}{
		{"", 0, "", "tsc\ncheck-waits\n"},
		{"tsc", 3, "E2E TypeScript check failed", "tsc\n"},
		{"check-waits", 4, "E2E wait rules failed", "tsc\ncheck-waits\n"},
	} {
		t.Run("failing="+tc.failing, func(t *testing.T) {
			dir := t.TempDir()
			ran := filepath.Join(dir, "ran")
			bin := filepath.Join(dir, "bin")
			mockexec.WriteIn(t, bin, "pnpm", `#!/bin/sh
case "$*" in
  *tsc*) echo tsc >> "$AO_TEST_RAN"; [ "$AO_TEST_FAIL" = tsc ] && exit 3 ;;
  *check-waits*) echo check-waits >> "$AO_TEST_RAN"; [ "$AO_TEST_FAIL" = check-waits ] && exit 4 ;;
  *) echo "unexpected: $*" >> "$AO_TEST_RAN"; exit 9 ;;
esac
exit 0
`)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AO_TEST_RAN", ran)
			t.Setenv("AO_TEST_FAIL", tc.failing)
			t.Setenv(typecheckedEnv, "1")
			var out strings.Builder
			code, err := checkSuite(dir, &out, &out)
			if code != tc.code {
				t.Fatalf("checkSuite = %d, %v, want %d; output:\n%s", code, err, tc.code, out.String())
			}
			if (err == nil) != (tc.says == "") || (err != nil && !strings.Contains(err.Error(), tc.says)) {
				t.Fatalf("checkSuite error %v, want one saying %q", err, tc.says)
			}
			got, readErr := os.ReadFile(ran)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.ran {
				t.Fatalf("checks run = %q, want %q", got, tc.ran)
			}
		})
	}
}
