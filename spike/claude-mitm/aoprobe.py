#!/usr/bin/env python3
"""Shared harness for the hook-channel interactive probes.

Encapsulates the isolation + PTY-driving that every probe needs:
  - seed an isolated CLAUDE_CONFIG_DIR (copied creds + GlobalConfig so onboarding
    is skipped + the throwaway cwd pre-trusted), with a settings.json that points
    the requested hook events at hook_relay.py,
  - fork `claude` in a PTY pointed at the capture proxy,
  - a read loop that answers the trust dialog, nudges submit if needed, and runs
    until a caller-supplied predicate or a timeout,
  - outcome detection from the hook payload log (never TUI scraping).

Outcomes are detected on the filesystem (hook payloads, marker files) and the
proxy wire — the PTY bytes are logged only for forensics.
"""
import json
import os
import pty
import re
import select
import struct
import fcntl
import termios
import shutil
import time

_CSI = re.compile(rb"\x1b\[[0-9;?]*[a-zA-Z]")
_OSC = re.compile(rb"\x1b\][0-9][^\x07]*\x07")


def _norm(data):
    """De-ANSI + keep printable ASCII + drop ALL whitespace + lowercase.

    The TUI positions text with cursor-movement escapes, so words in a rendered
    prompt are NOT separated by literal spaces; a raw substring match for
    "do you want to" fails. Normalizing to space-free lowercase lets us match
    stable markers like b"doyouwanttoproceed" / b"requiresconfirmation".
    """
    s = _OSC.sub(b"", _CSI.sub(b"", data))
    s = bytes(c for c in s if 0x20 <= c < 0x7f)
    return s.replace(b" ", b"").lower()

SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike/spike/claude-mitm"
HOOK = f"{SPIKE_DIR}/hook_relay.py"
CONFIG_DIR = "/tmp/aoclaude"
CWD = "/tmp/aocwd"
AOHOOK = "/tmp/aohook"
PAYLOADS = f"{AOHOOK}/payloads.jsonl"
CTL = f"{AOHOOK}/ctl.json"
REAL_CREDS = os.path.expanduser("~/.claude/.credentials.json")
REAL_GLOBAL = os.path.expanduser("~/.claude.json")

# PostToolUseFailure is a DISTINCT event from PostToolUse: it fires on tool
# FAILURE (non-zero exit / MCP isError), carrying an `error` field (exit code +
# stderr) and `is_interrupt`. PostToolUse fires only on SUCCESS. AO must register
# both to observe every completion (confirmed live on 2.1.158; the source also
# exposes PermissionDenied/PermissionRequest/Elicitation/SubagentStart/etc.).
ALL_EVENTS = ["PreToolUse", "PostToolUse", "PostToolUseFailure", "UserPromptSubmit",
              "Notification", "Stop", "SubagentStop", "PreCompact",
              "SessionStart", "SessionEnd"]


def seed_config(events, decision="allow", sleep_s=0.0, schema="modern",
                timeout_s=None, answer_questions=False,
                default_mode=None, ask_rules=None, mcp_servers=None):
    """Write the isolated config dir + control file. `events` get the relay hook.

    timeout_s: if set, the per-hook `timeout` field (seconds) — used to test the
      fail-open path (hook killed before it can return a decision).
    answer_questions: tells the relay to answer AskUserQuestion via updatedInput.
    default_mode: settings.json permissions.defaultMode (e.g. "default") — forces a
      known permission posture instead of any auto-accept opt-in carried in the
      copied GlobalConfig.
    ask_rules: settings.json permissions.ask list (e.g. ["Bash(rm:*)"]) — a rule
      that DETERMINISTICALLY forces matching tool calls to prompt in normal flow,
      so a probe can prove a command needs approval without guessing Claude's risk
      classifier.
    mcp_servers: dict of user-scope MCP servers to inject into .claude.json
      `mcpServers` (e.g. {"aoprobe": {"type": "stdio", "command": "python3",
      "args": [path]}}). User scope auto-enables (no project-MCP approval prompt).
    """
    os.makedirs(AOHOOK, exist_ok=True)
    os.makedirs(CWD, exist_ok=True)
    os.makedirs(CONFIG_DIR, exist_ok=True)
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
    if mcp_servers:
        gc.setdefault("mcpServers", {}).update(mcp_servers)
    json.dump(gc, open(f"{CONFIG_DIR}/.claude.json", "w"))
    cmd = {"type": "command", "command": f"python3 {HOOK}"}
    if timeout_s is not None:
        cmd["timeout"] = timeout_s
    hooks = {ev: [{"hooks": [cmd]}] for ev in events}
    settings = {"hooks": hooks}
    perms = {}
    if default_mode is not None:
        perms["defaultMode"] = default_mode
    if ask_rules:
        perms["ask"] = list(ask_rules)
    if perms:
        settings["permissions"] = perms
    json.dump(settings, open(f"{CONFIG_DIR}/settings.json", "w"))
    json.dump({"decision": decision, "sleep": sleep_s, "schema": schema,
               "answer_questions": answer_questions},
              open(CTL, "w"))
    try:
        os.remove(PAYLOADS)
    except OSError:
        pass


