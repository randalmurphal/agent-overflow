//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// Each source check stops the run with its own exit code and names itself,
// and a check after a failed one does not run: the wait rules sit in the same
// step as the typecheck, so whatever runs or skips one runs or skips both.
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
			var out strings.Builder
			if code := checkSuite(dir, &out, &out); code != tc.code {
				t.Fatalf("checkSuite = %d, want %d; output:\n%s", code, tc.code, out.String())
			}
			if !strings.Contains(out.String(), tc.says) {
				t.Fatalf("output %q does not say %q", out.String(), tc.says)
			}
			got, err := os.ReadFile(ran)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.ran {
				t.Fatalf("checks run = %q, want %q", got, tc.ran)
			}
		})
	}
}
