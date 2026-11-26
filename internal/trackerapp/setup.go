package trackerapp

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// A project's tracker setup: the per-project choices of the Issues
// settings beyond its boards, in one file in the project's app directory
// (project.ConfigDir, beside its workflow profile), which the service
// reads and replaces whole. The file is the only durable copy. Its
// sections are a JSON object's keys, each carrying its own version:
//
//   - status_groups: by tracker site, the group a status counts in, a
//     tracker group or left out of counts, which the board's sections and
//     totals take (StatusGroups);
//   - ticket_pins: by tracker site, the fields and the sections a ticket
//     shows first;
//   - start_work: by tracker site and tracker project, what Start work
//     does to a ticket and where its thread runs (StartWorkDefaults);
//   - dashboards: by tracker site and board, the dashboards the project
//     keeps (BoardDashboards). It is not part of Setup: a replace keeps
//     it as held, and only its own edits write it (dashboard_setup.go), so
//     a screen that saves the setup it read never carries it.
//
// A section this build does not know, or one at a version newer than it
// reads, is kept byte for byte and written back unchanged by a replace,
// which may not change it (ErrSetupNewer); a replace's setup argument
// that names one is refused as unreadable (Setup.UnmarshalJSON). A site
// is held as its connection names it (checkSite). The revision is the SHA-256 of
// the file's bytes, empty while there is no file; a replace names the
// revision it was built from and is refused, with what is stored now, when
// the stored one differs. A file that cannot be read or does not validate
// is refused as what it is, never read as empty and never overwritten.
//
// The service holds each used project's setup in memory, by slug, under
// setupMu: the file as the service last saw it, loaded at its first use.
// A save reads the file again and holds what it saw: what it read, or
// what it wrote. A save that cannot read the file, or whose write or
// removal failed, holds nothing, and neither does a first use that cannot
// read it, so the next use reads the file again. A file edited by hand
// while the app runs is read by the next save or the next start.

const (
	// setupFileName is the setup's file in the project's app directory.
	setupFileName = "tracker.json"
	// setupMaxBytes bounds the file, and so the memory a setup holds.
	setupMaxBytes = 256 << 10
	// The bounds of one site's entry.
	setupMaxStatuses = 1000
	setupMaxFields   = 200
	setupMaxSections = 100
	// setupMaxStartWork bounds the start_work entries, and
	// setupMaxBranchTemplate a branch template.
	setupMaxStartWork      = 200
	setupMaxBranchTemplate = 100
	// setupMaxID bounds a site, a status, a field or a section ID.
	setupMaxID = 255
	// The sections this build reads, and the version of each it writes.
	sectionStatusGroups = "status_groups"
	sectionTicketPins   = "ticket_pins"
	sectionStartWork    = "start_work"
	sectionDashboards   = "dashboards"
	setupSectionVersion = 1
)

var (
	// ErrSetupUnreadable is a setup file that cannot be read, or that does
	// not validate; SetupFileError says which and why.
	ErrSetupUnreadable = errors.New("trackerapp: the project's tracker setup file cannot be read")
	// ErrSetupNewer is a replace that changes a section a newer version of
	// the app saved.
	ErrSetupNewer = errors.New("trackerapp: a newer version of the app saved this part of the tracker setup")
	// ErrInvalidSetup is a setup a replace gives that does not validate;
	// SetupError says why.
	ErrInvalidSetup = errors.New("trackerapp: invalid tracker setup")
	// ErrSetupWriteUnsure is a replace whose write or removal of the file
	// failed, after which the file may hold the change or not: a failed
	// directory sync follows the rename or the unlink. SetupWriteError
	// says which file and why.
	ErrSetupWriteUnsure = errors.New("trackerapp: the project's tracker setup file may or may not hold this change")
)

// SetupFileError is a setup file that cannot be read or does not
// validate, with its path and why.
type SetupFileError struct {
	Path, Reason string
}

func (e *SetupFileError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrSetupUnreadable, e.Path, e.Reason)
}

// Is matches ErrSetupUnreadable.
func (e *SetupFileError) Is(target error) bool { return target == ErrSetupUnreadable }

// SetupWriteError is a replace whose write or removal of the file at Path
// failed, with why. The file holds the change or the setup before it.
type SetupWriteError struct {
	Path string
	Err  error
}

