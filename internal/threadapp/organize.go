package threadapp

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// One thread's whole organizing change, validated before anything is
// written.
//
// The sidebar changes a thread's title, archive flag, pin and group one
// gesture at a time, and each gesture is its own accessor. The agent thread
// tools send all four at once, and the spec's rule for them is that a
// thread is either fully updated or untouched with a reason: a patch that
// pins a thread that its group already pins must not land a rename first
// and refuse afterwards. ApplyOrganizePatch is that rule — every field of
// the RESULTING state is checked against the row as it stands before the
// first write.

// Pin tiers, as the sidebar names them. They map onto the store's two
// burners; PinNone is the absence of a pin, which the store spells as a
// NULL pinned_at rather than as a third tier.
const (
	PinFront = "front"
	PinBack  = "back"
	PinNone  = "none"
)

// ErrEmptyThreadTitle is what a patch with a blank title reports. The store
// takes any title, including an empty one, and a nameless row is a sidebar
// entry nobody can read: the refusal belongs to the policy that knows the
// title is a display name.
var ErrEmptyThreadTitle = errors.New("threadapp: a thread title cannot be empty")

// ErrOrganizeGroupAndPin is a patch that both groups a thread and gives it
// a pin of its own. A group carries the pin for its members ("one pin per
// visible row"), so the two are contradictory whatever thread they name,
// which is why this is refused on the patch rather than per row.
var ErrOrganizeGroupAndPin = errors.New("threadapp: a grouped thread carries its group's pin, so a patch sets a group or a pin, not both")

// ErrThreadHasNoProject is a group asked for on a thread that belongs to no
// project. Groups are per project, so there is nowhere to create it.
var ErrThreadHasNoProject = errors.New("threadapp: a thread with no project cannot join a group")

// OrganizePatch is what one thread's organizing call changes. A nil field
// is a field the call did not carry and this thread keeps.
type OrganizePatch struct {
	// Title replaces the display name. It is trimmed, and blank is refused.
	Title *string
	// Archived flips the archive flag in either direction.
	Archived *bool
	// Pin is PinFront, PinBack or PinNone.
	Pin *string
	// Group is a group NAME in the thread's own project, created when no
	// group of that name is there. The empty string ungroups.
	Group *string
}

// Patched reports whether the patch asks for anything at all.
func (p OrganizePatch) Patched() bool {
	return p.Title != nil || p.Archived != nil || p.Pin != nil || p.Group != nil
}

// Validate checks what can be judged without reading a thread: the shape of
// the patch itself. ApplyOrganizePatch runs it too, so a caller that skips
// it cannot write an invalid patch; callers that apply one patch to many
// threads run it once first, because a shape refusal is the same answer for
// every id.
func (p OrganizePatch) Validate() error {
	if !p.Patched() {
		return fmt.Errorf("threadapp: an organize patch changes nothing")
	}
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return ErrEmptyThreadTitle
	}
	if p.Pin != nil {
		switch *p.Pin {
		case PinFront, PinBack, PinNone:
		default:
			return fmt.Errorf("threadapp: unknown pin tier %q", *p.Pin)
		}
	}
	if p.Group != nil && strings.TrimSpace(*p.Group) != "" && p.Pin != nil && *p.Pin != PinNone {
		return ErrOrganizeGroupAndPin
	}
	return nil
}

// OrganizeResult is what one applied patch leaves behind: the row as it now
// stands, whether anything durable moved, and the two facts root needs to
// announce it correctly.
type OrganizeResult struct {
	// Thread is the current row, whether or not the patch changed it.
	Thread store.Thread
	// Changed is false when every field already held the requested value.
	// Root emits nothing then, as it does for every other thread write.
	Changed bool
	// ArchivedChanged says the archive flag itself moved, which is what
	// decides between the `listed` and `unlisted` sidebar frames and what
	// tells root to release the thread's provider session.
	ArchivedChanged bool
	// Carried are the OTHER rows a group move wrote: the discussion
	// children that travel with their root. They are not part of the
	// patched thread's own frame, and a client that never re-reads them
	// would render a stale group on each.
	Carried []store.Thread
	// CreatedGroup is the group this patch had to create, if any. A client
	// holding no row for it has nowhere to render the moved thread, so
	// root announces it exactly as the sidebar's own create does.
	CreatedGroup *store.ThreadGroup
}

