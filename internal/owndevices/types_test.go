package owndevices

import (
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/entityid"
	"encoding/base64"
	"reflect"
	"testing"
)

func key(n byte) string {
	raw := make([]byte, 32)
	raw[0] = n
	return base64.RawURLEncoding.EncodeToString(raw)
}
func TestMergeRemovalWinsAndSponsorCannotReplaceKnownTrust(t *testing.T) {
	own := Member{KeyThumbprint: key(1), BackendID: entityid.New(), Name: "Mac", Generation: 1, Routes: []computerroute.Route{{Endpoint: "https://mac.test"}}}
	stale := own
	removed := own
	removed.Removed = true
	held, _, e := Merge(nil, []Member{own})
	if e != nil {
		t.Fatal(e)
	}
	held, changed, e := Merge(held, []Member{removed, stale})
	if e != nil || !changed || !held[0].Removed {
		t.Fatal(held, changed, e)
	}
	if _, changed, e = Merge(held, []Member{stale}); e != nil || changed {
		t.Fatal("stale presence cleared removal", e)
	}
	restored := own
	restored.Generation = 2
	restored.Routes = []computerroute.Route{{Endpoint: "https://replacement.test"}}
	held, _, e = Merge(held, []Member{restored})
	if e != nil || held[0].Removed || !reflect.DeepEqual(held[0].Routes, own.Routes) {
		t.Fatal("forwarded restoration replaced pinned routes", held, e)
	}
	other := Member{KeyThumbprint: key(2), BackendID: own.BackendID, Generation: 1}
	if _, _, e = Merge(held, []Member{other}); e == nil {
		t.Fatal("duplicate backend identity admitted")
	}
}
