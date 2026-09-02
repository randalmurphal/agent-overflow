package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/transport"
)

// The remote update flow, end to end, with nothing real behind it.
//
// The release feed is an httptest server on loopback: the flow's whole job is
// to fetch bytes and verify them, and a fake source that returned bytes would
// prove the verification only against itself. The BINARY is a description, not
// an executable — the preflight is injected — so no test here spawns anything,
// and the layout is a t.TempDir the supervisor half never sees.

const (
	serviceUpdateTestRepo   = "owner/repo"
	serviceUpdateTestAsset  = "agent-overflow-headless-linux-amd64"
	serviceUpdateTestSums   = "SHASUMS256"
	serviceUpdateRunningVer = "1.4.0"
)

// serviceUpdateArtifact is what the mock release ships. Not an ELF and not
// meant to be: nothing in this file executes it, because the preflight — the
// only step that would — is the injected seam.
var serviceUpdateArtifact = []byte("agent-overflow release artifact, deterministic test bytes\n")

// serviceUpdateRig is one supervised host under test.
type serviceUpdateRig struct {
	t      *testing.T
	app    *App
	layout supervise.Layout
	source *appupdate.ReleaseSource

	mu sync.Mutex
	// frames is every service:update-status payload, in order.
	frames []ServiceUpdateStatus
	// requested is every version handed to the supervisor.
	requested []string
	// requestErr, when set, is what the supervisor answers with.
	requestErr error
	// preflight answers what the downloaded file claims to be.
	preflightVersion  string
	preflightProtocol int
	preflightErr      error
}

type serviceUpdateOptions struct {
	// tags are the releases the feed publishes, newest first.
	tags []string
	// checksum, when non-empty, replaces the artifact's published digest, so
	// a test can describe bytes the sidecar does not cover.
	checksum string
	// noSidecar publishes releases with no SHASUMS256 at all.
	noSidecar bool
	// configure is false for the "this host cannot fetch releases" cases.
	configure bool
	// supervised is false for a `serve` nobody supervises.
	supervised bool
}

func newServiceUpdateRig(t *testing.T, opts serviceUpdateOptions) *serviceUpdateRig {
	t.Helper()
	if len(opts.tags) == 0 {
		opts.tags = []string{"v1.5.0"}
	}
	rig := &serviceUpdateRig{
		t:                 t,
		preflightVersion:  "1.5.0",
		preflightProtocol: supervise.ProtocolVersion,
	}

	layout, err := supervise.NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	rig.layout = layout

	srv := newServiceUpdateFeed(t, opts)
	rig.source, err = appupdate.NewReleaseSource(appupdate.Config{
		CurrentVersion: serviceUpdateRunningVer,
		Platform:       "headless-linux",
		Arch:           "amd64",
		Repository:     serviceUpdateTestRepo,
		ChecksumAsset:  serviceUpdateTestSums,
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewReleaseSource: %v", err)
	}

	app := &App{version: serviceUpdateRunningVer}
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(app.appCancel)
	app.testEmitHook = func(name string, data any) {
		if eventchan.Channel(name) != eventchan.ServiceUpdateStatus {
			return
		}
		status, ok := data.(ServiceUpdateStatus)
		if !ok {
			t.Errorf("service:update-status carried %T, not ServiceUpdateStatus", data)
			return
		}
		rig.mu.Lock()
		rig.frames = append(rig.frames, status)
		rig.mu.Unlock()
	}
	rig.app = app

	if opts.supervised {
		SetServiceUpdateRequester(app, func(target string) (string, error) {
			rig.mu.Lock()
			rig.requested = append(rig.requested, target)
			err := rig.requestErr
			rig.mu.Unlock()
			if err != nil {
				return "", err
			}
			return "upd-test-" + target, nil
		})
	}
	if opts.configure {
		deps := ServiceUpdateDeps{
			Layout:    layout,
			Preflight: rig.fakePreflight,
		}
		if opts.supervised || opts.configure {
			deps.Source = rig.source
		}
		ConfigureServiceUpdates(app, deps)
	}
	return rig
}

// configureWithoutSource is the supervised host that has no release artifact
// it could install: darwin's app bundle, or a build the feed does not publish.
func (r *serviceUpdateRig) configureWithoutSource() {
	ConfigureServiceUpdates(r.app, ServiceUpdateDeps{
		Layout:    r.layout,
		Preflight: r.fakePreflight,
	})
}

func (r *serviceUpdateRig) fakePreflight(context.Context, string) (supervise.Preflight, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.preflightErr != nil {
		return supervise.Preflight{}, r.preflightErr
	}
	return supervise.Preflight{ProtocolVersion: r.preflightProtocol, Version: r.preflightVersion}, nil
}

// settled waits for the flow to reach a terminal phase and returns the last
// status frame. The flow runs on its own goroutine by design — the RPC returns
// as soon as it is claimed — so every assertion below has to wait for it.
func (r *serviceUpdateRig) settled() ServiceUpdateStatus {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.app.GetServiceUpdateStatus()
		if err != nil {
			r.t.Fatalf("GetServiceUpdateStatus: %v", err)
		}
		switch status.Phase {
		case serviceUpdatePhaseRequested, serviceUpdatePhaseError:
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("the update flow never reached a terminal phase")
	return ServiceUpdateStatus{}
}

func (r *serviceUpdateRig) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.frames))
	for _, frame := range r.frames {
		if len(out) > 0 && out[len(out)-1] == frame.Phase {
			continue
		}
		out = append(out, frame.Phase)
	}
	return out
}

