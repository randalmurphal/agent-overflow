package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Playwright (and the flow runner's node --experimental-transform-types)
// strip types per file and never check them: a typo'd property inside a
// spec helper's filter or predicate does not throw at runtime, it yields
// undefined and lets an emptiness assertion pass vacuously. So the launcher
// typechecks the whole suite before a run, before the memory reservation,
// so a type error fails in seconds without holding the gate's reservation.
// tsc keeps its own incremental state in tsBuildInfo, under the ignored
// node_modules, so a check of an unchanged suite costs a re-read and no
// re-check.
//
// typecheckedEnv=1 skips the check. A CI job sets it when its harness build
// job typechecked this revision before packing the artifact the job runs
// (.github/workflows/ci.yml); a value other than "1" is unset.
const (
	typecheckedEnv = "AO_E2E_TYPECHECKED"
	tsBuildInfo    = "node_modules/.cache/ao-e2e.tsbuildinfo"
)

// ensureTypechecked typechecks the suite in testDir unless typecheckedEnv
// says this revision was checked already. A failed check returns tsc's
// exit code.
func ensureTypechecked(testDir string, stdout, stderr io.Writer) (int, error) {
	if os.Getenv(typecheckedEnv) == "1" {
		fmt.Fprintf(stdout, "ao-harness-e2e: skipping the E2E TypeScript check: %s=1 says this revision's harness build checked it\n", typecheckedEnv)
		return 0, nil
	}
	return typecheck(testDir, stdout, stderr)
}

// typecheck runs tsc over the suite. Fixed argv, as for the contained
// command; tsc's footprint needs the suite's containment no more than the
// `go run` hosting this launcher does.
func typecheck(testDir string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command("pnpm", "exec", "tsc", "--noEmit", "--incremental", "--tsBuildInfoFile", tsBuildInfo, "-p", ".")
	cmd.Dir = testDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), errors.New("E2E TypeScript check failed; fix the errors above")
		}
		return 1, fmt.Errorf("run the E2E TypeScript check: %w", err)
	}
	return 0, nil
}
