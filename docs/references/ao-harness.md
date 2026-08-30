# ao-harness

Shell driver for an isolated Agent Overflow harness instance.

Generated from `cmd/ao-harness` command descriptors. Run `go generate ./cmd/ao-harness` after changing the command tree.

## Usage

```text
ao-harness [global flags] <command> [args]
```

Global flags work before or after the command. `--instance` accepts a full instance id, a unique `idPrefix` (at least four hexadecimal characters), or a data root.

| Flag | Meaning |
| --- | --- |
| `--instance <id|idPrefix|dataRoot>` | Select the target instance. |
| `--registry-dir <dir>` | Override discovery state. |
| `-o <text|json>` | Select output format. |
| `--page-id <id>` | Select the exact attached frontend page for UI, perf, and bench commands. |

## Commands

| Command | Description |
| --- | --- |
| `up` | start a harness instance (detached) and print how to reach it |
| `down` | stop an instance (SIGTERM, then kill after 5s; --force for an orphaned pid) |
| `list` | list known instances, pruning rows whose process is gone |
| `info` | identity, evidence paths and URL for one instance |
| `open` | print the instance URL (--browser opens it) |
| `attach` | host the instance page in a headless browser so ui/perf/bench work unattended |
| `rpc` | call any App or Harness method by name with JSON arguments |
| `seed` | apply a HarnessSeed spec (-f file, or - for stdin) |
| `reset` | wipe app state without rebooting |
| `threads` | list thread rows, drafts included |
| `items` | list a thread's items |
| `send` | send a message to a thread |
| `scenario` | set|list|clear mock scenario rules, rebuild one from a real thread, or validate files offline |
| `clone` | build a harness data root from a copy of a real app data dir |
| `mock` | list|advance|emit|exit against registered mock providers |
| `events` | tail|await|count events on the wire |
| `record` | start|stop a replay bundle capture |
| `bundles` | list recorded replay bundles |
| `replay` | bundle|file|pause|resume|step|stop|status |
| `logs` | tail backend|frontend-errors|ui-trace |
| `db` | run one read-only SELECT against the instance database |
| `ui` | snapshot|query|state|diff the attached frontend |
| `perf` | start|stop|status|watch the perf meters |
| `monitor` | list or operate typed app-feel monitor specifications |
| `bench` | run a bench workload and write a perf report |
| `run` | run one strict managed workload plan |
| `profile` | record a CPU profile of one scripted turn (needs a Chromium devtools endpoint) |
| `compare` | prepare or run an offline A/B comparison capsule |
| `postmortem` | read-only offline inspection of a stopped harness evidence root |
| `artifacts` | list, pin, unpin, or clean failed-run artifacts |
| `health` | roll up an instance's liveness, errors, memory and mocks |
| `version` | print this CLI's build stamp |
| `help` | print this help |

### `scenario` subcommands

Use `ao-harness scenario <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `set` | install a mock scenario rule |
| `list` | list active rules and the built-in library |
| `show` | show one built-in scenario |
| `clear` | remove active scenario rules |
| `validate` | validate scenario files or library names offline |
| `from-thread` | rebuild recorded turns as a scenario |

### `mock` subcommands

Use `ao-harness mock <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `list` | list registered mock providers |
| `advance` | release a mock waitSignal gate |
| `emit` | send a raw wire line to a mock |
| `exit` | stop a mock provider |

### `events` subcommands

Use `ao-harness events <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `tail` | stream matching events |
| `await` | wait for one matching event |
| `count` | count retained matching events |
| `channels` | list registered event channels |

### `record` subcommands

Use `ao-harness record <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `start` | start a replay bundle capture |
| `stop` | stop the active replay bundle capture |

### `replay` subcommands

Use `ao-harness replay <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `bundle` | replay a recorded bundle |
| `file` | replay an event-log file |
| `pause` | pause replay |
| `resume` | resume replay |
| `step` | release one paused event |
| `stop` | stop replay |
| `status` | show replay status |

### `ui` subcommands

Use `ao-harness ui <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `snapshot` | capture a semantic viewport snapshot |
| `query` | query matching DOM elements |
| `state` | read one whitelisted frontend global |
| `diff` | compare the current viewport to the saved snapshot |
| `reload` | reload the attached page |
| `open` | open a thread in the attached page |

### `perf` subcommands

