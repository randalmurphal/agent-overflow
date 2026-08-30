// Package governor coordinates harness runs that share a host.
//
// A reservation is persisted outside any checkout and protected by an OS
// lock. This matters because harnesses are commonly started from several
// worktrees, and Go mutexes cannot coordinate those processes. Reservations
// are capacity claims, not process supervision. Monitor provides edge-triggered
// per-run ceiling and host-available-floor observations. The package never
// starts, stops, or signals an application.
package governor
