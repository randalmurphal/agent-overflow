#!/usr/bin/env python3
"""Live 2.1.170 spike: confirm the two claude-tui findings on the real binary.

Reproduces the user's scenario (a thinking turn + a mermaid turn that elicits an
offer and triggers the TUI's internal title generation) through a capturing proxy
that MIRRORS production gateway.go (plain Go forward to api.anthropic.com). It
answers, against the real wire:

  Q1 (thinking): launched with `--thinking-display summarized`, does the TUI put
     `thinking.display:"summarized"` on its /v1/messages requests AND does the
     response stream `thinking_delta` with non-empty text? (Opus 4.7+ default is
     `omitted` -> empty thinking + signature only; that is the bug the user hit.)
  Q2 (phantom): what does the TUI's internal title-generation request look like on
     2.1.170 -- does it carry tools (so classify.go marks it classAgent and the
     title leaks as a phantom assistant turn), and what is a robust marker to drop
     it by?

Genuine subscription traffic (real OAuth bearer copied into an isolated
CLAUDE_CONFIG_DIR). Short prompts keep cost negligible. Default permission mode
(no bypass flags) -- the pure-text scenario needs no tool approvals, and the
thinking/title request shapes are permission-mode-independent, so we avoid the
bypass-acceptance dialog. Teardown sends Esc (never Enter).
"""
import json
import os
import re
import subprocess
import sys
import time

import aoprobe

PROXY_SRC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "aocap")
PROXY_BIN = "/tmp/aocap/aocap"
UPSTREAM = "https://api.anthropic.com"
CAP = "/tmp/cap-thinking-title.jsonl"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-thinking-title.log"

TURN1 = os.environ.get("AO_TURN1",
                       "Think hard and show your step-by-step reasoning: what is 47 times 83?")
TURN2 = os.environ.get("AO_TURN2",
                       "Now give me a tiny mermaid flowchart diagram of a login flow.")
# Extra launch args. THINK_DISPLAY can be cleared via env to capture a control
# run; VERBOSE adds --verbose to test the user's fallback hypothesis.
EXTRA = ["--model", "opus"]
if os.environ.get("AO_NO_THINK_DISPLAY") != "1":
    EXTRA += ["--thinking-display", "summarized"]
if os.environ.get("AO_VERBOSE") == "1":
    EXTRA += ["--verbose"]

SERVER_TOOL = re.compile(r'^[a-z_]+_\d{8}$')


def read_cap():
    out = []
    if not os.path.exists(CAP):
        return out
    for ln in open(CAP, errors="replace"):
        ln = ln.strip()
        if not ln:
            continue
        try:
            out.append(json.loads(ln))
        except json.JSONDecodeError:
            pass
    return out


def per_req(cap):
    """req_id -> {body(parsed|None), sse(str), status, path}."""
    by = {}
    for r in cap:
        rid = r.get("req_id")
        if rid is None:
            continue
        slot = by.setdefault(rid, {"body": None, "sse": "", "status": None, "path": None})
        k = r.get("kind")
        if k == "request":
            slot["path"] = r.get("path")
            try:
                slot["body"] = json.loads(r.get("body") or "")
            except json.JSONDecodeError:
                slot["body"] = None
        elif k == "response_head":
            slot["status"] = r.get("status")
        elif k == "response_chunk":
            slot["sse"] += r.get("text", "")
    return by


def classify(b):
    """Go classify.go port."""
    if not isinstance(b, dict):
        return "unparseable"
    if (b.get("max_tokens") or 0) <= 1:
        return "preflight"
    tools = b.get("tools") or []
    if len(tools) == 0:
        return "auxiliary"
    if tools and all(SERVER_TOOL.match(t.get("type", "")) for t in tools):
        return "nested-subcall"
    return "AGENT"


def messages_count(b):
    return len(b.get("messages") or []) if isinstance(b, dict) else 0


def agent_turns_done(cap):
    """Count AGENT /v1/messages whose SSE shows end_turn (a completed real turn)."""
    n = 0
    for rid, s in per_req(cap).items():
        if s["path"] != "/v1/messages":
            continue
        if classify(s["body"]) == "AGENT" and '"stop_reason":"end_turn"' in s["sse"]:
            n += 1
    return n


def system_text(b):
    sysv = b.get("system") if isinstance(b, dict) else None
    if isinstance(sysv, str):
        return sysv
    if isinstance(sysv, list):
        return "\n".join(x.get("text", "") if isinstance(x, dict) else str(x) for x in sysv)
    return ""


