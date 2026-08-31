//go:build !(linux && cgo && !gtk3 && !android && !server && !nogui)

package browser

// newNativeEngine answers nil on every build without an in-process engine, so
// selectEngine falls through to managed Chrome. The build tag mirrors the
// Wails GTK4/WebKitGTK-6.0 glue's own exactly (its linux_cgo.go), because this
// engine shares that window and must not compile where that glue does not.
func newNativeEngine(string, ManagerOptions, engineEvents) browserEngine { return nil }
