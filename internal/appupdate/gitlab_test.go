package appupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// The GitLab feed is tested through the production runner: a git.Core
// whose isolated forge CLI policy runs this test binary as glab. That is
// the same path an isolated boot takes, and it can never reach a glab on
// PATH or the developer's glab login.

const (
	fakeGlabEnv       = "AO_APPUPDATE_FAKE_GLAB"
	fakeGlabRoutesEnv = "AO_APPUPDATE_FAKE_GLAB_ROUTES"
	fakeGlabCallsEnv  = "AO_APPUPDATE_FAKE_GLAB_CALLS"

	testGitLabHost     = "gitlab.example.com"
	testGitLabProject  = testGitLabHost + "/grp/app"
	testGitLabListPath = "projects/grp%2Fapp/releases?order_by=released_at&sort=desc&per_page=30"

	// The update notice glab prints after a command that succeeded; the
	// fake prints it on every success so every test proves stderr never
	// reaches the data.
	fakeGlabUpdateNotice = "A new version of glab has been released: v9.9.9"

	noremoteAssetName = "agent-overflow-wsl-noremote-amd64.exe"
	standardAssetName = "agent-overflow-wsl-amd64.exe"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeGlabEnv) == "1" {
		os.Exit(runFakeGlab())
	}
	os.Exit(m.Run())
}

