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
//     shows first.
//
// A section this build does not know, or one at a version newer than it
// reads, is kept byte for byte and written back unchanged by a replace,
// which may not change it (ErrSetupNewer). The revision is the SHA-256 of
// the file's bytes, empty while there is no file; a replace names the
// revision it was built from and is refused, with what is stored now, when
// the stored one differs. A file that cannot be read or does not validate
// is refused as what it is, never read as empty and never overwritten.
//
// The service holds each used project's setup in memory, by slug, loaded
// from the file on its first use and replaced by a save, both under
// setupMu; a file edited by hand while the app runs is read again only by
// the next save (whose comparison is with the file) or the next start. A
// file that cannot be read is not held, so each use reads it again.

const (
	// setupFileName is the setup's file in the project's app directory.
	setupFileName = "tracker.json"
	// setupMaxBytes bounds the file, and so the memory a setup holds.
	setupMaxBytes = 256 << 10
	// The bounds of one site's entry.
	setupMaxStatuses = 1000
	setupMaxFields   = 200
	setupMaxSections = 100
	// setupMaxID bounds a site, a status, a field or a section ID.
	setupMaxID = 255
	// The sections this build reads, and the version of each it writes.
	sectionStatusGroups = "status_groups"
	sectionTicketPins   = "ticket_pins"
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

// SetupError is a setup that does not validate, with why.
type SetupError struct {
	Reason string
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
	StatusGroups []SiteStatusGroups `json:"status_groups"`
	TicketPins   []SitePins         `json:"ticket_pins"`
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

// SetupView is a project's tracker setup as it is stored: the revision a
// replace names (empty while there is no file), the sections this build
// reads, and the names of the sections a newer version saved, which this
// build keeps as they are.
type SetupView struct {
	Revision     string             `json:"revision"`
	StatusGroups []SiteStatusGroups `json:"status_groups"`
	TicketPins   []SitePins         `json:"ticket_pins"`
	Newer        []string           `json:"newer,omitempty"`
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
// read under another version is refused (ErrStalePage). The zero value
// regroups nothing, at version "".
type StatusGroups struct {
	regroup store.MirrorRegroup
	version string
}

// setupGroups are the groups a status may be put in, by name, as the
// store numbers them.
var setupGroups = map[string]int{
	"backlog": store.MirrorGroupBacklog, "todo": store.MirrorGroupToDo, "in_progress": store.MirrorGroupInProgress,
	"done": store.MirrorGroupDone, "canceled": store.MirrorGroupCanceled, "left_out": store.MirrorGroupLeftOut,
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
// ErrSetupNewer. An empty setup removes the file, unless it keeps a
// section a newer version saved. A save that changed a site's status
// groups has every board subscription of the project on that site read
// again (ReadAgain), and every change of what is stored is told
// (Deps.SetupChanged).
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
	if err != nil {
		return SetupSave{}, err
	}
	s.setupMoved(slug, before, after)
	return save, nil
}

// replaceSetup does SetProjectSetup's work under setupMu, comparing the
// revision with the file's bytes, and returns the held setup before and
// after (before nil when none was held).
func (s *Service) replaceSetup(slug, revision string, setup Setup) (save SetupSave, before, after *heldSetup, err error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	before = s.setups[slug]
	path := s.setupPath(slug)
	stored, err := readSetupFile(path)
	if err != nil {
		return SetupSave{}, nil, nil, err
	}
	if stored.view.Revision != revision {
		s.setups[slug] = stored
		return SetupSave{Refused: setupChangedSentence, Stored: stored.view}, before, stored, nil
	}
	for _, name := range stored.view.Newer {
		if (name == sectionStatusGroups && len(setup.StatusGroups) > 0) || (name == sectionTicketPins && len(setup.TicketPins) > 0) {
			return SetupSave{}, nil, nil, fmt.Errorf("%w: section %s", ErrSetupNewer, name)
		}
	}
	data, err := encodeSetup(setup, stored.raw)
	if err != nil {
		return SetupSave{}, nil, nil, err
	}
	if data == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return SetupSave{}, nil, nil, fmt.Errorf("trackerapp: remove the tracker setup file %s: %w", path, err)
		}
	} else if err := atomicfile.Write(path, data); err != nil {
		return SetupSave{}, nil, nil, fmt.Errorf("trackerapp: write the tracker setup file %s: %w", path, err)
	}
	if after, err = heldOf(setup, stored.raw, stored.view.Newer, setupRevision(data)); err != nil {
		return SetupSave{}, nil, nil, err
	}
	s.setups[slug] = after
	return SetupSave{Saved: true, Stored: after.view}, before, after, nil
}

// setupMoved tells what a change of slug's held setup from before to
// after moves: every board subscription of the project on a site whose
// status groups changed reads again, and a change of what is stored is
// told. A setup held for the first time (before nil) was read by no
// board, so only the telling applies.
func (s *Service) setupMoved(slug string, before, after *heldSetup) {
	if before != nil && before.view.Revision == after.view.Revision {
		return
	}
	if before != nil {
		moved := map[siteKey]bool{}
		for site, g := range before.groups {
			moved[site] = after.groups[site].version != g.version
		}
		for site, g := range after.groups {
			moved[site] = moved[site] || before.groups[site].version != g.version
		}
		s.readBoardsAgain(slug, moved)
	}
	s.setupChanged()
}

// readBoardsAgain has every board pump with a handle of project slug on a
// site moved names send ReadAgain.
func (s *Service) readBoardsAgain(slug string, moved map[siteKey]bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	told := map[*boardPump]bool{}
	for _, h := range s.subs.handles {
		p := h.board
		if p == nil || told[p] || h.setup.slug != slug || !moved[h.setup.site] || p.over || p.core.DeadLocked() {
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
		return heldOf(Setup{}, nil, nil, "")
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
		if name != sectionStatusGroups && name != sectionTicketPins {
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
	return heldOf(valid, raw, newer, setupRevision(data))
}

// decodeStrict decodes data into v, refusing a field v does not have.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// encodeSetup is the file of setup and the sections raw keeps: a JSON
// object of the sections in name order, each kept section as its bytes;
// nil when it holds no section, which is no file.
func encodeSetup(setup Setup, raw map[string]json.RawMessage) ([]byte, error) {
	sections := map[string]json.RawMessage{}
	for name, section := range raw {
		sections[name] = section
	}
	add := func(name string, v any) error {
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
		return nil, invalidSetup("the setup is over %d KiB", setupMaxBytes>>10)
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

// heldOf is a valid setup as the service holds it, with the sections raw
// keeps (named by newer) and the revision of its file.
func heldOf(setup Setup, raw map[string]json.RawMessage, newer []string, revision string) (*heldSetup, error) {
	held := &heldSetup{
		view: SetupView{Revision: revision, StatusGroups: slicesx.OrEmpty(setup.StatusGroups), TicketPins: slicesx.OrEmpty(setup.TicketPins), Newer: newer},
		raw:  raw, groups: map[siteKey]StatusGroups{},
	}
	for _, site := range setup.StatusGroups {
		groups := make(map[string]int, len(site.Statuses))
		for id, name := range site.Statuses {
			groups[id] = setupGroups[name]
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
		held.groups[siteKey{site.Tracker, site.Site}] = StatusGroups{regroup: regroup, version: hex.EncodeToString(sum[:8])}
	}
	return held, nil
}

// validateSetup checks setup and returns it normalized: each section's
// sites in (tracker, site) order, without a site that sets nothing.
func validateSetup(setup Setup) (Setup, error) {
	var out Setup
	seen := map[siteKey]bool{}
	for _, site := range setup.StatusGroups {
		key, err := checkSite(site.Tracker, site.Site, sectionStatusGroups, seen)
		if err != nil {
			return Setup{}, err
		}
		if len(site.Statuses) > setupMaxStatuses {
			return Setup{}, invalidSetup("%s %s regroups %d statuses, over %d", key.tracker, key.site, len(site.Statuses), setupMaxStatuses)
		}
		for id, group := range site.Statuses {
			if err := checkSetupID("status", id); err != nil {
				return Setup{}, err
			}
			if _, ok := setupGroups[group]; !ok {
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
	slices.SortFunc(out.StatusGroups, func(a, b SiteStatusGroups) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	slices.SortFunc(out.TicketPins, func(a, b SitePins) int { return compareSites(a.Tracker, a.Site, b.Tracker, b.Site) })
	return out, nil
}

// checkSite checks a section's entry's tracker and site, once each in the
// section (seen).
func checkSite(tracker, site, section string, seen map[siteKey]bool) (siteKey, error) {
	key := siteKey{tracker, site}
	switch {
	case tracker != store.TrackerKindJira && tracker != store.TrackerKindLinear:
		return siteKey{}, invalidSetup("%s names tracker %q, which is neither jira nor linear", section, tracker)
	case strings.TrimSpace(site) == "" || len(site) > setupMaxID:
		return siteKey{}, invalidSetup("%s names a %s site that is empty or over %d bytes", section, tracker, setupMaxID)
	case seen[key]:
		return siteKey{}, invalidSetup("%s names %s %s twice", section, tracker, site)
	}
	seen[key] = true
	return key, nil
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
