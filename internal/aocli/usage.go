package aocli

import (
	"strings"

	"agent-overflow/internal/buildvariant"
)

const remoteUsage = `Usage: agent-overflow remote <command> [options]

  list                     List enabled computers and registered projects
  run --computer <uuid> --project <uuid> [--workspace <path>]
      [--id <request-uuid>] [--timeout <seconds>] -- <command> [arguments...]
  status --computer <uuid> <request-uuid>
  cancel --computer <uuid> <request-uuid>

Replies are JSON. Commands run on the destination with its environment and
survive frontend disconnection. Output retains the last 128 KiB. The default
time limit is one hour; the maximum is seven days. Up to four commands may run
per computer. There is no implicit queue or automatic retry.

Enable agent access for the destination in the originating computer's settings.
Workflow phases additionally require the remote-commands grant. Status and
cancellation are restricted to the conversation that started the command.

A request UUID is printed before run sends its request. After a lost reply,
inspect status or retry the identical command with that same --id. A computer
restart marks unfinished work interrupted; it never reruns it automatically.
`

// Every usage string the binary prints. They live together so the command tree
// is readable in one place, and so adding a subcommand without documenting it is
// an obvious omission rather than a hidden one.

// rootUsage is the root help text. A build without remote access
// (internal/buildvariant) omits the host commands and `remote`, which it
// refuses.
var rootUsage = rootUsageFor(buildvariant.RemoteAccess)

func rootUsageFor(remoteAccess bool) string {
	var b strings.Builder
	b.WriteString(rootUsageHead)
	if remoteAccess {
		b.WriteString(rootUsageHostCommands)
	}
	b.WriteString(rootUsageLocalCommands)
	if remoteAccess {
		b.WriteString(rootUsageSessionCommands)
	}
	b.WriteString(rootUsageFooter)
	return b.String()
}

const rootUsageHead = `Usage: agent-overflow <command> [options]

`

const rootUsageHostCommands = `Host commands (run on the machine the backend lives on):
  serve              Run the backend with no window, for access from elsewhere
  service install    Run that backend at login, and keep it running
  service start      Start an installed backend without a window
  service stop       Stop it while keeping the service installed
  pair --lan         Connect a device to the running backend
  service uninstall  Stop it and remove the service
  service status     Report what the service manager says about it

`

const rootUsageLocalCommands = `Local commands (run on the computer the thread is on, beside its running app):
  thread send        Message a thread from a script, as data rather than as you

`

const rootUsageSessionCommands = `Session commands (run inside an Agent Overflow agent session):
  remote             Run and inspect commands on enabled paired computers

`

const rootUsageFooter = `Exit codes: 0 success, 1 the asked-about thing said no (a service that is not
running), 2 error.

Put --config-root after the subcommand ("thread send --config-root ..."): the
app binary routes to these commands by their leading verb, so an invocation that
starts with a flag never reaches them.

Workflow runs, definitions and campaign memory are not commands: agent sessions
reach them through the ao-workflow-tools MCP server.
`

const serviceUsage = `Usage: agent-overflow service <command> [options]

Commands:
  install    Run the backend at login, and keep it running
  start      Start an installed backend without opening a window
  stop       Stop the backend and keep its service installed
  update     Point the installed service at a new binary
  uninstall  Stop it and remove the service
  status     Report what the service manager says about it

Manages a per-user background service: a systemd user unit on Linux, a launchd
LaunchAgent on macOS. On Windows the launcher already supervises its own
backend inside WSL; install the service inside the WSL distribution instead.

Pair a device while the service runs with "agent-overflow pair --lan".
No desktop window or service restart is required.
`

const serviceInstallUsage = `Usage: agent-overflow service install [options]

Options:
  --binary <path>       the agent-overflow binary the service starts
                        (defaults to this one)
  --listen <host:port>  bind address to pass to serve

Writes the unit file and tells the service manager to run it from now on.
Running it again replaces what is installed.

--binary defaults to the running binary's own resolved path. Name a path
instead when the file you actually maintain is somewhere else — a symlink you
replace on upgrade, for one, since the default resolves through it and would
pin the service to today's target.

--listen is rarely what you want. It overrides the saved network settings on
every start, so Settings -> Network can no longer move the backend: the setting
changes, the flag wins, and nothing says why. Set the bind toggle and the port
in Settings -> Network instead, and keep this for a host where the address must
be fixed outside the app.
`

const serviceUpdateUsage = `Usage: agent-overflow service update [options]

Options:
  --binary <path>  the binary to install as the new version
                   (defaults to this one)

Installs a new backend version into the service, with the service stopped.

This is the local update path, for the operator standing at the machine. It
stops the service, copies the binary into its own immutable version directory
under the config root, selects it, and starts the service again. There is no
trial and no automatic rollback, because you are here: those exist for an
update triggered from somewhere you cannot reach the machine from.

It is also how you replace the SUPERVISOR — the stable process the service
manager starts, which is what runs and updates the backend. Replacing the file
on disk and restarting the service does not do that on its own: the supervisor
selects whichever version its durable state names, which is what keeps a
committed update committed across a restart. Replace the file, then run this.
`

const serviceUninstallUsage = `Usage: agent-overflow service uninstall

Stops the service and removes its unit file. Nothing else is touched: the
config root, the history and the paired devices are all still there, and
installing again picks them back up.
`

const serviceStatusUsage = `Usage: agent-overflow service status

Reports whether the service manager knows about the backend, whether it starts
on its own, and whether it is up. Exits 1 when it is not running, so it reads
in a shell conditional.
`

const threadUsage = `Usage: agent-overflow thread send [--wait 30s] <thread-id> <text>

Queue a message to a thread on this computer, for a script that is not an
Agent Overflow session. The thread reads it as data, never as you speaking,
badged as an outside send and capped at 64 KiB with the dropped bytes stated;
a thread with no running session starts one. It returns once the message is
queued, and prints its send id; a session that then fails to start says so
in the thread. The app must be running here.
--wait waits for a starting backend (at most 2m).
`

const pairUsage = `Usage: agent-overflow pair [--lan] [--class browser|android|desktop] [--json] [--wait 30s]

Pair a device with this computer's running app or background service.
Compare the number on both devices, then enter the six digits here.
--wait waits for a starting backend (at most 2m).
--lan enables access on the local network. Existing tailnet settings remain.
--json emits invitation, verification, and paired records for SSH setup;
confirmation is still required on stdin. No launch credential is printed.
`