func (r *serviceUpdateRig) supervisorCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requested...)
}

// stagedVersions lists the version directories the layout holds, which is the
// assertion "nothing was staged" is made against.
func (r *serviceUpdateRig) stagedVersions() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.layout.VersionsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		r.t.Fatalf("read versions dir: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

// leftovers lists what a flow left in the layout root besides its own
// subdirectories: the temp download must never be one of them.
func (r *serviceUpdateRig) leftovers() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.layout.Root())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		r.t.Fatalf("read layout root: %v", err)
	}
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}

// newServiceUpdateFeed serves the subset of the GitHub releases API the source
// uses. Loopback, and it publishes exactly one artifact plus its sidecar.
func newServiceUpdateFeed(t *testing.T, opts serviceUpdateOptions) *httptest.Server {
	t.Helper()
	digest := sha256.Sum256(serviceUpdateArtifact)
	published := hex.EncodeToString(digest[:])
	if opts.checksum != "" {
		published = opts.checksum
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	base := "/repos/" + serviceUpdateTestRepo + "/releases"

	release := func(tag string) map[string]any {
		assets := []map[string]any{{
			"name":                 serviceUpdateTestAsset,
			"content_type":         "application/octet-stream",
			"size":                 len(serviceUpdateArtifact),
			"browser_download_url": srv.URL + "/dl/bin/" + tag,
		}}
		if !opts.noSidecar {
			assets = append(assets, map[string]any{
				"name":                 serviceUpdateTestSums,
				"content_type":         "text/plain",
				"size":                 128,
				"browser_download_url": srv.URL + "/dl/sums/" + tag,
			})
		}
		return map[string]any{"tag_name": tag, "name": "Release " + tag, "assets": assets}
	}

	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode release feed response: %v", err)
		}
	}

	mux.HandleFunc(base+"/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, base+"/tags/")
		for _, published := range opts.tags {
			if published == tag {
				write(w, release(tag))
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc(base, func(w http.ResponseWriter, _ *http.Request) {
		out := make([]map[string]any, 0, len(opts.tags))
		for _, tag := range opts.tags {
			out = append(out, release(tag))
		}
		write(w, out)
	})
	mux.HandleFunc("/dl/sums/", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, published+"  "+serviceUpdateTestAsset+"\n"); err != nil {
			t.Errorf("write sidecar: %v", err)
		}
	})
	mux.HandleFunc("/dl/bin/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if _, err := w.Write(serviceUpdateArtifact); err != nil {
			t.Errorf("write artifact: %v", err)
		}
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The happy path: the artifact lands under the PREFLIGHT's version, its bytes
// are the verified ones, the supervisor is asked for that same version, and
// the temp download is gone.
func TestRequestServiceUpdateStagesTheVerifiedBinaryAndAsksTheSupervisor(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	// Deliberately NOT the tag's version: the directory is named for what the
	// binary says it is, and a rig that made them equal would not prove which
	// one the flow used.
	rig.preflightVersion = "1.5.0+build7"

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	status := rig.settled()
	if status.Phase != serviceUpdatePhaseRequested {
		t.Fatalf("phase = %s (%s), want requested", status.Phase, status.Error)
	}
	if status.TargetTag != "v1.5.0" || status.TargetVersion != "1.5.0+build7" {
		t.Errorf("status = %+v, want tag v1.5.0 resolving to the preflight's version", status)
	}
	if status.UpdateID != "upd-test-1.5.0+build7" {
		t.Errorf("UpdateID = %q, want the supervisor's id", status.UpdateID)
	}

	if got := rig.supervisorCalls(); len(got) != 1 || got[0] != "1.5.0+build7" {
		t.Fatalf("supervisor asked for %v, want [1.5.0+build7]", got)
	}
	binary, err := rig.layout.VersionBinary("1.5.0+build7")
	if err != nil {
		t.Fatalf("VersionBinary: %v", err)
	}
	staged, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read the staged binary: %v", err)
	}
	if string(staged) != string(serviceUpdateArtifact) {
		t.Errorf("the staged bytes are not the downloaded artifact")
	}
	if got := rig.leftovers(); len(got) != 0 {
		t.Errorf("the flow left %v in the layout root, want the temp download removed", got)
	}
}

