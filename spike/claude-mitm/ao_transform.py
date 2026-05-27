#!/usr/bin/env python3
"""proxy capture -> AO event stream  (the portable artifact for AO).

Transforms a logging-proxy capture of *interactive* Claude's /v1/messages
traffic into the event stream AO renders, modeled 1:1 on Claude Code headless
`--output-format stream-json --include-partial-messages`.

This is the LOGIC AO ports to Go (per docs/references/spike-policy.md). It is
validated here by `compare()` against a headless stream-json reference of the
same prompt — the bar is *semantic parity* (same event classes, same assembled
blocks, tool_results recovered), not byte-equality (interactive injects extra
auxiliary calls and has no `result`/`system:init` of its own to copy).

Pipeline:
  proxy events --group by req_id--> request records (body + SSE chunks + headers)
    --classify--> drop preflight (max_tokens<=1) and auxiliary (no tools)
    --per agent turn--> token-level `stream_event`s (passthrough of the SSE)
                        + an assembled `assistant` message
    --diff next agent request's `messages`--> `user` tool_result events
    --aggregate--> `system:init` (head) + `result` (tail) + `rate_limit_event`

What is NOT in /v1/messages and must come from elsewhere (documented, not faked):
  - session_id, cwd, mcp_servers, permissionMode  -> transcript / AO already knows
  - the final cost number                         -> derived from usage + pricing
"""
import json
import re
import sys
from collections import OrderedDict

# Anthropic server-side tool types carry a dated suffix, e.g. "web_search_20250305",
# "bash_20250124". A request whose tools are ALL server tools is a client-side
# tool's internal API sub-call (e.g. Claude Code's WebSearch calling the native
# web_search tool), not a main-agent-loop turn — the proxy sees it but headless
# stdout does not. We detect and skip these.
SERVER_TOOL_TYPE = re.compile(r"^[a-z_]+_\d{8}$")


def _is_server_tool(t):
    return isinstance(t, dict) and bool(SERVER_TOOL_TYPE.match(t.get("type", "")))


# --- load + group -----------------------------------------------------------

def load(path):
    out = []
    for line in open(path, errors="replace"):
        line = line.strip()
        if line:
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def group(events):
    reqs = OrderedDict()
    for e in events:
        rid = e.get("req_id")
        if rid is None:
            continue
        r = reqs.setdefault(rid, {"chunks": [], "resp_headers": {}})
        k = e.get("kind")
        if k == "request":
            r["method"] = e.get("method")
            r["path"] = e.get("path")
            r["body"] = e.get("body", "")
        elif k == "response_head":
            r["status"] = e.get("status")
            r["resp_headers"] = e.get("headers", {})
        elif k == "response_chunk":
            r["chunks"].append(e["text"])
    return reqs


def classify(body):
    """preflight | auxiliary | nested_subcall | agent.

    - preflight:      max_tokens<=1 (the quota probe)
    - auxiliary:      no tools (title/topic generation)
    - nested_subcall: all tools are server tools -> a client tool's internal API
                      call (e.g. WebSearch -> web_search_20250305); not main-loop
    - agent:          a real main-agent-loop turn (has custom Claude Code tools)
    """
    try:
        b = json.loads(body)
    except (json.JSONDecodeError, TypeError):
        return "unparseable", {}
    if (b.get("max_tokens") or 0) <= 1:
        return "preflight", b
    tools = b.get("tools", []) or []
    if len(tools) == 0:
        return "auxiliary", b
    if all(_is_server_tool(t) for t in tools):
        return "nested_subcall", b
    return "agent", b


def parse_sse(raw):
    out = []
    for line in raw.splitlines():
        if line.startswith("data:"):
            try:
                out.append(json.loads(line[5:].strip()))
            except json.JSONDecodeError:
                pass
    return out


# --- assemble one assistant message from a turn's SSE -----------------------

def assemble_assistant(sse):
    """Replay content_block_* deltas into a complete assistant message.

    Mirrors how a streaming client materializes the final message: text and
    thinking accumulate their deltas; tool_use accumulates partial_json and
    parses it at block stop. Returns (message_dict, stop_reason, usage).
    """
    blocks = {}          # index -> block dict
    json_buf = {}        # index -> partial_json string (tool_use)
    order = []
    stop_reason = None
    usage = None
    for ev in sse:
        t = ev.get("type")
        if t == "content_block_start":
            i = ev["index"]
            cb = dict(ev["content_block"])
            blocks[i] = cb
            order.append(i)
            # tool_use AND server_tool_use stream their input as input_json_delta.
            if cb.get("type") in ("tool_use", "server_tool_use"):
                json_buf[i] = ""
        elif t == "content_block_delta":
            i = ev["index"]
            d = ev["delta"]
            dt = d.get("type")
            if i not in blocks:                      # orphan delta — don't drop it
                blocks[i] = {"type": "unknown"}
                order.append(i)
            if dt == "text_delta":
                blocks[i]["text"] = blocks[i].get("text", "") + d.get("text", "")
            elif dt == "thinking_delta":
                blocks[i]["thinking"] = blocks[i].get("thinking", "") + d.get("thinking", "")
            elif dt == "signature_delta":
                blocks[i]["signature"] = blocks[i].get("signature", "") + d.get("signature", "")
            elif dt == "input_json_delta":
                json_buf[i] = json_buf.get(i, "") + d.get("partial_json", "")
            elif dt == "citations_delta":            # web-search / doc citations on text
                blocks[i].setdefault("citations", []).append(d.get("citation"))
        elif t == "content_block_stop":
            i = ev["index"]
            if json_buf.get(i):
                try:
                    blocks[i]["input"] = json.loads(json_buf[i])
                except json.JSONDecodeError:
                    blocks[i]["input"] = {"__unparsed_partial_json__": json_buf[i]}
        elif t == "message_delta":
            stop_reason = ev.get("delta", {}).get("stop_reason", stop_reason)
            usage = ev.get("usage", usage)
        elif t == "message_start":
            m = ev.get("message", {})
            usage = m.get("usage", usage)
    content = [blocks[i] for i in order]
    return {"role": "assistant", "content": content}, stop_reason, usage


