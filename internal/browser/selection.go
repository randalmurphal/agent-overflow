package browser

import "context"

// The order of the user's tab selections. Each companion action runs on its
// own goroutine, so two selections made in a row can reach the Manager in
// either order. A selection the user makes in a pane carries that pane's
// mount and the number the client gave it when the user acted, increasing
// per mount (Selection). The Manager keeps, per mount, the newest selection
// it applied, and decides each numbered selection against it in the same
// m.mu hold that sets the active tab (orderSelectionLocked):
//
//   - An absolute selection (a tab chosen by id, or the new tab a "new",
//     "reopen", "duplicate" or "download-open" opens) older than the newest
//     absolute selection applied is overtaken: it changes nothing and its
//     restore work does not run; its call still succeeds. A new tab it
//     opened stays, in the background.
//   - A step (next or previous tab) is relative, so steps compose by
//     addition in any order: each applies to the active tab as it finds it,
//     and two forward steps delivered in either order move two tabs. A step
//     older than the newest absolute selection is overtaken. An absolute
//     selection that arrives after newer steps applied selects its tab moved
//     by those steps, so a click and then a next-tab end one tab past the
//     clicked tab whichever arrives first.
//
// A selection with no mount or number (an older client, a connected browser
// that mounts no pane, the chat header's show), one naming a mount that is
// gone or belongs to another thread, an agent's browser_select_page, and a
// selection from another mount are applied in arrival order. Closing the
// active tab reselects another (repairActivePage); that is a repair, not a
// selection: it carries no number, so a selection the user made before the
// close still applies when it arrives after, and it replaces only an active
// tab that is gone, so it never undoes a newer selection. Nor does the
// pane's restore of the tab it shows (RestoreActiveCompanionPage), which
// selects nothing.

// Selection is a user's tab selection, numbered in the pane mount it was
// made in. A zero Seq is unnumbered.
type Selection struct {
	Mount string
	Seq   uint64
}

type selectionKey struct{}

// WithSelection returns ctx carrying the user's numbered selection, which
// every Manager call that selects a tab reads (ActivateCompanionPage,
// StepCompanionPage, and the user's tab openings).
func WithSelection(ctx context.Context, sel Selection) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

func selectionOf(ctx context.Context) Selection {
	sel, _ := ctx.Value(selectionKey{}).(Selection)
	return sel
}

// maxSelectionSteps bounds the steps a mount keeps above its newest absolute
// selection; past it the oldest are forgotten, which matters only to an
// absolute selection overtaken by that many newer steps.
const maxSelectionSteps = 64

// mountSelection is one mount's selection order: the newest absolute
// selection applied and the steps applied after it in number, which an
// absolute selection arriving late replays from its own tab.
type mountSelection struct {
	absolute uint64
	steps    []numberedStep
}

type numberedStep struct {
	seq   uint64
	delta int
}

// orderSelectionLocked decides the user's selection sel, of the tab at index
// pick in tabs (absolute, step 0) or of step tabs from the active one
// (step != 0, pick being that tab), against its mount's record, and answers
// the tab to select, or false when sel is overtaken. An unnumbered or
// unattributable selection answers pick. The caller holds m.mu, has checked
// pick is in tabs, and selects the answer in the same hold.
func (m *Manager) orderSelectionLocked(threadID string, sel Selection, tabs []threadTab, pick, step int) (int, bool) {
	mount, ok := m.panes[sel.Mount]
	if sel.Seq == 0 || !ok || mount.threadID != threadID {
		return pick, true
	}
	order := mount.selection
	if order == nil {
		order = &mountSelection{}
		mount.selection = order
		m.panes[sel.Mount] = mount
	}
	if sel.Seq <= order.absolute {
		return 0, false
	}
	if step != 0 {
		order.steps = append(order.steps, numberedStep{seq: sel.Seq, delta: step})
		if len(order.steps) > maxSelectionSteps {
			order.steps = order.steps[len(order.steps)-maxSelectionSteps:]
		}
		return pick, true
	}
	order.absolute = sel.Seq
	newer, moved := order.steps[:0], 0
	for _, s := range order.steps {
		if s.seq > sel.Seq {
			newer = append(newer, s)
			moved += s.delta
		}
	}
	order.steps = newer
	return wrapTab(pick+moved, len(tabs)), true
}

// wrapTab is index i of a strip of n tabs, wrapping both ways.
func wrapTab(i, n int) int {
	return (i%n + n) % n
}
