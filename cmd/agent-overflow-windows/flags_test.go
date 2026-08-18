//go:build windows

package main

import (
	"errors"
	"flag"
	"testing"

	"agent-overflow/internal/appidentity"
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
