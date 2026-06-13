#!/usr/bin/env python3
"""Probe: cold-start first-message submit (paste+CR into a just-booted composer).

Reproduces the AO claude-tui bug the user hit: the FIRST Send after launch types
the text into the composer but does NOT submit; the SECOND message submits fine.

Hypothesis: AO's Send writes `[16xCtrl-U][60ms][bracketed-paste][60ms][CR]`. The
60ms gaps are gaps between our WRITES to the PTY master, not gaps in claude's
READS. On a cold start claude is still booting its Ink TUI and isn't draining
stdin yet, so all three writes accumulate in the slave input buffer and claude's
first read() gets `...\x1b[201~\r` in ONE chunk -> the CR is swallowed by the
paste handler instead of being seen as a separate submit keypress. On a warm
composer (2nd message) claude is idle and reading, so the paste and the CR are
two separate reads -> the CR submits.

Detection is CREDIT-FREE: ANTHROPIC_BASE_URL points at a LOCAL mock that records
whether a `POST /v1/messages` arrives (that POST == claude submitted the turn)
and returns a canned end_turn SSE so claude returns cleanly to idle. No real
upstream request, no credits, creds (if any) go only to localhost.

Scenarios:
  A repro-immediate   send the AO sequence the instant the PTY is forked
  A repro-250ms       send 250ms after fork
  A repro-800ms       send 800ms after fork
  B fix-2004h         wait for the bracketed-paste-enable marker (ESC[?2004h),
                      then send the AO sequence
  C fix-quiesce       wait until output has been quiet >=400ms (composer settled
                      / claude idle and reading), then send the AO sequence
  E fix-drain-cr      send clear+paste, then wait for the paste to render and the
                      output to quiesce, THEN send CR (gate the submit on drain)

Run from the spike dir. Reuses aoprobe.seed_config for the isolated config dir.
"""
import http.server
import os
import pty
import select
import socketserver
import struct
import fcntl
import termios
import threading
import time

import aoprobe

# The keystroke contract below mirrors AO's claude-tui Send. Source of truth:
# internal/provider/claudetui/session.go (composerClearKey/composerClearKeystrokes,
# bracketedPasteStart/End, submitKey, composerSettle). Re-sync if those change.
PROMPT = "test tests 123"
CTRL_U = b"\x15"            # composerClearKey
PASTE_START = b"\x1b[200~"  # bracketedPasteStart
PASTE_END = b"\x1b[201~"    # bracketedPasteEnd
CR = b"\r"                  # submitKey
BP_ENABLE = b"\x1b[?2004h"  # DECSET 2004 — claude enables bracketed paste when ready
SETTLE = 0.060             # AO's composerSettle (60ms)

CANNED_SSE = (
    "event: message_start\n"
    'data: {"type":"message_start","message":{"id":"msg_mock","type":"message",'
    '"role":"assistant","model":"claude-mock","content":[],"stop_reason":null,'
    '"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}\n\n'
    "event: content_block_start\n"
    'data: {"type":"content_block_start","index":0,'
    '"content_block":{"type":"text","text":""}}\n\n'
    "event: content_block_delta\n"
    'data: {"type":"content_block_delta","index":0,'
    '"delta":{"type":"text_delta","text":"ok"}}\n\n'
    "event: content_block_stop\n"
    'data: {"type":"content_block_stop","index":0}\n\n'
    "event: message_delta\n"
    'data: {"type":"message_delta","delta":{"stop_reason":"end_turn",'
    '"stop_sequence":null},"usage":{"output_tokens":1}}\n\n'
    "event: message_stop\n"
    'data: {"type":"message_stop"}\n\n'
).encode()


