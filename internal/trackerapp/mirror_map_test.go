package trackerapp

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// filled is a T with every member set, recursively: text to its path,
// numbers to 1 (a valid group, state, unit and kind), booleans true, one
// element in each list and a value behind each pointer. A mapping that
// drops a member fails a round trip of it.
func filled[T any]() T {
	var v T
	fill(reflect.ValueOf(&v).Elem(), "x")
	return v
}

var (
	timeType  = reflect.TypeFor[time.Time]()
	valueType = reflect.TypeFor[tracker.Value]()
	dateType  = reflect.TypeFor[tracker.Date]()
)

func fill(v reflect.Value, path string) {
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(time.UnixMilli(1_790_000_000_000 + int64(len(path))).UTC()))
		return
	case valueType:
		v.Set(reflect.ValueOf(tracker.TextValue(path)))
		return
	case dateType:
		d, err := tracker.NewDate(2026, time.November, 20)
		if err != nil {
			panic(err)
		}
		v.Set(reflect.ValueOf(d))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				fill(v.Field(i), path+"."+f.Name)
			}
		}
	case reflect.String:
		v.SetString(path)
	case reflect.Int, reflect.Int64:
		if v.Type() == reflect.TypeFor[time.Duration]() {
			v.SetInt(int64(time.Hour))
			return
		}
		v.SetInt(1)
	case reflect.Uint8:
		v.SetUint(1)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), path+"[0]")
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), path)
		v.Set(p)
	default:
		panic("fill: " + v.Type().String() + " at " + path)
	}
}

func same(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s round trip:\n got %+v\nwant %+v", what, got, want)
	}
}

func TestCardRoundTrip(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]tracker.Card{"filled": filled[tracker.Card](), "zero": {}} {
		m, err := mirrorCard(c)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := cardOf(m)
		if !ok {
			t.Fatalf("%s: the blob was a miss", name)
		}
		same(t, name+" card", got, c)
	}
	r := filled[tracker.Row]()
	m, err := mirrorRow(r)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rowOf(m)
	if !ok {
		t.Fatal("the row's blob was a miss")
	}
	same(t, "row", got, r)
	if got, _ := rowOf(must[store.MirrorRow](t)(mirrorRow(tracker.Row{}))); !reflect.DeepEqual(got, tracker.Row{}) {
		t.Errorf("zero row round trip %+v", got)
	}
}

func TestCatalogAndStructureRoundTrip(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]tracker.Catalog{"filled": filled[tracker.Catalog](), "zero": {}} {
		same(t, name+" catalog", catalogOf(must[store.MirrorCatalog](t)(mirrorCatalog(c))), c)
	}
	st := filled[tracker.Structure]()
	m := must[store.MirrorStructure](t)(mirrorStructure(st, 5))
	if m.ReadAt != 5 {
		t.Errorf("structure read at %d", m.ReadAt)
	}
	same(t, "structure", structureOf(m, catalogOf(m.Catalog)), st)
	zero := tracker.Structure{}
	mz := must[store.MirrorStructure](t)(mirrorStructure(zero, 5))
	same(t, "zero structure", structureOf(mz, tracker.Catalog{}), zero)
}

// A user or status named twice in one read is stored once, the later
// entry winning: the store refuses a repeat.
func TestCatalogKeepsTheLastOfARepeat(t *testing.T) {
	t.Parallel()
	c := tracker.Catalog{
		Users:    []tracker.User{{ID: "u1", Name: "Old"}, {ID: "u1", Name: "New"}},
		Statuses: []tracker.Status{{ID: "s1", Name: "Old", Group: 1}, {ID: "s1", Name: "New", Group: 2}},
	}
	got := catalogOf(must[store.MirrorCatalog](t)(mirrorCatalog(c)))
	if len(got.Users) != 1 || got.Users[0].Name != "New" || len(got.Statuses) != 1 || got.Statuses[0].Group != 2 {
		t.Fatalf("catalog %+v", got)
	}
}

