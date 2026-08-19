---
name: trace-triage
description: Triage a ui-trace bug-report bookmark. Use whenever the user pastes a ui-trace/bookmarks/bug-report-*.jsonl path (usually with a symptom description or screenshot) for jumps, flickers, freezes, scroll misbehavior, or layout shifts. Runs the standard evidence passes over the trace and returns ruled-in/ruled-out hypotheses with record-level evidence, never a bare "needs more instrumentation".
---

# Trace triage

The user pressed Ctrl+Shift+B after seeing a rendering bug. That wrote a bookmark: a copy of the rolling render trace (rotated `.1` history first, then the current file, earliest-first, up to ~20MB) into `<configDir>/ui-trace/bookmarks/bug-report-<UTC>.jsonl`. The deliverable is a verdict per hypothesis with record-level evidence, plus a leading hypothesis and how to falsify it today.

Traces exist only in `DEBUG=1` builds (`VITE_AGENT_OVERFLOW_UI_TRACE`). The oracle tier (`UI_ORACLES`) adds the row-resize / margin-divergence / reasoning-tail probes and the throttled `*.dom` snapshots; a light build has event traces and spring telemetry only. Check which tier the trace carries before declaring an oracle "silent".

## Format

One JSON object per line: `{seq, at, label, data}`. `at` is a monotonic ms clock; correlate by deltas relative to the marker, not wall clock. Producer: `frontend/src/lib/utils/uiRenderTrace.ts`; writer caps and rotation: `internal/uitrace/uitrace.go`.

The bug moment is marked: grep `user.bugReport`. The marker's `data` carries `capturedAt`, `href`, the full `stickState`, and `paneGeometry`; preceding `user.bugReportRecFrame` lines (`recId === capturedAt`) are the rolling pre-press pane-geometry frames. The press always comes after the user saw the bug, so the evidence window is the seconds BEFORE the marker (start with 30s, widen if quiet).

## Process

1. **Inputs.** Bookmark path + the user's symptom prose; screenshots if given. Get the symptom in observable terms (what moved, when, how far) before reading records. `wc -c` the file first; never read it wholesale. Work with `rg`/`jq` windows keyed on `seq` ranges around each marker.
2. **Markers.** `rg -n 'user.bugReport' <file>`. Multiple markers = multiple incidents; triage each window separately. Compare marker `stickState`/`paneGeometry` against what the user says they saw.
3. **Standard passes** over each window. Each pass ends ruled-in or ruled-out, with `seq` numbers as evidence:
   - **Frame health:** `frame.loaf` records (frames ≥50ms, script attribution). A visible jump with clean `frame.loaf` AND clean `scroll.spring.chase` cadence is renderer-exonerating evidence: the frames died after commit, in the compositor/presentation path (WebView2/DWM), which no renderer-side instrument can see. Confirm `frame.loaf.install` reported `supported: true` before treating absence as clean.
   - **Runtime errors:** the sibling always-on `<configDir>/ui-trace/frontend-errors.jsonl` for the same period. A silent render throw permanently leaks Svelte deriveds; it belongs in every triage.
   - **Scroll engine:** `scroll.writeRefusal` (the guard's MOVED/REFUSED/INCONCLUSIVE classification), `scroll.spring.chase` gaps, `scroll.pause.acquire`/`release` pairing, `scroll.forceStick.*`, `scroll.engineCompensation`, `scroll.intent.*` around the moment.
   - **Layout oracles** (oracle tier): `timeline.row.resize`, `timeline.margin.diverge`, `timeline.reasoning.tailJump`, `timeline.restore.*` anchor entries/bails.
   - **State snapshots:** nearest `chat.state` / `chat.dom` / `timeline.state` / `plan-sidebar.state` to the marker, diffed against the marker's own stick state.
4. **Prior incidents.** Check the symptom against known classes before inventing a new one: docs under `docs/architecture/` (frontend-scroll doctrine) and git history of the scroll/timeline utils. A match means verifying the existing fix is present and firing, not re-deriving it.
5. **Verdict.** Report: symptom restated in observable terms → per-pass ruled-in/ruled-out with evidence → leading hypothesis → how to falsify it today (harness repro, browser test, targeted spike). INCONCLUSIVE is a legal verdict for a pass, but the overall answer is never "wait for it to happen again": if the trace cannot discriminate the surviving hypotheses, the deliverable becomes the always-on forensic addition that would, proposed concretely.
6. **Ledger.** Fold the verdicts into the running investigation ledger (facts attested, causes ruled out) so a later session never re-proposes what this trace already excluded.
