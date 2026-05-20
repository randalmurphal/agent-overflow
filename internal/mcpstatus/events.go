package mcpstatus

// EventBus is the tiny seam the cache emits status updates through.
// app.go provides the production implementation that forwards each
// emission to the transport's `mcp:status` channel; tests pass a
// recording stub. nil bus = silent (no emissions); the cache still
// stores entries.
type EventBus interface {
	Emit(s ServerStatus)
}

// nullBus is the zero-value EventBus the cache falls back to when
// nil is passed, so call sites never need to guard Emit.
type nullBus struct{}

func (nullBus) Emit(ServerStatus) {}
