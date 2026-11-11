//go:build windows

// com_windows.go is the minimal WebView2 COM surface the pane host needs.
//
// Why hand-written rather than reused. The launcher already links Wails'
// WebView2 bindings, but they live under github.com/wailsapp/wails/v3/
// internal/ and Go's internal rule makes them unimportable from here. The
// public github.com/jchv/go-webview2 has the pieces but its high-level
// Chromium type carries the two bugs the 2026-08-31 spike found and
// patched (/tmp/spike-webview2-dual/VERDICTS.md, "Patches applied"):
//
//  1. `int64(res) < 0` never detects a failed HRESULT on 64-bit, because
//     0x80070057 widens to a POSITIVE int64. Every completion handler
//     there fell through the guard and nil-dereferenced the controller
//     instead of reporting the error, which is what turned a diagnosable
//     ERROR_INVALID_STATE into a crash. Everything below tests HRESULTs
//     as `uint32(hr) != 0`, and `hresult` is the one conversion.
//  2. `environmentOptions` was hard-wired to 0, so
//     AdditionalBrowserArguments — and therefore
//     --remote-debugging-port, the entire point of the pane environment —
//     was unreachable. Here the options object is a first-class argument
//     (envoptions_windows.go).
//
// Only the LOADER is reused, from github.com/jchv/go-webview2/
// webviewloader: it locates or memory-loads WebView2Loader.dll, which is
// the one genuinely awkward part and has no bug in it.
//
// VTABLE LAYOUTS. Every struct below is the FULL flattened chain, not
// just the leaf interface's own slots: a QueryInterface'd ICoreWebView2N
// points at a vtable that begins with IUnknown followed by every
// ancestor's methods in declaration order, and a struct that omitted them
// would call the wrong slot. Orders and IIDs are taken from the
// version-pinned SDK IDL the wails fork carries
// (v3/internal/webview2/scripts/WebView2.1.0.2903.40.idl), not from
// memory, and the prefixes are cross-checked against the equivalent
// structs in that fork's edge package.
package webview2host