// ApplyOrganizePatch validates patch against threadID's whole resulting
// state and then applies it.
//
// Order is load-bearing rather than incidental: grouping strips a thread's
// own pin and pinning refuses a grouped row, so the group move runs before
// the pin. The row is read back once at the end because the writes are
// separate accessors and only a fresh read is a coherent picture of all of
// them.
//
// Call it under the thread's mutation lock, after CheckMutable, as every
// other thread write is called.
func (s *Service) ApplyOrganizePatch(threadID string, patch OrganizePatch) (OrganizeResult, error) {
	database, err := s.database("organize thread")
	if err != nil {
		return OrganizeResult{}, err
	}
	if err := patch.Validate(); err != nil {
		return OrganizeResult{}, err
	}
	thread, err := database.GetOwnedThread(threadID)
	if err != nil {
		return OrganizeResult{}, err
	}
	plan, err := planOrganizePatch(database, thread, patch)
	if err != nil {
		return OrganizeResult{}, err
	}

	result := OrganizeResult{Thread: thread}
	if plan.title != "" {
		if err := database.UpdateTitle(threadID, plan.title); err != nil {
			return OrganizeResult{}, err
		}
		result.Changed = true
	}
	if plan.createGroup {
		// Created only here, past every refusal: a patch that names a new
		// group and is then refused for its pin must not leave an empty
		// group behind in the sidebar.
		created, err := database.CreateThreadGroup(thread.ProjectID, plan.groupName)
		if err != nil {
			return OrganizeResult{}, err
		}
		result.CreatedGroup = &created
		plan.groupID = created.ID
	}
	if plan.moveGroup {
		moved, err := database.SetThreadGroup([]string{threadID}, plan.groupID)
		if err != nil {
			return OrganizeResult{}, err
		}
		for _, row := range moved {
			if row.ID != threadID {
				result.Carried = append(result.Carried, row)
			}
		}
		result.Changed = true
	}
	if plan.pin != "" {
		changed, err := applyPinTier(database, threadID, thread, plan.pin)
		if err != nil {
			return OrganizeResult{}, err
		}
		result.Changed = result.Changed || changed
	}
	if plan.archive != nil {
		var changed bool
		if *plan.archive {
			_, changed, err = database.ArchiveThread(threadID)
		} else {
			_, changed, err = database.UnarchiveThread(threadID)
		}
		if err != nil {
			return OrganizeResult{}, err
		}
		result.Changed = result.Changed || changed
		result.ArchivedChanged = changed
	}

	if !result.Changed {
		return result, nil
	}
	current, err := database.GetThread(threadID)
	if err != nil {
		return OrganizeResult{}, err
	}
	result.Thread = current
	return result, nil
}

// organizePlan is the patch resolved against one thread: what actually has
// to be written, with every refusal already decided.
type organizePlan struct {
	// title is the new title, or empty for "leave it alone".
	title string
	// moveGroup says the group_id has to move; groupID is the destination,
	// empty for ungrouped.
	moveGroup bool
	groupID   string
	// createGroup says the destination group does not exist yet and has to
	// be created under groupName, in the thread's own project.
	createGroup bool
	groupName   string
	// pin is the tier to write, or empty for "leave it alone".
	pin string
	// archive is the archive flag to write, or nil for "leave it alone".
	archive *bool
}

// planOrganizePatch decides every write and every refusal before the first
// one runs. A field that already holds the requested value plans no write,
// which is what keeps the changed flag — and so the sidebar frame — honest.
func planOrganizePatch(database *store.Store, thread store.Thread, patch OrganizePatch) (organizePlan, error) {
	plan := organizePlan{}
	if patch.Title != nil {
		title := strings.TrimSpace(*patch.Title)
		if title == "" {
			return organizePlan{}, ErrEmptyThreadTitle
		}
		if title != thread.Title {
			plan.title = title
		}
	}
	if patch.Archived != nil && *patch.Archived != thread.Archived {
		archived := *patch.Archived
		plan.archive = &archived
	}

	// The group is resolved first because it decides whether a pin is
	// allowed at all: the destination group, not the current one, is what
	// carries the pin once this patch lands. Resolution only READS here;
	// a group that has to be created is created while the patch is
	// applied, past every refusal.
	grouped := thread.GroupID != ""
	if patch.Group != nil {
		name := strings.TrimSpace(*patch.Group)
		switch {
		case name == "":
			grouped = false
			plan.moveGroup = thread.GroupID != ""
		default:
			grouped = true
			if thread.ProjectID == "" {
				return organizePlan{}, ErrThreadHasNoProject
			}
			existing, err := findGroupInProject(database, thread.ProjectID, name)
			if err != nil {
				return organizePlan{}, err
			}
			plan.groupID, plan.groupName = existing, name
			plan.createGroup = existing == ""
			plan.moveGroup = plan.createGroup || existing != thread.GroupID
		}
	}

	if patch.Pin != nil {
		tier := *patch.Pin
		if tier != PinNone && grouped {
			// Same rule the store enforces on its own writes, refused here
			// so the rename beside it never lands first.
			return organizePlan{}, fmt.Errorf("threadapp: pin %s: %w", thread.ID, store.ErrThreadGrouped)
		}
		if tier != currentPinTier(thread) {
			plan.pin = tier
		}
	}
	return plan, nil
}