Use `ao-harness perf <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `start` | arm frontend and backend performance meters |
| `stop` | stop meters and print the report |
| `status` | show the active meter run |
| `watch` | stream one line per performance sample |

### `monitor` subcommands

Use `ao-harness monitor <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `list` | list typed app-feel monitor specifications |
| `start` | start one or more typed app-feel monitors |
| `heartbeat` | record a heartbeat for a monitor run |
| `overlap` | record overlap between two live monitor runs |
| `status` | collect the current state of a live monitor run |
| `collect` | collect observations from a live monitor run |
| `stop` | stop one monitor run and retain its result |
| `cleanup` | safely stop one monitor run during teardown |
| `last` | read retained stopped monitor results |

### `compare` subcommands

Use `ao-harness compare <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `prepare` | create a portable comparison capsule |
| `run` | run disposable A/B comparison legs |

### `artifacts` subcommands

Use `ao-harness artifacts <subcommand> -h` for flags.

| Subcommand | Description |
| --- | --- |
| `list` | list retained failed-run artifacts |
| `pin` | protect one retained run from cleanup |
| `unpin` | allow one retained run to be cleaned |
| `clean` | remove oldest eligible runs over the quota |

## Managed run example

Use `run --plan` for a fresh disposable workload. The data root must be absent or empty. Paths are absolute, and `output` belongs under `<dataRoot>.artifacts/<runId>`.

```json
{
  "version": 1,
  "runId": "burst-check",
  "workload": "burst-stream",
  "dataRoot": "/tmp/ao-burst-check",
  "instance": "/tmp/ao-burst-check",
  "adapter": "bench",
  "ownership": "fresh",
  "window": true,
  "preserveRoot": true,
  "ceiling": { "maxPrivateBytes": 629145600 }
}
```

```sh
ao-harness run --plan /tmp/burst-check.json
```

For a functional plan, set `adapter` to `functional`, provide an absolute `scenario` and an `output` under the derived artifact root. For a portable A/B run, set `adapter` to `compare` and provide an absolute capsule manifest. Use the ordinary command verbs only when deliberately selecting a borrowed harness.

## Monitor examples

Monitor commands require one exact attached page. With multiple pages, pass `--page-id` from `ao-harness info`; without it the command refuses to guess. Monitor results are bounded to 64 KiB by default. Use `--file <path>` or `--full` for a complete result.

`status` is the live snapshot spelling and performs the same typed collection as `collect`; it leaves the run active. `cleanup` is an explicit stop alias for teardown and retains the result. The page bridge also stops all live runs when the page unloads, so a lost page cannot leave observers armed.

```sh
# Start a clean renderer monitor and retain its run id
ao-harness monitor start --monitor frame-pacing --run-id frame-check
# Send a heartbeat, inspect without stopping, then retain the final result
ao-harness monitor heartbeat --run-id frame-check
ao-harness monitor status --run-id frame-check
ao-harness monitor cleanup --run-id frame-check --file /tmp/frame-check.json
# Read the bounded stopped-run ring
ao-harness monitor last
```

`bench` operates on and resets the selected borrowed harness. Use `run --plan` for a fresh managed run. `up --soak` starts soak backend mode but does not start the Windows launcher. `up --keep-home` exposes the real home only to child processes; backend provider state remains isolated. `up` applies a hard 2 GiB memory limit by default; use `--memory-limit-bytes` to set another positive limit within host capacity.

## Output

Read commands emit human-readable text by default. `-o json` emits one JSON document. Streaming commands (`events tail`, `perf watch`, `health --watch`, and bench progress) use NDJSON or stderr progress so stdout stays machine-readable. Event tails default to 1,000 records on stdout. `events tail --file` captures the complete stream without an event-count cap, bounded by `--max-bytes` (64 MiB by default) or `--timeout`; a bound reached by time is reported as incomplete. Frontend query output defaults to 64 KiB and refuses larger results. Use `ui query --full` or `ui query --file <path>` for complete frontend query output. Use `events tail --full` or `events tail --file <path>` for complete event output.

Exit codes are 0 for success, 1 for an operational refusal, 2 for invalid arguments, and 3 when a completed health or baseline gate reports bad news.

## RPC methods

The RPC method list is instance-specific and generated by the backend. Use `ao-harness rpc --list` for the complete current list. This page intentionally does not duplicate that wire surface.