# --- the transform ----------------------------------------------------------

def transform(capture_path):
    reqs = group(load(capture_path))

    # ordered agent turns: (record, parsed_body, sse)
    agent_turns = []
    rate_headers = {}
    for r in reqs.values():
        if r.get("path") != "/v1/messages":
            continue
        kind, b = classify(r.get("body", ""))
        # capture rate-limit headers from any messages response
        for k, v in (r.get("resp_headers") or {}).items():
            if k.lower().startswith("anthropic-ratelimit"):
                rate_headers[k] = v
        if kind != "agent":
            continue
        sse = parse_sse("".join(r["chunks"]))
        agent_turns.append((r, b, sse))

    events = []

    # system:init — model + tools come from the request; the rest is annotated.
    if agent_turns:
        b0 = agent_turns[0][1]
        events.append({
            "type": "system", "subtype": "init",
            "model": b0.get("model"),
            "tools": [t.get("name") for t in b0.get("tools", []) if isinstance(t, dict)],
            "_source": "model+tools from /v1/messages request; "
                       "session_id/cwd/mcp/permissionMode NOT on the wire "
                       "(read transcript or supply from AO)",
        })

    # Per-turn: token-level stream_events + assembled assistant + tool_results.
    total_in = total_out = cache_r = cache_c = 0
    last_stop = None
    for idx, (r, b, sse) in enumerate(agent_turns):
        for ev in sse:                       # token-level passthrough
            events.append({"type": "stream_event", "event": ev})
        msg, stop, usage = assemble_assistant(sse)
        events.append({"type": "assistant", "message": msg})
        last_stop = stop
        if usage:
            total_in += usage.get("input_tokens", 0) or 0
            total_out += usage.get("output_tokens", 0) or 0
            cache_r += usage.get("cache_read_input_tokens", 0) or 0
            cache_c += usage.get("cache_creation_input_tokens", 0) or 0

        # tool_results for THIS turn live in the NEXT agent request's messages.
        if idx + 1 < len(agent_turns):
            cur_msgs = b.get("messages", [])
            nxt_msgs = agent_turns[idx + 1][1].get("messages", [])
            for m in nxt_msgs[len(cur_msgs):]:
                if m.get("role") != "user":
                    continue
                content = m.get("content")
                results = [blk for blk in content
                           if isinstance(blk, dict) and blk.get("type") == "tool_result"] \
                    if isinstance(content, list) else []
                if results:
                    events.append({"type": "user", "message": {"role": "user", "content": results}})

    if agent_turns:
        events.append({
            "type": "result",
            "num_turns": len(agent_turns),
            "stop_reason": last_stop,
            "usage": {"input_tokens": total_in, "output_tokens": total_out,
                      "cache_read_input_tokens": cache_r,
                      "cache_creation_input_tokens": cache_c},
            "cost_usd": None,
            "_source": "usage summed from message_delta; cost = derive from pricing table",
        })
    if rate_headers:
        events.append({"type": "rate_limit_event", "headers": rate_headers})

    return events


# --- validation: semantic parity vs headless stream-json --------------------

def event_signature(events, is_headless):
    """Reduce an event stream to a comparable multiset of semantic classes."""
    from collections import Counter
    c = Counter()
    for e in events:
        t = e.get("type")
        if t == "stream_event":
            ev = e.get("event", {})
            et = ev.get("type")
            if et == "content_block_start":
                c["block:" + ev["content_block"]["type"]] += 1
            elif et == "content_block_delta":
                c["delta:" + ev["delta"].get("type", "?")] += 1
            elif et == "message_delta":
                c["stop:" + str(ev.get("delta", {}).get("stop_reason"))] += 1
        elif t in ("assistant", "user"):
            for blk in e.get("message", {}).get("content", []):
                if isinstance(blk, dict):
                    c["msg:" + t + ":" + blk.get("type", "?")] += 1
        elif t == "system":
            c["system:init"] += 1
        elif t == "result":
            c["result"] += 1
        elif t == "rate_limit_event":
            c["rate_limit_event"] += 1
    return c


def compare(ao_events, headless_path):
    head = load(headless_path)
    a = event_signature(ao_events, False)
    h = event_signature(head, True)
    keys = sorted(set(a) | set(h))
    print(f"{'signal class':36}  {'AO(transform)':>14}  {'headless':>9}  verdict")
    print("-" * 78)
    for k in keys:
        av, hv = a.get(k, 0), h.get(k, 0)
        if av and hv:
            verdict = "MATCH"
        elif hv and not av:
            verdict = "MISSING in AO"
        else:
            verdict = "AO-only (ok)"
        print(f"{k:36}  {av:>14}  {hv:>9}  {verdict}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: ao_transform.py <capture.jsonl> [headless-ref.jsonl]")
        sys.exit(2)
    ao = transform(sys.argv[1])
    from collections import Counter
    hist = Counter(e["type"] for e in ao)
    print("=== AO event stream produced by transform ===")
    for k, v in hist.most_common():
        print(f"  {v:4d}  {k}")
    print(f"  total: {len(ao)} events")
    if len(sys.argv) >= 3:
        print("\n=== semantic parity vs headless stream-json ===")
        compare(ao, sys.argv[2])