import (
	"errors"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// comProc is a COM vtable slot.
type comProc uintptr

func newComProc(fn any) comProc { return comProc(windows.NewCallback(fn)) }

// Call invokes the slot.
//
// The //go:uintptrescapes directive is required, not decorative: it stops
// the compiler moving a uintptr(unsafe.Pointer(x)) argument, which
// satisfies the unsafe.Pointer rule for syscall arguments and keeps a
// pointer onto a growing Go stack valid for the duration of the call.
//
//go:uintptrescapes
func (p comProc) Call(a ...uintptr) (uintptr, uintptr, error) {
	switch len(a) {
	case 0:
		return syscall.Syscall(uintptr(p), 0, 0, 0, 0)
	case 1:
		return syscall.Syscall(uintptr(p), 1, a[0], 0, 0)
	case 2:
		return syscall.Syscall(uintptr(p), 2, a[0], a[1], 0)
	case 3:
		return syscall.Syscall(uintptr(p), 3, a[0], a[1], a[2])
	case 4:
		return syscall.Syscall6(uintptr(p), 4, a[0], a[1], a[2], a[3], 0, 0)
	case 5:
		return syscall.Syscall6(uintptr(p), 5, a[0], a[1], a[2], a[3], a[4], 0)
	case 6:
		return syscall.Syscall6(uintptr(p), 6, a[0], a[1], a[2], a[3], a[4], a[5])
	default:
		panic("webview2host: COM call with too many arguments")
	}
}

type iUnknownVtbl struct {
	QueryInterface comProc
	AddRef         comProc
	Release        comProc
}

// hresult turns a syscall return into an error. The 32-bit conversion is
// the bug-1 fix: an HRESULT is a 32-bit value and every failure code has
// its top bit set, so widening to a signed 64-bit type makes every
// failure look like success.
func hresult(hr uintptr) error {
	if uint32(hr) == 0 {
		return nil
	}
	return syscall.Errno(uint32(hr))
}

// ---------------------------------------------------------------------
// ICoreWebView2Environment / ICoreWebView2Environment10
// ---------------------------------------------------------------------

type iCoreWebView2EnvironmentVtbl struct {
	iUnknownVtbl
	CreateCoreWebView2Controller     comProc
	CreateWebResourceResponse        comProc
	GetBrowserVersionString          comProc
	AddNewBrowserVersionAvailable    comProc
	RemoveNewBrowserVersionAvailable comProc
}

type iCoreWebView2Environment struct {
	vtbl *iCoreWebView2EnvironmentVtbl
}

func (e *iCoreWebView2Environment) addRef() { _, _, _ = e.vtbl.AddRef.Call(uintptr(unsafe.Pointer(e))) }
func (e *iCoreWebView2Environment) release() {
	_, _, _ = e.vtbl.Release.Call(uintptr(unsafe.Pointer(e)))
}

// iCoreWebView2Environment10Vtbl is the flattened chain through
// ICoreWebView2Environment10. Slots 0-18 are identical to the fork's
// iCoreWebView2Environment8Vtbl (which documents the same derivation);
// slot 19 is Environment9's CreateContextMenuItem and 20-22 are
// Environment10's three.
type iCoreWebView2Environment10Vtbl struct {
	iUnknownVtbl
	CreateCoreWebView2Controller                       comProc
	CreateWebResourceResponse                          comProc
	GetBrowserVersionString                            comProc
	AddNewBrowserVersionAvailable                      comProc
	RemoveNewBrowserVersionAvailable                   comProc
	CreateWebResourceRequest                           comProc
	CreateCoreWebView2CompositionController            comProc
	CreateCoreWebView2PointerInfo                      comProc
	GetAutomationProviderForWindow                     comProc
	AddBrowserProcessExited                            comProc
	RemoveBrowserProcessExited                         comProc
	CreatePrintSettings                                comProc
	GetUserDataFolder                                  comProc
	AddProcessInfosChanged                             comProc
	RemoveProcessInfosChanged                          comProc
	GetProcessInfos                                    comProc
	CreateContextMenuItem                              comProc
	CreateCoreWebView2ControllerOptions                comProc
	CreateCoreWebView2ControllerWithOptions            comProc
	CreateCoreWebView2CompositionControllerWithOptions comProc
}

type iCoreWebView2Environment10 struct {
	vtbl *iCoreWebView2Environment10Vtbl
}

// iidEnvironment10 is {ee0eb9df-6f12-46ce-b53f-3f47b9c928e0}, the IID the
// pinned SDK IDL gives ICoreWebView2Environment10. Runtime 110+ / SDK
// 1.0.1587.40 is where multi-profile support landed.
var iidEnvironment10 = windows.GUID{
	Data1: 0xee0eb9df, Data2: 0x6f12, Data3: 0x46ce,
	Data4: [8]byte{0xb5, 0x3f, 0x3f, 0x47, 0xb9, 0xc9, 0x28, 0xe0},
}

// queryEnvironment10 returns nil when the installed runtime predates
// multi-profile support. The host treats that as fatal rather than
// falling back to a shared profile: silently putting two workspaces in
// one cookie jar is the exact failure the env-var scrub exists to
// prevent, and it would be just as invisible here.
func (e *iCoreWebView2Environment) queryEnvironment10() *iCoreWebView2Environment10 {
	var result *iCoreWebView2Environment10
	_, _, _ = e.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(&iidEnvironment10)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (e *iCoreWebView2Environment10) release() {
	_, _, _ = e.vtbl.Release.Call(uintptr(unsafe.Pointer(e)))
}

func (e *iCoreWebView2Environment10) createControllerOptions() (*iCoreWebView2ControllerOptions, error) {
	var options *iCoreWebView2ControllerOptions
	hr, _, _ := e.vtbl.CreateCoreWebView2ControllerOptions.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(&options)),
	)
	if err := hresult(hr); err != nil {
		return nil, err
	}
	return options, nil
}

