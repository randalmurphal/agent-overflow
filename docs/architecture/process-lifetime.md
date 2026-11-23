# Child process lifetime

What ends a process the backend starts when the backend dies without
stopping it: a crash, SIGKILL, or the OOM killer. Graceful shutdown cancels
each owner's context and is covered by the owners.

## The orphan reaper

`internal/orphanreaper` runs a sidecar, a POSIX `/bin/sh` script
(`/bin/sh -s agent-overflow-reaper 2`), on macOS and on Linux, the WSL backend
included. A shell rather than the app's own binary keeps its private memory
to a few hundred KB. It leads its own process group, so a signal to the
backend's group does not end it. It holds the read end of a pipe whose
only writer is the backend; EOF means the backend is gone, whatever reparented
its children (init, launchd, or a subreaper such as `systemd --user`). It
then sends SIGTERM to every watched group and SIGKILL two seconds later.

The backend also records each watched group's leader in a registry: pid,
start time, and its own pid as the parent. The next launch kills a recorded
group whose leader is alive, has the recorded start time, and is no longer
the recording backend's child. A start time that differs means the pid was
reused, and that process is left alone. The registry is the backstop for a
sidecar that died with the backend.

The sidecar's script is in a pipe the backend fills and closes before the
sidecar starts, so once the sidecar is running it needs nothing more from the
backend. The sidecar hears of a group just after its leader forks, and the
registry records it after that. A backend that dies in between leaves the
group to the sidecar; one that dies before the sidecar hears leaves it
running.

Signalling only a group's leader is not enough: Linux's parent-death signal
reaches the leader, and a shell leader exits while what it started (`npm run
dev` under `cd web && ...`) keeps running.

After the backend dies the sidecar runs from the system shell, not from the
backend's executable, so an AppImage mount the runtime unmounts at exit
(`internal/appimage`) cannot take its kill path away. It signals the groups,
forks `sleep` for the grace, logs to the stderr it inherited and exits
(`orphanreaper/reaper.go`). The SIGKILL is sent from the script's EXIT trap:
a shell that cannot fork `sleep` exits there, and SIGKILL then follows
SIGTERM without the grace, or after the shell's own fork retries (bash
retries for about fifteen seconds).

## Which spawners end with the backend

A group is released only once no member of it is alive. A leader's exit is
not its group's: Wait proves only that the leader is gone, so the owner ends
what the leader left (SIGTERM, then SIGKILL after a grace, through
`procutil.StopConfiguredGroup`) before it releases. A group the owner cannot empty
stays watched and the owner reports the failure.

`procutil.StartGroup` and `RunGroup` take a `procutil.Reaper` and refuse a nil
one, so every supervised command group is watched for its whole run. Wait
stops leftovers, and returns `ErrGroupSurvived` when one survives SIGKILL.
Processes a command leaves running under `KeepLeftovers` when it exits on
its own are the one deliberate exception: they are released alive and
outlive the backend, as they outlive the command. Cancellation still stops
them. The headless browser's launch follows the same rule:
`startChromium` takes the reaper and refuses a nil one, the engine is not
built without one (`HeadlessChromiumOptions.Reaper`), and the group is
released once `stop` has seen every process exit. `provider.Spawn` takes a
required `Lifetime` with two forms, watched the same way: `Watched` for a
process that runs until its owner closes it, `Bounded` for one that finishes
within its context's deadline, which a bounded launch must have. When the
leader exits, the `provider.Process` ends what it left before Done closes,
and `Close` and `Kill` return a failure to end it. The terminal launcher has one
form: its `SessionOptions.Reaper` is required, since a pty process runs until
it is closed. A job the shell leaves in its group runs on while the terminal
is open, so the group stays watched until `Close` ends it. A launch with no
lifetime starts nothing.

| Spawner | Lifetime | Mechanism |
|---|---|---|
| Composer shell commands (`shellrun`) | Ends with the backend | Reaper |
| Worktree setup commands (`worktreesetup`, from chat and workflows) | Ends with the backend; kept leftovers outlive it | Reaper |
| Workflow tool commands (`workflowhost`) | Ends with the backend; kept leftovers outlive it | Reaper |
| Remote jobs (`remotejobs`) | Ends with the backend | Reaper |
| Standing agent watch scripts, interval and long-running (`app_agent_watch_process.go`) | Ends with the backend | Reaper |
| Provider update and doctor runs (`app_provider_update.go`) | Ends with the backend | Reaper |
| ACME DNS hooks (`acmecert`) | Ends with the backend | Reaper |
| Claude and Codex sessions, and a fork's temporary Codex app-server (`provider.Watched`) | Ends with the backend | Reaper |
| Claude and Codex sign-ins and MCP sign-ins, which wait on a person (`provider.Watched`) | Ends with the backend | Reaper |
| A conversation transfer's Codex snapshot, whose length follows the subagents it walks (`provider.Watched`) | Ends with the backend | Reaper |
| Account probes and identity reads, Codex usage, skills and model-list reads, a Codex fork's one-shot app-server (`provider.Bounded`) | Finishes within its deadline, or ends with the backend | Reaper |
| Claude TUI sessions (`claudetui`) and terminal panes (`terminal`) | Ends with the backend | Reaper. The pty hangup reaches them too: the kernel sends it when the master closes |
| Headless Chromium (`browser/headless_process*.go`) | Ends with the backend | Reaper. Its renderer, GPU, network and storage processes are in its group and write into the profile until they die |
| `caffeinate` (`power`, macOS) | Ends with the backend | `-w <backend pid>` |
| Bounded one-shots: git, `gh`/`glab` token reads, version and MCP status probes, the login-shell probe, `ssh` setup, `security`, `lsof`, `systemctl`/`launchctl`, `wslpath` | Finishes on its own | Each runs under a timeout or ends at EOF; none is watched |
| Text generation (`textgen`) | Finishes within its deadline, or ends with the backend | Reaper. A stop is SIGTERM to the group first, since the CLI may be writing a refreshed OAuth token |
| Editor, external URL, desktop app and update helper launches | Outlives the backend | Started detached in a new session on purpose |

Not backend children: the desktop supervisor's backend child and preflight
(`supervise`), the Windows launcher (`wsllauncher`, a Job Object), the
harness CLI's backend launch (`harnessclient`), and the test network helper
(`netisolate`).

No provider launch uses a parent-death signal, bounded ones included. A
bounded launch's deadline lives in the backend, and the Claude CLI does not
exit when its stdin closes, so on macOS nothing else would end one the
backend left. On Linux the signal reaches only the leader, which leaves what
a probe's hooks and plugins started, and it ends the leader that the next
launch's sweep identifies the group by.

The two seconds between the sidecar's SIGTERM and SIGKILL are the grace
`provider.Process.Close` gives a provider after its own SIGTERM, so a dying
backend shortens only the stdin wait before it. A provider killed while it
appends to its JSONL session file loses at most the line it was writing:
Claude's resume leaf scan, the Claude importer and fork, and the Codex
rollout reader each skip a cut last line and keep the rest.
