#!/usr/bin/env python3
"""Probe: can a hook on Bash calls recover the OS PID of a BACKGROUND task, so AO
could bind `backgroundTaskId -> PID` and SIGTERM it directly (deterministic kill,
no model, no TUI)?

Two independent ways a hook could yield the PID:
  (A) the hook PAYLOAD carries it (a `pid`/`shellPid`/… field on the Bash
      PreToolUse/PostToolUse event), or
  (B) the hook PROCESS observes it — the PostToolUse(Bash) dispatch hook fires
      *while the background process is alive* and is handed the backgroundTaskId,
      so it can scan its own /proc tree, match the command, and read the PID.

Method: register a capture hook on Pre/PostToolUse(Bash) that dumps the full
payload + its process ancestry + a /proc snapshot of processes carrying a unique
marker. Drive interactive claude to run `sleep <MARKER>` with run_in_background.
At the dispatch PostToolUse(Bash), check (A) is the PID in the payload? and (B)
did the hook see the marker process, and is it reachable from the claude PID?
The probe also scans /proc itself (it knows claude's pid) as ground truth.

Isolated config + copied creds (aoprobe). Cleans up the marker process at the end
so no long-lived `sleep` leaks. Real PIDs are local/ephemeral, not secrets.
"""
import json
import os
import re
import signal
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/ao-cap-bashpid.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-bashpid.log"
CAPTURE_LOG = f"{aoprobe.AOHOOK}/bashpid_capture.jsonl"
CAPTURE_HOOK = f"{aoprobe.AOHOOK}/bashpid_capture.py"

# A unique sleep duration so the command is unambiguous in a /proc scan and can't
# collide with anything else. ~88 days; killed in cleanup regardless.
MARKER = "7654321"
PROMPT = (
    "Use the Bash tool with run_in_background=true to run exactly this command:\n"
    f"    sleep {MARKER}\n"
    "Then reply with exactly BG-STARTED and stop. Do NOT run it in the foreground."
)

# The capture hook, written to disk at runtime with the marker/log baked in. It
# logs every event lightly and does the heavy /proc snapshot only for Bash. It
# returns NO decision (exit 0, empty stdout) so the relay's `allow` stands and the
# tool still runs.
CAPTURE_SRC = r'''#!/usr/bin/env python3
import sys, os, json, time
LOG = "__LOG__"
MARKER = "__MARKER__"

def proc_info(pid):
    try:
        ppid = name = None
        with open("/proc/%d/status" % pid) as f:
            for ln in f:
                if ln.startswith("PPid:"):
                    ppid = int(ln.split()[1])
                elif ln.startswith("Name:"):
                    name = ln.split(":", 1)[1].strip()
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            cmd = f.read().replace(b"\x00", b" ").decode("utf-8", "replace").strip()
        return {"pid": pid, "ppid": ppid, "name": name, "cmdline": cmd}
    except (OSError, ValueError):
        return None

def all_procs():
    out = []
    for d in os.listdir("/proc"):
        if d.isdigit():
            info = proc_info(int(d))
            if info:
                out.append(info)
    return out

def ancestry(pid):
    chain, cur, n = [], pid, 0
    while cur and cur > 0 and n < 40:
        info = proc_info(cur)
        if not info:
            break
        chain.append(info)
        cur, n = info["ppid"], n + 1
    return chain

def main():
    raw = sys.stdin.read()
    try:
        payload = json.loads(raw)
    except Exception:
        payload = {}
    rec = {
        "ts": time.time(),
        "event": payload.get("hook_event_name"),
        "tool": payload.get("tool_name"),
        "hook_pid": os.getpid(),
        "hook_ppid": os.getppid(),
        "payload": payload,
    }
    if payload.get("tool_name") == "Bash":
        rec["ancestry"] = ancestry(os.getpid())
        procs = all_procs()
        rec["proc_count"] = len(procs)
        rec["marker_procs"] = [p for p in procs if MARKER in (p.get("cmdline") or "")]
    with open(LOG, "a") as f:
        f.write(json.dumps(rec) + "\n")
    sys.exit(0)  # no decision; relay's allow stands

main()
'''