class Mock:
    """Serves canned SSE and flags a real turn submit.

    A submit is a POST /v1/messages whose BODY contains our unique prompt — that
    distinguishes our turn from claude's own startup /v1/messages traffic (warmup
    / classifier), which does NOT carry the prompt and would otherwise be a false
    positive.
    """

    def __init__(self):
        self.submit = threading.Event()   # set when a POST body matches any marker
        self.markers = [PROMPT.encode()]  # byte markers that identify a real turn
        self.seen = set()                 # markers observed in a POST body
        self.posts = []                   # [(path, matched, blen)] — never body content
        self.httpd = None
        self.port = None

    def reset(self, markers=None):
        self.submit.clear()
        self.markers = list(markers) if markers else [PROMPT.encode()]
        self.seen = set()
        self.posts = []

    def start(self):
        mock = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _body(self):
                n = int(self.headers.get("Content-Length", 0) or 0)
                return self.rfile.read(n) if n else b""

            def do_POST(self):
                body = self._body()
                path = self.path.split("?")[0]
                if path == "/v1/messages":
                    matched = [m for m in mock.markers if m in body]
                    # Record only length + match flag, never the body: it carries
                    # the prompt and the startup "quota" probe's device_id, and the
                    # harness discipline is to not retain request-body content.
                    mock.posts.append((path, bool(matched), len(body)))
                    if matched:
                        mock.seen.update(matched)
                        mock.submit.set()
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.end_headers()
                    try:
                        self.wfile.write(CANNED_SSE)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                # count_tokens / anything else: benign JSON, NOT a submit.
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"input_tokens":1,"output_tokens":1}')

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b"{}")

        self.httpd = socketserver.ThreadingTCPServer(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        return f"http://127.0.0.1:{self.port}"

    def stop(self):
        if self.httpd:
            self.httpd.shutdown()


def fork_claude(base_url, cols=160, rows=48):
    """Fork an interactive `claude` (empty composer, AO-representative flags)."""
    env = {k: v for k, v in os.environ.items()
           if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC"))}
    env["CLAUDE_CONFIG_DIR"] = aoprobe.CONFIG_DIR
    env["ANTHROPIC_BASE_URL"] = base_url
    env["TERM"] = "xterm-256color"
    os.chdir(aoprobe.CWD)
    pid, master = pty.fork()
    if pid == 0:
        argv = ["claude",
                "--permission-mode", "bypassPermissions",
                "--allow-dangerously-skip-permissions"]
        os.execvpe("claude", argv, env)
        os._exit(127)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    return pid, master


def ao_clear():
    return CTRL_U * 16


def ao_paste(text):
    safe = text.encode().replace(PASTE_END, b"")
    return PASTE_START + safe + PASTE_END


def run_scenario(mock, label, strategy, max_s=25):
    """Drive one fresh claude with `strategy`; report whether it submitted.

    strategy(ctx) is called every pump tick with a dict carrying:
      raw   : full raw PTY output so far (for escape-marker detection)
      quiet : seconds since last output byte
      since : seconds since fork
      send  : fn(bytes) -> write to the PTY
      state : a per-scenario scratch dict (persists across ticks)
    It performs its writes when its trigger condition is met.
    """
    mock.reset()
    pid, master = fork_claude(mock.url)
    raw = bytearray()
    last_out = time.time()
    start = time.time()
    state = {}
    submitted_at = None
    log_path = f"/tmp/cold_submit_{label}.log"
    log = open(log_path, "wb")
    try:
        while time.time() - start < max_s:
            r, _, _ = select.select([master], [], [], 0.05)
            now = time.time()
            if master in r:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                log.write(data)
                log.flush()
                raw += data
                last_out = now
            strategy({
                "raw": bytes(raw),
                "quiet": now - last_out,
                "since": now - start,
                "send": lambda b: os.write(master, b),
                "state": state,
            })
            if mock.submit.is_set():
                submitted_at = time.time() - start
                break
        time.sleep(0.6)
    finally:
        try:
            os.write(master, b"\x1b")  # Esc: cancel any open prompt — never Enter
            time.sleep(0.1)
            os.write(master, b"\x03\x03")
            os.kill(pid, 15)
            time.sleep(0.1)
            os.kill(pid, 9)
        except OSError:
            pass
        log.close()
    sent = state.get("sent_at")
    tail = aoprobe._norm(bytes(raw))[-90:].decode("ascii", "replace")
    has_text = b"testtests123" in aoprobe._norm(bytes(raw))
    posts = list(mock.posts)
    return {
        "label": label,
        "submitted": submitted_at is not None,
        "submit_s": round(submitted_at, 2) if submitted_at else None,
        "sent_at_s": round(sent, 2) if sent else None,
        "text_seen": has_text,
        "n_posts": len(posts),
        "n_startup": sum(1 for p in posts if not p[1]),  # non-prompt POSTs (e.g. quota)
        "tail": tail,
    }


# ---- strategies -----------------------------------------------------------

def make_immediate(delay):
    def strat(ctx):
        st = ctx["state"]
        if st.get("done"):
            return
        if ctx["since"] < delay:
            return
        send, since = ctx["send"], ctx["since"]
        send(ao_clear()); time.sleep(SETTLE)
        send(ao_paste(PROMPT)); time.sleep(SETTLE)
        send(CR)
        st["sent_at"] = since
        st["done"] = True
    return strat


def strat_2004h(ctx):
    st = ctx["state"]
    if st.get("done"):
        return
    if BP_ENABLE not in ctx["raw"]:
        return
    send = ctx["send"]
    send(ao_clear()); time.sleep(SETTLE)
    send(ao_paste(PROMPT)); time.sleep(SETTLE)
    send(CR)
    st["sent_at"] = ctx["since"]
    st["done"] = True


def make_quiesce(q, text=PROMPT):
    """Readiness gate = init output burst seen, then >=q s of quiet (claude idle
    and reading). This is the candidate AO fix, expressed PTY-side."""
    def strat(ctx):
        st = ctx["state"]
        if st.get("done"):
            return
        if len(ctx["raw"]) < 200 or ctx["quiet"] < q:
            return
        send = ctx["send"]
        send(ao_clear()); time.sleep(SETTLE)
        send(ao_paste(text)); time.sleep(SETTLE)
        send(CR)
        st["sent_at"] = ctx["since"]
        st["done"] = True
    return strat


def strat_echo_cr(ctx):
    """Per-send-robust alternative: paste once ready, then gate the CR on SEEING
    the pasted text echoed in the rendered composer (proves claude consumed the
    paste and is idle), instead of a fixed timer."""
    st = ctx["state"]
    if st.get("submitted"):
        return
    if not st.get("pasted"):
        if BP_ENABLE not in ctx["raw"] or ctx["quiet"] < 0.25:
            return
        send = ctx["send"]
        send(ao_clear()); time.sleep(SETTLE)
        send(ao_paste(PROMPT))
        st["pasted"] = True
        return
    if b"testtests123" not in aoprobe._norm(ctx["raw"]) or ctx["quiet"] < 0.12:
        return
    ctx["send"](CR)
    st["sent_at"] = ctx["since"]
    st["submitted"] = True


def run_two_messages(mock, max_s=40):
    """Reproduce the user's exact report in ONE claude: msg1 sent cold the instant
    the composer is up (no gate) -> expect STUCK; then msg2 sent once warm ->
    expect SUBMIT. Distinct markers tell the two turns apart on the wire."""
    m1, m2 = "alphaone msg", "bravotwo msg"
    mock.reset(markers=[b"alphaone", b"bravotwo"])
    pid, master = fork_claude(mock.url)
    raw = bytearray()
    start = time.time()
    phase = "boot"
    m1_sent = m2_sent = None
    log = open("/tmp/cold_submit_twomsg.log", "wb")
    try:
        while time.time() - start < max_s:
            r, _, _ = select.select([master], [], [], 0.05)
            if master in r:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                log.write(data); log.flush()
                raw += data
            since = time.time() - start
            # msg1: send cold as soon as the composer accepts input (2004h), no
            # readiness wait — this is the buggy path.
            if phase == "boot" and BP_ENABLE in bytes(raw):
                os.write(master, ao_clear()); time.sleep(SETTLE)
                os.write(master, ao_paste(m1)); time.sleep(SETTLE)
                os.write(master, CR)
                m1_sent = since
                phase = "await1"
                t_phase = time.time()
            # after giving msg1 time to (not) submit, send msg2 on the now-warm TUI.
            if phase == "await1" and time.time() - t_phase > 4.0:
                os.write(master, ao_clear()); time.sleep(SETTLE)
                os.write(master, ao_paste(m2)); time.sleep(SETTLE)
                os.write(master, CR)
                m2_sent = since
                phase = "await2"
                t_phase = time.time()
            if phase == "await2" and time.time() - t_phase > 4.0:
                break
        time.sleep(0.5)
    finally:
        try:
            os.write(master, b"\x1b"); time.sleep(0.1)
            os.write(master, b"\x03\x03"); os.kill(pid, 15)
            time.sleep(0.1); os.kill(pid, 9)
        except OSError:
            pass
        log.close()
    return {
        "m1_submitted": b"alphaone" in mock.seen,
        "m2_submitted": b"bravotwo" in mock.seen,
        "m1_sent_s": round(m1_sent, 2) if m1_sent else None,
        "m2_sent_s": round(m2_sent, 2) if m2_sent else None,
    }


def _preaccept_bypass():
    """Pre-accept the --dangerously-skip-permissions one-time dialog, mirroring a
    real config dir that has already accepted it. Newer builds read
    skipDangerousModePermissionPrompt in settings.json (migrated from the old
    global bypassPermissionsModeAccepted); set BOTH so any build is covered.
    Without this the acceptance modal blocks and the composer never renders, so
    the paste has nothing to land in (NOT the bug under test)."""
    import json
    sp = f"{aoprobe.CONFIG_DIR}/settings.json"
    s = json.load(open(sp))
    s["skipDangerousModePermissionPrompt"] = True
    json.dump(s, open(sp, "w"))
    gp = f"{aoprobe.CONFIG_DIR}/.claude.json"
    g = json.load(open(gp))
    g["bypassPermissionsModeAccepted"] = True
    json.dump(g, open(gp, "w"))


def main():
    aoprobe.seed_config(events=[])  # isolated config, no hooks needed here
    _preaccept_bypass()
    mock = Mock()
    mock.url = mock.start()
    print(f"mock listening at {mock.url}\n")

    # Repeat each strategy to gauge reliability (cold-start timing is racy).
    REPEATS = 3
    scenarios = [
        ("immediate", make_immediate(0.0)),   # the bug: send the instant we can
        ("2004h", strat_2004h),               # gate on bracketed-paste-enable (too early)
        ("quiesce-350", make_quiesce(0.35)),  # candidate fix: idle >=350ms
        ("quiesce-500", make_quiesce(0.50)),  # candidate fix: idle >=500ms
        ("echo-cr", strat_echo_cr),           # alt: gate CR on echoed text
    ]
    tally = {}
    for label, strat in scenarios:
        oks = 0
        for i in range(REPEATS):
            res = run_scenario(mock, f"{label}-{i}", strat)
            oks += 1 if res["submitted"] else 0
            print(f"--- {label}#{i}: submitted={res['submitted']} "
                  f"sent@{res['sent_at_s']}s submit@{res['submit_s']}s "
                  f"text={res['text_seen']} posts={res['n_posts']}", flush=True)
            time.sleep(0.4)
        tally[label] = (oks, REPEATS)

    print("\n--- two-message (user's exact report: msg1 cold, msg2 warm) ---", flush=True)
    two = []
    for i in range(2):
        r = run_two_messages(mock)
        two.append(r)
        print(f"    run#{i}: msg1_submitted={r['m1_submitted']} (sent@{r['m1_sent_s']}s)  "
              f"msg2_submitted={r['m2_submitted']} (sent@{r['m2_sent_s']}s)", flush=True)
        time.sleep(0.4)

    mock.stop()
    print("\n===== SUMMARY (submit = POST /v1/messages whose body carries the turn) =====")
    for label, (oks, n) in tally.items():
        print(f"  {label:<12} {oks}/{n} submitted")
    m1 = sum(1 for r in two if r["m1_submitted"])
    m2 = sum(1 for r in two if r["m2_submitted"])
    print(f"  two-message  msg1 {m1}/{len(two)} submitted   msg2 {m2}/{len(two)} submitted")


if __name__ == "__main__":
    main()
