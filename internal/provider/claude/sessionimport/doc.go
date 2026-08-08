// Package sessionimport reads existing Claude Code session transcripts so
// they can be imported into Agent Overflow as threads.
//
// It has three jobs and one hard rule.
//
//   - List (list.go / scan.go) enumerates every session under a Claude
//     projects directory using head/tail reads only — no full parse, no
//     JSON decode beyond the single line a first-prompt fallback needs.
//   - BuildBranches (dag.go) reconstructs the transcript's conversation DAG
//     and enumerates ALL of its leaves. One leaf is one importable thread.
//   - Convert (convert.go) projects a leaf's chain into the same
//     provider.ProviderEvent vocabulary a live session produces, wrapped in
//     importir.Event so the neutral writer can stamp provenance.
//
// The rule: this package NEVER resolves a home directory. Every path comes
// in through a parameter (Options.ProjectsDir, a session file path). The
// app layer owns os.UserHomeDir and the credential-home override, and
// passes the result down — see the root AGENTS.md invariant on provider
// homes.
package sessionimport