// currentPinTier names the tier a row sits on today. pinned_at is what
// makes a row pinned at all; pin_group is only which burner.
func currentPinTier(thread store.Thread) string {
	if thread.PinnedAt == nil {
		return PinNone
	}
	if thread.PinGroup != nil && *thread.PinGroup == store.PinGroupBack {
		return PinBack
	}
	return PinFront
}

// applyPinTier writes one tier, taking the same two-step the sidebar takes
// for the back burner: a row is pinned first and moved between burners
// second, because the store refuses a burner on an unpinned row.
func applyPinTier(database *store.Store, threadID string, thread store.Thread, tier string) (bool, error) {
	switch tier {
	case PinNone:
		_, changed, err := database.UnpinThread(threadID)
		return changed, err
	case PinFront:
		_, changed, err := database.PinThread(threadID)
		return changed, err
	case PinBack:
		if thread.PinnedAt == nil {
			if _, _, err := database.PinThread(threadID); err != nil {
				return false, err
			}
		}
		if _, _, err := database.SetThreadPinGroup(threadID, store.PinGroupBack); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("threadapp: unknown pin tier %q", tier)
}

// findGroupInProject returns the id of the named group in projectID, or the
// empty string when that project has none. A group is per project, so a
// group of that name in ANOTHER project is not this thread's group and is
// never reused; the answer to the same name in two projects is a second
// group in the one that needs it.
//
// The match ignores case because the name is a display name: two sidebar
// rows differing only in case are two rows a reader cannot tell apart, and
// creating the second one is not what "group these as auth work" asked for.
func findGroupInProject(database *store.Store, projectID, name string) (string, error) {
	groups, err := database.ListThreadGroups()
	if err != nil {
		return "", err
	}
	for _, group := range groups {
		if group.ProjectID == projectID && strings.EqualFold(group.Name, name) {
			return group.ID, nil
		}
	}
	return "", nil
}

// FindGroup answers the same question for a caller that resolves a group by
// name without touching a thread: thread_group names its group either by id
// or by name within one project.
func (s *Service) FindGroup(projectID, name string) (store.ThreadGroup, bool, error) {
	database, err := s.database("find thread group")
	if err != nil {
		return store.ThreadGroup{}, false, err
	}
	groups, err := database.ListThreadGroups()
	if err != nil {
		return store.ThreadGroup{}, false, err
	}
	trimmed := strings.TrimSpace(name)
	for _, group := range groups {
		if group.ProjectID == projectID && strings.EqualFold(group.Name, trimmed) {
			return group, true, nil
		}
	}
	return store.ThreadGroup{}, false, nil
}

// GroupMemberCount counts the threads a delete would ungroup, for the
// report that says so. The two listings are the sidebar's own reads: the
// project's active rows and this computer's archived ones, which together
// are every thread a person can see in a group.
func (s *Service) GroupMemberCount(group store.ThreadGroup) (int, error) {
	database, err := s.database("count thread group members")
	if err != nil {
		return 0, err
	}
	active, err := database.ListThreadsByProject(group.ProjectID)
	if err != nil {
		return 0, err
	}
	archived, err := database.ListArchivedThreads()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, thread := range active {
		if thread.GroupID == group.ID {
			count++
		}
	}
	for _, thread := range archived {
		if thread.GroupID == group.ID {
			count++
		}
	}
	return count, nil
}
