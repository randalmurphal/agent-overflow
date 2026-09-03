package transport

// InternalServiceMethods names every method on the App receiver that
// the dispatcher must NEVER expose on the wire, regardless of the
// AllowList. The set has two sources:
//
//  1. Wails framework lifecycle hooks (`ServiceName`, `ServiceStartup`,
//     `ServiceShutdown`, `ServeHTTP`) — Wails' own bindings package
//     filters these at framework level; we mirror that behavior so a
//     change in Wails doesn't accidentally expose them through us.
//
//  2. App-level lifecycle hooks marked //wails:ignore in source. These
//     get stripped from the binding generator's output AND skipped at
//     runtime by methodgen + the dispatcher. We also list them here so
//     a developer who accidentally drops the //wails:ignore directive
//     can't reach them from the wire — defense-in-depth alongside the
//     codegen filter.
//
// The codegen tool reads this list (via go-source AST) and the runtime
// dispatcher reads it directly. Single source of truth — change here
// when an App method should disappear from the wire surface.
var InternalServiceMethods = map[string]bool{
	// Wails framework lifecycle.
	"ServiceName":     true,
	"ServiceStartup":  true,
	"ServiceShutdown": true,
	"ServeHTTP":       true,

	// App-level wiring hooks. Marked //wails:ignore in source, but
	// listed here so the dispatcher refuses to expose them even if a
	// developer drops the directive on a future edit.
	"SetEventBus":        true,
	"SetTransportServer": true,
	"Shutdown":           true,
}
