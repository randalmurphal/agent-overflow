// Package sessionimport turns provider-session events recovered from
// disk into the same store rows a live session would have produced.
//
// Provider readers (internal/provider/claude/sessionimport,
// internal/provider/codex/rollout) parse their own session files into
// []importir.Event. This package is the neutral half: it consumes those
// events and builds one store.ImportBatch the caller commits with
// store.ApplyImportBatch.
//
// It deliberately does NOT drive triage.Router — the Router has
// live-only side effects and would persist imported prompts as
// "Injected provider context" notifications. Row shape parity instead
// comes from calling triage's exported, Router-free shaping helpers, and
// is pinned by parity_test.go.
package sessionimport
