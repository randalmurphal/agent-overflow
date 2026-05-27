# Claude Code MITM Spike

Throwaway spike answering one question for Agent Overflow (AO): **Anthropic is
cutting subscription (Pro/Max OAuth) auth from headless/SDK mode; interactive
mode keeps it. Can AO drive *interactive* `claude` and still recover the same
token-level structured stream it gets from headless?**

**Finding: yes, with no binary patching and no TLS interception.** Point Claude
at a local logging **reverse proxy** via `ANTHROPIC_BASE_URL`; it captures the
full raw Anthropic Messages API SSE stream (text / thinking / tool_use deltas,
usage, stop_reason, rate-limit headers). Pair that with the `~/.claude`
transcript for clean tool results and you reconstruct a superset of the headless
`stream-json` shape — over the subscription-authed path that survives the cutoff.

The governing design rule (see the spec): AO sees Claude through two channels —
**wire** (proxy capture, high-reliability structured JSON) and **PTY/TUI**
(ANSI byte stream, low-reliability). **Drive through the PTY with keystrokes;
detect every outcome on the wire/filesystem — never by scraping TUI text.**

## Start here

1. [`FINDINGS.md`](FINDINGS.md) — *why* this architecture: the four interception
   paths tried, why the reverse proxy won, what the binary forecloses.
2. [`INTERACTIVE_DRIVING_SPEC.md`](INTERACTIVE_DRIVING_SPEC.md) — *how* AO drives
   every operation (submit, interrupt, revert, plan mode, permissions, …) and
   detects each outcome, grounded in the probes and captures here. §6 is the
   evidence index mapping each claim to a script + `artifacts/` file.

## Layout

| Path | What it is |
|---|---|
| `proxy/main.go` | The logging reverse proxy. Records request/response bodies as JSONL; redacts credential headers (auth/cookie/`*-token`/`*api-key`). The whole mechanism. |
| `ao_transform.py` | **The portable artifact.** Transforms a proxy capture of interactive `/v1/messages` into AO's event stream — the logic that ports to Go. |
| `analyze.py` | Three-way diff: proxy capture vs `~/.claude` transcript vs a headless `stream-json` reference. Backs the transform-equivalence claim (FINDINGS §11). |
| `drive_interactive.py` / `drive_multi.py` | PTY drivers: prove single- and multi-turn interactive sessions route `/v1/messages` through the proxy and can be driven + exited cleanly. |
| `probe_rewind.py` | `/rewind` revert flow — selector navigation, scope sub-choice, on-disk file restore, wire truncation, transcript fork. |
| `probe_interrupt.py` | Esc-while-working — aborted SSE (no `message_stop`), session-usable-after. |
| `probe_planmode.py` / `2` / `3` | Plan mode: launch-flag resolution (combo → bypass, not plan), end-to-end shift+tab→plan→`ExitPlanMode`→approve, and the Esc-reject path. |
| `perm_probe.py` / `perm_world.py` | Permission surface: confirm `can_use_tool` / `--permission-prompt-tool` is print-only, and that a static launch policy works interactively. |
| `captures/cap-*.jsonl` | Raw proxy wire captures (the `--log` output) — the ground truth the transform/analysis run against. One per probe (`-headless` is the reference for the equivalence check). |
| `artifacts/ao-*` | Probe run outputs: PTY terminal logs (`*.log`) + distilled result/marks JSON. Referenced by spec §6. |
| `preload.js`, `bridge_logger.py`, `rc_probe.py` | Superseded side-investigations — the Node `--require` preload (the native Bun binary ignores it, FINDINGS §1) and the Remote Control bridge probe (FINDINGS §10). Kept for the record. |

## Re-running

```sh
go build -C proxy -o /tmp/ao-proxy .
/tmp/ao-proxy --listen 127.0.0.1:8090 --log /tmp/cap.jsonl &
# probes read AO_BASE_URL / AO_CAP_LOG; each forks its own PTY:
AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG=/tmp/cap.jsonl python3 probe_planmode2.py
```

Scripts default to `/tmp` for scratch + capture output; the committed
`captures/` and `artifacts/` are the frozen run that backs the spec, so re-runs
regenerate to `/tmp` rather than overwriting the record.

## Status

Per [`docs/references/spike-policy.md`](../../docs/references/spike-policy.md)
this is a **throwaway spike, not for merge to `main`** — the *learning* ports
into AO's real Claude provider package; this tree is the durable record of what
was probed and verified (`claude 2.1.150`, subscription OAuth, 2026-05-26/27).