func (e *iCoreWebView2Environment10) createControllerWithOptions(
	parent uintptr,
	options *iCoreWebView2ControllerOptions,
	handler *iControllerCompletedHandler,
) error {
	hr, _, _ := e.vtbl.CreateCoreWebView2ControllerWithOptions.Call(
		uintptr(unsafe.Pointer(e)),
		parent,
		uintptr(unsafe.Pointer(options)),
		uintptr(unsafe.Pointer(handler)),
	)
	return hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2Environment15 (find options)
// ---------------------------------------------------------------------

// iCoreWebView2Environment15Vtbl is the flattened chain through
// ICoreWebView2Environment15, taken from the C vtable
// ICoreWebView2Environment15Vtbl in WebView2.h of the official
// Microsoft.Web.WebView2 1.0.3405.78 NuGet package: slots 0-22 are
// iCoreWebView2Environment10Vtbl above; 23-28 are Environment11's
// get_FailureReportFolderPath, Environment12's CreateSharedBuffer,
// Environment13's GetProcessExtendedInfos and Environment14's
// CreateWebFileSystemFileHandle, CreateWebFileSystemDirectoryHandle and
// CreateObjectCollection, none of which is called; slot 29 is
// CreateFindOptions.
type iCoreWebView2Environment15Vtbl struct {
	iCoreWebView2Environment10Vtbl
	_                 [6]comProc
	CreateFindOptions comProc
}

type iCoreWebView2Environment15 struct {
	vtbl *iCoreWebView2Environment15Vtbl
}

// iidEnvironment15 is IID_ICoreWebView2Environment15,
// {2ac5ebfb-e654-5961-a667-7971885c7b27} (WebView2.h, SDK 1.0.3405.78).
var iidEnvironment15 = windows.GUID{
	Data1: 0x2ac5ebfb, Data2: 0xe654, Data3: 0x5961,
	Data4: [8]byte{0xa6, 0x67, 0x79, 0x71, 0x88, 0x5c, 0x7b, 0x27},
}

// queryEnvironment15 returns nil on a runtime that predates the find API.
func (e *iCoreWebView2Environment10) queryEnvironment15() *iCoreWebView2Environment15 {
	var result *iCoreWebView2Environment15
	_, _, _ = e.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(&iidEnvironment15)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (e *iCoreWebView2Environment15) release() {
	_, _, _ = e.vtbl.Release.Call(uintptr(unsafe.Pointer(e)))
}

// createFindOptions is CreateFindOptions([out, retval]
// ICoreWebView2FindOptions** value).
func (e *iCoreWebView2Environment15) createFindOptions() (*iCoreWebView2FindOptions, error) {
	var options *iCoreWebView2FindOptions
	hr, _, _ := e.vtbl.CreateFindOptions.Call(
		uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(&options)),
	)
	if err := hresult(hr); err != nil {
		return nil, err
	}
	if options == nil {
		return nil, errors.New("CreateFindOptions returned no options")
	}
	return options, nil
}

// ---------------------------------------------------------------------
// ICoreWebView2FindOptions
// ---------------------------------------------------------------------

// iCoreWebView2FindOptionsVtbl follows the C vtable
// ICoreWebView2FindOptionsVtbl in WebView2.h (SDK 1.0.3405.78): IUnknown,
// then each property's get/put pair in IDL order.
type iCoreWebView2FindOptionsVtbl struct {
	iUnknownVtbl
	GetFindTerm                  comProc
	PutFindTerm                  comProc
	GetIsCaseSensitive           comProc
	PutIsCaseSensitive           comProc
	GetShouldHighlightAllMatches comProc
	PutShouldHighlightAllMatches comProc
	GetShouldMatchWord           comProc
	PutShouldMatchWord           comProc
	GetSuppressDefaultFindDialog comProc
	PutSuppressDefaultFindDialog comProc
}

type iCoreWebView2FindOptions struct {
	vtbl *iCoreWebView2FindOptionsVtbl
}

func (o *iCoreWebView2FindOptions) release() {
	_, _, _ = o.vtbl.Release.Call(uintptr(unsafe.Pointer(o)))
}

// putFindTerm is put_FindTerm([in] LPCWSTR value). The callee copies the
// string; the UTF-16 buffer only has to outlive the call.
func (o *iCoreWebView2FindOptions) putFindTerm(term string) error {
	ptr, err := windows.UTF16PtrFromString(term)
	if err != nil {
		return err
	}
	hr, _, _ := o.vtbl.PutFindTerm.Call(uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(ptr)))
	return hresult(hr)
}

func boolArg(value bool) uintptr {
	if value {
		return 1
	}
	return 0
}

// putShouldHighlightAllMatches is put_ShouldHighlightAllMatches([in] BOOL).
func (o *iCoreWebView2FindOptions) putShouldHighlightAllMatches(value bool) error {
	hr, _, _ := o.vtbl.PutShouldHighlightAllMatches.Call(uintptr(unsafe.Pointer(o)), boolArg(value))
	return hresult(hr)
}