func TestHistoryAndWorklogRoundTrip(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]tracker.IssueHistory{"filled": filled[tracker.IssueHistory](), "zero": {}} {
		got, err := historyOf(mirrorHistory(h))
		if err != nil {
			t.Fatal(err)
		}
		// The store keeps whether a read was final (run.historyOf), not the
		// tracker's CompleteTo.
		h.CompleteTo = time.Time{}
		same(t, name+" history", got, h)
	}
	// A sprint change to no sprint stays a sprint change.
	h := tracker.IssueHistory{Issue: "1", CompleteTo: time.UnixMilli(5).UTC(), Sprint: []tracker.SprintChange{{Change: tracker.Change{At: time.UnixMilli(4).UTC()}}}}
	if got := must[tracker.IssueHistory](t)(historyOf(mirrorHistory(h))); len(got.Sprint) != 1 {
		t.Fatalf("an emptied sprint set %+v", got)
	}
	if _, err := historyOf(store.MirrorHistory{Changes: []store.MirrorChange{{Kind: "priority"}}}); err == nil {
		t.Fatal("a change of an unknown kind read")
	}
	w := filled[tracker.Worklog]()
	same(t, "worklog", must[tracker.Worklog](t)(worklogOf(mirrorWorklog(w))), w)
	if _, err := worklogOf(store.MirrorWorklog{ID: "1", Day: "not a day"}); err == nil {
		t.Fatal("a worklog with no day read")
	}
}

func TestDetailRoundTrip(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]tracker.Detail{"filled": filled[tracker.Detail](), "zero": {}} {
		m := store.MirrorDetail{IssueID: string(d.Card.ID), Data: must[[]byte](t)(mirrorDetail(d))}
		got, ok := detailOf(m)
		if !ok {
			t.Fatalf("%s: the blob was a miss", name)
		}
		same(t, name+" detail", got, d)
	}
}

// Every blob that is not exactly the current version is a miss: its
// record reads as its columns and nothing panics.
func TestBlobMisses(t *testing.T) {
	t.Parallel()
	c := card("10001", 1_799_000_000_000)
	c.Labels = []tracker.Choice{{ID: "l1", Name: "frontend"}}
	m := must[store.MirrorCard](t)(mirrorCard(c))
	good := string(m.Data)
	misses := map[string]string{
		"older version":  strings.Replace(good, `"v":1`, `"v":0`, 1),
		"newer version":  strings.Replace(good, `"v":1`, `"v":2`, 1),
		"unknown member": strings.Replace(good, `"v":1`, `"v":1,"x":true`, 1),
		"corrupt":        good[:len(good)/2],
		"empty":          "",
		"two values":     good + good,
		"not an object":  `[1]`,
	}
	for name, data := range misses {
		m.Data = []byte(data)
		got, ok := cardOf(m)
		if ok || got.Key != c.Key || got.Status != c.Status || got.Title != "" || got.Labels != nil {
			t.Errorf("card blob %s: ok %v, %+v", name, ok, got)
		}
		u := userOf(store.MirrorUser{ID: "u1", Name: "User 1", Data: []byte(data)})
		if u.Name != "User 1" || u.Email != "" {
			t.Errorf("user blob %s: %+v", name, u)
		}
		st := structureOf(store.MirrorStructure{Board: store.MirrorBoard{ID: "1", EstimateUnit: 1, Data: []byte(data)}}, tracker.Catalog{})
		if st.Estimate.Unit != tracker.UnitPoints || st.Estimate.Choices != nil {
			t.Errorf("board blob %s: %+v", name, st.Estimate)
		}
		if _, ok := detailOf(store.MirrorDetail{IssueID: "10001", Data: []byte(data)}); ok {
			t.Errorf("detail blob %s read", name)
		}
	}
	// A detail stored under another issue's ID is a miss.
	d := store.MirrorDetail{IssueID: "10002", Data: must[[]byte](t)(mirrorDetail(tracker.Detail{Card: c}))}
	if _, ok := detailOf(d); ok {
		t.Error("a detail of another issue read")
	}
}

