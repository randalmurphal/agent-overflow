#!/usr/bin/env python3
"""Turn `probe sample` JSON lines into alert lines, and nothing else.

Reads sampler lines on stdin (one JSON object per sample), writes one line
per THRESHOLD BREACH on stdout. A monitor tailing the sampler log through
this stays silent while the app is healthy, which is the point: the
sampler runs for hours and only the excursions are worth a notification.

Thresholds are the post-2026-08-25 working numbers, set above normal
range rather than at it (renderer bounces 210-290MB on the GC cycle, GPU
250-410MB with tile bursts). Sustained alerts need N consecutive samples
so one burst does not fire.
"""
import json
import sys

RENDERER_MB = 450
BLINK_GC_MB = 350
GPU_MB = 460
GPU_SUSTAINED_MB = 380
GPU_SUSTAINED_SAMPLES = 5


def role(procs, want):
    for proc in procs.values():
        if proc.get("role") == want:
            return proc
    return None


def main() -> None:
    gpu_streak = 0
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            sample = json.loads(line)
        except ValueError:
            continue
        procs = sample.get("procs") or {}
        renderer = role(procs, "renderer")
        gpu = role(procs, "gpu")
        stamp = sample.get("t", "?")
        alerts = []

        if renderer:
            priv = renderer.get("privMB") or 0
            blink = renderer.get("blink_gc") or 0
            live = renderer.get("blink_live") or 0
            if priv > RENDERER_MB:
                alerts.append(f"RENDERER HIGH {priv}MB (blink_gc {blink}, live {live})")
            if blink > BLINK_GC_MB:
                alerts.append(f"BLINK_GC PILE {blink}MB against {live}MB live")

        if gpu:
            priv = gpu.get("privMB") or 0
            if priv > GPU_MB:
                alerts.append(f"GPU HIGH {priv}MB")
            if priv > GPU_SUSTAINED_MB:
                gpu_streak += 1
                if gpu_streak == GPU_SUSTAINED_SAMPLES:
                    alerts.append(f"GPU SUSTAINED {priv}MB for {gpu_streak} samples")
            else:
                gpu_streak = 0

        for alert in alerts:
            print(f"{stamp} {alert}", flush=True)


if __name__ == "__main__":
    main()