func (e *SetupWriteError) Error() string {
	return fmt.Sprintf("%s: %s: %v", ErrSetupWriteUnsure, e.Path, e.Err)
}

// Is matches ErrSetupWriteUnsure.
func (e *SetupWriteError) Is(target error) bool { return target == ErrSetupWriteUnsure }

func (e *SetupWriteError) Unwrap() error { return e.Err }

// SetupError is a setup that does not validate, with why; Full when
// what does not fit is the file's bound (setupMaxBytes).
type SetupError struct {
	Reason string
	Full   bool
}

func (e *SetupError) Error() string { return ErrInvalidSetup.Error() + ": " + e.Reason }

// Is matches ErrInvalidSetup.
func (e *SetupError) Is(target error) bool { return target == ErrInvalidSetup }

func invalidSetup(format string, args ...any) error {
	return &SetupError{Reason: fmt.Sprintf(format, args...)}
}

// Setup is the part of a project's tracker setup this build reads: the
// sections a replace gives and a read answers. A site with no entry in a
// section has nothing set there.
type Setup struct {
	StatusGroups []SiteStatusGroups  `json:"status_groups"`
	TicketPins   []SitePins          `json:"ticket_pins"`
	StartWork    []StartWorkDefaults `json:"start_work"`
}

// UnmarshalJSON reads a replace's setup whole or refuses it: a JSON object
// of the sections this build reads, each entry holding only its own
// fields, as the file's reader refuses a field of a section it reads
// (decodeStrict). Null, anything but an object, a section this build does
// not read (a newer app's) and a field it does not know are refused, so a
// setup is never saved as less than it says; {} is the empty setup.
func (s *Setup) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("trackerapp: a tracker setup is a JSON object of its sections")
	}
	type fields Setup
	var out fields
	if err := decodeStrict(data, &out); err != nil {
		return fmt.Errorf("trackerapp: read the tracker setup: %w", err)
	}
	*s = Setup(out)
	return nil
}

// SiteStatusGroups is the group each status of one tracker site counts in
// for the project, by status ID: backlog, todo, in_progress, done,
// canceled, or left_out (out of counts: the status's rows show in their
// columns and sprints, are not in the backlog, and add to no total). A
// status it does not name counts in the tracker's group.
type SiteStatusGroups struct {
	Tracker  string            `json:"tracker"`
	Site     string            `json:"site"`
	Statuses map[string]string `json:"statuses"`
}

// SitePins is what a ticket of one tracker site shows first for the
// project: its pinned fields (FieldView.ID), in order, and its pinned
// sections (a rich-text field's ID, description, links or attachments),
// in order. Absent means not set, and the ticket shows its default; an
// empty list means nothing pinned. A Linear site pins no fields: its field
// list is fixed.
type SitePins struct {
	Tracker  string    `json:"tracker"`
	Site     string    `json:"site"`
	Fields   *[]string `json:"fields,omitempty"`
	Sections *[]string `json:"sections,omitempty"`
}