// The phases the client renders, in order, on the path that succeeds.
func TestRequestServiceUpdatePublishesItsPhasesInOrder(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	rig.settled()

	want := []string{
		serviceUpdatePhaseResolving,
		serviceUpdatePhaseDownloading,
		serviceUpdatePhaseVerifying,
		serviceUpdatePhaseStaging,
		serviceUpdatePhaseRequested,
	}
	got := rig.phases()
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}
}

// Every synchronous refusal, and the two things they all have in common: no
// flow starts, and the supervisor is never asked.
func TestRequestServiceUpdateRefusalsTouchNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts serviceUpdateOptions
		// trial parks the activation gate, which is what a supervisor trial
		// boot looks like from inside this process.
		trial bool
		tag   string
		want  error
	}{
		{
			name:  "a trial boot",
			opts:  serviceUpdateOptions{configure: true, supervised: true},
			trial: true,
			tag:   "v1.5.0",
			want:  ErrServiceUpdateTrial,
		},
		{
			name: "an unsafe tag",
			opts: serviceUpdateOptions{configure: true, supervised: true},
			tag:  "../../etc/passwd",
			want: ErrInvalidReleaseTag,
		},
		{
			name: "an empty tag",
			opts: serviceUpdateOptions{configure: true, supervised: true},
			tag:  "   ",
			want: ErrInvalidReleaseTag,
		},
		{
			name: "the version already running",
			opts: serviceUpdateOptions{configure: true, supervised: true},
			tag:  "v" + serviceUpdateRunningVer,
			want: ErrServiceUpdateAlreadyRunning,
		},
		{
			name: "no supervisor to apply it",
			opts: serviceUpdateOptions{configure: true},
			tag:  "v1.5.0",
			want: errNoSupervisor,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newServiceUpdateRig(t, tc.opts)
			if tc.trial {
				ParkUnattendedWork(rig.app)
			}
			if err := rig.app.RequestServiceUpdate(context.Background(), tc.tag); !errors.Is(err, tc.want) {
				t.Fatalf("RequestServiceUpdate = %v, want %v", err, tc.want)
			}
			if got := rig.supervisorCalls(); len(got) != 0 {
				t.Errorf("the supervisor was asked %v for a refused request", got)
			}
			if got := rig.stagedVersions(); len(got) != 0 {
				t.Errorf("a refused request staged %v", got)
			}
			if got := rig.leftovers(); len(got) != 0 {
				t.Errorf("a refused request downloaded %v", got)
			}
			status, err := rig.app.GetServiceUpdateStatus()
			if err != nil {
				t.Fatalf("GetServiceUpdateStatus: %v", err)
			}
			if status.Phase != serviceUpdatePhaseIdle {
				t.Errorf("phase = %s after a refused request, want idle", status.Phase)
			}
		})
	}
}

