//go:build windows

package webview2host

import (
	"sync"
	"sync/atomic"
)

// Go-backed COM objects (the completion and event handlers and the
// environment options) are Go structs passed to WebView2 as interface
// pointers. The collector cannot see WebView2's references, so each object
// counts them: it is rooted in comRoots while its count is above zero and
// unrooted by the final Release.
//
// Every such struct starts with the comObject layout: the vtable pointer,
// then its comRef. The constructor returns with one reference, the
// creator's, which the creator releases once it has handed the object to
// WebView2 (whether that call succeeded or not); WebView2's own references
// keep it alive from then on. A handler a page reuses keeps the
// constructor's reference until the page closes.

// comObject is the layout every Go-backed COM object starts with, which is
// what lets one AddRef and one Release serve every vtable. Each vtable
// starts with iUnknownVtbl.
type comObject struct {
	vtbl *iUnknownVtbl
	ref  comRef
}

// comRef is a COM reference count.
type comRef struct {
	n atomic.Int32
}

// comRoots holds every Go-backed COM object with a reference outstanding.
var comRoots = struct {
	mu  sync.Mutex
	set map[*comRef]struct{}
}{set: make(map[*comRef]struct{})}

// own gives a new object its creator's reference.
func (r *comRef) own() {
	r.n.Store(1)
	comRoots.mu.Lock()
	comRoots.set[r] = struct{}{}
	comRoots.mu.Unlock()
}

func (r *comRef) addRef() uint32 {
	return uint32(r.n.Add(1))
}

// release drops one reference and unroots the object with the last. A
// release past zero is a caller bug; the count stays at zero.
func (r *comRef) release() uint32 {
	n := r.n.Add(-1)
	if n > 0 {
		return uint32(n)
	}
	if n < 0 {
		r.n.Store(0)
	}
	comRoots.mu.Lock()
	delete(comRoots.set, r)
	comRoots.mu.Unlock()
	return 0
}

var (
	comAddRef  = newComProc(func(this *comObject) uintptr { return uintptr(this.ref.addRef()) })
	comRelease = newComProc(func(this *comObject) uintptr { return uintptr(this.ref.release()) })
)

// handlerIUnknown is the IUnknown of every handler. WebView2 never queries a
// handler for another interface.
var handlerIUnknown = iUnknownVtbl{
	QueryInterface: newComProc(noInterface),
	AddRef:         comAddRef,
	Release:        comRelease,
}

func noInterface(_ uintptr, _, ppv *uintptr) uintptr {
	if ppv != nil {
		*ppv = 0
	}
	return 0x80004002 // E_NOINTERFACE
}

// callback is a handler's Go function. Detaching it leaves the handler a
// valid COM object whose late Invoke does nothing, so a reference WebView2
// keeps after its page closed retains only the small handler.
type callback[F any] struct {
	fn atomic.Pointer[F]
}

func (c *callback[F]) set(fn F) { c.fn.Store(&fn) }

// load returns the function unless it was detached.
func (c *callback[F]) load() (F, bool) {
	if p := c.fn.Load(); p != nil {
		return *p, true
	}
	var zero F
	return zero, false
}

// take returns the function and detaches it, for a completion WebView2
// invokes once.
func (c *callback[F]) take() (F, bool) {
	if p := c.fn.Swap(nil); p != nil {
		return *p, true
	}
	var zero F
	return zero, false
}

func (c *callback[F]) detach() { c.fn.Store(nil) }
