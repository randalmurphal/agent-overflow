# agent-overflow CLI reference

Shapes and surface for the CLI half of the app binary (D30). The rules and the
reasons behind them live in
[internal/aocli/AGENTS.md](../../internal/aocli/AGENTS.md). Flags are
authoritative in `internal/aocli/usage.go`, printed by `agent-overflow help` and
`agent-overflow <group> --help`.

## Command tree

```
agent-overflow <command>
  serve [--listen|--data-dir|--reset-transport-port]
                                               NOT an aocli command: a boot mode routed by
                                               main's entry dispatch, listed in the root usage
                                               because it is a verb a person types
                                               (docs/architecture/serve-mode.md)
  supervise [same boot flags as serve]         also NOT an aocli command, and deliberately NOT in
                                               the root usage: a service manager starts it, never
                                               a person. It owns the backend as a child and is
                                               what makes a serve host updatable
                                               (docs/architecture/serve-mode.md § The supervisor)
  service install [--listen <host:port>] [--binary <path>]
  service update [--binary <path>]
  service uninstall
  service status                               host commands: manage the machine's service
                                               manager, talk to no app, hold no credential.
                                               status exits 1 when it is not running. Refused on
                                               Windows, where the launcher already supervises its
                                               WSL backend (docs/architecture/serve-mode.md).
                                               install writes ExecStart=<binary> supervise, not
                                               serve; update stages a binary as a new version and
                                               restarts the unit, and is the only way to replace
                                               the supervisor itself
  pair [--lan|--class browser|android|desktop|--json|--wait <dur>]
                                               host command: pairs a device with this computer's
                                               running app or service; the six-digit code is
                                               confirmed on stdin. --json emits the invitation,
                                               verification and paired records for SSH setup
  remote list|run|status|cancel                session command: commands on an enabled paired
                                               computer (below)
  thread send <thread-id> <text> [--config-root|--wait <dur>]
                                               messages a thread on this computer from a script
                                               outside the app, through the owner's local control
                                               file rather than a session credential; the message
                                               is badged as sent by a script and quoted as data
                                               (docs/specs/standing-agents.md#wake-supervisor)
```

Workflows, runs and campaign memory are not CLI commands. Agents reach them
through the `ao-workflow-tools` MCP server
([agent-workflow-tools.md](../specs/agent-workflow-tools.md), D77).

## Commands on another computer

`agent-overflow remote list` returns enabled paired computers and their registered
projects as JSON, including an error for an unavailable computer. Use
`remote run --computer <uuid> --project <uuid> [--workspace <registered-worktree>]
[--id <request-uuid>] [--timeout <seconds>] -- <command> [args...]`. There is
no time limit by default; `--timeout` allows up to seven days, with four
concurrent commands per host.
The generated request ID is printed before dispatch. After a missing reply,
`remote status --computer <uuid> <request-uuid>` reconciles it; retrying run must
reuse the same ID and exact arguments. `remote cancel` takes the same selectors.

Agent access is opt-in at the originating computer, and phases also need the
`remote-commands` grant. Status/cancel belongs to the initiating conversation.
Jobs outlive frontend loss. Backend shutdown interrupts them; restart never
reruns them. The compatibility CLI output is the last 128 KiB; SQLite keeps 128 finished tails,
while accepted IDs remain to prevent old retries from executing again.

Remote MCP agents additionally have durable disk-log read/search and artifact tools; see [remote-commands.md](../architecture/remote-commands.md).
