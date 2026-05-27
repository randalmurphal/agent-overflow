#!/usr/bin/env python3
"""Three-way comparison for the Claude MITM spike.

Compares the same prompt as seen through:
  (a) MITM  — raw /v1/messages request bodies + SSE responses (proxy capture)
  (b) TRANS — the ~/.claude transcript JSONL written during the run
  (c) HEAD  — headless `--output-format stream-json --include-partial-messages`

It answers "what signal do we get and not get" by showing, for each headless
stream-json event, whether it is recoverable from the MITM capture and/or the
transcript, and how.

Usage:
  analyze.py <mitm.jsonl> <transcript.jsonl> <headless-stream.jsonl>
"""
import json
import sys
from collections import Counter, OrderedDict


def load_jsonl(path):
    out = []
    with open(path, errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def parse_sse(raw):
    """Yield decoded data objects from an SSE response body."""
    for line in raw.splitlines():
        if line.startswith("data:"):
            payload = line[5:].strip()
            try:
                yield json.loads(payload)
            except json.JSONDecodeError:
                continue


def group_mitm(events):
    """Group proxy events by req_id into request/response records."""
    reqs = OrderedDict()
    for e in events:
        rid = e.get("req_id")
        if rid is None:
            continue
        r = reqs.setdefault(rid, {"chunks": []})
        k = e["kind"]
        if k == "request":
            r.update(method=e["method"], path=e["path"], query=e.get("query", ""), body=e.get("body", ""))
        elif k == "response_head":
            r["status"] = e.get("status")
        elif k == "response_chunk":
            r["chunks"].append(e["text"])
        elif k == "error":
            r["error"] = e.get("error")
    return reqs


def classify_turn(body):
    """Distinguish the real agent turn from interactive auxiliary calls."""
    try:
        b = json.loads(body)
    except (json.JSONDecodeError, TypeError):
        return "unparseable", {}
    info = {
        "model": b.get("model"),
        "max_tokens": b.get("max_tokens"),
        "n_tools": len(b.get("tools", []) or []),
        "n_msgs": len(b.get("messages", []) or []),
        "stream": b.get("stream"),
    }
    if info["max_tokens"] == 1:
        return "quota_preflight", info
    if info["n_tools"] == 0:
        return "auxiliary(title/topic, no tools)", info
    return "AGENT_TURN", info


def main():
    if len(sys.argv) != 4:
        print(__doc__)
        sys.exit(2)
    mitm_path, trans_path, head_path = sys.argv[1:4]

    mitm = group_mitm(load_jsonl(mitm_path))
    trans = load_jsonl(trans_path)
    head = load_jsonl(head_path)

    print("=" * 78)
    print("(a) MITM CAPTURE  — what Claude actually sent/received over the wire")
    print("=" * 78)
    msg_turns = []
    for rid, r in mitm.items():
        path = r.get("path", "?")
        if path != "/v1/messages":
            print(f"  {r.get('method'):4} {path}  -> status {r.get('status')}  (non-message; ignored)")
            continue
        kind, info = classify_turn(r.get("body", ""))
        sse = list(parse_sse("".join(r["chunks"])))
        block_types = [d["content_block"]["type"] for d in sse if d.get("type") == "content_block_start"]
        stop = next((d["delta"].get("stop_reason") for d in sse if d.get("type") == "message_delta"), None)
        n_deltas = sum(1 for d in sse if d.get("type") == "content_block_delta")
        err = f"  ERROR={r['error']}" if r.get("error") else ""
        print(f"  [{kind}] status={r.get('status')} tools={info.get('n_tools')} msgs={info.get('n_msgs')} "
              f"max_tok={info.get('max_tokens')}{err}")
        print(f"     SSE blocks={block_types}  deltas={n_deltas}  stop_reason={stop}")
        if kind == "AGENT_TURN" and r.get("status") == 200:
            msg_turns.append((rid, r))

    print()
    print("  Tool-result reconstruction (diff of consecutive AGENT_TURN request bodies):")
    prev_msgs = None
    for rid, r in msg_turns:
        b = json.loads(r["body"])
        msgs = b.get("messages", [])
        if prev_msgs is not None:
            new = msgs[len(prev_msgs):]
            for m in new:
                content = m.get("content")
                if isinstance(content, list):
                    for blk in content:
                        if isinstance(blk, dict) and blk.get("type") == "tool_result":
                            c = blk.get("content")
                            txt = c if isinstance(c, str) else json.dumps(c)
                            print(f"     + tool_result for {blk.get('tool_use_id','?')[:18]}: {txt[:80]!r}")
        prev_msgs = msgs

    print()
    print("=" * 78)
    print("(b) TRANSCRIPT  — ~/.claude/projects/.../<session>.jsonl")
    print("=" * 78)
    tc = Counter()
    for r in trans:
        t = r.get("type")
        if t in ("assistant", "user"):
            blocks = [b.get("type") for b in r.get("message", {}).get("content", []) if isinstance(b, dict)]
            t = f"{t}:{','.join(blocks)}"
        tc[t] += 1
    for k, v in tc.items():
        print(f"  {v:3d}  {k}")

    print()
    print("=" * 78)
    print("(c) HEADLESS stream-json  — the reference shape we want to reproduce")
    print("=" * 78)
    hc = Counter()
    for e in head:
        t = e.get("type")
        if t == "stream_event":
            t = "stream_event:" + e.get("event", {}).get("type", "?")
        elif t in ("assistant", "user"):
            blocks = [b.get("type") for b in e.get("message", {}).get("content", []) if isinstance(b, dict)]
            t = f"{t}:{','.join(blocks)}"
        hc[t] += 1
    for k, v in hc.items():
        print(f"  {v:3d}  {k}")

    print()
    print("=" * 78)
    print("MAPPING: headless event  ->  source in MITM / TRANSCRIPT")
    print("=" * 78)
    rows = [
        ("system:init (session_id, tools, model, cwd, mcp)", "NOT in MITM (Claude-internal)", "first records / system rows"),
        ("stream_event:message_start", "SSE message_start", "n/a (transcript is per-message)"),
        ("stream_event:content_block_start/delta/stop", "SSE content_block_* (TOKEN-LEVEL)", "n/a"),
        ("assistant:text|thinking|tool_use", "assemble from SSE deltas", "assistant record message.content"),
        ("user:tool_result", "DIFF req N+1 vs N (next body)", "user record + toolUseResult sidecar"),
        ("stream_event:message_delta (usage)", "SSE message_delta", "message.usage on assistant record"),
        ("result (cost, duration, num_turns)", "compute from usage; cost = derived", "NOT present as one event"),
        ("rate_limit_event", "response headers (anthropic-ratelimit-*)", "not present"),
    ]
    for ev, mitm_src, trans_src in rows:
        print(f"\n  • {ev}")
        print(f"      MITM : {mitm_src}")
        print(f"      TRANS: {trans_src}")


if __name__ == "__main__":
    main()
