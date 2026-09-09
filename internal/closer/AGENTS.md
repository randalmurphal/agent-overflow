# Cleanup coordination

This package contains two independent cleanup helpers:

- `Stack` records successful setup steps and runs their cleanup callbacks in
  reverse order. `Add` ignores nil callbacks. `Run` executes every callback
  and joins their errors.
- `RunParallel` starts each labeled `Task.Close` in its own goroutine,
  collects errors, and applies one wall-clock timeout to the whole set. Timed
  out tasks are reported and left running because Go cannot cancel an arbitrary
  callback.

Neither helper provides one-time close semantics or owns resource state.
Callers remain responsible for idempotence, synchronization, cancellation, and
deciding whether setup succeeded and the stack should be discarded.
