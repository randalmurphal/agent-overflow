#!/usr/bin/env python3
"""Probe: does headless `claude --effort <level>` actually reach the wire?

AO's headless provider (internal/provider/claude/session.go) appends
`--effort <level>` to the `claude --output-format stream-json ...` launch. This
probe answers the question we CANNOT settle by reading AO's code: on the real
binary, does that flag land in the Anthropic /v1/messages REQUEST BODY (expected
shape: output_config.effort) and vary by level — or is it silently ignored?

Method (the reusable part): redirect a headless `claude -p` turn through the
aocap capturing loopback proxy via ANTHROPIC_BASE_URL, once per effort level,
and dump every effort-bearing field of each captured request body. `-p` is a
plain non-interactive subprocess that makes exactly one /v1/messages call, so no
PTY/hook machinery is needed. The `--effort` flag is identical to the one AO
passes in stream-json mode; only the I/O framing differs, which does not affect
the model-request effort parameter.

Self-contained: seeds an isolated CLAUDE_CONFIG_DIR (copied creds + a pre-trusted
throwaway cwd), builds + runs aocap, runs one turn per level. Run:
    python3 probe_headless_effort.py

The captures under /tmp/ao-effort-caps hold request + response bodies (system
prompt, model output) — treat as sensitive; the probe prints a reminder to
delete them. The proxy captures bodies only, never the credential headers.
"""
import json
import os
import shutil
import signal
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
AOCAP_SRC = os.path.join(HERE, "aocap")
CONFIG_DIR = "/tmp/ao-effort-cfg"
CWD = "/tmp/ao-effort-cwd"
CAP_DIR = "/tmp/ao-effort-caps"
REAL_CREDS = os.path.expanduser("~/.claude/.credentials.json")
REAL_GLOBAL = os.path.expanduser("~/.claude.json")
MODEL = "claude-opus-4-8"
PROMPT = "Reply with exactly: OK"
# None = control (no flag). The rest are the values `claude --help` documents.
LEVELS = [None, "low", "medium", "high", "xhigh", "max"]


def seed():
    os.makedirs(CONFIG_DIR, exist_ok=True)
    os.makedirs(CWD, exist_ok=True)
    os.makedirs(CAP_DIR, exist_ok=True)
    shutil.copy(REAL_CREDS, f"{CONFIG_DIR}/.credentials.json")
    try:
        gc = json.load(open(REAL_GLOBAL))
    except (OSError, json.JSONDecodeError):
        gc = {"numStartups": 5, "theme": "dark", "hasCompletedOnboarding": True}
    gc.setdefault("projects", {})
    gc["projects"][CWD] = {
        "hasTrustDialogAccepted": True,
        "hasCompletedProjectOnboarding": True,
        "allowedTools": [], "history": [],
    }
    json.dump(gc, open(f"{CONFIG_DIR}/.claude.json", "w"))


def build_proxy():
    out = "/tmp/ao-effort-aocap"
    r = subprocess.run(["go", "build", "-o", out, "."], cwd=AOCAP_SRC,
                       capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"build aocap failed:\n{r.stderr}")
    return out


def clean_env():
    # Strip the parent agent's CLAUDE*/ANTHROPIC* env (this probe runs inside a
    # Claude Code agent that exports CLAUDECODE=1 etc.; a child that inherits it
    # adopts a nested/auto-accept posture). Re-add only what we mean to set —
    # the AO-representative clean-launch posture.
    env = {k: v for k, v in os.environ.items()
           if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC"))}
    env["CLAUDE_CONFIG_DIR"] = CONFIG_DIR
    env["TERM"] = "xterm-256color"
    return env


def run_level(proxy_bin, level):
    cap = os.path.join(CAP_DIR, f"cap-{level or 'none'}.jsonl")
    proc = subprocess.Popen([proxy_bin, "--listen", "127.0.0.1:0", "--cap", cap],
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    try:
        listen = json.loads(proc.stdout.readline())["listen"]
        env = clean_env()
        env["ANTHROPIC_BASE_URL"] = f"http://{listen}"
        args = ["claude", "-p", PROMPT, "--model", MODEL]
        if level is not None:
            args += ["--effort", level]
        try:
            r = subprocess.run(args, cwd=CWD, env=env, capture_output=True,
                               text=True, timeout=120)
            ok, err = r.returncode == 0, r.stderr.strip().replace("\n", " ")[:200]
        except subprocess.TimeoutExpired:
            ok, err = False, "TIMEOUT"
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    return cap, ok, err


def find_effort(obj, path=""):
    """Every key whose name contains 'effort', as (json_path, value) pairs."""
    hits = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            p = f"{path}.{k}" if path else k
            if "effort" in k.lower():
                hits.append((p, v))
            hits += find_effort(v, p)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            hits += find_effort(v, f"{path}[{i}]")
    return hits


def main_turn_body(cap):
    """The main agent turn's request body + how many auxiliary /v1/messages
    side-calls were filtered out.

    A single headless turn produces MORE than one /v1/messages request: Claude
    Code also fires auxiliary calls — title generation (json_schema {title}
    output_config.format, NO tools) and preflight/quota checks — and each runs
    at a FIXED effort:high regardless of --effort. Reading one of those is how
    the first cut of this probe got fooled into reporting "effort never
    changes". They are the same side-calls claudetui's classify.go drops; the
    real turn is the one carrying the full tool set, so select by max tools."""
    reqs = []
    for ln in open(cap, errors="replace"):
        try:
            r = json.loads(ln)
        except (json.JSONDecodeError, ValueError):
            continue
        if (r.get("kind") == "request" and r.get("path", "").endswith("/v1/messages")
                and r.get("body")):
            try:
                reqs.append(json.loads(r["body"]))
            except (json.JSONDecodeError, ValueError):
                pass
    if not reqs:
        return None, 0
    main = max(reqs, key=lambda b: len(b.get("tools") or []))
    return main, sum(1 for b in reqs if b is not main)


def main():
    if not os.path.exists(REAL_CREDS):
        sys.exit(f"no creds at {REAL_CREDS} — run `claude` once to authenticate")
    seed()
    proxy = build_proxy()
    print(f"model={MODEL}  prompt={PROMPT!r}\n")
    print(f"{'--effort flag':<14} {'ok':<5} {'effort on the MAIN agent turn':<34} {'tools':<7} aux side-calls filtered")
    print("-" * 92)
    for level in LEVELS:
        cap, ok, err = run_level(proxy, level)
        body, n_aux = main_turn_body(cap)
        if body is None:
            print(f"{str(level):<14} {str(ok):<5} <no /v1/messages request captured>  err={err}")
            continue
        hits = find_effort(body)
        desc = ", ".join(f"{p}={v!r}" for p, v in hits) if hits else "(no 'effort' key in body)"
        tools = len(body.get("tools") or [])
        print(f"{str(level):<14} {str(ok):<5} {desc:<34} {tools:<7} {n_aux}")
        if not ok:
            print(f"{'':<14} (stderr: {err})")
    print("\nMAIN turn = the request carrying the full tool set; the filtered")
    print("side-calls (title-gen / preflight) run at a fixed effort:high and are")
    print("not the turn AO's --effort targets.")
    print(f"captures: {CAP_DIR} (request/response bodies — delete after review)")


if __name__ == "__main__":
    main()
