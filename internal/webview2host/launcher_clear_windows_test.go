//go:build windows

package webview2host

import "testing"

func TestClearSettlesCreateBeforeItCanUseTheReleasedEnvironment(t *testing.T) {
	r := newPopupRig(t)
	r.host.config.UserDataDir = t.TempDir()
	calls := 0
	r.beforeMain = func() bool {
		calls++
		if calls != 2 {
			return false
		}
		r.beforeMain = nil
		r.host.Apply(Directive{Op: OpClearData, PageID: "clear"})
		return true
	}
	r.host.Apply(Directive{Op: OpCreate, PageID: "page", ProfileID: "prof"})
	if len(r.env.pending) != 0 || r.page("page") != nil {
		t.Error("controller creation reached the released environment")
	}
	sameReports(t, "clear crossing create", kinds(t, r.takeReports()), "page:closed", "clear:cleared")
	r.done()
}

func TestClearRefusesAdmissionAndAnotherClearUntilItFinishes(t *testing.T) {
	r := newPopupRig(t)
	r.host.config.UserDataDir = t.TempDir()
	r.createPage("page", "prof", false)
	main := r.host.config.OnMain
	r.host.config.OnMain = func(fn func()) {
		r.host.config.OnMain = main
		main(fn)
		r.host.Apply(Directive{Op: OpCreate, PageID: "during", ProfileID: "prof"})
		r.host.Apply(Directive{Op: OpClearData, PageID: "second"})
	}
	r.host.Apply(Directive{Op: OpClearData, PageID: "first"})
	if len(r.env.pending) != 0 {
		t.Error("a controller was started while clearing")
	}
	sameReports(t, "overlapping clear and create", kinds(t, r.takeReports()),
		"page:closed", "during:create-failed", "second:clear-failed", "first:cleared")
	r.createPage("after", "prof", false)
	r.done()
	r.takeReports()
	r.host.Apply(Directive{Op: OpClearData, PageID: "after-close"})
	sameReports(t, "clear of closed host", kinds(t, r.takeReports()), "after-close:clear-failed")
}