// StartWorkDefaults is what Start work does for the project to a ticket of
// one tracker project (Project, its key's prefix: WEB of WEB-1, a Linear
// team's key) of one tracker site, each absent field meaning its built-in
// default: Status, the status the ticket moves to (absent: it does not
// move); Assign, whether the ticket is assigned to the connection's
// account (absent: it is); Branch, the template of the branch's name,
// whose {key} is the ticket's key and {title} its title made a branch
// name (absent: {key}-{title}); Repository, the app project the thread
// runs in (absent: this project). The tracker project is the one identity
// Jira and Linear share that a ticket carries, and a status belongs to a
// project's workflow or a team, so the entry is per tracker project.
type StartWorkDefaults struct {
	Tracker    string `json:"tracker"`
	Site       string `json:"site"`
	Project    string `json:"project"`
	Status     string `json:"status,omitempty"`
	Assign     *bool  `json:"assign,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// SetupView is a project's tracker setup as it is stored: the revision a
// replace names (empty while there is no file), the setup itself, which a
// replace gives back as it is or changed, and the names of the sections a
// newer version saved, which this build keeps as they are. The setup is a
// field of its own, beside what a replace does not give, so the value a
// screen edits and saves is the value it read.
type SetupView struct {
	Revision string   `json:"revision"`
	Setup    Setup    `json:"setup"`
	Newer    []string `json:"newer,omitempty"`
}

// SetupSave is a replace's answer: whether it saved, the sentence that
// says why not when it did not (the stored setup moved since the revision
// it named), and the setup as it is stored now either way.
type SetupSave struct {
	Saved   bool      `json:"saved"`
	Refused string    `json:"refused,omitempty"`
	Stored  SetupView `json:"stored"`
}

// setupChangedSentence is a refused replace's sentence.
const setupChangedSentence = "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now"

// StatusGroups is how a project regroups the statuses of one tracker
// site, as a board read applies it, with its version: a board's view, its
// section cursors and the totals a patch names carry the version, and a
// read under another version is refused (ErrStalePage). statuses is the
// site's entry, each regrouped status's group by name, which a board view
// carries so that a client counting rows it holds counts each in the
// group the store's totals do. All three come from one site's entry of
// the setup, so they change together. The zero value regroups nothing, at
// version "".
type StatusGroups struct {
	regroup  store.MirrorRegroup
	version  string
	statuses map[string]string
}

// setupGroup is the store's number of the group named name, which a
// status may be put in, and false for any other name.
func setupGroup(name string) (int, bool) {
	switch name {
	case "backlog":
		return store.MirrorGroupBacklog, true
	case "todo":
		return store.MirrorGroupToDo, true
	case "in_progress":
		return store.MirrorGroupInProgress, true
	case "done":
		return store.MirrorGroupDone, true
	case "canceled":
		return store.MirrorGroupCanceled, true
	case "left_out":
		return store.MirrorGroupLeftOut, true
	}
	return 0, false
}

// siteKey names a tracker site: its kind and site, as a connection
// names them. A setup entry names a site, not a connection, so it
// survives a connection removed and added again.
type siteKey struct {
	tracker, site string
}

// setupKey names the status groups a board handle reads with: its
// project's slug and its connection's site.
type setupKey struct {
	slug string
	site siteKey
}

// heldSetup is a project's setup as the service holds it.
type heldSetup struct {
	view SetupView
	// raw is every section the file holds that this build does not read,
	// by name, as its bytes.
	raw map[string]json.RawMessage
	// dashboards is the dashboards section, which a replace keeps.
	dashboards []BoardDashboards
	// groups is each site's status groups.
	groups map[siteKey]StatusGroups
}

// ProjectSetup is projectID's tracker setup as it is stored.
func (s *Service) ProjectSetup(projectID string) (SetupView, error) {
	if err := s.enter(); err != nil {
		return SetupView{}, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return SetupView{}, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(slug)
	if err != nil {
		return SetupView{}, err
	}
	return held.view, nil
}

// SetProjectSetup replaces projectID's tracker setup with setup, built
// from the stored setup at revision. When the stored revision is another,
// nothing is written and the answer says so with the setup stored now.
// A setup that does not validate fails with ErrInvalidSetup, a stored
// file that cannot be read with ErrSetupUnreadable (it is not
// overwritten), and a change to a section a newer version saved with
// ErrSetupNewer, and a write or removal of the file that failed with
// ErrSetupWriteUnsure (the file may hold the change or not). An empty
// setup removes the file, unless it keeps a section a newer version
// saved. What the save saw of the file is what the service holds after
// it, refused or failed (replaceSetup), and who is told follows from that
// (setupMoved).
func (s *Service) SetProjectSetup(projectID, revision string, setup Setup) (SetupSave, error) {
	if err := s.enter(); err != nil {
		return SetupSave{}, err
	}
	defer s.leave()
	slug, err := s.projectSlug(projectID)
	if err != nil {
		return SetupSave{}, err
	}
	setup, err = validateSetup(setup)
	if err != nil {
		return SetupSave{}, err
	}
	save, before, after, err := s.replaceSetup(slug, revision, setup)
	s.setupMoved(slug, before, after)
	if err != nil {
		return SetupSave{}, err
	}
	return save, nil
}

// replaceSetup does SetProjectSetup's work under setupMu, comparing the
// revision with the file's bytes, and returns the held setup before and
// after, each nil when none is held. After is what the save last saw of
// the file, which the service holds when it returns, whatever it returns:
// the file it read, or the setup it wrote; nil when it could not read the
// file, or its write may or may not have happened, so the next use reads
// the file.
func (s *Service) replaceSetup(slug, revision string, setup Setup) (save SetupSave, before, after *heldSetup, err error) {
	moved := false
	before, after, err = s.rewriteSetup(slug, func(stored *heldSetup) (Setup, []BoardDashboards, bool) {
		moved = stored.view.Revision != revision
		return setup, stored.dashboards, !moved
	})
	switch {
	case err != nil:
		return SetupSave{}, before, after, err
	case moved:
		return SetupSave{Refused: setupChangedSentence, Stored: after.view}, before, after, nil
	}
	return SetupSave{Saved: true, Stored: after.view}, before, after, nil
}

// rewriteSetup reads slug's file again under setupMu and writes what
// change makes of what it holds: the setup and the dashboards, both
// valid, or nothing when change answers false. It returns the held setup
// before and after, each nil when none is held. After is what the write
// last saw of the file, which the service holds when it returns, whatever
// it returns: the file it read, or the setup it wrote; nil when it could
// not read the file, or its write may or may not have happened, so the
// next use reads the file.
func (s *Service) rewriteSetup(slug string, change func(stored *heldSetup) (Setup, []BoardDashboards, bool)) (before, after *heldSetup, err error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	before = s.setups[slug]
	defer func() {
		if after == nil {
			delete(s.setups, slug)
		} else {
			s.setups[slug] = after
		}
	}()
	path := s.setupPath(slug)
	if after, err = readSetupFile(path); err != nil {
		return before, nil, err
	}
	setup, dashboards, write := change(after)
	if !write {
		return before, after, nil
	}
	data, err := encodeSetup(setup, dashboards, after.raw, after.view.Newer)
	if err != nil {
		return before, after, err
	}
	written, err := heldOf(setup, dashboards, after.raw, after.view.Newer, setupRevision(data))
	if err != nil {
		return before, after, err
	}
	if data == nil {
		err = atomicfile.Remove(path)
	} else {
		err = atomicfile.Write(path, data)
	}
	if err != nil {
		return before, nil, &SetupWriteError{Path: path, Err: err}
	}
	return before, written, nil
}

// setupMoved tells what a change of slug's held setup from before to
// after moves, either nil when none is held: every board subscription of
// the project on a site whose status groups changed reads again, and a
// change of what is stored is told. A side that holds nothing is not
// known to agree with the other on any site, so every site counts as
// moved: a board of the project read with groups that may be stale, or
// failed to read them, reads again.
func (s *Service) setupMoved(slug string, before, after *heldSetup) {
	switch {
	case before == nil && after == nil:
		return
	case before != nil && after != nil && before.view.Revision == after.view.Revision:
		return
	}
	s.readBoardsAgain(slug, func(site siteKey) bool {
		return before == nil || after == nil || before.groups[site].version != after.groups[site].version
	})
	s.setupChanged()
}

// readBoardsAgain has every board pump with a handle of project slug on a
// site moved holds for send ReadAgain.
func (s *Service) readBoardsAgain(slug string, moved func(siteKey) bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	told := map[*boardPump]bool{}
	for _, h := range s.subs.handles {
		p := h.board
		if p == nil || told[p] || h.setup.slug != slug || !moved(h.setup.site) || p.over || p.core.DeadLocked() {
			continue
		}
		told[p] = true
		p.pending.add(nil, nil, true)
		s.flushBoardLocked(p)
	}
}

// boardStatusGroups is the status groups key's project gives its site,
// loading the project's setup when none is held.
func (s *Service) boardStatusGroups(key setupKey) (StatusGroups, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	held, err := s.heldSetupLocked(key.slug)
	if err != nil {
		return StatusGroups{}, err
	}
	return held.groups[key.site], nil
}

// heldSetupLocked is slug's held setup, read from its file when none is
// held; a file that cannot be read is not held. The caller holds setupMu.
func (s *Service) heldSetupLocked(slug string) (*heldSetup, error) {
	if held := s.setups[slug]; held != nil {
		return held, nil
	}
	held, err := readSetupFile(s.setupPath(slug))
	if err != nil {
		return nil, err
	}
	s.setups[slug] = held
	return held, nil
}

// projectSlug is projectID's slug, which names its app directory.
func (s *Service) projectSlug(projectID string) (string, error) {
	if projectID == "" {
		return "", fmt.Errorf("%w: a project's tracker setup names the project", ErrProjectNotFound)
	}
	p, err := s.store.GetProject(projectID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s: %w", ErrProjectNotFound, projectID, err)
	case err != nil:
		return "", fmt.Errorf("trackerapp: read project %s: %w", projectID, err)
	}
	return p.Slug, nil
}

// setupPath is the setup file of the project slug names.
func (s *Service) setupPath(slug string) string {
	return filepath.Join(project.ConfigDir(s.configDir, slug), setupFileName)
}

// readSetupFile reads and validates the setup file at path; no file is no
// setup.
func readSetupFile(path string) (*heldSetup, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return heldOf(Setup{}, nil, nil, nil, "")
	}
	if err != nil {
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	}
	data, err := io.ReadAll(io.LimitReader(f, setupMaxBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	case len(data) > setupMaxBytes:
		return nil, &SetupFileError{Path: path, Reason: fmt.Sprintf("it is over %d KiB", setupMaxBytes>>10)}
	}
	held, err := decodeSetup(data)
	if err != nil {
		return nil, &SetupFileError{Path: path, Reason: err.Error()}
	}
	return held, nil
}

// setupSection is a section's version, which a section of any version
// holds.
type setupSection struct {
	Version int `json:"version"`
}

// The sections this build reads, at the version it writes.
type (
	statusGroupsV1 struct {
		Version int                `json:"version"`
		Sites   []SiteStatusGroups `json:"sites"`
	}
	ticketPinsV1 struct {
		Version int        `json:"version"`
		Sites   []SitePins `json:"sites"`
	}
	startWorkV1 struct {
		Version  int                 `json:"version"`
		Projects []StartWorkDefaults `json:"projects"`
	}
)

// decodeSetup is the setup data holds, every section this build does not
// read kept as its bytes.
func decodeSetup(data []byte) (*heldSetup, error) {
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		return nil, fmt.Errorf("it is not a JSON object of sections: %w", err)
	}
	if sections == nil {
		return nil, errors.New("it is not a JSON object of sections")
	}
	var setup Setup
	var dashboards []BoardDashboards
	var raw map[string]json.RawMessage
	var newer []string
	keep := func(name string, section json.RawMessage) {
		if raw == nil {
			raw = map[string]json.RawMessage{}
		}
		raw[name], newer = section, append(newer, name)
	}
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		section := sections[name]
		if name != sectionStatusGroups && name != sectionTicketPins && name != sectionStartWork && name != sectionDashboards {
			keep(name, section)
			continue
		}
		var v setupSection
		if err := json.Unmarshal(section, &v); err != nil {
			return nil, fmt.Errorf("section %s: %w", name, err)
		}
		switch {
		case v.Version > setupSectionVersion:
			keep(name, section)
			continue
		case v.Version < setupSectionVersion:
			return nil, fmt.Errorf("section %s has no version this build reads (version %d)", name, v.Version)
		}
		var err error
		switch name {
		case sectionStatusGroups:
			var s statusGroupsV1
			err = decodeStrict(section, &s)
			setup.StatusGroups = s.Sites
		case sectionTicketPins:
			var s ticketPinsV1
			err = decodeStrict(section, &s)
			setup.TicketPins = s.Sites
		case sectionStartWork:
			var s startWorkV1
			err = decodeStrict(section, &s)
			setup.StartWork = s.Projects
		case sectionDashboards:
			var s dashboardsV1
			if err = decodeStrict(section, &s); err == nil {
				dashboards, err = validateDashboards(s.Boards)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("section %s: %w", name, err)
		}
	}
	valid, err := validateSetup(setup)
	if err != nil {
		var invalid *SetupError
		if errors.As(err, &invalid) {
			return nil, errors.New(invalid.Reason)
		}
		return nil, err
	}
	return heldOf(valid, dashboards, raw, newer, setupRevision(data))
}

// decodeStrict decodes data into v, refusing a field v does not have.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// encodeSetup is the file of setup, the dashboards and the sections raw
// keeps: a JSON object of the sections in name order, each kept section
// as its bytes; nil when it holds no section, which is no file. A section
// that would replace one a newer version saved (newer, SetupView.Newer)
// is ErrSetupNewer: this is the one place a section is written, so every
// section, one added later included, keeps that rule.
func encodeSetup(setup Setup, dashboards []BoardDashboards, raw map[string]json.RawMessage, newer []string) ([]byte, error) {
	sections := map[string]json.RawMessage{}
	for name, section := range raw {
		sections[name] = section
	}
	add := func(name string, v any) error {
		if slices.Contains(newer, name) {
			return fmt.Errorf("%w: section %s", ErrSetupNewer, name)
		}
		encoded, err := json.MarshalIndent(v, "  ", "  ")
		if err != nil {
			return fmt.Errorf("trackerapp: encode the tracker setup's %s: %w", name, err)
		}
		sections[name] = encoded
		return nil
	}
	if len(setup.StatusGroups) > 0 {
		if err := add(sectionStatusGroups, statusGroupsV1{Version: setupSectionVersion, Sites: setup.StatusGroups}); err != nil {
			return nil, err
		}
	}
	if len(setup.TicketPins) > 0 {
		if err := add(sectionTicketPins, ticketPinsV1{Version: setupSectionVersion, Sites: setup.TicketPins}); err != nil {
			return nil, err
		}
	}
	if len(setup.StartWork) > 0 {
		if err := add(sectionStartWork, startWorkV1{Version: setupSectionVersion, Projects: setup.StartWork}); err != nil {
			return nil, err
		}
	}
	if len(dashboards) > 0 {
		if err := add(sectionDashboards, dashboardsV1{Version: setupSectionVersion, Boards: dashboards}); err != nil {
			return nil, err
		}
	}
	if len(sections) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	out.WriteString("{\n")
	for i, name := range slices.Sorted(maps.Keys(sections)) {
		if i > 0 {
			out.WriteString(",\n")
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: encode the tracker setup's section name %q: %w", name, err)
		}
		out.WriteString("  ")
		out.Write(key)
		out.WriteString(": ")
		out.Write(sections[name])
	}
	out.WriteString("\n}\n")
	if out.Len() > setupMaxBytes {
		return nil, &SetupError{Reason: fmt.Sprintf("the setup is over %d KiB", setupMaxBytes>>10), Full: true}
	}
	return out.Bytes(), nil
}

// setupRevision is the revision of a file of data: its SHA-256, empty
// for no file.
func setupRevision(data []byte) string {
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// heldOf is a valid setup and valid dashboards as the service holds
// them, with the sections raw keeps (named by newer) and the revision of
// its file.
func heldOf(setup Setup, dashboards []BoardDashboards, raw map[string]json.RawMessage, newer []string, revision string) (*heldSetup, error) {
	held := &heldSetup{
		view: SetupView{Revision: revision, Setup: Setup{StatusGroups: slicesx.OrEmpty(setup.StatusGroups), TicketPins: slicesx.OrEmpty(setup.TicketPins), StartWork: slicesx.OrEmpty(setup.StartWork)}, Newer: newer},
		raw:  raw, dashboards: dashboards, groups: map[siteKey]StatusGroups{},
	}
	for _, site := range setup.StatusGroups {
		groups := make(map[string]int, len(site.Statuses))
		for id, name := range site.Statuses {
			groups[id], _ = setupGroup(name)
		}
		regroup, err := store.NewMirrorRegroup(groups)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the status groups of %s %s: %w", site.Tracker, site.Site, err)
		}
		// json.Marshal writes a map's keys in order, so equal groups have
		// one version.
		encoded, err := json.Marshal(site.Statuses)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: encode the status groups of %s %s: %w", site.Tracker, site.Site, err)
		}
		sum := sha256.Sum256(encoded)
		held.groups[siteKey{site.Tracker, site.Site}] = StatusGroups{regroup: regroup, version: hex.EncodeToString(sum[:8]), statuses: site.Statuses}
	}
	return held, nil
}

// validateSetup checks setup and returns it normalized: each site as its
// connection names it (checkSite), each section's sites in (tracker,
// site) order, without a site that sets nothing.
func validateSetup(setup Setup) (Setup, error) {
	var out Setup
	seen := map[siteKey]bool{}
	for _, site := range setup.StatusGroups {
		key, err := checkSite(site.Tracker, site.Site, sectionStatusGroups, seen)
		if err != nil {
			return Setup{}, err
		}
		site.Site = key.site
		if len(site.Statuses) > setupMaxStatuses {
			return Setup{}, invalidSetup("%s %s regroups %d statuses, over %d", key.tracker, key.site, len(site.Statuses), setupMaxStatuses)
		}
		for id, group := range site.Statuses {
			if err := checkSetupID("status", id); err != nil {
				return Setup{}, err
			}
			if _, ok := setupGroup(group); !ok {
				return Setup{}, invalidSetup("status %s of %s %s is put in group %q, which is none of backlog, todo, in_progress, done, canceled or left_out",
					id, key.tracker, key.site, group)
			}
		}
		if len(site.Statuses) > 0 {
			out.StatusGroups = append(out.StatusGroups, site)
		}
	}
	clear(seen)
	for _, site := range setup.TicketPins {
		key, err := checkSite(site.Tracker, site.Site, sectionTicketPins, seen)
		if err != nil {
			return Setup{}, err
		}
		site.Site = key.site
		if site.Fields != nil && key.tracker == store.TrackerKindLinear {
			return Setup{}, invalidSetup("linear %s pins fields, but a Linear ticket's fields are fixed", key.site)
		}
		if err := checkSetupList(key, "field", site.Fields, setupMaxFields); err != nil {
			return Setup{}, err
		}
		if err := checkSetupList(key, "section", site.Sections, setupMaxSections); err != nil {
			return Setup{}, err
		}
		if site.Fields != nil || site.Sections != nil {
			out.TicketPins = append(out.TicketPins, site)
		}
	}
	if len(setup.StartWork) > setupMaxStartWork {
		return Setup{}, invalidSetup("%s has %d entries, over %d", sectionStartWork, len(setup.StartWork), setupMaxStartWork)
	}
	seenProjects := map[startWorkKey]bool{}
	for _, entry := range setup.StartWork {
		valid, set, err := checkStartWork(entry, seenProjects)
		if err != nil {
			return Setup{}, err
		}
		if set {
			out.StartWork = append(out.StartWork, valid)
		}
	}
	slices.SortFunc(out.StartWork, func(a, b StartWorkDefaults) int {
		if c := compareSites(a.Tracker, a.Site, b.Tracker, b.Site); c != 0 {
			return c
		}
		return strings.Compare(a.Project, b.Project)
	})
	slices.SortFunc(out.StatusGroups, func(a, b SiteStatusGroups) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	slices.SortFunc(out.TicketPins, func(a, b SitePins) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	return out, nil
}

// checkSite checks a section's entry's tracker and site and returns the
// site as a connection of the tracker names it, once each in the section
// (seen). It keeps three things true:
//   - the site it returns is the connection's rule's (jiraSite,
//     linearSite): the value stored and the key a board handle looks up,
//     so an entry applies however its site is spelled;
//   - every bound applies to that value, so checking a checked setup
//     returns it unchanged, and whatever a save writes the reader of the
//     same build accepts;
//   - a refusal names the section, the tracker and the rule's reason,
//     never the text given for the site, which may hold a credential (a
//     pasted address's user info or token); a sentence quotes only a value
//     the rule returned, which holds none.
func checkSite(tracker, site, section string, seen map[siteKey]bool) (siteKey, error) {
	if tracker != store.TrackerKindJira && tracker != store.TrackerKindLinear {
		return siteKey{}, invalidSetup("%s names tracker %q, which is neither jira nor linear", section, tracker)
	}
	if strings.TrimSpace(site) == "" {
		return siteKey{}, invalidSetup("%s names an empty %s site", section, tracker)
	}
	named := jiraSite
	if tracker == store.TrackerKindLinear {
		named = linearSite
	}
	canonical, err := named(site)
	if err != nil {
		reason := "it is not one"
		if refused, ok := errors.AsType[*InvalidError](err); ok {
			reason = refused.Reason
		}
		return siteKey{}, invalidSetup("%s names a %s site its connection could not use: %s", section, tracker, reason)
	}
	if len(canonical) > setupMaxID {
		return siteKey{}, invalidSetup("%s names a %s site over %d bytes as its connection names it", section, tracker, setupMaxID)
	}
	key := siteKey{tracker, canonical}
	if seen[key] {
		return siteKey{}, invalidSetup("%s names %s %s twice", section, tracker, canonical)
	}
	seen[key] = true
	return key, nil
}

// startWorkKey names a start_work entry: its site and tracker project.
type startWorkKey struct {
	site    siteKey
	project string
}

// checkStartWork checks a start_work entry and returns it with its site as
// its connection names it; set is false for an entry that sets nothing.
// The tracker project is a key's prefix (TicketKeys), once per site; a
// branch template is at most setupMaxBranchTemplate bytes and holds no
// token but {key} and {title}.
func checkStartWork(entry StartWorkDefaults, seen map[startWorkKey]bool) (StartWorkDefaults, bool, error) {
	key, err := checkSite(entry.Tracker, entry.Site, sectionStartWork, map[siteKey]bool{})
	if err != nil {
		return StartWorkDefaults{}, false, err
	}
	entry.Site = key.site
	if !isTicketProject(entry.Project) {
		return StartWorkDefaults{}, false, invalidSetup("%s names tracker project %q of %s %s, which is not a key's prefix (WEB of WEB-1)",
			sectionStartWork, truncateID(entry.Project), key.tracker, key.site)
	}
	project := startWorkKey{key, entry.Project}
	if seen[project] {
		return StartWorkDefaults{}, false, invalidSetup("%s names %s of %s %s twice", sectionStartWork, entry.Project, key.tracker, key.site)
	}
	seen[project] = true
	if entry.Status != "" {
		if err := checkSetupID("status", entry.Status); err != nil {
			return StartWorkDefaults{}, false, err
		}
	}
	if entry.Repository != "" {
		if err := checkSetupID("repository", entry.Repository); err != nil {
			return StartWorkDefaults{}, false, err
		}
	}
	if len(entry.Branch) > setupMaxBranchTemplate {
		return StartWorkDefaults{}, false, invalidSetup("the branch template of %s is over %d bytes", entry.Project, setupMaxBranchTemplate)
	}
	rest := strings.NewReplacer("{key}", "", "{title}", "").Replace(entry.Branch)
	if strings.ContainsAny(rest, "{}") {
		return StartWorkDefaults{}, false, invalidSetup("the branch template of %s is %q; its only tokens are {key} and {title}", entry.Project, entry.Branch)
	}
	set := entry.Status != "" || entry.Assign != nil || entry.Branch != "" || entry.Repository != ""
	return entry, set, nil
}

// isTicketProject is whether project is a ticket key's project, as
// TicketKeys spells it: a capital letter, then one or more capitals,
// digits or underscores.
func isTicketProject(project string) bool {
	if len(project) < 2 || len(project) > setupMaxID || project[0] < 'A' || project[0] > 'Z' {
		return false
	}
	for i := 1; i < len(project); i++ {
		c := project[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// checkSetupID checks a status, field or section ID.
func checkSetupID(what, id string) error {
	if id == "" || len(id) > setupMaxID {
		return invalidSetup("a %s ID is empty or over %d bytes: %q", what, setupMaxID, truncateID(id))
	}
	return nil
}

// checkSetupList checks a pinned list of key's site: at most most IDs,
// each valid and named once.
func checkSetupList(key siteKey, what string, list *[]string, most int) error {
	if list == nil {
		return nil
	}
	if len(*list) > most {
		return invalidSetup("%s %s pins %d %ss, over %d", key.tracker, key.site, len(*list), what, most)
	}
	seen := make(map[string]bool, len(*list))
	for _, id := range *list {
		if err := checkSetupID(what, id); err != nil {
			return err
		}
		if seen[id] {
			return invalidSetup("%s %s pins %s %q twice", key.tracker, key.site, what, id)
		}
		seen[id] = true
	}
	return nil
}

// truncateID is id cut to what a refusal quotes.
func truncateID(id string) string {
	if len(id) > 64 {
		return id[:64] + "…"
	}
	return id
}

func compareSites(trackerA, siteA, trackerB, siteB string) int {
	if c := strings.Compare(trackerA, trackerB); c != 0 {
		return c
	}
	return strings.Compare(siteA, siteB)
}
