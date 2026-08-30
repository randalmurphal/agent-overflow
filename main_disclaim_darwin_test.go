//go:build darwin && cgo

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const disclaimTestChildEnv = "AO_TEST_DISCLAIM_CHILD"

func TestDarwinHarnessDisclaimsInheritedResponsibility(t *testing.T) {
	if os.Getenv(disclaimTestChildEnv) == "1" {
		if err := disclaimHarnessResponsibility(); err != nil {
			t.Fatal(err)
		}
		if got := currentResponsiblePID(); got != os.Getpid() {
			t.Fatalf("responsible pid = %d, want self %d", got, os.Getpid())
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinHarnessDisclaimsInheritedResponsibility$")
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, harnessDisclaimEnv+"=") && !strings.HasPrefix(value, disclaimTestChildEnv+"=") {
			env = append(env, value)
		}
	}
	cmd.Env = append(env, harnessDisclaimEnv+"=1", disclaimTestChildEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("disclaimed child: %v\n%s", err, output)
	}
}
