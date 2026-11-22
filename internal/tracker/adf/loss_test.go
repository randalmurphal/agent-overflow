package adf

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestCheckLossKinds proves the refusal's kinds are the union of the lost
// warnings' sets, markers before details, read from the set alone: a
// message naming an expand or a marker changes nothing.
func TestCheckLossKinds(t *testing.T) {
	for _, tc := range []struct {
		sets []LossSet
		want []LossKind
	}{
		{[]LossSet{lossMarkers}, []LossKind{LossMarkers}},
		{[]LossSet{lossDetails}, []LossKind{LossDetails}},
		{[]LossSet{lossDetails, lossMarkers}, []LossKind{LossMarkers, LossDetails}},
		{[]LossSet{lossMarkers | lossDetails}, []LossKind{LossMarkers, LossDetails}},
		{[]LossSet{lossDetails, lossDetails}, []LossKind{LossDetails}},
	} {
		ws := []Warning{{Msg: "<details> expand <!-- macro --> panel"}}
		for _, s := range tc.sets {
			ws = append(ws, Warning{Msg: "<!-- macro --> expand", Loses: s})
		}
		err := CheckLoss(ws, false)
		var le *LossError
		if !errors.As(err, &le) || !slices.Equal(le.Kinds, tc.want) || len(le.Lost) != len(tc.sets) {
			t.Errorf("%v: %#v, want kinds %v", tc.sets, err, tc.want)
		}
	}
}

// TestCheckLossNamesWhatIsLost proves the error carries each Flattens
// warning, with its line, and no plain one, and that its text names what
// would be lost without naming any way to accept it.
func TestCheckLossNamesWhatIsLost(t *testing.T) {
	ws := []Warning{
		{Msg: "a plain warning"},
		{Line: 3, Msg: "replaced a block that had a custom panel; those are now plain", Loses: lossMarkers},
		{Msg: "dropped the text color"},
		{Line: 7, Msg: `the expand "T" is still in the Markdown, but the edit would lose it`,
			Loses: lossDetails},
	}
	err := CheckLoss(ws, false)
	var le *LossError
	if !errors.As(err, &le) {
		t.Fatalf("got %T %v, want a *LossError", err, err)
	}
	if want := []Warning{ws[1], ws[3]}; !slices.Equal(le.Lost, want) {
		t.Errorf("Lost = %v, want %v", le.Lost, want)
	}
	want := "the edit would lose what Markdown can't show: line 3: replaced a block that " +
		`had a custom panel; those are now plain; line 7: the expand "T" is still in the ` +
		"Markdown, but the edit would lose it"
	if err.Error() != want {
		t.Errorf("Error() = %q\nwant      %q", err.Error(), want)
	}
	for _, flagLike := range []string{"--", "allow", "re-run", "run again"} {
		if strings.Contains(err.Error(), flagLike) {
			t.Errorf("Error() names %q: %s", flagLike, err)
		}
	}
}

// TestLossKindKeep proves each kind says how to keep its content and how
// to drop it on purpose.
func TestLossKindKeep(t *testing.T) {
	for kind, want := range map[LossKind][]string{
		LossMarkers: {"keep each container's <!-- open --> and <!-- /close --> markers",
			"to drop a macro or lozenge, delete its marker or [TEXT]"},
		LossDetails: {"keep each <details><summary>TITLE</summary> and its </details>",
			"to drop an expand, delete both tags"},
	} {
		for _, w := range want {
			if !strings.Contains(kind.Keep(), w) {
				t.Errorf("%s: Keep() lacks %q: %s", kind, w, kind.Keep())
			}
		}
	}
	if LossKind("other").Keep() != "" {
		t.Error("an unknown kind has advice")
	}
}

// TestCheckLossPasses proves an edit that flattens nothing, or whose loss
// the user accepted, passes.
func TestCheckLossPasses(t *testing.T) {
	plain := []Warning{{Line: 2, Msg: "dropped the text color"}, {Msg: "x expand"}}
	flat := append(slices.Clone(plain), Warning{Line: 4, Msg: "x panel", Loses: lossMarkers})
	for name, tc := range map[string]struct {
		ws       []Warning
		accepted bool
	}{
		"no warnings":            {nil, false},
		"no flattening":          {plain, false},
		"no flattening accepted": {plain, true},
		"flattening accepted":    {flat, true},
	} {
		if err := CheckLoss(tc.ws, tc.accepted); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCheckLossOnEdit proves the check refuses a real edit that flattens a
// container, names the container, and lets the same edit through once
// the user accepts; an edit inside the container passes.
func TestCheckLossOnEdit(t *testing.T) {
	col := func(s string) *Node {
		return &Node{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0},
			Content: []*Node{para(s)}}
	}
	orig := Doc(&Node{Type: "layoutSection", Content: []*Node{col("left"), col("right")}})
	md := ToMarkdown(orig)
	inside := strings.Replace(md, "left", "LEFT", 1)
	_, ws, err := Edit(t.Context(), orig, inside, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLoss(ws, false); err != nil {
		t.Errorf("edit inside the layout: %v", err)
	}
	flattened := md
	for _, m := range []string{"<!-- layout -->", "<!-- column 50% -->", "<!-- /column -->",
		"<!-- column 50% -->", "<!-- /column -->", "<!-- /layout -->"} {
		flattened = strings.Replace(flattened, m+"\n\n", "", 1)
	}
	_, ws, err = Edit(t.Context(), orig, flattened, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckLoss(ws, false)
	var le *LossError
	if !errors.As(err, &le) || !slices.Equal(le.Kinds, []LossKind{LossMarkers}) ||
		!strings.Contains(err.Error(), "a column layout") {
		t.Errorf("flattened layout: %#v (warnings %v)", err, ws)
	}
	if err := CheckLoss(ws, true); err != nil {
		t.Errorf("accepted: %v", err)
	}
}