// putSuppressDefaultFindDialog is put_SuppressDefaultFindDialog([in] BOOL).
func (o *iCoreWebView2FindOptions) putSuppressDefaultFindDialog(value bool) error {
	hr, _, _ := o.vtbl.PutSuppressDefaultFindDialog.Call(uintptr(unsafe.Pointer(o)), boolArg(value))
	return hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2Find
// ---------------------------------------------------------------------

// iCoreWebView2FindVtbl follows the C vtable ICoreWebView2FindVtbl in
// WebView2.h (SDK 1.0.3405.78).
type iCoreWebView2FindVtbl struct {
	iUnknownVtbl
	GetActiveMatchIndex           comProc
	GetMatchCount                 comProc
	AddActiveMatchIndexChanged    comProc
	RemoveActiveMatchIndexChanged comProc
	AddMatchCountChanged          comProc
	RemoveMatchCountChanged       comProc
	Start                         comProc
	FindNext                      comProc
	FindPrevious                  comProc
	Stop                          comProc
}

type iCoreWebView2Find struct {
	vtbl *iCoreWebView2FindVtbl
}

func (f *iCoreWebView2Find) release() {
	_, _, _ = f.vtbl.Release.Call(uintptr(unsafe.Pointer(f)))
}

// start is Start([in] ICoreWebView2FindOptions* options, [in]
// ICoreWebView2FindStartCompletedHandler* handler). With the default dialog
// not suppressed, the engine's own find bar shows once it completes.
func (f *iCoreWebView2Find) start(options *iCoreWebView2FindOptions, handler *iFindStartCompletedHandler) error {
	hr, _, _ := f.vtbl.Start.Call(
		uintptr(unsafe.Pointer(f)),
		uintptr(unsafe.Pointer(options)),
		uintptr(unsafe.Pointer(handler)),
	)
	return hresult(hr)
}

// stop is Stop(): ends the session and hides the find bar; a no-op with no
// session.
func (f *iCoreWebView2Find) stop() error {
	hr, _, _ := f.vtbl.Stop.Call(uintptr(unsafe.Pointer(f)))
	return hresult(hr)
}

// matchCount is get_MatchCount([out, retval] INT32* value).
func (f *iCoreWebView2Find) matchCount() (int32, error) {
	var value int32
	hr, _, _ := f.vtbl.GetMatchCount.Call(uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(&value)))
	return value, hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2_28 (Find)
// ---------------------------------------------------------------------

// iCoreWebView2_28Vtbl is the flattened chain through ICoreWebView2_28, from
// the C vtable ICoreWebView2_28Vtbl in WebView2.h (SDK 1.0.3405.78): slots
// 0-60 are iCoreWebView2Vtbl below, 61-134 the methods ICoreWebView2_2
// through ICoreWebView2_27 add (none of which is called), and 135 is
// get_Find.
type iCoreWebView2_28Vtbl struct {
	iCoreWebView2Vtbl
	_       [74]comProc
	GetFind comProc
}

type iCoreWebView2_28 struct {
	vtbl *iCoreWebView2_28Vtbl
}

// iidWebView2_28 is IID_ICoreWebView2_28,
// {62e50381-5bf5-51a8-aae0-f20a3a9c8a90} (WebView2.h, SDK 1.0.3405.78).
var iidWebView2_28 = windows.GUID{
	Data1: 0x62e50381, Data2: 0x5bf5, Data3: 0x51a8,
	Data4: [8]byte{0xaa, 0xe0, 0xf2, 0x0a, 0x3a, 0x9c, 0x8a, 0x90},
}

// queryWebView28 returns nil on a runtime that predates the find API.
func (v *iCoreWebView2) queryWebView28() *iCoreWebView2_28 {
	var result *iCoreWebView2_28
	_, _, _ = v.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(&iidWebView2_28)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (v *iCoreWebView2_28) release() {
	_, _, _ = v.vtbl.Release.Call(uintptr(unsafe.Pointer(v)))
}

// find is get_Find([out, retval] ICoreWebView2Find** value).
func (v *iCoreWebView2_28) find() (*iCoreWebView2Find, error) {
	var find *iCoreWebView2Find
	hr, _, _ := v.vtbl.GetFind.Call(uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(&find)))
	if err := hresult(hr); err != nil {
		return nil, err
	}
	if find == nil {
		return nil, errors.New("get_Find returned no find")
	}
	return find, nil
}

// pageFind answers the view's find, or nil on a runtime without one.
func (v *iCoreWebView2) pageFind() (*iCoreWebView2Find, error) {
	view28 := v.queryWebView28()
	if view28 == nil {
		return nil, nil
	}
	defer view28.release()
	return view28.find()
}

// ---------------------------------------------------------------------
// ICoreWebView2ControllerOptions
// ---------------------------------------------------------------------

type iCoreWebView2ControllerOptionsVtbl struct {
	iUnknownVtbl
	GetProfileName            comProc
	PutProfileName            comProc
	GetIsInPrivateModeEnabled comProc
	PutIsInPrivateModeEnabled comProc
}

type iCoreWebView2ControllerOptions struct {
	vtbl *iCoreWebView2ControllerOptionsVtbl
}

func (o *iCoreWebView2ControllerOptions) release() {
	_, _, _ = o.vtbl.Release.Call(uintptr(unsafe.Pointer(o)))
}

func (o *iCoreWebView2ControllerOptions) putProfileName(name string) error {
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	hr, _, _ := o.vtbl.PutProfileName.Call(
		uintptr(unsafe.Pointer(o)),
		uintptr(unsafe.Pointer(ptr)),
	)
	return hresult(hr)
}

func (o *iCoreWebView2ControllerOptions) putInPrivate(enabled bool) error {
	var value int32
	if enabled {
		value = 1
	}
	hr, _, _ := o.vtbl.PutIsInPrivateModeEnabled.Call(
		uintptr(unsafe.Pointer(o)),
		uintptr(value),
	)
	return hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2Controller
// ---------------------------------------------------------------------

type iCoreWebView2ControllerVtbl struct {
	iUnknownVtbl
	GetIsVisible                      comProc
	PutIsVisible                      comProc
	GetBounds                         comProc
	PutBounds                         comProc
	GetZoomFactor                     comProc
	PutZoomFactor                     comProc
	AddZoomFactorChanged              comProc
	RemoveZoomFactorChanged           comProc
	SetBoundsAndZoomFactor            comProc
	MoveFocus                         comProc
	AddMoveFocusRequested             comProc
	RemoveMoveFocusRequested          comProc
	AddGotFocus                       comProc
	RemoveGotFocus                    comProc
	AddLostFocus                      comProc
	RemoveLostFocus                   comProc
	AddAcceleratorKeyPressed          comProc
	RemoveAcceleratorKeyPressed       comProc
	GetParentWindow                   comProc
	PutParentWindow                   comProc
	NotifyParentWindowPositionChanged comProc
	Close                             comProc
	GetCoreWebView2                   comProc
}

type iCoreWebView2Controller struct {
	vtbl *iCoreWebView2ControllerVtbl
}

// rect (the Win32 RECT shape put_Bounds takes) is declared in
// panelayout.go, with the pure layout arithmetic that produces it.

func (c *iCoreWebView2Controller) addRef() {
	_, _, _ = c.vtbl.AddRef.Call(uintptr(unsafe.Pointer(c)))
}

func (c *iCoreWebView2Controller) release() {
	_, _, _ = c.vtbl.Release.Call(uintptr(unsafe.Pointer(c)))
}

func (c *iCoreWebView2Controller) putBounds(bounds rect) error {
	hr, _, _ := c.vtbl.PutBounds.Call(
		uintptr(unsafe.Pointer(c)),
		uintptr(unsafe.Pointer(&bounds)),
	)
	return hresult(hr)
}

func (c *iCoreWebView2Controller) addAcceleratorKeyPressed(handler *iAcceleratorKeyPressedHandler) (eventRegistrationToken, error) {
	var token eventRegistrationToken
	hr, _, _ := c.vtbl.AddAcceleratorKeyPressed.Call(
		uintptr(unsafe.Pointer(c)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return token, hresult(hr)
}

// moveFocusProgrammatic is COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC (0)
// in WebView2.h.
const moveFocusProgrammatic = 0

// moveFocus is MoveFocus([in] COREWEBVIEW2_MOVE_FOCUS_REASON reason): keyboard
// focus moves into the page.
func (c *iCoreWebView2Controller) moveFocus(reason uint32) error {
	hr, _, _ := c.vtbl.MoveFocus.Call(uintptr(unsafe.Pointer(c)), uintptr(reason))
	return hresult(hr)
}

func (c *iCoreWebView2Controller) putIsVisible(visible bool) error {
	var value int32
	if visible {
		value = 1
	}
	hr, _, _ := c.vtbl.PutIsVisible.Call(uintptr(unsafe.Pointer(c)), uintptr(value))
	return hresult(hr)
}

func (c *iCoreWebView2Controller) close() error {
	hr, _, _ := c.vtbl.Close.Call(uintptr(unsafe.Pointer(c)))
	return hresult(hr)
}

func (c *iCoreWebView2Controller) coreWebView2() (*iCoreWebView2, error) {
	var view *iCoreWebView2
	hr, _, _ := c.vtbl.GetCoreWebView2.Call(
		uintptr(unsafe.Pointer(c)),
		uintptr(unsafe.Pointer(&view)),
	)
	if err := hresult(hr); err != nil {
		return nil, err
	}
	return view, nil
}

// ---------------------------------------------------------------------
// ICoreWebView2Controller2
// ---------------------------------------------------------------------

// iCoreWebView2Controller2Vtbl is the flattened chain through
// ICoreWebView2Controller2. Slots 0-25 are byte-for-byte
// iCoreWebView2ControllerVtbl above (IUnknown plus ICoreWebView2
// Controller's 23 methods, get_IsVisible .. get_CoreWebView2); the
// derived interface appends exactly its own two, in IDL declaration
// order:
//
//	[propget] HRESULT DefaultBackgroundColor([out, retval] COREWEBVIEW2_COLOR* value);
//	[propput] HRESULT DefaultBackgroundColor([in] COREWEBVIEW2_COLOR value);
//
// from WebView2.1.0.2903.40.idl line 7346, cross-checked against the
// wails fork's edge.ICoreWebView2Controller2.
type iCoreWebView2Controller2Vtbl struct {
	iCoreWebView2ControllerVtbl
	GetDefaultBackgroundColor comProc
	PutDefaultBackgroundColor comProc
}

type iCoreWebView2Controller2 struct {
	vtbl *iCoreWebView2Controller2Vtbl
}

// iidController2 is {c979903e-d4ca-4228-92eb-47ee3fa96eab}, the IID the
// pinned SDK IDL gives ICoreWebView2Controller2 (runtime 90+).
var iidController2 = windows.GUID{
	Data1: 0xc979903e, Data2: 0xd4ca, Data3: 0x4228,
	Data4: [8]byte{0x92, 0xeb, 0x47, 0xee, 0x3f, 0xa9, 0x6e, 0xab},
}

// queryController2 returns nil on a runtime old enough to lack the
// interface. The caller logs once and keeps the engine default: a pane
// whose freshly exposed strips flash white is worse-looking, not broken,
// and failing a bounds directive over it would be.
func (c *iCoreWebView2Controller) queryController2() *iCoreWebView2Controller2 {
	var result *iCoreWebView2Controller2
	_, _, _ = c.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(c)),
		uintptr(unsafe.Pointer(&iidController2)),
		uintptr(unsafe.Pointer(&result)),
	)
	return result
}

func (c *iCoreWebView2Controller2) release() {
	_, _, _ = c.vtbl.Release.Call(uintptr(unsafe.Pointer(c)))
}

// putDefaultBackgroundColor sets the color WebView2 paints under all web
// content — before the first navigation, between navigations, and behind
// any part of a page that has not presented yet.
//
// The argument is a COREWEBVIEW2_COLOR BY VALUE, not a pointer. See
// paneColor.word for why a 4-byte struct travels as one register word.
func (c *iCoreWebView2Controller2) putDefaultBackgroundColor(color paneColor) error {
	hr, _, _ := c.vtbl.PutDefaultBackgroundColor.Call(
		uintptr(unsafe.Pointer(c)),
		uintptr(color.word()),
	)
	return hresult(hr)
}

// ---------------------------------------------------------------------
// ICoreWebView2
// ---------------------------------------------------------------------

type iCoreWebView2Vtbl struct {
	iUnknownVtbl
	GetSettings                            comProc
	GetSource                              comProc
	Navigate                               comProc
	NavigateToString                       comProc
	AddNavigationStarting                  comProc
	RemoveNavigationStarting               comProc
	AddContentLoading                      comProc
	RemoveContentLoading                   comProc
	AddSourceChanged                       comProc
	RemoveSourceChanged                    comProc
	AddHistoryChanged                      comProc
	RemoveHistoryChanged                   comProc
	AddNavigationCompleted                 comProc
	RemoveNavigationCompleted              comProc
	AddFrameNavigationStarting             comProc
	RemoveFrameNavigationStarting          comProc
	AddFrameNavigationCompleted            comProc
	RemoveFrameNavigationCompleted         comProc
	AddScriptDialogOpening                 comProc
	RemoveScriptDialogOpening              comProc
	AddPermissionRequested                 comProc
	RemovePermissionRequested              comProc
	AddProcessFailed                       comProc
	RemoveProcessFailed                    comProc
	AddScriptToExecuteOnDocumentCreated    comProc
	RemoveScriptToExecuteOnDocumentCreated comProc
	ExecuteScript                          comProc
	CapturePreview                         comProc
	Reload                                 comProc
	PostWebMessageAsJSON                   comProc
	PostWebMessageAsString                 comProc
	AddWebMessageReceived                  comProc
	RemoveWebMessageReceived               comProc
	CallDevToolsProtocolMethod             comProc
	GetBrowserProcessID                    comProc
	GetCanGoBack                           comProc
	GetCanGoForward                        comProc
	GoBack                                 comProc
	GoForward                              comProc
	GetDevToolsProtocolEventReceiver       comProc
	Stop                                   comProc
	AddNewWindowRequested                  comProc
	RemoveNewWindowRequested               comProc
	AddDocumentTitleChanged                comProc
	RemoveDocumentTitleChanged             comProc
	GetDocumentTitle                       comProc
	AddHostObjectToScript                  comProc
	RemoveHostObjectFromScript             comProc
	OpenDevToolsWindow                     comProc
	AddContainsFullScreenElementChanged    comProc
	RemoveContainsFullScreenElementChanged comProc
	GetContainsFullScreenElement           comProc
	AddWebResourceRequested                comProc
	RemoveWebResourceRequested             comProc
	AddWebResourceRequestedFilter          comProc
	RemoveWebResourceRequestedFilter       comProc
	AddWindowCloseRequested                comProc
	RemoveWindowCloseRequested             comProc
}

type iCoreWebView2 struct {
	vtbl *iCoreWebView2Vtbl
}

type eventRegistrationToken struct{ value int64 }

func (v *iCoreWebView2) release() { _, _, _ = v.vtbl.Release.Call(uintptr(unsafe.Pointer(v))) }

func (v *iCoreWebView2) openDevToolsWindow() error {
	hr, _, _ := v.vtbl.OpenDevToolsWindow.Call(uintptr(unsafe.Pointer(v)))
	return hresult(hr)
}

func (v *iCoreWebView2) addProcessFailed(handler *iProcessFailedHandler) (eventRegistrationToken, error) {
	var token eventRegistrationToken
	hr, _, _ := v.vtbl.AddProcessFailed.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(handler)),
		uintptr(unsafe.Pointer(&token)),
	)
	return token, hresult(hr)
}