def set_ctl(**kw):
    try:
        cur = json.load(open(CTL))
    except (OSError, json.JSONDecodeError):
        cur = {}
    cur.update(kw)
    json.dump(cur, open(CTL, "w"))


def payloads():
    """All hook payloads captured so far, parsed."""
    out = []
    try:
        for ln in open(PAYLOADS, errors="replace"):
            try:
                e = json.loads(ln)
                e["payload"] = json.loads(e["raw"]) if e.get("raw") else {}
                out.append(e)
            except (json.JSONDecodeError, ValueError):
                continue
    except OSError:
        pass
    return out


def events_seen():
    return [(e.get("event"), e.get("tool")) for e in payloads()]


# ---- proxy wire capture helpers (the cap-*.jsonl the proxy writes) ----
# Records are kind-tagged: "request" {req_id, path, body}, "response_head"
# {req_id, status}, "response_chunk" {req_id, text}, "response_end" {req_id},
# "error" {req_id, stage, error}. Several probes need the wire, not just hooks:
# bg-task completion arrives as a `<task-notification>` in a later request body,
# and a mid-stream interrupt is visible as SSE deltas with no `message_stop`.
def wire_records(cap_path):
    out = []
    try:
        for ln in open(cap_path, errors="replace"):
            try:
                out.append(json.loads(ln))
            except (json.JSONDecodeError, ValueError):
                continue
    except OSError:
        pass
    return out


def wire_request_bodies(cap_path):
    """Parsed JSON body of every captured request, in order."""
    bodies = []
    for r in wire_records(cap_path):
        if r.get("kind") == "request" and r.get("body"):
            try:
                bodies.append(json.loads(r["body"]))
            except (json.JSONDecodeError, ValueError):
                continue
    return bodies


def wire_sse_by_req(cap_path):
    """req_id -> concatenated SSE text, plus the request's status/ended state.

    Returns {req_id: {"text": str, "status": int|None, "ended": bool}} so a probe
    can ask, per turn: did text stream? did `message_stop` arrive? did it end?
    """
    by = {}
    for r in wire_records(cap_path):
        rid = r.get("req_id")
        if rid is None:
            continue
        slot = by.setdefault(rid, {"text": "", "status": None, "ended": False})
        k = r.get("kind")
        if k == "response_chunk":
            slot["text"] += r.get("text", "")
        elif k == "response_head":
            slot["status"] = r.get("status")
        elif k == "response_end":
            slot["ended"] = True
    return by


