//go:build darwin

package main

import (
	"reflect"
	"testing"
)

func TestSupervisedUpArgsUsesHarnessDriver(t *testing.T) {
	got, err := supervisedUpArgs([]string{"--harness", "--window"}, "/tmp/run.app/Contents/MacOS/agent-overflow", "/tmp/run", "/tmp/ao-mockprovider", "/tmp/ao-mockforge")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"up", "--window", "--binary", "/tmp/run.app/Contents/MacOS/agent-overflow", "--data-dir", "/tmp/run", "--mock-provider", "/tmp/ao-mockprovider", "--mock-forge", "/tmp/ao-mockforge"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supervised args = %v, want %v", got, want)
	}
}

func TestSupervisedUpArgsRejectsUnscopedArguments(t *testing.T) {
	if _, err := supervisedUpArgs([]string{"--harness", "--window", "--data-dir", "/tmp/other"}, "/tmp/app", "/tmp/run", "/tmp/mock", ""); err == nil {
		t.Fatal("unsupported backend argument was accepted")
	}
}

func TestSupervisedUpArgsPreservesSoakAutopilot(t *testing.T) {
	got, err := supervisedUpArgs([]string{"--soak", "--autopilot", "--window"}, "/tmp/app", "/tmp/run", "/tmp/mock", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"up", "--window", "--binary", "/tmp/app", "--data-dir", "/tmp/run", "--mock-provider", "/tmp/mock", "--soak", "--autopilot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supervised soak args = %v, want %v", got, want)
	}
}
