// Package leftover is the test package the leftover check's tests run with
// go test: each test starts a process and returns without ending it.
package leftover

import (
	"os"
	"os/exec"
	"testing"

	"agent-overflow/internal/testutil/providerpath"
)

func TestMain(m *testing.M) { os.Exit(providerpath.Run(m)) }

func TestLeavesAProcess(t *testing.T) {
	if err := exec.Command("sleep", "30").Start(); err != nil {
		t.Fatal(err)
	}
}

// The process ends a second after the test, inside the check's grace.
func TestLeavesAProcessThatExitsSoon(t *testing.T) {
	if err := exec.Command("sleep", "1").Start(); err != nil {
		t.Fatal(err)
	}
}
