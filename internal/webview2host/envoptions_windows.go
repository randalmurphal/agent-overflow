//go:build windows

package webview2host

import (
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A hand-rolled ICoreWebView2EnvironmentOptions.
//
// This is the object go-webview2 could not pass (spike bug 2), and it is
// the only way --remote-debugging-port reaches the pane environment. The
// loader calls the GETTERS only, so a minimal implementation is enough,
// but two details are not optional:
//
//   - Strings handed back MUST be CoTaskMemAlloc'd. The loader frees them
//     with CoTaskMemFree; a Go pointer here corrupts the heap.
//   - TargetCompatibleBrowserVersion must be a REAL version string. The
//     loader validates it with CompareBrowserVersions, and an empty one
//     makes CreateCoreWebView2EnvironmentWithOptions fail E_INVALIDARG.
//     (Both measured in the 2026-08-31 spike.)
//
// The object outlives every call by construction: it is retained by the
// host for the process lifetime, so its refcount is bookkeeping only.

var (
	ole32              = windows.NewLazySystemDLL("ole32.dll")
	procCoTaskMemAlloc = ole32.NewProc("CoTaskMemAlloc")
)

// minTargetBrowserVersion is the WebView2 SDK's own floor. The host
// passes the installed runtime's version when it can read it, and falls
// back to this so a failed version probe degrades to "any runtime will
// do" rather than to E_INVALIDARG.
const minTargetBrowserVersion = "86.0.616.0"

type envOptionsVtbl struct {
	iUnknownVtbl
	GetAdditionalBrowserArguments      comProc
	PutAdditionalBrowserArguments      comProc
	GetLanguage                        comProc
	PutLanguage                        comProc
	GetTargetCompatibleBrowserVersion  comProc
	PutTargetCompatibleBrowserVersion  comProc
	GetAllowSingleSignOnUsingOSPrimary comProc
	PutAllowSingleSignOnUsingOSPrimary comProc
}

type envOptions struct {
	vtbl *envOptionsVtbl
	refs int32
	// args and targetVersion are kept as Go strings so the values stay
	// alive; every handout is a fresh CoTaskMemAlloc copy.
	args          string
	targetVersion string
}

// IID_ICoreWebView2EnvironmentOptions, from the pinned SDK IDL.
var iidEnvironmentOptions = windows.GUID{
	Data1: 0x2fde08a8, Data2: 0x1e9a, Data3: 0x4766,
	Data4: [8]byte{0x8c, 0x05, 0x95, 0xa9, 0xce, 0xb9, 0xd1, 0xc5},
}

var iidIUnknown = windows.GUID{
	Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
}

// coTaskMemString copies s into COM-allocated memory as a NUL-terminated
// UTF-16 string. Returns 0 on allocation failure, which the loader reads
// as "no value" — the correct degradation for an empty language string
// and a hard failure for the arguments, which is what we want.
func coTaskMemString(s string) uintptr {
	encoded := windows.StringToUTF16(s)
	size := uintptr(len(encoded) * 2)
	ptr, _, _ := procCoTaskMemAlloc.Call(size)
	if ptr == 0 {
		return 0
	}
	// `go vet -unsafeptr` flags the conversion below, and the warning is
	// expected rather than a defect: the address came from the COM task
	// allocator, so it names memory outside the Go heap that the
	// collector neither tracks nor moves. It is exactly the case the
	// unsafe.Pointer rules permit and the checker cannot see. Note the
	// checker is not part of `go test`'s default vet set, so this does
	// not affect the gates.
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(encoded)), encoded)
	return ptr
}

var envOptionsVtblInstance = envOptionsVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(func(this *envOptions, riid *windows.GUID, ppv *uintptr) uintptr {
			if *riid == iidEnvironmentOptions || *riid == iidIUnknown {
				atomic.AddInt32(&this.refs, 1)
				*ppv = uintptr(unsafe.Pointer(this))
				return 0
			}
			*ppv = 0
			return 0x80004002 // E_NOINTERFACE
		}),
		AddRef: newComProc(func(this *envOptions) uintptr {
			return uintptr(atomic.AddInt32(&this.refs, 1))
		}),
		Release: newComProc(func(this *envOptions) uintptr {
			return uintptr(atomic.AddInt32(&this.refs, -1))
		}),
	},
	GetAdditionalBrowserArguments: newComProc(func(this *envOptions, out *uintptr) uintptr {
		*out = coTaskMemString(this.args)
		return 0
	}),
	PutAdditionalBrowserArguments: newComProc(func(_ *envOptions, _ uintptr) uintptr { return 0 }),
	GetLanguage: newComProc(func(_ *envOptions, out *uintptr) uintptr {
		*out = coTaskMemString("")
		return 0
	}),
	PutLanguage: newComProc(func(_ *envOptions, _ uintptr) uintptr { return 0 }),
	GetTargetCompatibleBrowserVersion: newComProc(func(this *envOptions, out *uintptr) uintptr {
		*out = coTaskMemString(this.targetVersion)
		return 0
	}),
	PutTargetCompatibleBrowserVersion: newComProc(func(_ *envOptions, _ uintptr) uintptr { return 0 }),
	GetAllowSingleSignOnUsingOSPrimary: newComProc(func(_ *envOptions, out *int32) uintptr {
		*out = 0
		return 0
	}),
	PutAllowSingleSignOnUsingOSPrimary: newComProc(func(_ *envOptions, _ int32) uintptr { return 0 }),
}

// newEnvOptions returns a COM pointer usable as the environmentOptions
// argument. The Go object is retained by the caller for the environment's
// lifetime; the returned uintptr is not a Go pointer the collector
// tracks.
func newEnvOptions(args, targetVersion string) (*envOptions, uintptr) {
	if targetVersion == "" {
		targetVersion = minTargetBrowserVersion
	}
	options := &envOptions{
		vtbl:          &envOptionsVtblInstance,
		refs:          1,
		args:          args,
		targetVersion: targetVersion,
	}
	return options, uintptr(unsafe.Pointer(options))
}
