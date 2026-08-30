//go:build darwin

package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestSupervisedUpArgsUsesHarnessDriver(t *testing.T) {
	got, err := supervisedUpArgs([]string{"--harness", "--window"}, "/tmp/run.app/Contents/MacOS/agent-overflow", "/tmp/run", "/tmp/ao-mockprovider")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"up", "--window", "--binary", "/tmp/run.app/Contents/MacOS/agent-overflow", "--data-dir", "/tmp/run", "--mock-provider", "/tmp/ao-mockprovider"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supervised args = %v, want %v", got, want)
	}
}

func TestSupervisedUpArgsRejectsUnscopedArguments(t *testing.T) {
	if _, err := supervisedUpArgs([]string{"--harness", "--window", "--data-dir", "/tmp/other"}, "/tmp/app", "/tmp/run", "/tmp/mock"); err == nil {
		t.Fatal("unsupported backend argument was accepted")
	}
}

func TestSupervisedUpArgsPreservesSoakAutopilot(t *testing.T) {
	got, err := supervisedUpArgs([]string{"--soak", "--autopilot", "--window"}, "/tmp/app", "/tmp/run", "/tmp/mock")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"up", "--window", "--binary", "/tmp/app", "--data-dir", "/tmp/run", "--mock-provider", "/tmp/mock", "--soak", "--autopilot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supervised soak args = %v, want %v", got, want)
	}
}

func TestFlagSeparatorPreservesBackendArgs(t *testing.T) {
	flags := newFlagSetForTest()
	if err := flags.Parse([]string{"--binary", "app", "--data-root", "root", "--", "--harness", "--window"}); err != nil {
		t.Fatal(err)
	}
	if got, want := flags.Args(), []string{"--harness", "--window"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backend args = %v, want %v", got, want)
	}
}

func newFlagSetForTest() *flag.FlagSet {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("binary", "", "")
	flags.String("data-root", "", "")
	flags.String("plist", "", "")
	flags.String("mock-provider", "", "")
	return flags
}