class ClaudeSession:
    def __init__(self, prompt, base_url, pty_log, cols=200, rows=50, extra_args=None):
        self.prompt = prompt
        self.base_url = base_url
        self.pty_log = pty_log
        self.cols, self.rows = cols, rows
        self.extra_args = extra_args or []
        self.pid = self.master = None
        self.trust_handled = False
        self.submit_nudged = False
        self.saw_tui_perm = False
        self.keystrokes = 0       # bytes we wrote to the PTY (proves no driving)
        self._log = None
        self._start = None
        self._last_out = None
        self._rawtail = b""       # recent raw PTY bytes, normalized for detection

    def start(self):
        # Strip the PARENT agent's Claude/Anthropic env so the child starts in a
        # pristine posture. These probes run inside a Claude Code agent, which
        # exports CLAUDECODE=1, CLAUDE_VIBE, CLAUDE_CODE_ENTRYPOINT, session id,
        # etc.; a child `claude` that inherits them is told it is already nested
        # in a session and adopts an auto-accept posture (observed: it auto-ran
        # `rm` even under settings.json defaultMode=default + an explicit ask
        # rule). AO spawns claude with its own clean env, so stripping these is
        # the AO-representative posture, not a workaround. We re-add only what we
        # mean to set (config dir, base url, term).
        env = {k: v for k, v in os.environ.items()
               if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC"))}
        env["CLAUDE_CONFIG_DIR"] = CONFIG_DIR
        env["ANTHROPIC_BASE_URL"] = self.base_url
        env["TERM"] = "xterm-256color"
        os.chdir(CWD)
        self.pid, self.master = pty.fork()
        if self.pid == 0:
            # prompt=None launches an empty interactive composer (e.g. to paste
            # an attachment before typing); otherwise the positional auto-submits.
            argv = ["claude", *self.extra_args]
            if self.prompt is not None:
                argv.append(self.prompt)
            os.execvpe("claude", argv, env)
            os._exit(127)
        fcntl.ioctl(self.master, termios.TIOCSWINSZ,
                    struct.pack("HHHH", self.rows, self.cols, 0, 0))
        self._log = open(self.pty_log, "wb")
        self._start = self._last_out = time.time()

    def send(self, s):
        b = s.encode() if isinstance(s, str) else s
        self.keystrokes += len(b)
        os.write(self.master, b)

    def _pump_once(self, no_hook_yet):
        r, _, _ = select.select([self.master], [], [], 0.4)
        if self.master in r:
            try:
                data = os.read(self.master, 65536)
            except OSError:
                return False
            if not data:
                return False
            self._log.write(data); self._log.flush()
            self._last_out = time.time()
            # Normalize the recent raw stream (across chunk boundaries, so a split
            # escape can't break a marker) and match space-free prompt signals.
            self._rawtail = (self._rawtail + data)[-32768:]
            norm = _norm(self._rawtail)
            if not self.trust_handled and b"doyoutrust" in norm:
                self.send("\r"); self.trust_handled = True
            if (b"doyouwanttoproceed" in norm or b"requiresconfirmation" in norm
                    or b"wantsto" in norm):
                self.saw_tui_perm = True
        # submit nudge if the positional prompt didn't auto-send
        quiet = time.time() - self._last_out
        if (no_hook_yet and not self.submit_nudged
                and time.time() - self._start > 8 and quiet > 8):
            self.send("\r"); self.submit_nudged = True; self._last_out = time.time()
        return True

    def run(self, until, max_s=120, no_hook_probe=None):
        """Pump until `until()` returns True or max_s elapses.

        `no_hook_probe` (optional callable -> bool) tells the submit-nudger
        whether a hook has fired yet (so it only nudges before the turn starts).
        """
        while time.time() - self._start < max_s:
            no_hook_yet = no_hook_probe() if no_hook_probe else (not payloads())
            if not self._pump_once(no_hook_yet):
                break
            if until():
                # let the last event settle
                time.sleep(0.6)
                self._drain(0.6)
                return True
        return False

    def _drain(self, secs):
        end = time.time() + secs
        while time.time() < end:
            r, _, _ = select.select([self.master], [], [], 0.2)
            if self.master in r:
                try:
                    data = os.read(self.master, 65536)
                except OSError:
                    break
                if not data:
                    break
                self._log.write(data)
            else:
                break

    def exit(self):
        # CRITICAL: never send Enter here. If a native permission prompt is open,
        # Enter selects the default ("1. Yes") and RUNS the tool — which silently
        # corrupts any "did the tool run?" result. Esc cancels an open prompt
        # without accepting it; then interrupt and signal-kill.
        try:
            self.send("\x1b"); time.sleep(0.3)          # Esc: cancel open prompt
            self.send("\x03"); self.send("\x03"); time.sleep(0.2)  # Ctrl-C
            os.kill(self.pid, 15); time.sleep(0.2)
            os.kill(self.pid, 9)
        except OSError:
            pass
        if self._log:
            self._log.close()

    def elapsed(self):
        return time.time() - self._start if self._start else 0
