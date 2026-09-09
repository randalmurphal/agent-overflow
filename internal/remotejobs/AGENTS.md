# internal/remotejobs

Durable commands accepted from another computer. The package owns admission,
process lifetime, receipts, and bounded output logs. It is not a provider
session, scheduler, queue, or frontend-lifetime service.

## Admission and execution

- Accept exact argv or a bounded script plus caller-selected interpreter for a
  registered workspace. Use the destination-owned environment and
  `procutil.ConfigureGroup`. Never interpolate scripts into another shell
  command or detach processes implicitly.
- Persist the immutable request identity, owner, source conversation,
  destination workspace, and digest before spawning. An identical retry returns
  the existing receipt, including at capacity; the same UUID with different
  content is refused.
- Boot marks orphaned running receipts interrupted. It must not infer that an
  unacknowledged command never executed.
- Four jobs may be active. Timeouts are bounded unless `unlimited` is explicit;
  cancellation and destination shutdown still stop unlimited jobs. Shutdown
  cancels process groups and joins them before storage closes.
- A completion-persistence failure retains the result and capacity slot until it
  is stored or the backend stops. Do not advise rerunning an operation with an
  unknown completion record.

## Output

Reserve each accepted job's private disk ring before spawning. Preserve the
existing per-job, aggregate, and free-space limits; never evict an active
writer. Disk failures must drain the process, record lost output, and resume
capture when possible. A damaged ring or partial overwrite after a crash is an
explicit terminal refusal rather than stale output.

Inline output is a bounded tail. Durable log reads and literal searches
authorize through the receipt owner before opening files, use absolute byte
offsets, and return bounded chunks. Missing expired files do not remove the
acceptance receipt. Do not add unbounded regex scans or whole-log allocation.

Expected refusals use stable `errorsx.Public` codes and recovery instructions.
Keep private process and persistence causes in host logs.

The process runner is mandatory and injected. Unit tests use closures; real
process tests isolate HOME and execute only a fixed helper binary.
