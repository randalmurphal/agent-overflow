package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// The launcher runs the suite's source checks before a run, before the
// memory reservation, so a failure costs seconds and holds nothing:
//
//   - The typecheck. Playwright (and the flow runner's node
//     --experimental-transform-types) strip types per file and never check
//     them: a typo'd property inside a spec helper's filter or predicate does
//     not throw at runtime, it yields undefined and lets an emptiness
//     assertion pass vacuously. tsc keeps its own incremental state in
//     tsBuildInfo, under the ignored node_modules, so a check of an unchanged
//     suite costs a re-read and no re-check.
//   - The wait rules (e2e/src/wait-scan.ts): no sleep, no wait that passes by
//     timing out, and no test outside tests/base-test.ts.
//
// typecheckedEnv=1 skips both. A CI job sets it when its harness build job
// ran them on this revision before packing the artifact the job runs
// (.github/workflows/ci.yml); a value other than "1" is unset.
const (
	typecheckedEnv = "AO_E2E_TYPECHECKED"
	tsBuildInfo    = "node_modules/.cache/ao-e2e.tsbuildinfo"
)

// suiteChecks are the source checks, in order. Fixed argv, as for the
// contained command; their footprint needs the suite's containment no more
// than the `go run` hosting this launcher does.
var suiteChecks = []struct {
	name string
	argv []string
	fix  string
}{
	{"E2E TypeScript check", []string{"pnpm", "exec", "tsc", "--noEmit", "--incremental", "--tsBuildInfoFile", tsBuildInfo, "-p", "."}, "fix the errors above"},
	{"E2E wait rules", []string{"pnpm", "exec", "node", "scripts/check-waits.ts"}, "fix the findings above"},
}

// ensureSuiteChecked runs the source checks in testDir unless
// typecheckedEnv says this revision was checked already.
func ensureSuiteChecked(testDir string, stdout, stderr io.Writer) (int, error) {
	if os.Getenv(typecheckedEnv) == "1" {
		_, _ = fmt.Fprintf(stdout, "ao-harness-e2e: skipping the E2E TypeScript check and wait rules: %s=1 says this revision's harness build ran them\n", typecheckedEnv)
		return 0, nil
	}
	return checkSuite(testDir, stdout, stderr)
}

// checkSuite runs each source check in testDir and stops at the first that
// fails, returning its exit code and an error that names it.
func checkSuite(testDir string, stdout, stderr io.Writer) (int, error) {
	for _, check := range suiteChecks {
		cmd := exec.Command(check.argv[0], check.argv[1:]...)
		cmd.Dir = testDir
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return exitErr.ExitCode(), fmt.Errorf("%s failed; %s", check.name, check.fix)
			}
			return 1, fmt.Errorf("run the %s: %w", check.name, err)
		}
	}
	return 0, nil
}
