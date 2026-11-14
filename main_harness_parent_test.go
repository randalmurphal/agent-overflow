package main

import (
	"strings"
	"testing"
)

func TestExitWithParentRequiresHarnessAndAProcessID(t *testing.T) {
	flags, err := parseFlags([]string{"--harness", "--data-dir", "/tmp/x", "--exit-with-parent", "42"})
	if err != nil || flags.exitWithParent != 42 {
		t.Fatalf("parse --harness --exit-with-parent 42 = %+v, %v", flags.exitWithParent, err)
	}
	for _, args := range [][]string{
		{"--exit-with-parent", "42"},
		{"--soak", "--exit-with-parent", "42"},
		{"--harness", "--data-dir", "/tmp/x", "--exit-with-parent", "-1"},
	} {
		if _, err := parseFlags(args); err == nil || !strings.Contains(err.Error(), "--exit-with-parent") {
			t.Fatalf("parseFlags(%q) = %v, want a refusal naming --exit-with-parent", args, err)
		}
	}
}
