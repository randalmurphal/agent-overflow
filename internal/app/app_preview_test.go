package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/devscan"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

// Nothing here scans the machine. A real scan reads this host's socket
// tables and then dials every candidate port on loopback, which on a
// developer's box means their own work; previewScanner refuses to build
// one inside a test binary, and these fixtures install their own.

// fakeScanner records what it was asked and answers what it was told to.
type fakeScanner struct {
	servers []devscan.DevServer
	err     error

	calls   int
	owners  []devscan.Owner
	allowed []int
}

func (f *fakeScanner) Scan(_ context.Context, owners []devscan.Owner, allowed []int) ([]devscan.DevServer, error) {
	f.calls++
	f.owners = owners
	f.allowed = allowed
	if f.err != nil {
		return nil, f.err
	}
	return f.servers, nil
}

func newPreviewTestApp(t *testing.T, scanner devServerScanner) *App {
	t.Helper()
	app := &App{}
	app.setSettingsService(settings.NewService(t.TempDir()))
	app.preview.scanner = scanner
	return app
}

// The refusal is the DEFAULT, not something a fixture opts into: a test
// that forgets to install a scanner must fail loudly rather than probe
// the developer's own ports.
func TestPreviewScannerRefusesToScanInsideATestBinary(t *testing.T) {
	app := &App{}
	app.setSettingsService(settings.NewService(t.TempDir()))

	if _, err := app.GetDevServers(context.Background()); err == nil {
		t.Fatal("GetDevServers scanned this machine from a test binary")
	}
}

func TestGetDevServersScansOnDemand(t *testing.T) {
	scanner := &fakeScanner{servers: []devscan.DevServer{
		{Port: 5173, ThreadID: "thread-a", Allowed: true, Source: devscan.SourceAttributed, Listening: true},
	}}
	app := newPreviewTestApp(t, scanner)

	list, err := app.GetDevServers(context.Background())
	if err != nil {
		t.Fatalf("GetDevServers: %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scans = %d, want 1: the loop is idle here, so the RPC must scan itself", scanner.calls)
	}
	if len(list.Servers) != 1 || list.Servers[0].Port != 5173 {
		t.Fatalf("servers = %+v", list.Servers)
	}
	// Loopback bind, no tailnet: there is no address to share a preview
	// on, and the empty string is what the client renders its own
	// sentence for.
	if list.PreviewHost != "" {
		t.Fatalf("PreviewHost = %q, want empty on a loopback-only backend", list.PreviewHost)
	}
}

// The machine saying it cannot be looked at is an ANSWER, and it must
// reach the caller. An empty list would read as "nothing is listening",
// which is a different sentence.
func TestGetDevServersSurfacesAHaltedScan(t *testing.T) {
	scanner := &fakeScanner{err: devscan.ErrUnsupported}
	app := newPreviewTestApp(t, scanner)

	if _, err := app.GetDevServers(context.Background()); !errors.Is(err, devscan.ErrUnsupported) {
		t.Fatalf("error = %v, want the platform refusal verbatim", err)
	}
	// And it is remembered rather than re-asked.
	if _, err := app.GetDevServers(context.Background()); !errors.Is(err, devscan.ErrUnsupported) {
		t.Fatalf("second call: error = %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scans = %d, want 1: a machine that already said no is not asked again", scanner.calls)
	}
}

func TestAllowAndDisallowPreviewPortsMoveTheStoredSet(t *testing.T) {
	scanner := &fakeScanner{}
	app := newPreviewTestApp(t, scanner)
	ctx := context.Background()

	ports, err := app.AllowPreviewPort(ctx, 5173)
	if err != nil {
		t.Fatalf("AllowPreviewPort: %v", err)
	}
	if !slices.Equal(ports, []int{5173}) {
		t.Fatalf("ports = %v, want [5173]", ports)
	}
	if _, err := app.AllowPreviewPort(ctx, 3000); err != nil {
		t.Fatalf("AllowPreviewPort: %v", err)
	}
	// Allowing a port twice is the same request, not a duplicate row.
	ports, err = app.AllowPreviewPort(ctx, 5173)
	if err != nil {
		t.Fatalf("AllowPreviewPort repeat: %v", err)
	}
	if !slices.Equal(ports, []int{3000, 5173}) {
		t.Fatalf("ports = %v, want the sorted set [3000 5173]", ports)
	}

	// The scan sees the new set, so the list refreshes in the same act
	// rather than on a tick that may never come.
	if !slices.Equal(scanner.allowed, []int{3000, 5173}) {
		t.Fatalf("the scan was handed allowed = %v", scanner.allowed)
	}

	ports, err = app.DisallowPreviewPort(ctx, 3000)
	if err != nil {
		t.Fatalf("DisallowPreviewPort: %v", err)
	}
	if !slices.Equal(ports, []int{5173}) {
		t.Fatalf("ports = %v, want [5173]", ports)
	}

	// Emptying the set answers with a list, not null: a client should not
	// have to coalesce one absent value per read.
	ports, err = app.DisallowPreviewPort(ctx, 5173)
	if err != nil {
		t.Fatalf("DisallowPreviewPort: %v", err)
	}
	if ports == nil || len(ports) != 0 {
		t.Fatalf("ports = %v, want an empty list", ports)
	}
	if stored := app.currentSettings().Network.PreviewPorts; len(stored) != 0 {
		t.Fatalf("stored previewPorts = %v, want none", stored)
	}
}