def main():
    os.makedirs(os.path.dirname(PROXY_BIN), exist_ok=True)
    build = subprocess.run(["go", "build", "-o", PROXY_BIN, PROXY_SRC],
                           capture_output=True, text=True)
    if build.returncode != 0:
        print("ABORT: proxy build failed:\n", build.stderr)
        sys.exit(1)
    try:
        os.remove(CAP)
    except OSError:
        pass

    aoprobe.seed_config(events=["UserPromptSubmit", "Stop"], decision="allow")

    proxy = subprocess.Popen(
        [PROXY_BIN, "--listen", "127.0.0.1:0", "--upstream", UPSTREAM, "--cap", CAP],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    listen = None
    deadline = time.time() + 10
    while time.time() < deadline:
        line = proxy.stdout.readline()
        if not line:
            if proxy.poll() is not None:
                break
            continue
        try:
            listen = json.loads(line)["listen"]
            break
        except (json.JSONDecodeError, KeyError):
            continue
    if not listen:
        print("ABORT: proxy never reported listen;",
              (proxy.stderr.read()[-400:] if proxy.stderr else ""))
        proxy.kill()
        sys.exit(1)
    base_url = f"http://{listen}"
    print(f"[spike] proxy={listen}  extra_args={EXTRA}", file=sys.stderr)

    sess = aoprobe.ClaudeSession(TURN1, base_url, PTY_LOG, extra_args=EXTRA)
    try:
        sess.start()

        # Turn 1: positional prompt auto-submits. Wait for first end_turn.
        ok1 = sess.run(lambda: agent_turns_done(read_cap()) >= 1, max_s=90)
        print(f"[spike] turn1 done={ok1} at {sess.elapsed():.1f}s", file=sys.stderr)
        time.sleep(1.0)

        # Turn 2: send via PTY (warm). text, settle, CR.
        sess.send(TURN2)
        time.sleep(0.6)
        sess.send("\r")
        ok2 = sess.run(lambda: agent_turns_done(read_cap()) >= 2, max_s=90)
        print(f"[spike] turn2 done={ok2} at {sess.elapsed():.1f}s", file=sys.stderr)

        # Give the TUI time to fire title generation (it's async, post-turn).
        title_deadline = time.time() + 15
        while time.time() < title_deadline:
            sess._pump_once(False)
            if any(classify(s["body"]) != "AGENT" and "title" in system_text(s["body"]).lower()
                   for s in per_req(read_cap()).values() if s["path"] == "/v1/messages"):
                break
        sess._drain(2.0)
    finally:
        sess.exit()
        proxy.terminate()
        try:
            proxy.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proxy.kill()

    # ---------------- analysis ----------------
    cap = read_cap()
    reqs = per_req(cap)
    msg_reqs = [(rid, s) for rid, s in sorted(reqs.items()) if s["path"] == "/v1/messages"]

    print("\n================ /v1/messages REQUESTS ================")
    title_reqs = []
    agent_reqs = []
    for rid, s in msg_reqs:
        b = s["body"]
        cls = classify(b)
        thinking = b.get("thinking") if isinstance(b, dict) else None
        n_tools = len(b.get("tools") or []) if isinstance(b, dict) else 0
        sysv = system_text(b)
        is_title = "generate a concise" in sysv.lower() or "title" in sysv.lower()
        marker = sysv[:90].replace("\n", " ")
        print(f"\n  req#{rid} [{cls}] status={s['status']} model={b.get('model') if isinstance(b,dict) else '?'}")
        print(f"     max_tokens={b.get('max_tokens') if isinstance(b,dict) else '?'} n_tools={n_tools} n_msgs={messages_count(b)}")
        print(f"     thinking={thinking!r}")
        print(f"     system[:90]={marker!r}")
        if is_title:
            title_reqs.append((rid, s, cls, n_tools, sysv))
        if cls == "AGENT":
            agent_reqs.append((rid, s))

    print("\n================ Q1: THINKING ON THE WIRE ================")
    for rid, s in agent_reqs:
        sse = s["sse"]
        b = s["body"]
        td = re.findall(r'"thinking_delta"[^}]*"thinking":"((?:[^"\\]|\\.)*)"', sse)
        td_text = "".join(td)
        sig = sse.count('"signature_delta"')
        textd = sse.count('"text_delta"')
        disp = (b.get("thinking") or {}).get("display") if isinstance(b, dict) and isinstance(b.get("thinking"), dict) else None
        print(f"  AGENT req#{rid}: request thinking.display={disp!r}")
        print(f"     thinking_delta events={len(td)}  thinking_delta TEXT chars={len(td_text)}")
        print(f"     signature_delta events={sig}  text_delta events={textd}")
        if td_text:
            print(f"     thinking sample: {td_text[:160]!r}")
    any_thinking = any(re.search(r'"thinking_delta".*"thinking":"[^"]', s["sse"]) for _, s in agent_reqs)
    print(f"\n  => thinking_delta TEXT present on wire: {any_thinking}")

    print("\n================ Q2: TITLE-GEN / PHANTOM ================")
    if not title_reqs:
        print("  No title-generation request captured (may need more turns / longer wait).")
    for rid, s, cls, n_tools, sysv in title_reqs:
        leaks = (cls == "AGENT")
        print(f"  title req#{rid}: classify={cls}  n_tools={n_tools}  WOULD-LEAK-AS-PHANTOM={leaks}")
        # show the response text (what would become the phantom)
        rt = re.findall(r'"text_delta"[^}]*"text":"((?:[^"\\]|\\.)*)"', s["sse"])
        print(f"     response text: {''.join(rt)[:160]!r}")
        # robust marker candidates
        low = sysv.lower()
        print(f"     marker 'generate a concise' present: {'generate a concise' in low}")
        print(f"     marker '<session>' wrapper in messages: "
              f"{any('<session>' in (json.dumps(m)) for m in (s['body'].get('messages') or []))}")

    print("\n  cap:", CAP, "| pty:", PTY_LOG)
    statuses = [s["status"] for _, s in msg_reqs]
    print("  statuses:", statuses, "(all 200 => no fingerprint rejection)")


if __name__ == "__main__":
    main()