func (v *iCoreWebView2) callDevToolsProtocolMethod(method, paramsJSON string, handler *iDevToolsCompletedHandler) error {
	methodPtr, err := windows.UTF16PtrFromString(method)
	if err != nil {
		return err
	}
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	paramsPtr, err := windows.UTF16PtrFromString(paramsJSON)
	if err != nil {
		return err
	}
	hr, _, _ := v.vtbl.CallDevToolsProtocolMethod.Call(
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(methodPtr)),
		uintptr(unsafe.Pointer(paramsPtr)),
		uintptr(unsafe.Pointer(handler)),
	)
	return hresult(hr)
}

// ---------------------------------------------------------------------
// Completion / event handlers
// ---------------------------------------------------------------------

// Handler objects are Go structs whose first field is a vtable pointer,
// which is what makes &handler a valid COM interface pointer. Their
// IUnknown is a no-op: each handler is kept alive by a Go reference for
// exactly as long as WebView2 can call it (the host's handler map, or the
// pending-create slot), so refcounting would add bookkeeping that decides
// nothing. AddRef/Release therefore return 1 rather than a real count.

type handlerVtbl struct {
	iUnknownVtbl
	Invoke comProc
}

func noopQueryInterface(_ uintptr, _, _ uintptr) uintptr {
	// E_NOINTERFACE. WebView2 never queries these back to another
	// interface; answering "no" is both honest and safe.
	return 0x80004002
}

