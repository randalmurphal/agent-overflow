package orphanreaper

import "time"

// killGracePeriod is the SIGTERM→SIGKILL window when tearing down watched
// groups. SIGTERM first lets a provider finish its session file write and
// drop its own subtree; SIGKILL guarantees it's gone. It matches the grace
// provider.Process.Close gives a provider after its own SIGTERM.
const killGracePeriod = 2 * time.Second

// sidecarScript is the sidecar, read by /bin/sh from its stdin with
// SidecarName as $1 and the grace in seconds as $2. It reads watch/release lines from fd 3, the read end of the control
// pipe, until EOF, which arrives however the parent died (clean exit, panic,
// or SIGKILL all close its write end), then sends SIGTERM to every
// still-watched group, waits the grace and sends SIGKILL. A shell keeps the
// process that lives as long as the app to a few hundred KB of private
// memory; the app's own binary would carry its Go heap from every package's
// init and the relocations of every library it links.
//
// The script is POSIX sh, and kill takes the `-SIG -pgid` form, which dash,
// bash and busybox all parse. A line is rejected and logged unless it is
// `watch <pgid>` or `release <pgid>` with a pgid of at most ten digits
// without a leading zero, greater than 1: pgid 0 is the sidecar's own group
// and kill -1 signals every process the user owns. Kill errors are
// discarded: a group that exited on SIGTERM fails the SIGKILL with ESRCH.
// The summary is logged after the kills, since the stderr the sidecar
// inherited may have died with the parent.
//
// The SIGKILL runs from the EXIT trap, so it needs no child process. sleep
// is the script's one external command, and a shell that cannot fork it
// (the user's process limit is spent) exits there: bash, dash and busybox
// all do. The trap then sends the SIGKILL without the grace.
const sidecarScript = `watched=
while read -r verb pgid extra; do
	case $verb:$pgid:$extra in
	::) continue ;;
	watch:*: | release:*:) ;;
	*)
		echo "orphanreaper: malformed control line: $verb $pgid $extra" >&2
		continue
		;;
	esac
	case $pgid in
	'' | 0* | 1 | *[!0-9]* | ???????????*)
		echo "orphanreaper: refusing unsafe pgid $pgid" >&2
		continue
		;;
	esac
	kept=
	for g in $watched; do
		[ "$g" = "$pgid" ] || kept="$kept $g"
	done
	watched=$kept
	[ "$verb" = release ] || watched="$watched $pgid"
done <&3
[ -n "$watched" ] || exit 0
for g in $watched; do kill -TERM "-$g" 2>/dev/null; done
trap 'for g in $watched; do kill -KILL "-$g" 2>/dev/null; done
set -- $watched
echo "orphanreaper: parent died; reaped $# process group(s)" >&2' EXIT
sleep "$2"
`
