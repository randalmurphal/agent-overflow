// Package importir is the neutral intermediate representation the session
// importer passes from a provider-specific reader to the provider-agnostic
// writer.
//
// Provider packages (internal/provider/claude/sessionimport,
// internal/provider/codex/rollout) read their own on-disk session files and
// emit []Event; internal/sessionimport turns those into store rows. Neither
// side may import the other, so the shared vocabulary lives here — and it
// stays deliberately thin: stdlib plus internal/provider only, so a
// provider package can depend on it without acquiring a path to store or
// triage.
package importir