func noopAddRefRelease(_ uintptr) uintptr { return 1 }

// --- environment created ---

type iEnvironmentCompletedHandler struct {
	vtbl *handlerVtbl
	done func(hr uintptr, env *iCoreWebView2Environment)
}

var environmentCompletedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iEnvironmentCompletedHandler, hr uintptr, env *iCoreWebView2Environment) uintptr {
		this.done(hr, env)
		return 0
	}),
}

func newEnvironmentCompletedHandler(done func(uintptr, *iCoreWebView2Environment)) *iEnvironmentCompletedHandler {
	return &iEnvironmentCompletedHandler{vtbl: &environmentCompletedVtbl, done: done}
}

// --- controller created ---

type iControllerCompletedHandler struct {
	vtbl *handlerVtbl
	done func(hr uintptr, controller *iCoreWebView2Controller)
}

var controllerCompletedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iControllerCompletedHandler, hr uintptr, controller *iCoreWebView2Controller) uintptr {
		this.done(hr, controller)
		return 0
	}),
}

func newControllerCompletedHandler(done func(uintptr, *iCoreWebView2Controller)) *iControllerCompletedHandler {
	return &iControllerCompletedHandler{vtbl: &controllerCompletedVtbl, done: done}
}

// --- CallDevToolsProtocolMethod completed ---