// An impossible port is refused by the one settings write path, and the
// refusal names the value rather than losing it silently.
func TestAllowPreviewPortRefusesAnImpossiblePort(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})

	if _, err := app.AllowPreviewPort(context.Background(), 70000); err == nil {
		t.Fatal("a port outside the range was accepted")
	}
	if stored := app.currentSettings().Network.PreviewPorts; len(stored) != 0 {
		t.Fatalf("a refused write still persisted %v", stored)
	}
}

// The whole reason the loop has a gate: on an install nobody is watching
// this machine from, discovery must not run at all.
func TestDevServerScansAreGatedOnAnOffMachineViewer(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})

	if app.devServerScanWanted() {
		t.Fatal("a scan was wanted with no event bus at all")
	}

	bus := transport.NewEventBus(8)
	defer bus.Close()
	app.SetEventBus(bus)
	if app.devServerScanWanted() {
		t.Fatal("a scan was wanted with nobody subscribed")
	}

	// The webview in front of the machine is not a reason to scan.
	local := bus.Subscribe()
	defer local.Close()
	local.SetOriginLoopback(true)
	if app.devServerScanWanted() {
		t.Fatal("the local webview alone made the backend scan")
	}
}

// A backend with no sessions and no terminals owns nothing, so nothing
// can be attributed to a thread. The nil managers are the shape every
// fixture that never called Start has, and the owner walk must read them
// as "no owners" rather than dereference them.
func TestDevServerOwnersAreEmptyWithNothingRunning(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})
	if owners := app.devServerOwners(); len(owners) != 0 {
		t.Fatalf("owners = %+v, want none", owners)
	}
}

// Nothing below binds a preview listener. A gateway on a loopback-only
// backend with no tailnet has no address to serve on, so every port in
// the set lands as a note, which is exactly the state these assert.

// A backend nobody wired a transport server into serves no previews, and
// says so rather than handing back a link to nowhere.
func TestMintPreviewURLRefusesWithNoTransportServer(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})

	if app.previewGateway() != nil {
		t.Fatal("a gateway was built with no transport server behind it")
	}
	if _, err := app.MintPreviewURL(context.Background(), "thread-a", 5173, "/"); err == nil {
		t.Fatal("a preview URL was minted on a backend serving no previews")
	}
	// The thread is what routes the call, so a call without one is a bug
	// in the caller and is named as such.
	if _, err := app.MintPreviewURL(context.Background(), "", 5173, "/"); err == nil {
		t.Fatal("a preview URL was minted for no thread")
	}
}

// One gateway per App: the listeners and the grants are its state, so a
// second one would serve a set nobody reconciles and hand out cookies
// nobody honours.
func TestThePreviewGatewayIsBuiltOnceAndClosedOnce(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})
	app.SetTransportServer(startTestTransportServer(t))

	first := app.previewGateway()
	if first == nil {
		t.Fatal("no gateway was built with a transport server wired")
	}
	if second := app.previewGateway(); second != first {
		t.Fatal("a second gateway was built")
	}
	if !app.previewGatewayBuilt() {
		t.Fatal("previewGatewayBuilt said no after one was built")
	}

	if err := app.closePreviewGateway(); err != nil {
		t.Fatalf("closePreviewGateway: %v", err)
	}
	if app.previewGatewayBuilt() {
		t.Fatal("the gateway survived its close")
	}
}

// The list may only call a port shareable when a listener is actually
// serving it. Here nothing can be: no tailnet, loopback bind, so the row
// comes back refused with the gateway's own sentence on it.
func TestAllowedPortsWithNowhereToServeThemComeBackRefused(t *testing.T) {
	scanner := &fakeScanner{servers: []devscan.DevServer{
		{Port: 5173, ThreadID: "thread-a", Allowed: true, Source: devscan.SourceAttributed, Listening: true},
	}}
	app := newPreviewTestApp(t, scanner)
	app.SetTransportServer(startTestTransportServer(t))
	t.Cleanup(func() { _ = app.closePreviewGateway() })

	list, err := app.GetDevServers(context.Background())
	if err != nil {
		t.Fatalf("GetDevServers: %v", err)
	}
	if len(list.Servers) != 1 {
		t.Fatalf("servers = %+v", list.Servers)
	}
	row := list.Servers[0]
	if row.Allowed {
		t.Fatal("a port with no listener behind it was published as shareable")
	}
	if row.Note == "" {
		t.Fatal("a refused row carries no sentence saying why")
	}
	// And the mint agrees with the list: one refusal, not two answers.
	if _, err := app.MintPreviewURL(context.Background(), "thread-a", 5173, "/"); err == nil {
		t.Fatal("a URL was minted for a port the list refused")
	}
}

// previewHost is the sources' answer, not a second derivation of it. On
// a loopback-only backend with no tailnet neither source can serve, so
// the host is empty and the client renders its own sentence.
func TestPreviewHostIsEmptyWhenNoSourceCanServe(t *testing.T) {
	app := newPreviewTestApp(t, &fakeScanner{})
	if host := app.previewHost(); host != "" {
		t.Fatalf("previewHost = %q with no transport server", host)
	}

	app.SetTransportServer(startTestTransportServer(t))
	if host := app.previewHost(); host != "" {
		t.Fatalf("previewHost = %q on a loopback-only backend with no tailnet", host)
	}
	if len(app.previewSources(app.transportServer.Load())) != 2 {
		t.Fatal("the source order is the whole address policy; both must be present")
	}
}
