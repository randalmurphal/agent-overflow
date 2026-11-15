// Package orphanreaper guarantees that the process groups the app watches
// don't outlive it on macOS and Linux. A parent-death signal reaches only a
// group's leader, so it cannot end what a shell leader started.
//
// Two layers:
//
//   - A live sidecar process (the `__reap` subcommand) holds the read end
//     of a control pipe whose write end the app keeps. The app sends
//     `watch <pgid>` / `release <pgid>` as sessions start and stop. If the
//     app dies by any means — clean exit, panic, or SIGKILL — the kernel
//     closes its pipe end, the sidecar reads EOF, and it kills every
//     still-watched process group. This is a userspace stand-in for
//     PR_SET_PDEATHSIG.
//
//   - A startup sweep reaps anything the sidecar missed (e.g. both app and
//     sidecar were SIGKILLed, or the machine lost power): a durable
//     registry records {pid, pgid, start-time, parent} per spawn, and on the
//     next launch any recorded group still alive, start-time-matched
//     (PID-reuse safe), and no longer the recording parent's child is killed.
//
// Process groups come from the provider package's Setpgid and from
// procutil.StartGroup's Setsid; killing a
// negative pgid needs only same-uid permission, so neither layer has to
// be the provider's parent.
package orphanreaper