type iDevToolsCompletedHandler struct {
	vtbl *handlerVtbl
	done func(hr uintptr, resultJSON string)
}

var devToolsCompletedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iDevToolsCompletedHandler, hr uintptr, result *uint16) uintptr {
		this.done(hr, windows.UTF16PtrToString(result))
		return 0
	}),
}

func newDevToolsCompletedHandler(done func(uintptr, string)) *iDevToolsCompletedHandler {
	return &iDevToolsCompletedHandler{vtbl: &devToolsCompletedVtbl, done: done}
}

// --- Find Start completed ---

// iFindStartCompletedHandler is ICoreWebView2FindStartCompletedHandler,
// whose Invoke is Invoke([in] HRESULT errorCode) (WebView2.h, SDK
// 1.0.3405.78).
type iFindStartCompletedHandler struct {
	vtbl *handlerVtbl
	done func(hr uintptr)
}

var findStartCompletedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iFindStartCompletedHandler, hr uintptr) uintptr {
		this.done(hr)
		return 0
	}),
}

func newFindStartCompletedHandler(done func(uintptr)) *iFindStartCompletedHandler {
	return &iFindStartCompletedHandler{vtbl: &findStartCompletedVtbl, done: done}
}

// --- AcceleratorKeyPressed ---

// ICoreWebView2AcceleratorKeyPressedEventArgs. Only the three members the
// chord gate reads are called; the vtable is complete so the offsets hold.
type iAcceleratorKeyPressedEventArgsVtbl struct {
	iUnknownVtbl
	GetKeyEventKind      comProc
	GetVirtualKey        comProc
	GetKeyEventLParam    comProc
	GetPhysicalKeyStatus comProc
	GetHandled           comProc
	PutHandled           comProc
}

