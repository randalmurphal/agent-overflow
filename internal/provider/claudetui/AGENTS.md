# Claude TUI provider

This package runs the interactive Claude TUI in a PTY and reconstructs the
headless Claude event stream from a loopback API gateway and hook relay. It
feeds reconstructed envelopes through the shared `claude.Parser`; it must not
create a second parser or use the lagging transcript as a live source.

Read [claude-tui-provider.md](../../../docs/architecture/claude-tui-provider.md)
for launch, compaction, take-control, and recovery design. Re-verify captured
binary behavior on provider upgrades according to
[spike-policy.md](../../../docs/references/spike-policy.md).

## Ordering and reconstruction

One feed loop owns `ParseLine`, and all event emission is serialized.
`feedReorder` holds a hook completion until its matching wire tool start has
been fed. Preserve parent IDs on subagent envelopes. The gateway may bind a
subagent request to an unclaimed launch by its opening prompt; when no safe
match exists, forward without attributing it to the parent.

`system:init` and user replay echoes are independent turn signals. Compaction
is detected only through the public Pre/PostCompact hook lifecycle. Background
terminal notifications come from later request bodies; accept user and system
injections, extract all notifications, and emit task update before task
notification.

## Runtime and terminal control

`EnforcesRuntimeMode` is false. Runtime modes must not alter TUI launch
permissions. System prompt, disallowed tools, and additional directories remain
spawn-time flags shared with the headless Claude implementation.

Terminal input has one attachment-scoped lease holder. Only that attachment may
write, and normal `Send` is refused while the lease is held. Attach fan-out is
reference-counted; the terminal ring remains active independently. Use
`RefreshTerminal` for repaint and resize coordination.

The gateway never logs credentials. The hook relay is loopback-only and checks
its per-session capability token in constant time. Hook-command observation
errors must not interfere with the provider process.
