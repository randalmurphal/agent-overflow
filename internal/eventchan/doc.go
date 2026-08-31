// Package eventchan names every event channel the backend pushes to the
// frontend. It exists so a Go emit site cannot typo a channel or invent
// one without registering it.
//
// # The contract
//
// Adding a channel is TWO edits, and a test enforces both directions:
//
//  1. a named constant here, and
//  2. a `ChannelPolicy` row in `internal/transport/event_channels.go`
//     deciding who may receive its frames (Audience) and how deep its
//     replay ring is (Retention).
//
// `internal/transport.TestEveryEventChannelConstantHasAPolicyRow` and
// `TestEveryChannelPolicyRowHasAConstant` fail on either half being
// missing. The registry stays the source of truth for the two POLICY
// questions; this package is the source of truth for the SPELLING. The
// registry's rows are typed `Channel`, so the two cannot be parallel
// string tables that drift.
//
// # Why a named type
//
// `EventBus.Emit`, `(*App).emit`, the triage router's emit callback and
// the workflow engine's `Emitter` all take a `Channel`, not a string. A
// call site that wants a channel the registry has never heard of has to
// spell `eventchan.Channel("...")` — visibly deliberate, greppable, and
// exactly what the three unregistrable escape hatches do
// (`HarnessEmit`, `harness.Replayer`, and the Windows launcher's
// caller-supplied replay cursors). Everything else pays nothing.
//
// # Dependencies
//
// This package imports NOTHING — not even from this repo. Every layer
// that emits (`main`, `internal/transport`, `internal/triage`,
// `internal/workflow/engine`, `internal/browser`, `internal/notify`,
// `internal/selfupdate`) can therefore depend on it without acquiring a
// path to anything else. Keep it that way.
//
// # What this does NOT cover
//
// The client half of the wire. Subscribe frames, replay cursors, and
// the TypeScript SPA all carry plain strings chosen by the peer; those
// stay `string` and look the registry up directly. Converting untrusted
// wire input into this type would assert a registration nobody checked.
package eventchan
