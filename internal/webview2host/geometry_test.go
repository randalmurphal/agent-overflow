package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func geometryMessage(t *testing.T, fields map[string]any) string {
	t.Helper()
	body := map[string]any{
		"mount": "mount-1", "seq": 1,
		"x": 10, "y": 20, "width": 800, "height": 600,
		"clipX": 10, "clipY": 20, "clipWidth": 800, "clipHeight": 600,
		"viewportWidth": 1600, "viewportHeight": 900, "devicePixelRatio": 1.5,
		"visible": true, "background": "#112233",
	}
	for k, v := range fields {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return GeometryMessagePrefix + string(raw)
}

func TestParseGeometryMessageAcceptsAReportAndIgnoresOtherMessages(t *testing.T) {
	g, err := ParseGeometryMessage(geometryMessage(t, map[string]any{"futureField": "ignored"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if g.Mount != "mount-1" || g.Seq != 1 || g.X != 10 || g.Width != 800 || g.DevicePixelRatio != 1.5 || !g.Visible || g.Background != "#112233" {
		t.Fatalf("parsed %+v", g)
	}
	// A pane with nothing to show is a valid report: it hides the page.
	if _, err := ParseGeometryMessage(geometryMessage(t, map[string]any{"width": 0, "height": 0, "clipWidth": 0, "clipHeight": 0, "visible": false})); err != nil {
		t.Fatalf("an empty pane must parse: %v", err)
	}
	for _, other := range []string{"", "wails:runtime:ready", "hello", "ao:pane-geometr"} {
		if _, err := ParseGeometryMessage(other); !errors.Is(err, ErrNotGeometry) {
			t.Fatalf("%q: err = %v, want ErrNotGeometry", other, err)
		}
	}
}

// The report moves a real window, so it is held to the bounds directive's
// contract before it reaches the mailbox.
func TestParseGeometryMessageRejectsWhatABoundsDirectiveWould(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"no mount":            {"mount": nil},
		"mount with a slash":  {"mount": "a/b"},
		"zero sequence":       {"seq": 0},
		"huge x":              {"x": 1e9},
		"negative width":      {"width": -1},
		"half clip":           {"clipWidth": 0},
		"half viewport":       {"viewportHeight": 0},
		"negative dpr":        {"devicePixelRatio": -1},
		"absurd dpr":          {"devicePixelRatio": 1000},
		"named background":    {"background": "red"},
		"string where number": {"x": "10"},
	} {
		if _, err := ParseGeometryMessage(geometryMessage(t, fields)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	long := GeometryMessagePrefix + `{"mount":"m","seq":1,"background":"` + strings.Repeat("a", maxGeometryMessageBytes) + `"}`
	if _, err := ParseGeometryMessage(long); err == nil || errors.Is(err, ErrNotGeometry) {
		t.Fatalf("an oversized message must be refused, got %v", err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		g := PaneGeometry{Mount: "m", Seq: 1, X: v}
		if err := g.Validate(); err == nil {
			t.Fatalf("x=%v accepted", v)
		}
	}
}

// Wails delivers each raw message on a goroutine of its own, so an older
// report can arrive after a newer one: it must not replace it.
func TestGeometryMailboxDropsStaleSequences(t *testing.T) {
	box := NewGeometryMailbox()
	if !box.Put(PaneGeometry{Mount: "m", Seq: 2, X: 2}) {
		t.Fatal("the first report must schedule a drain")
	}
	box.Put(PaneGeometry{Mount: "m", Seq: 1, X: 1})
	box.Put(PaneGeometry{Mount: "m", Seq: 2, X: 99})
	if g, _ := box.Latest("m"); g.X != 2 {
		t.Fatalf("a stale or repeated sequence replaced the newest report: %+v", g)
	}
	box.Put(PaneGeometry{Mount: "m", Seq: 3, X: 3})
	if g, _ := box.Latest("m"); g.X != 3 {
		t.Fatalf("a newer report must replace: %+v", g)
	}
	if _, ok := box.Latest("other"); ok {
		t.Fatal("an unknown mount has no report")
	}
	var nilBox *GeometryMailbox
	if _, ok := nilBox.Latest("m"); ok {
		t.Fatal("a nil mailbox has no report")
	}
}

// One drain is pending at a time however many reports arrive, and a report
// that lands after the drain took the mailbox schedules the next one.
func TestGeometryMailboxSchedulesAtMostOneDrain(t *testing.T) {
	box := NewGeometryMailbox()
	scheduled := 0
	for seq := uint64(1); seq <= 100; seq++ {
		if box.Put(PaneGeometry{Mount: fmt.Sprintf("m%d", seq%3), Seq: seq}) {
			scheduled++
		}
	}
	if scheduled != 1 {
		t.Fatalf("scheduled %d drains for one burst, want 1", scheduled)
	}
	taken := box.Take()
	if len(taken) != 3 {
		t.Fatalf("took %d reports, want the newest of each of 3 mounts", len(taken))
	}
	for _, g := range taken {
		if g.Seq < 98 {
			t.Fatalf("took a superseded report: %+v", g)
		}
	}
	if got := box.Take(); len(got) != 0 {
		t.Fatalf("a second take with nothing new returned %d", len(got))
	}
	if !box.Put(PaneGeometry{Mount: "m1", Seq: 200}) {
		t.Fatal("a report after the drain must schedule the next")
	}
}

func TestGeometryMailboxIsBounded(t *testing.T) {
	box := NewGeometryMailbox()
	for i := range MaxGeometryMounts {
		box.Put(PaneGeometry{Mount: fmt.Sprintf("m%d", i), Seq: 1})
	}
	// m0 is refreshed, so m1 is now the least recently updated.
	box.Put(PaneGeometry{Mount: "m0", Seq: 2})
	box.Put(PaneGeometry{Mount: "new", Seq: 1})
	if len(box.entries) != MaxGeometryMounts {
		t.Fatalf("mailbox holds %d mounts, bound is %d", len(box.entries), MaxGeometryMounts)
	}
	if _, ok := box.Latest("m1"); ok {
		t.Fatal("the least recently updated mount must be evicted")
	}
	for _, mount := range []string{"m0", "new", "m2"} {
		if _, ok := box.Latest(mount); !ok {
			t.Fatalf("%s was evicted", mount)
		}
	}
}

func TestTrustedGeometrySourceIsTheBackendPageOrigin(t *testing.T) {
	const page = "http://localhost:41234/?host=webview"
	if !TrustedGeometrySource("http://localhost:41234/threads/1", "http://localhost:41234/", page) {
		t.Fatal("the SPA's own document must be trusted")
	}
	for name, src := range map[string][2]string{
		"other port":        {"http://localhost:41235/", "http://localhost:41235/"},
		"other host":        {"http://127.0.0.1:41234/", "http://127.0.0.1:41234/"},
		"picker asset":      {"wails://localhost/picker", "wails://localhost/picker"},
		"framed in another": {"http://localhost:41234/", "https://evil.example/"},
		"empty":             {"", ""},
		"file":              {"file:///C:/x.html", "file:///C:/x.html"},
	} {
		if TrustedGeometrySource(src[0], src[1], page) {
			t.Errorf("%s: trusted", name)
		}
	}
	if TrustedGeometrySource("http://localhost:41234/", "http://localhost:41234/", "") {
		t.Fatal("no backend page means nothing is trusted")
	}
}
