package browser

import (
	"slices"
	"testing"
)

// endsPage is an adopted popup's driver whose engine held ends for its
// listing, as the WebKit engines and headless Chromium do.
type endsPage struct {
	*handledPage
	ends   *pageEnds
	events engineEvents
}

func (p endsPage) Adopted() { p.ends.adopted(p.events, p.handle) }

// A popup whose crash and close were both held for its listing is no tab
// once listed, crashed or live. One whose crash alone was held stays a
// crashed tab.
func TestAPopupThatCrashedAndClosedBeforeItsListingIsNoTab(t *testing.T) {
	cases := []struct {
		name        string
		end         pageEnd
		wantCrashed bool
	}{
		{"crashed then closed", pageEnd{crashed: "render-process-exited", closed: true}, false},
		{"crashed", pageEnd{crashed: "render-process-exited"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManager(t)
			events := engineEvents{PageClosed: manager.removeClosedPage, PageCrashed: manager.pageCrashed}
			ends := &pageEnds{popup: true}
			ends.hold(tc.end)
			engine.wrap = func(driver pageDriver) pageDriver {
				return endsPage{handledPage: driver.(*handledPage), ends: ends, events: events}
			}
			before := tabIDs(manager, access)
			manager.adoptPopup(engine.popup(opener, "popup"))
			pages := manager.CompanionState(access).Pages
			var added []PageInfo
			for _, page := range pages {
				if !slices.Contains(before, page.ID) {
					added = append(added, page)
				}
			}
			switch {
			case tc.wantCrashed && (len(added) != 1 || !added[0].Suspended || added[0].Error == ""):
				t.Fatalf("the popup's tabs are %+v, want one crashed", added)
			case !tc.wantCrashed && len(added) != 0:
				t.Fatalf("the popup left tabs %+v, want none", added)
			}
		})
	}
}