// A supervised host with no release source refuses the flow AND says why in
// the status, so the client shows a sentence rather than a dead button.
func TestASupervisedHostWithNoReleaseSourceSaysSo(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{supervised: true})
	rig.configureWithoutSource()

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); !errors.Is(err, ErrServiceUpdateUnavailable) {
		t.Fatalf("RequestServiceUpdate = %v, want ErrServiceUpdateUnavailable", err)
	}
	if _, err := rig.app.ListServiceReleases(); !errors.Is(err, ErrServiceUpdateUnavailable) {
		t.Fatalf("ListServiceReleases = %v, want ErrServiceUpdateUnavailable", err)
	}
	status, err := rig.app.GetServiceUpdateStatus()
	if err != nil {
		t.Fatalf("GetServiceUpdateStatus: %v", err)
	}
	if !status.Supervised || status.Available {
		t.Fatalf("status = %+v, want supervised and unavailable", status)
	}
	if status.Unavailable == "" {
		t.Fatal("Available is false and Unavailable says nothing")
	}
	if strings.Contains(status.Unavailable, "—") {
		t.Errorf("the sentence a person reads carries an em dash: %q", status.Unavailable)
	}
}

// The desktop and unsupervised-serve answer: no error, no surface.
func TestGetServiceUpdateStatusOffASupervisedHostIsNotAnError(t *testing.T) {
	app := &App{version: serviceUpdateRunningVer}
	status, err := app.GetServiceUpdateStatus()
	if err != nil {
		t.Fatalf("GetServiceUpdateStatus: %v", err)
	}
	if status.Supervised || status.Available {
		t.Errorf("status = %+v, want neither supervised nor available", status)
	}
	if status.CurrentVersion != serviceUpdateRunningVer {
		t.Errorf("CurrentVersion = %q, want %q", status.CurrentVersion, serviceUpdateRunningVer)
	}
	if status.Phase != serviceUpdatePhaseIdle {
		t.Errorf("phase = %q, want idle", status.Phase)
	}
	if status.Unavailable != "" {
		t.Errorf("an unsupervised host explained why it is unavailable (%q); it is not supervised, which the client already knows", status.Unavailable)
	}
}

// One flow at a time. The second caller is refused rather than queued: two
// downloads racing for one staging layout is a corrupted version directory.
func TestRequestServiceUpdateRefusesASecondFlow(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	// Hold the flow inside the preflight so the fence is provably still held
	// when the second request lands.
	release := make(chan struct{})
	rig.app.serviceUpdate.mu.Lock()
	rig.app.serviceUpdate.deps.Preflight = func(context.Context, string) (supervise.Preflight, error) {
		<-release
		return supervise.Preflight{ProtocolVersion: supervise.ProtocolVersion, Version: "1.5.0"}, nil
	}
	rig.app.serviceUpdate.mu.Unlock()

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := rig.app.GetServiceUpdateStatus()
		if err != nil {
			t.Fatalf("GetServiceUpdateStatus: %v", err)
		}
		if status.Phase == serviceUpdatePhaseVerifying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the flow never reached verifying (phase %s, %s)", status.Phase, status.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); !errors.Is(err, ErrServiceUpdateBusy) {
		t.Fatalf("the second RequestServiceUpdate = %v, want ErrServiceUpdateBusy", err)
	}
	close(release)
	if status := rig.settled(); status.Phase != serviceUpdatePhaseRequested {
		t.Fatalf("phase = %s (%s), want the first flow to finish", status.Phase, status.Error)
	}
	if got := rig.supervisorCalls(); len(got) != 1 {
		t.Fatalf("the supervisor was asked %v, want one update", got)
	}
}