def proc_info(pid):
    """Probe-side /proc reader (ground truth, independent of the hook)."""
    try:
        ppid = name = None
        with open("/proc/%d/status" % pid) as f:
            for ln in f:
                if ln.startswith("PPid:"):
                    ppid = int(ln.split()[1])
                elif ln.startswith("Name:"):
                    name = ln.split(":", 1)[1].strip()
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            cmd = f.read().replace(b"\x00", b" ").decode("utf-8", "replace").strip()
        return {"pid": pid, "ppid": ppid, "name": name, "cmdline": cmd}
    except (OSError, ValueError):
        return None


def scan_marker():
    out = []
    for d in os.listdir("/proc"):
        if d.isdigit():
            info = proc_info(int(d))
            if info and MARKER in (info.get("cmdline") or ""):
                out.append(info)
    return out


def ancestry(pid):
    chain, cur, n = [], pid, 0
    while cur and cur > 0 and n < 40:
        info = proc_info(cur)
        if not info:
            break
        chain.append(info)
        cur, n = info["ppid"], n + 1
    return chain


def capture_records():
    out = []
    try:
        for ln in open(CAPTURE_LOG, errors="replace"):
            try:
                out.append(json.loads(ln))
            except (json.JSONDecodeError, ValueError):
                continue
    except OSError:
        pass
    return out


def cleanup_marker():
    """Kill any process carrying the unique marker so no long sleep leaks."""
    killed = []
    for p in scan_marker():
        try:
            os.kill(p["pid"], signal.SIGKILL)
            killed.append(p["pid"])
        except OSError:
            pass
    return killed


def find_pid_in_payload(payload, real_pid):
    """Does the payload carry the real PID anywhere (by value or a pid-ish key)?"""
    blob = json.dumps(payload)
    by_value = bool(real_pid) and re.search(r'\b%d\b' % real_pid, blob) is not None
    pid_keys = re.findall(r'"([a-zA-Z_]*[pP]id[a-zA-Z_]*)"\s*:', blob)
    return by_value, sorted(set(pid_keys))


