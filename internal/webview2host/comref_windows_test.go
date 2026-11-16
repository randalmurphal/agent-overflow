//go:build windows

package webview2host

import (
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"golang.org/x/sys/windows"
)

// The Go-backed COM objects WebView2 holds, built the way the host builds
// them.
var comObjectKinds = []struct {
	name string
	make func() unsafe.Pointer
}{
	{"environment options", func() unsafe.Pointer { return unsafe.Pointer(newEnvOptions("--x", "")) }},
	{"environment completed", func() unsafe.Pointer {
		return unsafe.Pointer(newEnvironmentCompletedHandler(func(uintptr, *iCoreWebView2Environment) {}))
	}},
	{"controller completed", func() unsafe.Pointer {
		return unsafe.Pointer(newControllerCompletedHandler(func(uintptr, *iCoreWebView2Controller) {}))
	}},
	{"devtools completed", func() unsafe.Pointer { return unsafe.Pointer(newDevToolsCompletedHandler(func(uintptr, string) {})) }},
	{"find start completed", func() unsafe.Pointer { return unsafe.Pointer(newFindStartCompletedHandler(func(uintptr) {})) }},
	{"accelerator key pressed", func() unsafe.Pointer {
		return unsafe.Pointer(newAcceleratorKeyPressedHandler(func(*iAcceleratorKeyPressedEventArgs) {}))
	}},
	{"process failed", func() unsafe.Pointer { return unsafe.Pointer(newProcessFailedHandler(func(uint32) {})) }},
}

// One AddRef and one Release serve every vtable only while each object
// starts with the comObject layout.
func TestEveryCOMObjectStartsWithTheSharedLayout(t *testing.T) {
	want := unsafe.Offsetof(comObject{}.ref)
	for name, got := range map[string]uintptr{
		"environment options":     unsafe.Offsetof(envOptions{}.ref),
		"environment completed":   unsafe.Offsetof(iEnvironmentCompletedHandler{}.ref),
		"controller completed":    unsafe.Offsetof(iControllerCompletedHandler{}.ref),
		"devtools completed":      unsafe.Offsetof(iDevToolsCompletedHandler{}.ref),
		"find start completed":    unsafe.Offsetof(iFindStartCompletedHandler{}.ref),
		"accelerator key pressed": unsafe.Offsetof(iAcceleratorKeyPressedHandler{}.ref),
		"process failed":          unsafe.Offsetof(iProcessFailedHandler{}.ref),
	} {
		if got != want {
			t.Errorf("%s keeps its count at offset %d, want %d", name, got, want)
		}
	}
}

func collect() {
	for range 3 {
		runtime.GC()
	}
}

// A new object holds its creator's reference. The creator hands it to
// WebView2, which takes its own reference, and releases its own; WebView2's
// alone keeps the object through collection, and its final Release lets the
// object go.
func TestEveryCOMObjectLivesExactlyAsLongAsItsReferences(t *testing.T) {
	for _, kind := range comObjectKinds {
		t.Run(kind.name, func(t *testing.T) {
			base := comRootCount()
			held := func() weak.Pointer[comObject] {
				obj := (*comObject)(kind.make())
				if n := obj.ref.n.Load(); n != 1 || comRootCount() != base+1 {
					t.Fatalf("a new object has %d references and %d roots, want 1 and %d", n, comRootCount(), base+1)
				}
				if n, _, _ := obj.vtbl.AddRef.Call(uintptr(unsafe.Pointer(obj))); n != 2 {
					t.Fatalf("WebView2's AddRef answered %d, want 2", n)
				}
				obj.ref.release()
				if n := obj.ref.n.Load(); n != 1 || comRootCount() != base+1 {
					t.Fatalf("after the creator's release: %d references, %d roots", n, comRootCount())
				}
				return weak.Make(obj)
			}()
			collect()
			obj := held.Value()
			if obj == nil {
				t.Fatal("collected while WebView2 held a reference")
			}
			if n, _, _ := obj.vtbl.Release.Call(uintptr(unsafe.Pointer(obj))); n != 0 {
				t.Fatalf("WebView2's final Release answered %d, want 0", n)
			}
			if comRootCount() != base {
				t.Fatalf("%d roots after the final Release, want %d", comRootCount(), base)
			}
			obj = nil
			collect()
			if held.Value() != nil {
				t.Fatal("the object outlived its final Release")
			}
		})
	}
}

