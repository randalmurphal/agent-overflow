//go:build darwin

package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestVerifyMacUpdateChecksSignatureRequirementNotarizationAndGatekeeper(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "/usr/bin/codesign" && len(args) > 1 && args[0] == "--display" {
			return []byte("Executable=" + args[len(args)-1] + "\ndesignated => identifier \"com.agentoverflow.app\" and certificate leaf[subject.OU] = TEAM\n"), nil
		}
		return nil, nil
	}
	if err := verifyMacUpdateWith("/Applications/Agent Overflow.app", "/tmp/Agent Overflow.app", run); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/codesign --verify --deep --strict /tmp/Agent Overflow.app",
		"/usr/bin/codesign --display --requirements - /Applications/Agent Overflow.app",
		"/usr/bin/codesign --display --requirements - /tmp/Agent Overflow.app",
		"/usr/bin/xcrun stapler validate /tmp/Agent Overflow.app",
		"/usr/sbin/spctl --assess --type execute /tmp/Agent Overflow.app",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v\nwant  %#v", calls, want)
	}
}

func TestVerifyMacUpdateRejectsDifferentSignerBeforeAssessment(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/codesign" && len(args) > 1 && args[0] == "--display" {
			team := "TEAM-A"
			if strings.HasPrefix(args[len(args)-1], "/tmp/") {
				team = "TEAM-B"
			}
			return []byte("designated => identifier \"com.agentoverflow.app\" and certificate leaf[subject.OU] = " + team), nil
		}
		return nil, nil
	}
	err := verifyMacUpdateWith("/Applications/Agent Overflow.app", "/tmp/Agent Overflow.app", run)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyMacUpdateReportsGatekeeperFailure(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/codesign" && len(args) > 1 && args[0] == "--display" {
			return []byte("designated => same"), nil
		}
		if name == "/usr/sbin/spctl" {
			return []byte("rejected"), errors.New("exit status 3")
		}
		return nil, nil
	}
	err := verifyMacUpdateWith("/Applications/Agent Overflow.app", "/tmp/Agent Overflow.app", run)
	if err == nil || !strings.Contains(err.Error(), "Gatekeeper") || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnclosingAppBundle(t *testing.T) {
	got, ok := enclosingAppBundle("/Applications/Agent Overflow.app/Contents/MacOS/agent-overflow")
	if !ok || got != "/Applications/Agent Overflow.app" {
		t.Fatalf("bundle = %q, %v", got, ok)
	}
	if _, ok := enclosingAppBundle("/usr/local/bin/agent-overflow"); ok {
		t.Fatal("bare executable reported as an app bundle")
	}
}
