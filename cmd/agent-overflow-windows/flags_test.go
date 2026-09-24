//go:build windows

package main

import (
	"errors"
	"flag"
	"testing"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"
)

func TestParseLauncherFlags_Empty(t *testing.T) {
	got, err := parseLauncherFlags(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Distro != "" {
		t.Fatalf("Distro = %q, want empty", got.Distro)
	}
}

func TestParseLauncherFlags_Distro(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"long form equals", []string{"--distro=Ubuntu-24.04"}, "Ubuntu-24.04"},
		{"long form space", []string{"--distro", "Ubuntu-24.04"}, "Ubuntu-24.04"},
		{"short form equals", []string{"-distro=Ubuntu"}, "Ubuntu"},
		{"trims whitespace", []string{"--distro", "  Ubuntu  "}, "Ubuntu"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLauncherFlags(tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Distro != tc.want {
				t.Fatalf("Distro = %q, want %q", got.Distro, tc.want)
			}
		})
	}
}

// A relaunch carries the launch's choice of distro: the new launcher
// chooses the same distro and saves it exactly when the old one would have.
func TestParseLauncherFlags_CarriesTheDistroChoice(t *testing.T) {
	distros := []wsllauncher.Distro{{Name: "Ubuntu-24.04"}, {Name: "Debian"}}
	for _, transient := range []bool{false, true} {
		got, err := parseLauncherFlags(wsllauncher.DistroArgs("Debian", transient))
		if err != nil {
			t.Fatalf("transient=%v: %v", transient, err)
		}
		chosen, gotTransient := resolveChosenDistro(got, &wsldistro.Config{Distro: "Ubuntu-24.04"}, distros)
		if chosen != "Debian" || gotTransient != transient {
			t.Fatalf("transient=%v: chose %q, transient %v", transient, chosen, gotTransient)
		}
	}
	if _, err := parseLauncherFlags([]string{"--" + wsllauncher.RememberDistroFlag}); err == nil {
		t.Fatal("--remember-distro without --distro was accepted")
	}
}

func TestParseLauncherFlags_UnknownFlagErrors(t *testing.T) {
	_, err := parseLauncherFlags([]string{"--no-such-flag"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseLauncherFlags_AcceptsWindowsToastEmbeddingMode(t *testing.T) {
	got, err := parseLauncherFlags([]string{"-Embedding"})
	if err != nil {
		t.Fatalf("parse -Embedding: %v", err)
	}
	if !got.Embedding {
		t.Fatal("Embedding = false, want true")
	}
}

func TestParseLauncherFlags_HelpReturnsErrHelp(t *testing.T) {
	// flag.ContinueOnError surfaces -h via flag.ErrHelp so callers can
	// distinguish "user asked for help" from a real parse failure and
	// exit cleanly without logging a phantom error.
	_, err := parseLauncherFlags([]string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

func TestParseLauncherFlags_Profile(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"default", nil, "", ""},
		{"flag", []string{"--profile", "soak"}, "", appidentity.ProfileSoak},
		{"flag equals", []string{"--profile=SOAK"}, "", appidentity.ProfileSoak},
		{"env fallback", nil, "soak", appidentity.ProfileSoak},
		{"flag beats env", []string{"--profile", ""}, "soak", ""},
		{"harness flag", []string{"--profile", "harness"}, "", appidentity.ProfileHarness},
		{"harness env fallback", nil, "HARNESS", appidentity.ProfileHarness},
		{"perf flag", []string{"--profile", "perf"}, "", appidentity.ProfilePerf},
		{"perf env fallback", nil, "PERF", appidentity.ProfilePerf},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(profileEnv, tc.env)
			got, err := parseLauncherFlags(tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Profile != tc.want {
				t.Fatalf("Profile = %q, want %q", got.Profile, tc.want)
			}
		})
	}
}

// TestParseLauncherFlags_UnknownProfileErrors: a typo must never fall
// back to the default instance — that would point a soak run at the
// developer's own launcher.log, WebView2 profile, and instance identity.
func TestParseLauncherFlags_UnknownProfileErrors(t *testing.T) {
	if _, err := parseLauncherFlags([]string{"--profile", "sokk"}); err == nil {
		t.Fatal("expected error for an unknown profile")
	}
	t.Setenv(profileEnv, "sokk")
	if _, err := parseLauncherFlags(nil); err == nil {
		t.Fatal("expected error for an unknown profile from the environment")
	}
}

func TestParseLauncherFlags_UpdateModes(t *testing.T) {
	const id = "0123456789abcdef"
	got, err := parseLauncherFlags([]string{"--update-apply", id, "--distro", "Ubuntu", "--wait-pid", "4242", "--wait-start", "133000000000000000"})
	if err != nil {
		t.Fatalf("parse --update-apply: %v", err)
	}
	if got.UpdateApply != id || got.Distro != "Ubuntu" || got.Wait != (supervise.ProcessRef{PID: 4242, Start: "133000000000000000"}) {
		t.Fatalf("apply flags = %+v", got)
	}
	got, err = parseLauncherFlags([]string{
		"--update-preflight", `C:\cfg\runtime\preflight-` + id + ".json", "--update-id", id,
		"--distro", "Ubuntu", "--update-stable", "/home/u/.local/bin/agent-overflow",
	})
	if err != nil {
		t.Fatalf("parse --update-preflight: %v", err)
	}
	if got.UpdateID != id || got.Distro != "Ubuntu" || got.UpdateStable != "/home/u/.local/bin/agent-overflow" {
		t.Fatalf("preflight flags = %+v", got)
	}
	for _, bad := range [][]string{
		{"--update-apply", "../x", "--distro", "Ubuntu"},
		{"--update-apply", id},
		{"--update-apply", "0123456789ABCDEF", "--distro", "Ubuntu"},
		{"--update-preflight", "answer.json"},
		{"--update-preflight", "answer.json", "--update-id", "short"},
		{"--wait-pid", "-1"},
		{"--wait-pid", "4242"},
		{"--wait-start", "133000000000000000"},
	} {
		if _, err := parseLauncherFlags(bad); err == nil {
			t.Errorf("parseLauncherFlags(%q) accepted it", bad)
		}
	}
}