def main():
    # Base isolation + relay (copies creds, trusts cwd, relay on all events -> the
    # standard payload log + the `allow` decision the bg bash needs).
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow")
    # Write the capture hook and bolt it onto Pre/PostToolUse ALONGSIDE the relay.
    os.makedirs(aoprobe.AOHOOK, exist_ok=True)
    with open(CAPTURE_HOOK, "w") as f:
        f.write(CAPTURE_SRC.replace("__LOG__", CAPTURE_LOG).replace("__MARKER__", MARKER))
    relay = {"type": "command", "command": f"python3 {aoprobe.HOOK}"}
    capture = {"type": "command", "command": f"python3 {CAPTURE_HOOK}"}
    hooks = {ev: [{"hooks": [relay]}] for ev in aoprobe.ALL_EVENTS}
    hooks["PreToolUse"] = [{"hooks": [relay, capture]}]
    hooks["PostToolUse"] = [{"hooks": [relay, capture]}]
    json.dump({"hooks": hooks}, open(f"{aoprobe.CONFIG_DIR}/settings.json", "w"))
    for p in (CAPTURE_LOG, CAP):
        try:
            os.remove(p)
        except OSError:
            pass
    cleanup_marker()  # in case a previous run leaked one
    open(CAP, "w").close()

    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    def dispatched():
        # The dispatch is a PostToolUse(Bash) whose tool_response references a
        # background task id (proves it backgrounded, process still alive).
        for e in aoprobe.payloads():
            if e.get("event") == "PostToolUse" and e.get("tool") == "Bash":
                tr = json.dumps(e["payload"].get("tool_response", ""))
                if "backgroundtaskid" in tr.lower() or "background" in tr.lower():
                    return True
        return False

    ok = sess.run(until=dispatched, max_s=150)
    sess._drain(1.5)

    # Ground truth from the probe side (independent of the hook): find the marker
    # process and whether it is reachable from the claude PID.
    gt = scan_marker()
    real_pids = [p["pid"] for p in gt]
    claude_pid = sess.pid
    reach = []
    for p in gt:
        chain = ancestry(p["pid"])
        chain_pids = [c["pid"] for c in chain]
        reach.append({
            "pid": p["pid"], "ppid": p["ppid"], "cmdline": p["cmdline"],
            "claude_in_ancestry": claude_pid in chain_pids,
            "ancestry": [f'{c["pid"]}:{c["name"]}' for c in chain],
        })

    rows = aoprobe.payloads()
    bash_events = [e for e in rows if e.get("tool") == "Bash"
                   and e.get("event") in ("PreToolUse", "PostToolUse")]
    caps = capture_records()
    bash_caps = [c for c in caps if c.get("tool") == "Bash"]

    # (A) PID in payload?
    primary_real = real_pids[0] if real_pids else 0
    payload_hits = []
    for e in bash_events:
        by_value, pid_keys = find_pid_in_payload(e["payload"], primary_real)
        payload_hits.append({"event": e.get("event"), "pid_value_present": by_value,
                             "pid_like_keys": pid_keys})

    # (B) Did the hook SEE the marker process, and at which event?
    hook_view = []
    for c in bash_caps:
        mp = c.get("marker_procs", [])
        hook_view.append({
            "event": c.get("event"),
            "hook_pid": c.get("hook_pid"),
            "hook_ppid": c.get("hook_ppid"),
            "hook_ancestry": [f'{a["pid"]}:{a["name"]}' for a in c.get("ancestry", [])],
            "marker_procs_seen": [{"pid": p["pid"], "ppid": p["ppid"],
                                   "cmd": (p["cmdline"] or "")[:40]} for p in mp],
        })

    sess.exit()
    killed = cleanup_marker()

    # ---- verdict ----
    pid_in_payload = any(h["pid_value_present"] for h in payload_hits)
    post_saw = next((h for h in hook_view if h["event"] == "PostToolUse"), None)
    hook_saw_marker = bool(post_saw and post_saw["marker_procs_seen"])
    reachable = any(r["claude_in_ancestry"] for r in reach)

    print("==== BASH BACKGROUND-TASK PID-VIA-HOOK PROBE ====")
    print(f"dispatch observed: {ok}   claude pid: {claude_pid}")
    print(f"\n-- GROUND TRUTH (probe-side /proc scan for `sleep {MARKER}`) --")
    print(f"   marker processes: {real_pids or 'NONE (model may not have backgrounded it)'}")
    for r in reach:
        print(f"   pid={r['pid']} ppid={r['ppid']} claude_in_ancestry={r['claude_in_ancestry']}")
        print(f"      cmd: {r['cmdline'][:60]!r}")
        print(f"      ancestry: {r['ancestry']}")
    print(f"\n-- (A) PID IN HOOK PAYLOAD? --")
    for h in payload_hits:
        print(f"   {h['event']:<12} real-pid-present={h['pid_value_present']} "
              f"pid-like-keys={h['pid_like_keys']}")
    print(f"\n-- (B) HOOK PROCESS's VIEW (can it find the PID itself?) --")
    for h in hook_view:
        print(f"   [{h['event']}] hook_pid={h['hook_pid']} hook_ppid={h['hook_ppid']}")
        print(f"      hook ancestry: {h['hook_ancestry']}")
        print(f"      marker procs the hook saw: {h['marker_procs_seen']}")
    print(f"\n-- ANALYSIS --")
    print(f"   (A) PID present in any Bash payload field:     {pid_in_payload}")
    print(f"   (B) PostToolUse hook saw the marker process:   {hook_saw_marker}")
    print(f"   marker process reachable from claude pid:      {reachable}")
    bindable = pid_in_payload or hook_saw_marker
    print(f"\nVERDICT: taskId->PID binding from a Bash hook is "
          f"{'POSSIBLE' if bindable else 'NOT demonstrated'} "
          f"({'payload carries PID' if pid_in_payload else 'via /proc match at dispatch' if hook_saw_marker else 'neither path worked'})")
    print(f"cleanup: killed marker procs {killed}")
    print(f"pty log: {PTY_LOG}   capture log: {CAPTURE_LOG}")
    print("=================================================")


if __name__ == "__main__":
    main()
