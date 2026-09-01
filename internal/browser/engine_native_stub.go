//go:build !(linux && cgo && !gtk3 && !android && !server && !nogui) && !(darwin && cgo && !ios && !server && !nogui)

package browser

// newNativeEngine answers nil on every build without an in-process engine, so
// selectEngine falls through to unavailableEngine. Each half of the build tag
// mirrors that platform's Wails glue exactly — `linux_cgo.go` and the
// `*_darwin.go` set — because these engines share that glue's window and must
// not compile where it does not.
func newNativeEngine(string, ManagerOptions, engineEvents) browserEngine { return nil }