// Every asynchronous failure, and what they all have to leave behind: an error
// phase naming the step, nothing staged, no temp file, and a supervisor that
// was never asked.
func TestAFailedFlowLeavesTheSupervisorAndTheLayoutUntouched(t *testing.T) {
	wrongDigest := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name    string
		opts    serviceUpdateOptions
		arrange func(*serviceUpdateRig)
		names   string
	}{
		{
			name:  "the bytes are not what the checksum covers",
			opts:  serviceUpdateOptions{tags: []string{"v1.5.0"}, checksum: wrongDigest, configure: true, supervised: true},
			names: "downloading",
		},
		{
			name:  "the release ships no checksum",
			opts:  serviceUpdateOptions{tags: []string{"v1.5.0"}, noSidecar: true, configure: true, supervised: true},
			names: "downloading",
		},
		{
			name:  "the tag is not published",
			opts:  serviceUpdateOptions{tags: []string{"v1.5.0"}, configure: true, supervised: true},
			names: "downloading",
		},
		{
			name:    "the download is not a binary this host can run",
			opts:    serviceUpdateOptions{tags: []string{"v1.5.0"}, configure: true, supervised: true},
			arrange: func(r *serviceUpdateRig) { r.preflightErr = errors.New("it did not answer") },
			names:   "checking",
		},
		{
			name:    "the download speaks a newer update protocol",
			opts:    serviceUpdateOptions{tags: []string{"v1.5.0"}, configure: true, supervised: true},
			arrange: func(r *serviceUpdateRig) { r.preflightProtocol = supervise.ProtocolVersion + 1 },
			names:   "service update",
		},
		{
			name:    "the download reports a version that cannot name a directory",
			opts:    serviceUpdateOptions{tags: []string{"v1.5.0"}, configure: true, supervised: true},
			arrange: func(r *serviceUpdateRig) { r.preflightVersion = "../escape" },
			names:   "cannot name a directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newServiceUpdateRig(t, tc.opts)
			if tc.arrange != nil {
				tc.arrange(rig)
			}
			tag := "v1.5.0"
			if tc.name == "the tag is not published" {
				tag = "v9.9.9"
			}
			if err := rig.app.RequestServiceUpdate(context.Background(), tag); err != nil {
				t.Fatalf("RequestServiceUpdate: %v", err)
			}
			status := rig.settled()
			if status.Phase != serviceUpdatePhaseError {
				t.Fatalf("phase = %s, want error", status.Phase)
			}
			if !strings.Contains(status.Error, tc.names) {
				t.Errorf("Error = %q, want it to name %q", status.Error, tc.names)
			}
			if got := rig.supervisorCalls(); len(got) != 0 {
				t.Errorf("the supervisor was asked %v for a flow that failed", got)
			}
			if got := rig.stagedVersions(); len(got) != 0 {
				t.Errorf("a failed flow staged %v", got)
			}
			if got := rig.leftovers(); len(got) != 0 {
				t.Errorf("a failed flow left %v behind", got)
			}
		})
	}
}

// The supervisor's own refusal is the last thing that can go wrong, and its
// reason is what the client shows. The staged version stays: the supervisor
// refused to SELECT it, not to have it, and the next attempt restages the same
// bytes under the same name.
func TestASupervisorRefusalBecomesTheError(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	rig.requestErr = errors.New("an update is already in flight")

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	status := rig.settled()
	if status.Phase != serviceUpdatePhaseError {
		t.Fatalf("phase = %s, want error", status.Phase)
	}
	if !strings.Contains(status.Error, "an update is already in flight") {
		t.Errorf("Error = %q, want the supervisor's own reason", status.Error)
	}
	if got := rig.leftovers(); len(got) != 0 {
		t.Errorf("the temp download survived a supervisor refusal: %v", got)
	}
}

// A new flow clears the previous one's failure. A status carrying a fresh
// phase beside a stale error is a client rendering two updates at once.
func TestANewFlowClearsTheLastOnesFailure(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	rig.preflightErr = errors.New("it did not answer")
	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	if status := rig.settled(); status.Error == "" {
		t.Fatal("the first flow recorded no error")
	}

	rig.mu.Lock()
	rig.preflightErr = nil
	rig.mu.Unlock()
	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("the second RequestServiceUpdate: %v", err)
	}
	status := rig.settled()
	if status.Phase != serviceUpdatePhaseRequested {
		t.Fatalf("phase = %s (%s), want requested", status.Phase, status.Error)
	}
	if status.Error != "" {
		t.Errorf("Error = %q, want the previous flow's failure cleared", status.Error)
	}
}

