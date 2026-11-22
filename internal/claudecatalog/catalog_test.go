package claudecatalog

import (
	"errors"
	"testing"

	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func catalogTestKey() provider.ProbeCacheKey {
	return provider.ProbeCacheKey{Binary: "/bin/claude", AccountID: "account", WorkDir: "/tmp"}
}

// A model answer stored from a probe or seeded from a saved record closes
// the channel a waiter took before it, and only then; the next waiter gets
// a fresh one. A command answer is not a model answer.
func TestAModelAnswerWakesWhoeverWaitsForOne(t *testing.T) {
	Reset()
	key := catalogTestKey()
	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	waiting := ModelsReported()
	commands := CommandCapture{}
	commands.Capture([]provider.SlashCommand{{Name: "usage"}}, nil)
	commands.Store(key)
	if closed(waiting) {
		t.Fatal("a command answer woke a wait for models")
	}
	probe := ModelCapture{}
	probe.Capture([]claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}}, nil)
	probe.Store(key)
	if !closed(waiting) {
		t.Fatal("a probe's model answer did not wake the wait")
	}
	next := ModelsReported()
	if closed(next) {
		t.Fatal("the next wait starts woken")
	}
	Seed(key, claudemodels.Snapshot{Wire: []claude.WireModel{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}}})
	if !closed(next) {
		t.Fatal("a seeded model answer did not wake the wait")
	}
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