// The mirror's enumerations are the neutral model's numbers.
func TestMirrorEnumerationsMatchTheModel(t *testing.T) {
	t.Parallel()
	pairs := [][2]int{
		{int(tracker.GroupBacklog), store.MirrorGroupBacklog}, {int(tracker.GroupToDo), store.MirrorGroupToDo},
		{int(tracker.GroupInProgress), store.MirrorGroupInProgress}, {int(tracker.GroupDone), store.MirrorGroupDone},
		{int(tracker.GroupCanceled), store.MirrorGroupCanceled},
		{int(tracker.SprintFuture), store.MirrorSprintFuture}, {int(tracker.SprintActive), store.MirrorSprintActive},
		{int(tracker.SprintClosed), store.MirrorSprintClosed},
		{int(tracker.UnitNone), store.MirrorEstimateNone}, {int(tracker.UnitPoints), store.MirrorEstimatePoints},
		{int(tracker.UnitTime), store.MirrorEstimateTime},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("model %d is mirror %d", p[0], p[1])
		}
	}
	for _, k := range []FailureKind{FailureTransient, FailureRateLimited, FailureSetup, FailureForbidden, FailureTracker} {
		if !slices.Contains([]string{store.MirrorFailureTransient, store.MirrorFailureRateLimited, store.MirrorFailureSetup,
			store.MirrorFailureForbidden, store.MirrorFailureTracker}, string(k)) {
			t.Errorf("failure kind %q has no mirror kind", k)
		}
	}
}

// shapeOf describes t's members recursively, the types with their own
// encoding by name.
func shapeOf(t reflect.Type) string {
	switch t {
	case timeType, valueType, dateType:
		return t.String()
	}
	switch t.Kind() {
	case reflect.Struct:
		var parts []string
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				parts = append(parts, f.Name+":"+shapeOf(f.Type))
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	case reflect.Slice:
		return "[]" + shapeOf(t.Elem())
	case reflect.Pointer:
		return "*" + shapeOf(t.Elem())
	}
	return t.Kind().String()
}

// wantDetailShape is tracker.Detail's shape that detailBlobVersion 1
// encodes.
const wantDetailShape = `{Card:{ID:string,Key:string,Project:string,Type:{ID:string,Name:string,Level:int},Title:string,Status:string,Assignee:string,Priority:{ID:string,Name:string},Labels:[]{ID:string,Name:string},Parent:string,Sprints:[]string,Created:time.Time,Updated:time.Time,Resolved:time.Time},URL:string,Description:string,Fields:[]{ID:string,Name:string,Kind:uint8,List:bool,Required:bool,Editable:bool,Value:tracker.Value},Comments:{Comments:[]{ID:string,Author:string,Body:string,Created:time.Time,Updated:time.Time,Internal:bool},Next:string,Catalog:{Users:[]{ID:string,Name:string,Email:string,AvatarURL:string,Active:bool},Statuses:[]{ID:string,Name:string,Group:uint8}}},Links:[]{ID:string,Type:{ID:string,Name:string,Outward:string,Inward:string},Outward:bool,Issue:{ID:string,Key:string,Title:string,Status:string}},Attachments:[]{ID:string,Name:string,MimeType:string,Size:int64,Created:time.Time,Author:string},History:[]{At:time.Time,Author:string,Field:string,From:string,To:string},HistoryFrom:time.Time,Moves:[]{ID:string,Name:string,To:string,Fields:[]{ID:string,Name:string,Kind:uint8,List:bool,Required:bool,Editable:bool,Value:tracker.Value}},Catalog:{Users:[]{ID:string,Name:string,Email:string,AvatarURL:string,Active:bool},Statuses:[]{ID:string,Name:string,Group:uint8}}}`

// A change to tracker.Detail changes what a stored detail blob means: a
// member added would read as empty from every blob stored before it.
// Bump detailBlobVersion, so those read as misses, and update
// wantDetailShape.
func TestDetailBlobShape(t *testing.T) {
	t.Parallel()
	if got := shapeOf(reflect.TypeFor[tracker.Detail]()); got != wantDetailShape {
		t.Fatalf("tracker.Detail changed shape; bump detailBlobVersion and set wantDetailShape to\n%s", got)
	}
}
