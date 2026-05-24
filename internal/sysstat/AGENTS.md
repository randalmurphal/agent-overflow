# internal/sysstat

Host CPU + memory sampler backing the sidebar's system-stats footer.
Wraps `gopsutil/v4` so the rest of the codebase doesn't import a vendor
SDK directly.

## Ownership

- Pure read-only sampler. No emit, no goroutine — the App owns
  cadence and emission (see `app_sysstat.go`).
- Cross-platform via `gopsutil`: Linux uses `/proc`, macOS uses Mach
  host stats, Windows uses Performance Counters. No CGo.
- The package keeps two indirection points (`readCPUPercent`,
  `readMem`) for test-time substitution so unit tests don't depend on
  the developer machine's process state.

## Testing

- Unit tests substitute the indirection points to exercise the shape
  mapping + error propagation. Don't reach for real gopsutil reads
  from a test — gopsutil's first CPU read returns 0 (by design) and
  developer-machine memory is non-deterministic.
- `firstOrZero` is a pure helper; test it directly.

## Notes

- `Sample` returns the gopsutil-defined `MemUsedBytes` (on Linux:
  total - free - buffers - cached, matches htop). If the field ever
  feels off compared to `top`/`Activity Monitor`, the alternative is
  `Total - Available` from the same struct — same shape on the wire,
  just a different convention.
- The CPU delta state lives inside the `gopsutil/v4/cpu` package
  (process-global). That's why we call `Prime` once at startup rather
  than holding our own previous-tick snapshot.