// A detached handler stays a valid object whose late Invoke does nothing,
// and a one-shot completion runs its function once.
func TestALateInvokeOfADetachedHandlerIsANoOp(t *testing.T) {
	base := comRootCount()
	calls := 0
	count := func() { calls++ }
	fakeArgs := uintptr(unsafe.Pointer(new(iAcceleratorKeyPressedEventArgs)))
	result, err := windows.UTF16PtrFromString(`{"targetInfo":{}}`)
	if err != nil {
		t.Fatal(err)
	}

	failed := newProcessFailedHandler(func(uint32) { count() })
	pressed := newAcceleratorKeyPressedHandler(func(*iAcceleratorKeyPressedEventArgs) { count() })
	find := newFindStartCompletedHandler(func(uintptr) { count() })
	devtools := newDevToolsCompletedHandler(func(uintptr, string) { count() })
	env := newEnvironmentCompletedHandler(func(uintptr, *iCoreWebView2Environment) { count() })
	controller := newControllerCompletedHandler(func(uintptr, *iCoreWebView2Controller) { count() })
	invoke := func() {
		_, _, _ = processFailedVtbl.Invoke.Call(uintptr(unsafe.Pointer(failed)), 0, 0)
		_, _, _ = acceleratorKeyPressedVtbl.Invoke.Call(uintptr(unsafe.Pointer(pressed)), 0, fakeArgs)
		_, _, _ = findStartCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(find)), 0)
		_, _, _ = devToolsCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(devtools)), 0, uintptr(unsafe.Pointer(result)))
		_, _, _ = environmentCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(env)), 1, 0)
		_, _, _ = controllerCompletedVtbl.Invoke.Call(uintptr(unsafe.Pointer(controller)), 1, 0)
	}
	invoke()
	if calls != 6 {
		t.Fatalf("first invokes ran %d functions, want 6", calls)
	}
	invoke()
	if calls != 9 {
		t.Fatalf("second invokes ran %d functions in all, want 9: the completions run once, the events every time", calls)
	}

	page := &hostPage{failHandler: failed, keyHandler: pressed, targetInfo: devtools, findStarted: find}
	(&Host{}).retireHandlers(page)
	invoke()
	if calls != 9 {
		t.Fatalf("a retired page's handlers ran %d more functions", calls-9)
	}
	if page.findStarted != nil || find.ref.n.Load() != 0 {
		t.Fatalf("retiring kept the page's find reference (%d)", find.ref.n.Load())
	}
	for _, ref := range []*comRef{&failed.ref, &pressed.ref, &devtools.ref, &env.ref, &controller.ref} {
		ref.release()
	}
	if got := comRootCount(); got != base {
		t.Fatalf("%d roots after every reference went, want %d", got, base)
	}
}

// A page's whole life, the way controllerCompleted and a find build it and
// closePages retires it, with WebView2 releasing its references when the
// controller goes, leaves the root set as it found it.
func TestRepeatedPagesDoNotGrowTheRootSet(t *testing.T) {
	base := comRootCount()
	// webView2Holds takes WebView2's reference through the vtable and returns
	// its final Release.
	webView2Holds := func(p unsafe.Pointer) func() {
		obj := (*comObject)(p)
		_, _, _ = obj.vtbl.AddRef.Call(uintptr(p))
		return func() { _, _, _ = obj.vtbl.Release.Call(uintptr(p)) }
	}
	for range 200 {
		page := &hostPage{}
		var held []func()
		controller := newControllerCompletedHandler(func(uintptr, *iCoreWebView2Controller) {})
		held = append(held, webView2Holds(unsafe.Pointer(controller)))
		controller.ref.release()
		page.failHandler = newProcessFailedHandler(func(uint32) { _ = page })
		held = append(held, webView2Holds(unsafe.Pointer(page.failHandler)))
		page.failHandler.ref.release()
		page.keyHandler = newAcceleratorKeyPressedHandler(func(*iAcceleratorKeyPressedEventArgs) { _ = page })
		held = append(held, webView2Holds(unsafe.Pointer(page.keyHandler)))
		page.keyHandler.ref.release()
		page.targetInfo = newDevToolsCompletedHandler(func(uintptr, string) { _ = page })
		held = append(held, webView2Holds(unsafe.Pointer(page.targetInfo)))
		page.targetInfo.ref.release()
		page.findStarted = newFindStartCompletedHandler(func(uintptr) { _ = page })
		held = append(held, webView2Holds(unsafe.Pointer(page.findStarted)))

		(&Host{}).retireHandlers(page)
		for _, release := range held {
			release()
		}
	}
	if got := comRootCount(); got != base {
		t.Fatalf("200 pages left %d roots, want %d", got, base)
	}
}