// The picker's read, and the boot check that fills LatestVersion. Both are the
// only network reads on this surface and neither installs anything.
func TestTheReleaseListAndThePassiveCheckReportWhatIsPublished(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.6.0", "v1.5.0", "v1.4.0"}, configure: true, supervised: true,
	})

	releases, err := rig.app.ListServiceReleases()
	if err != nil {
		t.Fatalf("ListServiceReleases: %v", err)
	}
	if len(releases) != 3 || releases[0].Tag != "v1.6.0" || !releases[0].IsLatest {
		t.Fatalf("ListServiceReleases = %+v, want three rows newest first", releases)
	}
	if !releases[2].IsCurrent {
		t.Errorf("v1.4.0 is the running version and is not marked current: %+v", releases[2])
	}

	// ConfigureServiceUpdates started the passive check; it is one call with
	// no retry, so wait for its answer rather than assuming it landed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := rig.app.GetServiceUpdateStatus()
		if err != nil {
			t.Fatalf("GetServiceUpdateStatus: %v", err)
		}
		if status.LatestVersion != "" {
			if status.LatestVersion != "1.6.0" || status.LatestTag != "v1.6.0" {
				t.Fatalf("the passive check found %+v, want 1.6.0", status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the passive check never recorded a newer release")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The passive check is a NETWORK READ, and the rule for the activation gate's
// parked set is "would restoring the database undo it?". This one undoes
// itself by ending, so it deliberately runs during a trial — and a trial that
// reported no known release for its whole life would be a worse answer for no
// property gained.
func TestThePassiveCheckRunsDuringATrial(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{supervised: true})
	ParkUnattendedWork(rig.app)
	ConfigureServiceUpdates(rig.app, ServiceUpdateDeps{
		Source: rig.source, Layout: rig.layout, Preflight: rig.fakePreflight,
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := rig.app.GetServiceUpdateStatus()
		if err != nil {
			t.Fatalf("GetServiceUpdateStatus: %v", err)
		}
		if status.LatestVersion == "1.5.0" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a parked backend never ran its release check (%+v)", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A shutting-down backend refuses rather than starting work its own teardown
// is about to cancel halfway through a staging copy.
func TestRequestServiceUpdateRefusesWhileShuttingDown(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	rig.app.shuttingDown.Store(true)
	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("RequestServiceUpdate = %v, want ErrShuttingDown", err)
	}
	if got := rig.leftovers(); len(got) != 0 {
		t.Errorf("a refused request downloaded %v", got)
	}
}

// The temp download lands under the layout ROOT, on the same filesystem as the
// version directory it becomes, so StageBinary is a local copy rather than a
// cross-device move that could tear. The assertion is on the path, because the
// property is where the file is and not what it is called.
func TestTheDownloadLandsBesideTheVersionsItBecomes(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	seen := make(chan string, 1)
	rig.app.serviceUpdate.mu.Lock()
	rig.app.serviceUpdate.deps.Preflight = func(_ context.Context, binary string) (supervise.Preflight, error) {
		select {
		case seen <- binary:
		default:
		}
		return supervise.Preflight{ProtocolVersion: supervise.ProtocolVersion, Version: "1.5.0"}, nil
	}
	rig.app.serviceUpdate.mu.Unlock()

	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate: %v", err)
	}
	rig.settled()

	var downloaded string
	select {
	case downloaded = <-seen:
	default:
		t.Fatal("the preflight never saw a downloaded file")
	}
	if dir := filepath.Dir(downloaded); dir != rig.layout.Root() {
		t.Fatalf("the download landed in %s, want the layout root %s", dir, rig.layout.Root())
	}
	if _, err := os.Stat(downloaded); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temp download at %s survived the flow (%v)", downloaded, err)
	}
}

// The struct that crosses the wire is JSON, and the fields a client reads by
// name have to be the names it reads. Pinned because renaming a Go field is a
// silent wire break for every client that already shipped.
func TestServiceUpdateStatusWireNames(t *testing.T) {
	encoded, err := json.Marshal(ServiceUpdateStatus{
		Supervised: true, Available: true, CurrentVersion: "1.4.0",
		LatestVersion: "1.5.0", LatestTag: "v1.5.0", Phase: serviceUpdatePhaseDownloading,
		TargetTag: "v1.5.0", TargetVersion: "1.5.0", UpdateID: "upd-1",
		Written: 10, Total: 20, Error: "", Unavailable: "",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, name := range []string{
		"supervised", "available", "currentVersion", "latestVersion", "latestTag",
		"phase", "targetTag", "targetVersion", "updateId", "written", "total",
	} {
		if _, ok := decoded[name]; !ok {
			t.Errorf("the wire shape has no %q field: %s", name, encoded)
		}
	}
	if _, ok := decoded["error"]; ok {
		t.Errorf("an empty Error rode the wire: %s", encoded)
	}
}

// The gate's answer for this surface, stated where the surface is. It is the
// precondition every test above rests on: a wave that put these back on `host`
// would leave the whole feature reachable only from the machine it exists to
// save a trip to, and nothing else in this file would notice.
func TestTheUpdateSurfaceIsReachableByAPairedAdmin(t *testing.T) {
	admin := []string{string(transport.ScopeAccessAdmin)}
	for _, method := range []string{"GetServiceUpdateStatus", "ListServiceReleases"} {
		if refusal := transport.AuthorizeSessionMethod(admin, method, transport.CallerProof{}); refusal != nil {
			t.Errorf("an access:admin session is refused %s: %+v", method, refusal)
		}
	}

	// The trigger is admitted by the same grant and then STOPPED for want of
	// a fresh proof, which is the whole shape of //ao:stepup: no standing
	// grant opens it, and the client's next move is to produce a proof.
	refusal := transport.AuthorizeSessionMethod(admin, "RequestServiceUpdate", transport.CallerProof{})
	if refusal == nil {
		t.Fatal("RequestServiceUpdate was admitted with no step-up proof")
	}
	if refusal.Code != transport.ErrCodeStepUpRequired {
		t.Fatalf("refusal code = %q, want %q", refusal.Code, transport.ErrCodeStepUpRequired)
	}
	if refusal := transport.AuthorizeSessionMethod(
		admin, "RequestServiceUpdate", transport.CallerProof{StepUp: true},
	); refusal != nil {
		t.Errorf("a proved step-up is still refused: %+v", refusal)
	}
	// A session with no grants gets neither.
	for _, method := range []string{"GetServiceUpdateStatus", "ListServiceReleases", "RequestServiceUpdate"} {
		if transport.AuthorizeSessionMethod(nil, method, transport.CallerProof{StepUp: true}) == nil {
			t.Errorf("%s answered a session granted nothing", method)
		}
	}
}

// The in-method recheck, which reads the proof the gate already resolved
// rather than asking again. It matters because the flow is claimed and handed
// to a goroutine: a call that got past the gate by some other route must still
// not start one.
func TestRequestServiceUpdateRefusesASessionWithNoProvenStepUp(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{
		tags: []string{"v1.5.0"}, configure: true, supervised: true,
	})
	err := rig.app.RequestServiceUpdate(callFrom("session-under-test", false), "v1.5.0")
	if err == nil {
		t.Fatal("RequestServiceUpdate ran for a session that proved nothing")
	}
	if !strings.Contains(err.Error(), "fresh proof") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
	if got := rig.leftovers(); len(got) != 0 {
		t.Errorf("a refused request downloaded %v", got)
	}
	if got := rig.supervisorCalls(); len(got) != 0 {
		t.Errorf("the supervisor was asked %v", got)
	}

	// The same call with the proof the transport resolved for it runs.
	if err := rig.app.RequestServiceUpdate(callSteppedUp("session-under-test"), "v1.5.0"); err != nil {
		t.Fatalf("RequestServiceUpdate with a proved step-up: %v", err)
	}
	if status := rig.settled(); status.Phase != serviceUpdatePhaseRequested {
		t.Fatalf("phase = %s (%s), want requested", status.Phase, status.Error)
	}
}

// A sanity check on the fixture itself: the artifact the feed serves is the
// one the sidecar covers, so a checksum failure in any test above is the
// flow's answer rather than the rig's arrangement.
func TestTheReleaseFeedFixtureIsInternallyConsistent(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{tags: []string{"v1.5.0"}})
	var got strings.Builder
	resolved, err := rig.source.Fetch(context.Background(), "v1.5.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	digest := sha256.Sum256(serviceUpdateArtifact)
	if resolved.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("resolved digest %s, want %s", resolved.Digest, hex.EncodeToString(digest[:]))
	}
	if got.String() != string(serviceUpdateArtifact) {
		t.Fatalf("the feed served %q", got.String())
	}
	if fmt.Sprint(resolved.AssetName) != serviceUpdateTestAsset {
		t.Fatalf("resolved asset %q", resolved.AssetName)
	}
}
