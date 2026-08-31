package claudecatalog

import (
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

func catalogTestKey() provider.ProbeCacheKey {
	return provider.ProbeCacheKey{Binary: "/bin/claude", AccountID: "account", WorkDir: "/tmp"}
}

func TestCommandsDistinguishUnknownEmptyAndReported(t *testing.T) {
	Reset()
	key := catalogTestKey()
	if got, probed := Commands(key); probed || len(got) != 0 {
		t.Fatalf("before capture: got=%v probed=%v", got, probed)
	}

	empty := CommandCapture{}
	empty.Capture(nil, nil)
	empty.Store(key)
	if got, probed := Commands(key); !probed || len(got) != 0 {
		t.Fatalf("empty report: got=%v probed=%v", got, probed)
	}

	Reset()
	reported := CommandCapture{}
	reported.Capture([]provider.SlashCommand{{Name: "usage", Description: "Show plan usage"}}, nil)
	reported.Store(key)
	got, probed := Commands(key)
	if !probed || len(got) != 1 || got[0].Name != "usage" {
		t.Fatalf("reported commands: got=%v probed=%v", got, probed)
	}

	failed := CommandCapture{}
	failed.Capture(nil, errors.New("commands array unreadable"))
	failed.Store(key)
	got, probed = Commands(key)
	if !probed || len(got) != 1 || got[0].Name != "usage" {
		t.Fatalf("wire error replaced the previous answer: got=%v probed=%v", got, probed)
	}
}