// fakeGlabRoute is the fake's answer for one API path.
type fakeGlabRoute struct {
	Stdout []byte `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// fakeGlabCall is what one fake invocation saw.
type fakeGlabCall struct {
	Argv0       string   `json:"argv0"`
	Args        []string `json:"args"`
	CLI         string   `json:"cli"`
	CheckUpdate string   `json:"checkUpdate"`
	DebugHTTP   string   `json:"debugHTTP"`
}

func (c fakeGlabCall) path() string {
	if len(c.Args) == 0 {
		return ""
	}
	return c.Args[len(c.Args)-1]
}

// runFakeGlab answers `glab api --hostname HOST -- PATH` from the routes
// file, like glab does: the body on stdout, glab's message on stderr.
func runFakeGlab() int {
	call := fakeGlabCall{
		Argv0:       os.Args[0],
		Args:        os.Args[1:],
		CLI:         os.Getenv(gitops.ForgeCLINameEnv),
		CheckUpdate: os.Getenv("GLAB_CHECK_UPDATE"),
		DebugHTTP:   os.Getenv("GLAB_DEBUG_HTTP"),
	}
	line, _ := json.Marshal(call)
	if f, err := os.OpenFile(os.Getenv(fakeGlabCallsEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	args := os.Args[1:]
	if len(args) != 5 || args[0] != "api" || args[1] != "--hostname" || args[3] != "--" {
		fmt.Fprintf(os.Stderr, "fake glab: unexpected argv %q\n", args)
		return 2
	}
	raw, err := os.ReadFile(os.Getenv(fakeGlabRoutesEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake glab:", err)
		return 2
	}
	var routes map[string]fakeGlabRoute
	if err := json.Unmarshal(raw, &routes); err != nil {
		fmt.Fprintln(os.Stderr, "fake glab:", err)
		return 2
	}
	route, ok := routes[args[4]]
	if !ok {
		_, _ = os.Stdout.WriteString(`{"message":"404 Not Found"}`)
		fmt.Fprintln(os.Stderr, "glab: 404 Not Found (HTTP 404)")
		return 1
	}
	_, _ = os.Stdout.Write(route.Stdout)
	if route.Stderr != "" {
		fmt.Fprintln(os.Stderr, route.Stderr)
	}
	if route.Exit == 0 {
		// glab prints its notices after a command that succeeded.
		fmt.Fprintln(os.Stderr, fakeGlabUpdateNotice)
	}
	return route.Exit
}

// fakeGitLab is one project's releases as the fake glab serves them.
type fakeGitLab struct {
	t        *testing.T
	dir      string
	routes   map[string]fakeGlabRoute
	releases []map[string]any
}

// fakeAsset is one release link. An empty link is the project's generic
// package registry on the configured host, which is what the release
// script publishes.
type fakeAsset struct {
	name string
	body []byte
	link string
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{t: t, dir: t.TempDir(), routes: map[string]fakeGlabRoute{}}
	f.flush()
	return f
}

func packageLink(tag, name string) string {
	return "https://" + testGitLabHost + "/api/v4/projects/7/packages/generic/agent-overflow/" + trimVPrefix(tag) + "/" + name
}

func packagePath(tag, name string) string {
	return strings.TrimPrefix(packageLink(tag, name), "https://"+testGitLabHost+"/api/v4/")
}

// publish adds a release newer than every release published before it
// returns, so call it newest first, as GitLab lists them.
func (f *fakeGitLab) publish(tag string, assets ...fakeAsset) {
	f.t.Helper()
	links := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		link := asset.link
		if link == "" {
			link = packageLink(tag, asset.name)
			f.routes[packagePath(tag, asset.name)] = fakeGlabRoute{Stdout: asset.body}
		}
		links = append(links, map[string]any{"name": asset.name, "url": link, "link_type": "package"})
	}
	release := map[string]any{
		"tag_name":         tag,
		"name":             "Agent Overflow " + tag,
		"description":      "notes for " + tag,
		"released_at":      time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(len(f.releases)) * time.Hour).Format(time.RFC3339),
		"upcoming_release": false,
		"assets":           map[string]any{"count": len(links), "links": links},
	}
	f.releases = append(f.releases, release)
	byTag, err := json.Marshal(release)
	if err != nil {
		f.t.Fatal(err)
	}
	f.routes["projects/grp%2Fapp/releases/"+tag] = fakeGlabRoute{Stdout: byTag}
	listing, err := json.Marshal(f.releases)
	if err != nil {
		f.t.Fatal(err)
	}
	f.routes[testGitLabListPath] = fakeGlabRoute{Stdout: listing}
	f.flush()
}

func (f *fakeGitLab) set(path string, route fakeGlabRoute) {
	f.routes[path] = route
	f.flush()
}

func (f *fakeGitLab) flush() {
	f.t.Helper()
	raw, err := json.Marshal(f.routes)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "routes.json"), raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// runner is the production runner over an isolated Core whose only glab
// is this test binary.
func (f *fakeGitLab) runner() GlabRunner {
	f.t.Helper()
	self, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	core := gitops.NewCore(gitops.WithIsolatedForgeCLIs(self, []string{
		fakeGlabEnv + "=1",
		fakeGlabRoutesEnv + "=" + filepath.Join(f.dir, "routes.json"),
		fakeGlabCallsEnv + "=" + filepath.Join(f.dir, "calls.jsonl"),
	}))
	return core.StreamGitLabAPI
}

func (f *fakeGitLab) calls() []fakeGlabCall {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []fakeGlabCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var call fakeGlabCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			f.t.Fatalf("decode call %q: %v", line, err)
		}
		out = append(out, call)
	}
	return out
}

func (f *fakeGitLab) source(t *testing.T, platform, current string) *ReleaseSource {
	t.Helper()
	src, err := newReleaseSource(Config{
		CurrentVersion: current,
		Platform:       platform,
		Arch:           "amd64",
		GlabRunner:     f.runner(),
	}, testGitLabProject)
	if err != nil {
		t.Fatalf("newReleaseSource: %v", err)
	}
	return src
}

// setGitLabProject stands in for the link-time stamp.
func setGitLabProject(t *testing.T, project string) {
	t.Helper()
	previous := gitlabProject
	gitlabProject = project
	t.Cleanup(func() { gitlabProject = previous })
}

func payload(name, tag string) []byte {
	return []byte("MZ " + name + " " + tag + " deterministic test bytes\n")
}

// sumsOf is a SHASUMS256 in the release script's `<hex>  ./name` form.
func sumsOf(assets ...fakeAsset) fakeAsset {
	var b strings.Builder
	for _, asset := range assets {
		digest := sha256.Sum256(asset.body)
		b.WriteString(hex.EncodeToString(digest[:]) + "  ./" + asset.name + "\n")
	}
	return fakeAsset{name: "SHASUMS256", body: []byte(b.String())}
}

func noremoteAsset(tag string) fakeAsset {
	return fakeAsset{name: noremoteAssetName, body: payload(noremoteAssetName, tag)}
}

func standardAsset(tag string) fakeAsset {
	return fakeAsset{name: standardAssetName, body: payload(standardAssetName, tag)}
}

// publishTypical publishes a pre-release, the latest stable release with
// both WSL variants, a release with no noremote launcher, and the running
// release.
func publishTypical(f *fakeGitLab) {
	rc := noremoteAsset("v0.3.0-rc1")
	f.publish("v0.3.0-rc1", rc, sumsOf(rc))
	nr, std := noremoteAsset("v0.2.0"), standardAsset("v0.2.0")
	f.publish("v0.2.0", std, nr, sumsOf(std, nr))
	only := standardAsset("v0.1.5")
	f.publish("v0.1.5", only, sumsOf(only))
	cur := noremoteAsset("v0.1.0")
	f.publish("v0.1.0", cur, sumsOf(cur))
}

func TestGitLabFeedListsResolvesAndFetchesVerified(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	src := f.source(t, "wsl-noremote", "0.1.0")
	ctx := context.Background()

	releases, err := src.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tags []string
	for _, r := range releases {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, ",") != "v0.3.0-rc1,v0.2.0,v0.1.0" {
		t.Fatalf("listing = %v, want the noremote releases newest first", tags)
	}
	if !releases[0].Prerelease || releases[0].IsLatest {
		t.Errorf("v0.3.0-rc1 = %+v, want a prerelease that is not latest", releases[0])
	}
	if !releases[1].IsLatest || releases[1].IsCurrent || releases[1].IsOlder {
		t.Errorf("v0.2.0 = %+v, want latest", releases[1])
	}
	if !releases[2].IsCurrent {
		t.Errorf("v0.1.0 = %+v, want current", releases[2])
	}

	latest, err := src.Latest(ctx)
	if err != nil || latest == nil || latest.Tag != "v0.2.0" {
		t.Fatalf("Latest = %+v, %v; want v0.2.0", latest, err)
	}

	var got bytes.Buffer
	resolved, err := src.Fetch(ctx, "v0.2.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := payload(noremoteAssetName, "v0.2.0")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("fetched %q, want %q", got.Bytes(), want)
	}
	digest := sha256.Sum256(want)
	if resolved.AssetName != noremoteAssetName || resolved.Digest != hex.EncodeToString(digest[:]) || resolved.Tag != "v0.2.0" {
		t.Fatalf("resolved = %+v", resolved)
	}

	calls := f.calls()
	if len(calls) == 0 {
		t.Fatal("the fake glab never ran")
	}
	for _, call := range calls {
		if call.Argv0 != "glab" || call.CLI != "glab" {
			t.Errorf("call ran as %q (%s=%q), want glab", call.Argv0, gitops.ForgeCLINameEnv, call.CLI)
		}
		if len(call.Args) != 5 || call.Args[0] != "api" || call.Args[1] != "--hostname" || call.Args[2] != testGitLabHost || call.Args[3] != "--" {
			t.Errorf("argv = %q, want api --hostname %s -- PATH", call.Args, testGitLabHost)
		}
		if strings.Contains(call.path(), "://") {
			t.Errorf("glab was handed an absolute URL: %q", call.path())
		}
		if call.CheckUpdate != "false" || call.DebugHTTP != "false" {
			t.Errorf("GLAB_CHECK_UPDATE=%q GLAB_DEBUG_HTTP=%q, want both false", call.CheckUpdate, call.DebugHTTP)
		}
		if strings.Contains(call.path(), standardAssetName) {
			t.Errorf("the noremote feed requested the standard launcher: %q", call.path())
		}
	}
}

func TestGitLabFeedOffersOnlyItsOwnVariant(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	src := f.source(t, "wsl", "0.1.0")

	releases, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var tags []string
	for _, r := range releases {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, ",") != "v0.2.0,v0.1.5" {
		t.Fatalf("standard listing = %v, want only releases shipping %s", tags, standardAssetName)
	}
	var got bytes.Buffer
	resolved, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resolved.AssetName != standardAssetName {
		t.Fatalf("standard target fetched %s", resolved.AssetName)
	}
}

func newGitLabService(t *testing.T, f *fakeGitLab, current string) *Service {
	t.Helper()
	setGitLabProject(t, testGitLabProject)
	a := New(current, Deps{})
	if err := a.Configure(updater.New(noopUpdaterHost{}), Config{
		CurrentVersion: current,
		Platform:       "wsl-noremote",
		Arch:           "amd64",
		GlabRunner:     f.runner(),
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return a
}

// The passive check and the framework download go through gitlabProvider,
// which reads latest off the listing and streams the link through glab.
func TestGitLabProviderServesTheLatestCheckAndDownload(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.1.0")
	if got := a.providerName(); got != "gitlab" {
		t.Fatalf("provider name = %q, want gitlab", got)
	}

	availability, err := a.CheckForUpdate()
	if err != nil || availability.CheckError != "" {
		t.Fatalf("CheckForUpdate = %+v, %v", availability, err)
	}
	if !availability.Available || availability.LatestVersion != "0.2.0" || availability.ReleaseNotes != "notes for v0.2.0" {
		t.Fatalf("availability = %+v, want 0.2.0 available", availability)
	}

	var progressCalls int
	var lastWritten int64
	rel := a.updater.pending
	var got bytes.Buffer
	err = verifiedProvider{inner: a.updater.provider}.Download(context.Background(), rel, &got, func(written, total int64) {
		progressCalls++
		lastWritten = written
		if total != 0 {
			t.Errorf("total = %d; GitLab links carry no size", total)
		}
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	want := payload(noremoteAssetName, "v0.2.0")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("downloaded %q, want %q", got.Bytes(), want)
	}
	if progressCalls == 0 || lastWritten != int64(len(want)) {
		t.Fatalf("progress calls = %d, last written = %d, want %d", progressCalls, lastWritten, len(want))
	}

	if err := a.updater.handle.DownloadAndInstall(context.Background()); err != nil {
		t.Fatalf("DownloadAndInstall: %v", err)
	}
	staged, err := os.ReadFile(a.updater.handle.DownloadedPath())
	if err != nil {
		t.Fatalf("read the framework's download: %v", err)
	}
	if !bytes.Equal(staged, want) {
		t.Fatalf("framework downloaded %q, want %q", staged, want)
	}
}

func TestGitLabProviderReportsUpToDate(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.2.0")
	availability, err := a.CheckForUpdate()
	if err != nil || availability.CheckError != "" || availability.Available {
		t.Fatalf("CheckForUpdate = %+v, %v; want up to date", availability, err)
	}
}

func TestGitLabDownloadUpdateErrorNamesTheGitLabProvider(t *testing.T) {
	f := newFakeGitLab(t)
	publishTypical(f)
	a := newGitLabService(t, f, "0.1.0")
	got := make(chan updater.ErrorInfo, 1)
	a.deps.Emit = func(name eventchan.Channel, data any) {
		if info, ok := data.(updater.ErrorInfo); ok && name == updaterErrorChannel {
			select {
			case got <- info:
			default:
			}
		}
	}
	if err := a.DownloadUpdate("v9.9.9"); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	select {
	case info := <-got:
		if info.Provider != "gitlab" || info.Stage != updater.StageCheck {
			t.Fatalf("error = %+v, want a gitlab check error", info)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no updater:error for a tag the project does not have")
	}
}

func TestGitLabFetchRejectsChecksumMismatch(t *testing.T) {
	f := newFakeGitLab(t)
	nr := noremoteAsset("v0.2.0")
	wrong := sumsOf(fakeAsset{name: noremoteAssetName, body: []byte("other bytes")})
	f.publish("v0.2.0", nr, wrong)
	src := f.source(t, "wsl-noremote", "0.1.0")

	var got bytes.Buffer
	_, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
	if err == nil || !strings.Contains(err.Error(), "published checksum") {
		t.Fatalf("Fetch error = %v, want a checksum refusal", err)
	}
}

func TestGitLabDownloadIsCapped(t *testing.T) {
	f := newFakeGitLab(t)
	big := fakeAsset{name: noremoteAssetName, body: bytes.Repeat([]byte("x"), 256<<10)}
	f.publish("v0.2.0", big, sumsOf(big))
	src := f.source(t, "wsl-noremote", "0.1.0")
	rel, err := src.resolve(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got bytes.Buffer
	err = downloadBoundedArtifact(context.Background(), src.targetable, rel, &got, nil, 1024)
	if !errors.Is(err, errDownloadTooLarge) {
		t.Fatalf("download error = %v, want errDownloadTooLarge", err)
	}
	if got.Len() > 1024 {
		t.Fatalf("wrote %d bytes past a 1024-byte cap", got.Len())
	}
}

func TestGitLabSidecarIsCapped(t *testing.T) {
	f := newFakeGitLab(t)
	nr := noremoteAsset("v0.2.0")
	sums := sumsOf(nr)
	sums.body = append(sums.body, bytes.Repeat([]byte("#"), maxChecksumBytes)...)
	f.publish("v0.2.0", nr, sums)
	src := f.source(t, "wsl-noremote", "0.1.0")

	_, err := src.resolve(context.Background(), "v0.2.0")
	if !errors.Is(err, errGitLabResponseTooLarge) {
		t.Fatalf("resolve error = %v, want the sidecar refused for size", err)
	}
}

func TestGitLabListingIsCapped(t *testing.T) {
	f := newFakeGitLab(t)
	f.set(testGitLabListPath, fakeGlabRoute{Stdout: bytes.Repeat([]byte(" "), maxReleaseListBytes+1)})
	src := f.source(t, "wsl-noremote", "0.1.0")
	if _, err := src.List(context.Background()); !errors.Is(err, errGitLabResponseTooLarge) {
		t.Fatalf("List error = %v, want the listing refused for size", err)
	}
}

// A release link is data. glab sends the user's token to any absolute URL,
// so a link off the configured host's REST API must be refused before glab
// runs.
func TestGitLabFeedRefusesLinksOffTheAPI(t *testing.T) {
	cases := map[string]string{
		"foreign host":     "https://evil.example.net/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"http":             "http://" + testGitLabHost + "/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"outside /api/v4/": "https://" + testGitLabHost + "/grp/app/-/releases/v0.2.0/downloads/" + noremoteAssetName,
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitLab(t)
			nr := fakeAsset{name: noremoteAssetName, body: payload(noremoteAssetName, "v0.2.0"), link: link}
			f.publish("v0.2.0", nr, sumsOf(nr))
			src := f.source(t, "wsl-noremote", "0.1.0")

			var got bytes.Buffer
			_, err := src.Fetch(context.Background(), "v0.2.0", &got, nil)
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("Fetch error = %v, want the link refused", err)
			}
			for _, call := range f.calls() {
				if strings.Contains(call.path(), noremoteAssetName) {
					t.Fatalf("glab was asked for the refused link: %q", call.Args)
				}
			}

			// The same rule holds for the sidecar.
			sideFeed := newFakeGitLab(t)
			sums := sumsOf(nr)
			sums.link = strings.Replace(link, noremoteAssetName, "SHASUMS256", 1)
			plain := noremoteAsset("v0.2.0")
			sideFeed.publish("v0.2.0", plain, sums)
			if _, err := sideFeed.source(t, "wsl-noremote", "0.1.0").resolve(context.Background(), "v0.2.0"); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("resolve error = %v, want the sidecar link refused", err)
			}
			for _, call := range sideFeed.calls() {
				if strings.Contains(call.path(), "SHASUMS256") {
					t.Fatalf("glab was asked for the refused sidecar: %q", call.Args)
				}
			}
		})
	}
}

func TestGitLabAPIPath(t *testing.T) {
	feed := &gitlabFeed{host: testGitLabHost, project: "grp/app"}
	accepted := map[string]string{
		"https://gitlab.example.com/api/v4/projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName: "projects/7/packages/generic/agent-overflow/0.2.0/" + noremoteAssetName,
		"https://GitLab.Example.com/api/v4/projects/grp%2Fapp/packages/generic/a/1/SHASUMS256":                    "projects/grp%2Fapp/packages/generic/a/1/SHASUMS256",
	}
	for link, want := range accepted {
		got, err := feed.apiPath(link)
		if err != nil || got != want {
			t.Errorf("apiPath(%q) = %q, %v; want %q", link, got, err, want)
		}
	}
	refused := []string{
		"",
		"projects/7/x",
		"//gitlab.example.com/api/v4/projects/7/x",
		"ftp://gitlab.example.com/api/v4/projects/7/x",
		"http://gitlab.example.com/api/v4/projects/7/x",
		"https://gitlab.example.com.evil.net/api/v4/projects/7/x",
		"https://evil.net/api/v4/projects/7/x",
		"https://gitlab.example.com:8443/api/v4/projects/7/x",
		"https://user:secret@gitlab.example.com/api/v4/projects/7/x",
		"https://gitlab.example.com/api/v4/projects/7/x?private_token=1",
		"https://gitlab.example.com/api/v4/projects/7/x#frag",
		"https://gitlab.example.com/api/v4/",
		"https://gitlab.example.com/api/v40/projects/7/x",
		"https://gitlab.example.com/api/v4/projects/../../x",
		"https://gitlab.example.com/api/v4/projects/%2e%2e/x",
		"https://gitlab.example.com/api/v4/projects//x",
		"https://gitlab.example.com/api/v4/projects/:id/x",
		"https://gitlab.example.com/grp/app/-/releases/v1/downloads/x",
		"https://user:secret@gitlab.example.com/api/v4/%zz",
	}
	for _, link := range refused {
		got, err := feed.apiPath(link)
		if err == nil {
			t.Errorf("apiPath(%q) = %q, want a refusal", link, got)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("refusal of %q repeats its credentials: %v", link, err)
		}
	}
}

func TestGitLabErrorsAreMapped(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   error
		text   string
	}{
		{name: "signed out", stderr: "glab: 401 Unauthorized (HTTP 401)", want: ErrGlabSignedOut, text: "sign in with glab"},
		{name: "unauthenticated", stderr: "Unauthenticated.", want: ErrGlabSignedOut, text: "glab auth login --hostname " + testGitLabHost},
		{name: "no access", stderr: "glab: 404 Project Not Found (HTTP 404)", want: ErrGitLabNoAccess, text: "no access to " + testGitLabProject},
		{name: "other", stderr: "glab: 500 Internal Server Error (HTTP 500)", text: "500 Internal Server Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.set(testGitLabListPath, fakeGlabRoute{Stdout: []byte(`{"message":"x"}`), Stderr: tc.stderr, Exit: 1})
			_, err := f.source(t, "wsl-noremote", "0.1.0").List(context.Background())
			if err == nil {
				t.Fatal("List succeeded on a failed glab call")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			for _, other := range []error{ErrGlabSignedOut, ErrGitLabNoAccess, ErrGlabNotInstalled} {
				if other != tc.want && errors.Is(err, other) {
					t.Errorf("error = %v also matches %v", err, other)
				}
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("error %q does not say %q", err, tc.text)
			}
			if strings.Contains(err.Error(), fakeGlabUpdateNotice) {
				t.Errorf("error %q carries glab's update notice", err)
			}
		})
	}
}

// The user-facing state for a signed-out glab: the check's result says so.
func TestGitLabSignedOutReachesTheCheckResult(t *testing.T) {
	f := newFakeGitLab(t)
	f.set(testGitLabListPath, fakeGlabRoute{Stderr: "glab: 401 Unauthorized (HTTP 401)", Exit: 1})
	a := newGitLabService(t, f, "0.1.0")
	availability, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !availability.Supported || !strings.Contains(availability.CheckError, "sign in with glab") {
		t.Fatalf("availability = %+v, want the sign-in message as CheckError", availability)
	}
}

func TestGitLabMissingGlabIsReported(t *testing.T) {
	// An ordinary Core with nothing on PATH: glab cannot resolve, so the
	// real one cannot run either.
	t.Setenv("PATH", t.TempDir())
	src, err := newReleaseSource(Config{
		CurrentVersion: "0.1.0",
		Platform:       "wsl-noremote",
		Arch:           "amd64",
		GlabRunner:     gitops.NewCore().StreamGitLabAPI,
	}, testGitLabProject)
	if err != nil {
		t.Fatalf("newReleaseSource: %v", err)
	}
	_, err = src.List(context.Background())
	if !errors.Is(err, ErrGlabNotInstalled) {
		t.Fatalf("List error = %v, want ErrGlabNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "glab is not installed") {
		t.Fatalf("error %q does not say glab is not installed", err)
	}
}

func TestParseGitLabProject(t *testing.T) {
	valid := map[string]gitlabSource{
		"gitlab.com/fortressinfosec/agent-overflow": {host: "gitlab.com", project: "fortressinfosec/agent-overflow"},
		"GitLab.Example.com/a/b_c/d.e":              {host: "gitlab.example.com", project: "a/b_c/d.e"},
	}
	for raw, want := range valid {
		got, err := parseGitLabProject(raw)
		if err != nil || got != want {
			t.Errorf("parseGitLabProject(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	invalid := []string{
		"",
		"gitlab.com",
		"gitlab.com/project",
		"https://gitlab.com/a/b",
		"gitlab.com:443/a/b",
		"-gitlab.com/a/b",
		"gitlab..com/a/b",
		"gitlab.com/a/../b",
		"gitlab.com/a/b/",
		"gitlab.com//b",
		"gitlab.com/a b/c",
		"gitlab.com/a/.b",
		"gitlab.com/a/b?x",
		"gitlab.com/a/b:id",
	}
	for _, raw := range invalid {
		if got, err := parseGitLabProject(raw); err == nil {
			t.Errorf("parseGitLabProject(%q) = %+v, want an error", raw, got)
		}
	}
}

func TestGitLabSourceConfigurationIsValidated(t *testing.T) {
	config := Config{CurrentVersion: "0.1.0", Platform: "wsl-noremote", Arch: "amd64"}
	if _, err := newReleaseSource(config, testGitLabProject); err == nil || !strings.Contains(err.Error(), "glab runner") {
		t.Fatalf("source without a runner = %v, want refused", err)
	}
	config.GlabRunner = newFakeGitLab(t).runner()
	if _, err := newReleaseSource(config, "gitlab.com/only-one"); err == nil || !strings.Contains(err.Error(), "HOST/NAMESPACE/PROJECT") {
		t.Fatalf("malformed project = %v, want refused", err)
	}
	setGitLabProject(t, "not a project")
	if err := New("0.1.0", Deps{}).Configure(updater.New(noopUpdaterHost{}), config); err == nil {
		t.Fatal("Configure accepted a malformed linked project")
	}
}
