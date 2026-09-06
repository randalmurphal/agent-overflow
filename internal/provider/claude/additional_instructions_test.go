package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
)

func TestAdditionalInstructionsKeepThePrimaryPromptAndRemoveBothFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMPDIR", t.TempDir())
	script := filepath.Join(t.TempDir(), "mock-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec cat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := provider.SessionOptions{SystemPrompt: "user's primary prompt", AdditionalInstructions: "AO peer command guidance"}
	cfg := ConfigFromOptions(opts)
	cfg.Binary = script
	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for path, want := range map[string]string{s.systemPromptPath: opts.SystemPrompt, s.additionalPromptPath: opts.AdditionalInstructions} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("prompt file: %s %v", got, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private file: %v %v", info, err)
		}
	}
	primary, additional := s.systemPromptPath, s.additionalPromptPath
	_ = s.Close()
	for _, path := range []string{primary, additional} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("prompt survived close: %v", err)
		}
	}
	next := opts
	next.AdditionalInstructions = ""
	if _, live := PlanLiveUpdate(opts, next); live {
		t.Fatal("removing appended instructions must use the deferred restart boundary")
	}
}

func TestAdditionalInstructionFilesAreRemovedWhenSpawnFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	_, err := NewSession(context.Background(), testThread, Config{Binary: filepath.Join(t.TempDir(), "missing"), SystemPrompt: "primary", AdditionalInstructions: "extra"}, func(provider.ProviderEvent) {})
	if err == nil {
		t.Fatal("missing executable started")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed spawn leaked prompt files: %v %v", entries, err)
	}
}
