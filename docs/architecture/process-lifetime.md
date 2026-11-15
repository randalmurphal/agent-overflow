# Child process lifetime

What ends a process the backend starts when the backend dies without
stopping it: a crash, SIGKILL, or the OOM killer. Graceful shutdown cancels
each owner's context and is covered by the owners.

## The orphan reaper

`internal/orphanreaper` runs a sidecar, `<binary> __reap`, on macOS and on
Linux, the WSL backend included. It leads its own process group, so a signal
to the backend's group does not end it. It holds the read end of a pipe whose
only writer is the backend; EOF means the backend is gone, whatever reparented
its children (init, launchd, or a subreaper such as `systemd --user`). It
then sends SIGTERM to every watched group and SIGKILL two seconds later.

The backend also records each watched group's leader in a registry: pid,
start time, and its own pid as the parent. The next launch kills a recorded
group whose leader is alive, has the recorded start time, and is no longer
the recording backend's child. A start time that differs means the pid was
reused, and that process is left alone. The registry is the backstop for a
sidecar that died with the backend.

The group is watched from just after the leader forks. A backend that dies
before the sidecar hears of the group leaves it to the next launch.

Signalling only a group's leader is not enough: Linux's parent-death signal
reaches the leader, and a shell leader exits while what it started (`npm run
dev` under `cd web && ...`) keeps running.

After the backend dies the sidecar execs nothing and opens nothing: it
signals the groups, sleeps, logs to the stderr it inherited and exits
(`orphanreaper/reaper.go`). Its code is still read from its executable on
first use, because the kernel loads an executable's pages when they first
run, and the kill path first runs after the backend died. Under an AppImage
that executable is in the squashfs mount the AppImage runtime unmounts when
the backend exits (`internal/appimage`), so the sidecar can fail to load its
kill path there and leave the groups to the next launch's sweep. The update
helper avoids the same mount by running the `.AppImage` file itself
(`desktopInstall` in `main_update_apply.go`); the sidecar does not yet.

## Which spawners end with the backend

`procutil.StartGroup` and `RunGroup` take a `procutil.Reaper` and refuse a nil
one, so every supervised command group is watched for its whole run and
released once Wait has seen it end. The headless browser's launch follows the
same rule: `startChromium` takes the reaper and refuses a nil one, the engine
is not built without one (`HeadlessChromiumOptions.Reaper`), and the group is
released once `stop` has killed it. Processes a command leaves running under
`KeepLeftovers` are released with it and outlive the backend, as they outlive
the command. A group the kill could not end stays watched.

| Spawner | Lifetime | Mechanism |
|---|---|---|
| Composer shell commands (`shellrun`) | Ends with the backend | Reaper |
| Worktree setup commands (`worktreesetup`, from chat and workflows) | Ends with the backend; kept leftovers outlive it | Reaper |
| Workflow tool commands (`workflowhost`) | Ends with the backend; kept leftovers outlive it | Reaper |
| Remote jobs (`remotejobs`) | Ends with the backend | Reaper |
| Provider update and doctor runs (`app_provider_update.go`) | Ends with the backend | Reaper |
| ACME DNS hooks (`acmecert`) | Ends with the backend | Reaper |
| Provider sessions (`provider/process*.go`) | Ends with the backend | Linux: SIGTERM to the leader (Pdeathsig). macOS: reaper |
| Claude TUI sessions (`claudetui`) | Ends with the backend | Linux: pty hangup. macOS: reaper |
| Terminal panes (`terminal`) | Ends with the backend | The pty master closes, and the kernel hangs up the session |
| Headless Chromium (`browser/headless_process*.go`) | Ends with the backend | Reaper. Its renderer, GPU, network and storage processes are in its group and write into the profile until they die |
| `caffeinate` (`power`, macOS) | Ends with the backend | `-w <backend pid>` |
| Bounded one-shots: git, `gh`/`glab` token reads, version and MCP status probes, the login-shell probe, `ssh` setup, `security`, `lsof`, `systemctl`/`launchctl`, `wslpath` | Finishes on its own | Each runs under a timeout or ends at EOF; none is watched |
| Text generation (`textgen`) | Finishes on its own | Deliberately not killed abruptly: the CLI may be writing a refreshed OAuth token |
| Editor, external URL, desktop app and update helper launches | Outlives the backend | Started detached in a new session on purpose |

Not backend children: the desktop supervisor's backend child and preflight
(`supervise`), the Windows launcher (`wsllauncher`, a Job Object), the
harness CLI's backend launch (`harnessclient`), and the test network helper
(`netisolate`).

Provider sessions keep their Linux rule because the reaper would add a group
SIGTERM and a SIGKILL two seconds later to every provider a dying backend
leaves (`providerGroupsReaped` in `internal/app/app_orphan_reaper.go`).