type iAcceleratorKeyPressedEventArgs struct {
	vtbl *iAcceleratorKeyPressedEventArgsVtbl
}

// COREWEBVIEW2_KEY_EVENT_KIND: the two DOWN kinds. SYSTEM_KEY_DOWN is how
// every Alt chord arrives.
const (
	keyEventKindKeyDown       uint32 = 0
	keyEventKindSystemKeyDown uint32 = 2
)

func (a *iAcceleratorKeyPressedEventArgs) keyEventKind() uint32 {
	value := uint32(0xffffffff)
	_, _, _ = a.vtbl.GetKeyEventKind.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&value)))
	return value
}

func (a *iAcceleratorKeyPressedEventArgs) virtualKey() uint32 {
	var value uint32
	_, _, _ = a.vtbl.GetVirtualKey.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(&value)))
	return value
}

func (a *iAcceleratorKeyPressedEventArgs) putHandled(handled bool) error {
	var value int32
	if handled {
		value = 1
	}
	hr, _, _ := a.vtbl.PutHandled.Call(uintptr(unsafe.Pointer(a)), uintptr(value))
	return hresult(hr)
}

type iAcceleratorKeyPressedHandler struct {
	vtbl    *handlerVtbl
	pressed func(args *iAcceleratorKeyPressedEventArgs)
}

var acceleratorKeyPressedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iAcceleratorKeyPressedHandler, _ *iCoreWebView2Controller, args *iAcceleratorKeyPressedEventArgs) uintptr {
		if args != nil {
			this.pressed(args)
		}
		return 0
	}),
}

func newAcceleratorKeyPressedHandler(pressed func(*iAcceleratorKeyPressedEventArgs)) *iAcceleratorKeyPressedHandler {
	return &iAcceleratorKeyPressedHandler{vtbl: &acceleratorKeyPressedVtbl, pressed: pressed}
}

// --- ProcessFailed ---

type iProcessFailedEventArgsVtbl struct {
	iUnknownVtbl
	GetProcessFailedKind comProc
}

type iProcessFailedEventArgs struct {
	vtbl *iProcessFailedEventArgsVtbl
}

func (a *iProcessFailedEventArgs) kind() uint32 {
	value := uint32(0xffffffff)
	_, _, _ = a.vtbl.GetProcessFailedKind.Call(
		uintptr(unsafe.Pointer(a)),
		uintptr(unsafe.Pointer(&value)),
	)
	return value
}

type iProcessFailedHandler struct {
	vtbl *handlerVtbl
	done func(kind uint32)
}

var processFailedVtbl = handlerVtbl{
	iUnknownVtbl: iUnknownVtbl{
		QueryInterface: newComProc(noopQueryInterface),
		AddRef:         newComProc(noopAddRefRelease),
		Release:        newComProc(noopAddRefRelease),
	},
	Invoke: newComProc(func(this *iProcessFailedHandler, _ *iCoreWebView2, args *iProcessFailedEventArgs) uintptr {
		kind := uint32(0xffffffff)
		if args != nil {
			kind = args.kind()
		}
		this.done(kind)
		return 0
	}),
}

func newProcessFailedHandler(done func(uint32)) *iProcessFailedHandler {
	return &iProcessFailedHandler{vtbl: &processFailedVtbl, done: done}
}

// processFailedKindName renders COREWEBVIEW2_PROCESS_FAILED_KIND for the
// report detail. Only the two kinds the backend acts on differently are
// named; the rest report their numeric value rather than a guess.
func processFailedKindName(kind uint32) string {
	switch kind {
	case 0:
		return "browser-process-exited"
	case 1:
		return "render-process-exited"
	case 2:
		return "render-process-unresponsive"
	default:
		return "kind-" + strconv.FormatUint(uint64(kind), 10)
	}
}
