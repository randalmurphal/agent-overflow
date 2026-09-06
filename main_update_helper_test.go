package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"agent-overflow/internal/kerneltest"
)

func TestUpdaterHelperRunsBeforeEntryDispatch(t *testing.T) {
	if os.Getenv("AO_TEST_UPDATE_HELPER") == "1" {
		// A valid helper marker with incomplete swap input must exit immediately,
		// before the session guard, flags, providers or a native window can run.
		os.Args = []string{os.Args[0], "--not-a-boot-flag"}
		main()
		os.Exit(99)
	}
	kerneltest.IsolateSpawns(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestUpdaterHelperRunsBeforeEntryDispatch$")
	cmd.Env = append(os.Environ(), "AO_TEST_UPDATE_HELPER=1", "WAILS_UPDATER_HELPER=1", "WAILS_UPDATER_HELPER_TARGET=", "WAILS_UPDATER_HELPER_NEW=", "AO_ENDPOINT=http://127.0.0.1:1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || len(output) != 0 {
		t.Fatalf("helper reached ordinary boot: err=%v output=%q", err, output)
	}
}
