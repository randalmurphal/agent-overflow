// Package orphanreaper guarantees that provider subprocess groups don't
// outlive the app on macOS, where there's no Pdeathsig (Linux) or Job
// Object (Windows) to lean on.
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
//     registry records {pid, pgid, start-time} per spawn, and on the next
//     launch any recorded group still alive, start-time-matched (PID-reuse
//     safe), and reparented to init is killed.
//
// Process groups come from the provider package's Setpgid; killing a
// negative pgid needs only same-uid permission, so neither layer has to
// be the provider's parent.
package orphanreaper
