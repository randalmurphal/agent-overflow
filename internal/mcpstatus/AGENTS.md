# `internal/mcpstatus`

Concurrency-safe in-memory MCP server status cache shared by provider adapters. SQLite and UI projections do not belong here.

- Keep stale entries after expiry or fetch failure; `Invalidate` is the explicit removal path.
- `RefreshProvider` always fetches and coalesces concurrent refreshes. Clone result slices for every caller.
- Cache every status returned by a fetcher, even when the fetch also reports an
  error. This preserves useful partial results while returning the error to the
  caller. A fetch error with no returned statuses leaves prior entries intact.
- Preserve a provider-supplied error across an error-less probe when status is unchanged. A provider update or status transition ends that retention.
- Stamp source inside the cache so stored, emitted, and returned values agree.
- Keep provider parsing behind `Fetcher` implementations in provider packages.

Transition tests are required for error retention and invalidation behavior.
