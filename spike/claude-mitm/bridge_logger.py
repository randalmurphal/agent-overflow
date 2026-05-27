#!/usr/bin/env python3
"""Raw TCP byte-logger for the Remote Control local bridge.

`claude` dials `ws://localhost:8765` for its Remote Control bridge when
`LOCAL_BRIDGE` (or `USE_LOCAL_OAUTH`) is set, instead of the cloud relay
`wss://bridge.claudeusercontent.com`. We listen there and dump every byte the
session sends so we can tell *what* the bridge transport actually is:

  - HTTP `GET ... Upgrade: websocket`  -> it's a real WebSocket.
  - 4-byte LE length prefix + JSON      -> it's the raw-TCP framing seen in the
                                           binary ("connected to bridge server").

We intentionally send nothing back (no handshake completion) on this first pass;
the goal is only to capture the opening bytes / handshake request.
"""
import socket
import threading
import time
import sys

LOG = "/tmp/rc-bridge.log"


def hexdump(b, width=16):
    out = []
    for i in range(0, len(b), width):
        chunk = b[i:i + width]
        hexs = " ".join(f"{x:02x}" for x in chunk)
        ascii_ = "".join(chr(x) if 32 <= x < 127 else "." for x in chunk)
        out.append(f"  {i:04x}  {hexs:<{width*3}}  {ascii_}")
    return "\n".join(out)


def handle(conn, addr, logf):
    msg = f"\n=== bridge connection from {addr} at {time.time():.3f} ===\n"
    sys.stdout.write(msg); sys.stdout.flush(); logf.write(msg); logf.flush()
    conn.settimeout(8.0)
    total = b""
    try:
        while True:
            data = conn.recv(65536)
            if not data:
                logf.write("  <peer closed>\n"); logf.flush()
                break
            total += data
            block = f"  +{len(data)} bytes:\n{hexdump(data)}\n"
            sys.stdout.write(block); sys.stdout.flush()
            logf.write(block); logf.flush()
            # If it's an HTTP/WS upgrade, the request ends with a blank line.
            if b"\r\n\r\n" in total and total[:4] in (b"GET ", b"POST"):
                logf.write("  <looks like HTTP upgrade; holding open, sending nothing>\n")
                logf.flush()
    except socket.timeout:
        logf.write("  <recv timeout>\n"); logf.flush()
    except OSError as e:
        logf.write(f"  <error {e}>\n"); logf.flush()
    finally:
        conn.close()


def main():
    logf = open(LOG, "w")
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 8765))
    srv.listen(8)
    banner = "bridge_logger listening on 127.0.0.1:8765\n"
    sys.stdout.write(banner); sys.stdout.flush(); logf.write(banner); logf.flush()
    while True:
        conn, addr = srv.accept()
        threading.Thread(target=handle, args=(conn, addr, logf), daemon=True).start()


if __name__ == "__main__":
    main()
