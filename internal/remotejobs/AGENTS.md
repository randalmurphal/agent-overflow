# internal/remotejobs/

Commands accepted by another computer. Four active processes maximum; exact argv
or an explicit script/interpreter, destination-owned environment, registered
workspace, bounded timeout unless `unlimited` is explicit. Reuse
`procutil.ConfigureGroup` and `TailBuffer`. No provider sessions, task scheduler,
queue, or frontend-lifetime ownership. Never add implicit shell detachment:
process-group lifetime belongs to the durable job, independently of tool waits.

The injected process runner is mandatory. Unit tests supply a Go closure and
never execute the developer's commands or provider binaries. Real-process tests
must isolate HOME and use a fixed test helper executable.

Persist acceptance with full durability BEFORE spawning. Request UUID, owner,
source conversation, destination workspace and request digest are immutable. An
identical retry returns the existing receipt even at capacity. A changed request
with the same UUID is refused. Boot settles orphaned receipts as interrupted;
it must never infer that an unacknowledged command did not execute. New optional
request fields use `omitempty` to preserve old argv request fingerprints.

Production supplies `Options.LogDir` beneath the durable private data root;
omitting it creates disposable test storage. Every accepted job has a disk log
reserved before spawning. Logs are byte rings, capped at 4 GiB per job and 20 GiB
retained overall (plus small headers), with 256 MiB free-disk headroom. Admission
reserves active writers' full capacities and expires completed logs oldest-first;
it never evicts an active writer. Disk failures always drain the process, record
lost output, and resume capture when storage recovers. A partial ring overwrite
is explicitly unreadable after a crash, never silently presented at stale byte
offsets. Logs sync at completion; power loss can still lose recent writes.

Inline output retains a 128 KiB memory tail and SQLite retains the latest 128
settled tails for older peers. Disk log metadata distinguishes truncation from
inline omission; callers reading old jobs use `ReadLog` rather than assuming the
SQLite tail still exists. Missing log files report expiry while acceptance
receipts remain. Log reads/searches authorize through the receipt owner before
opening files, bound reads to 128 KiB, and use absolute byte offsets. Literal
search returns a continuation offset with overlap to preserve boundary matches;
no unbounded regex scan or full-file allocation. Source conversation ownership
is additionally checked by the application adapter.

Scripts are at most 1 MiB, stored with private permissions and passed as a file
argument to the caller-selected interpreter, never interpolated into another
shell string. Temporary scripts are removed after execution and at boot. Explicit
unlimited jobs still stop on cancellation or destination shutdown; they are not
autorestarted or automatically detached with `&`/`nohup`.

Shutdown cancels process groups and joins them before SQLite closes. A failed
completion write keeps the result and its bounded slot until persistence works
or the backend stops. Remote updates must count these slots as active work.

Expected refusals use errorsx.Public with stable remote_* codes and recovery
instructions, so paired callers receive actionable errors. Process/persistence
failures retain private causes only in host logs; receipts explain the outcome
and safe next action. A failed completion write must not suggest rerunning.
