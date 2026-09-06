# internal/remotejobs/

Commands accepted by another computer. Four active processes maximum; exact argv,
destination-owned environment, registered workspace, explicit timeout. Reuse
`procutil.ConfigureGroup` and `TailBuffer`. No provider sessions, task scheduler,
queue, or frontend-lifetime ownership.

The injected process runner is mandatory. Unit tests supply a Go closure and
never execute the developer's commands or provider binaries. Real-process tests
must isolate HOME and use a fixed test helper executable.

Persist acceptance with full durability BEFORE spawning. Request UUID, owner,
source conversation, destination workspace and argv digest are immutable. An
identical retry returns the existing receipt even at capacity. A changed request
with the same UUID is refused. Boot settles orphaned receipts as interrupted;
it must never infer that an unacknowledged command did not execute.

Live output retains a 128 KiB tail. Completed output is written once to SQLite;
only the latest 128 settled tails survive retention. Older acceptance receipts
remain, independently of conversation history and snapshot restore. Output
still buffered when the backend process crashes may be lost; the acceptance
record cannot be. A lost frontend never cancels the process.

Shutdown cancels process groups and joins them before SQLite closes. A failed
completion write keeps the result and its bounded slot until persistence works
or the backend stops. Remote updates must count these slots as active work.
