//go:build !windows

package codexapp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

// A skills or usage read with no live session to ask starts its own
// app-server, which is handed to the service's reaper as it starts so it
// ends with the backend. The mock never answers, so each read ends at its
// deadline.
func TestAFreshProcessReadIsWatchedByTheReaper(t *testing.T) {
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "codex"), "#!/bin/sh\nexec sleep 60\n")
	for _, c := range []struct {
		name string
		read func(context.Context, *Service) error
	}{
		{"skills", func(ctx context.Context, s *Service) error {
			_, err := s.readSkills(ctx, binary, t.TempDir(), false)
			return err
		}},
		{"account usage", func(ctx context.Context, s *Service) error {
			_, err := s.readAccountUsage(ctx, binary, "")
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reaper := &procutiltest.Recorder{}
			workDir := t.TempDir()
			service := New(Deps{Reaper: reaper, ProbeWorkDir: func() string { return workDir }})
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := c.read(ctx, service); err == nil {
				t.Fatal("a read the app-server never answered succeeded")
			}
			if watched, _ := reaper.Calls(); len(watched) != 1 {
				t.Fatalf("watched %v, want the read's app-server", watched)
			}
		})
	}
}
