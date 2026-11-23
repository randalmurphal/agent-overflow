package trackerapp

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The one place the neutral model becomes the store's mirror types and
// back. What a query filters or sorts by is a column the store's types
// fix; the rest of a record is a data blob this file owns.
//
// Each blob is a JSON object whose "v" member is its encoding's version.
// A read accepts exactly the current version and the members it defines;
// any other blob (an older or newer version, unknown members, corrupt
// bytes) is a miss: the record reads as its columns alone and the next
// read from the tracker replaces it. A blob is never an error a person
// sees. Changing what a blob holds means bumping its version.
const (
	cardBlobVersion   = 1
	userBlobVersion   = 1
	boardBlobVersion  = 1
	detailBlobVersion = 2
)

// millis is t in Unix milliseconds, 0 for the zero time.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// timeOf is millis' inverse, in UTC.
func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// encodeBlob is v as a blob.
func encodeBlob(what string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: encode the %s blob: %w", what, err)
	}
	return raw, nil
}

// decodeBlob reads raw into v, a struct whose V member it fills, and
// reports whether raw is a blob of version, wholly: no unknown or
// repeated member and nothing after the object. Blobs are written by
// encoding/json and read by encoding/json/v2, whose Unmarshal checks
// all of that without a Decoder per blob.
func decodeBlob(raw []byte, version int, v any, gotVersion *int) bool {
	if err := jsonv2.Unmarshal(raw, v, jsonv2.RejectUnknownMembers(true)); err != nil {
		return false
	}
	return *gotVersion == version
}

type choiceBlob struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// cardBlob is the part of a card no column holds.
type cardBlob struct {
	V        int          `json:"v"`
	Title    string       `json:"title,omitempty"`
	TypeName string       `json:"type_name,omitempty"`
	Priority string       `json:"priority_name,omitempty"`
	Labels   []choiceBlob `json:"labels,omitempty"`
}

// mirrorCard is c as the store holds it. Its sprints are sorted, as the
// store reads them back.
func mirrorCard(c tracker.Card) (store.MirrorCard, error) {
	blob := cardBlob{V: cardBlobVersion, Title: c.Title, TypeName: c.Type.Name, Priority: c.Priority.Name}
	for _, l := range c.Labels {
		blob.Labels = append(blob.Labels, choiceBlob(l))
	}
	data, err := encodeBlob("card", blob)
	if err != nil {
		return store.MirrorCard{}, err
	}
	sprints := idStrings(c.Sprints)
	slices.Sort(sprints)
	return store.MirrorCard{
		IssueID: string(c.ID), Key: c.Key, ProjectID: string(c.Project),
		TypeID: string(c.Type.ID), TypeLevel: c.Type.Level,
		StatusID: string(c.Status), AssigneeID: string(c.Assignee),
		PriorityID: c.Priority.ID, ParentID: string(c.Parent),
		CreatedAt: millis(c.Created), UpdatedAt: millis(c.Updated), ResolvedAt: millis(c.Resolved),
		Sprints: sprints, Data: data,
	}, nil
}

// cardOf is the card m holds. ok is false when its blob is a miss: the
// card then holds what the columns hold, and no title, type name,
// priority name or labels.
func cardOf(m store.MirrorCard) (c tracker.Card, ok bool) {
	c = tracker.Card{
		ID: tracker.IssueID(m.IssueID), Key: m.Key, Project: tracker.ProjectID(m.ProjectID),
		Type:   tracker.IssueType{ID: tracker.IssueTypeID(m.TypeID), Level: m.TypeLevel},
		Status: tracker.StatusID(m.StatusID), Assignee: tracker.UserID(m.AssigneeID),
		Priority: tracker.Choice{ID: m.PriorityID}, Parent: tracker.IssueID(m.ParentID),
		Created: timeOf(m.CreatedAt), Updated: timeOf(m.UpdatedAt), Resolved: timeOf(m.ResolvedAt),
	}
	for _, s := range m.Sprints {
		c.Sprints = append(c.Sprints, tracker.SprintID(s))
	}
	var blob cardBlob
	if !decodeBlob(m.Data, cardBlobVersion, &blob, &blob.V) {
		return c, false
	}
	c.Title, c.Type.Name, c.Priority.Name = blob.Title, blob.TypeName, blob.Priority
	for _, l := range blob.Labels {
		c.Labels = append(c.Labels, tracker.Choice(l))
	}
	return c, true
}

// mirrorRow is r as the store holds it.
func mirrorRow(r tracker.Row) (store.MirrorRow, error) {
	card, err := mirrorCard(r.Card)
	if err != nil {
		return store.MirrorRow{}, err
	}
	out := store.MirrorRow{Card: card, OrderKey: string(r.Order)}
	if r.Estimate != nil {
		e := *r.Estimate
		out.Estimate = &e
	}
	return out, nil
}

// rowOf is the row m holds; ok as cardOf reports it.
func rowOf(m store.MirrorRow) (tracker.Row, bool) {
	card, ok := cardOf(m.Card)
	r := tracker.Row{Card: card, Order: tracker.OrderKey(m.OrderKey)}
	if m.Estimate != nil {
		e := *m.Estimate
		r.Estimate = &e
	}
	return r, ok
}

// userBlob is the part of a user no column holds.
type userBlob struct {
	V         int    `json:"v"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Active    bool   `json:"active,omitempty"`
}

// mirrorCatalog is c as the store holds it. The store refuses a user or
// status named twice, so a later entry replaces an earlier one.
func mirrorCatalog(c tracker.Catalog) (store.MirrorCatalog, error) {
	var out store.MirrorCatalog
	users := map[tracker.UserID]int{}
	for _, u := range c.Users {
		data, err := encodeBlob("user", userBlob{V: userBlobVersion, Email: u.Email, AvatarURL: u.AvatarURL, Active: u.Active})
		if err != nil {
			return store.MirrorCatalog{}, err
		}
		m := store.MirrorUser{ID: string(u.ID), Name: u.Name, Data: data}
		if i, ok := users[u.ID]; ok {
			out.Users[i] = m
			continue
		}
		users[u.ID] = len(out.Users)
		out.Users = append(out.Users, m)
	}
	statuses := map[tracker.StatusID]int{}
	for _, st := range c.Statuses {
		m := store.MirrorStatus{ID: string(st.ID), Name: st.Name, Group: int(st.Group)}
		if i, ok := statuses[st.ID]; ok {
			out.Statuses[i] = m
			continue
		}
		statuses[st.ID] = len(out.Statuses)
		out.Statuses = append(out.Statuses, m)
	}
	return out, nil
}

// userOf is the user m holds; a blob that is a miss leaves the user its
// ID and name.
func userOf(m store.MirrorUser) tracker.User {
	u := tracker.User{ID: tracker.UserID(m.ID), Name: m.Name}
	var blob userBlob
	if decodeBlob(m.Data, userBlobVersion, &blob, &blob.V) {
		u.Email, u.AvatarURL, u.Active = blob.Email, blob.AvatarURL, blob.Active
	}
	return u
}

// statusOf is the status m holds.
func statusOf(m store.MirrorStatus) tracker.Status {
	return tracker.Status{ID: tracker.StatusID(m.ID), Name: m.Name, Group: tracker.StatusGroup(m.Group)}
}

// catalogOf is the users and statuses m holds. The neutral catalog has no
// projects; the mirror stores none.
func catalogOf(m store.MirrorCatalog) tracker.Catalog {
	var c tracker.Catalog
	for _, u := range m.Users {
		c.Users = append(c.Users, userOf(u))
	}
	for _, st := range m.Statuses {
		c.Statuses = append(c.Statuses, statusOf(st))
	}
	return c
}

type estimateChoiceBlob struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}

// boardBlob is the part of a board no column holds.
type boardBlob struct {
	V       int                  `json:"v"`
	Choices []estimateChoiceBlob `json:"choices,omitempty"`
}

// mirrorStructure is st, read from read, as the store holds it.
func mirrorStructure(st tracker.Structure, read store.MirrorRead) (store.MirrorStructure, error) {
	blob := boardBlob{V: boardBlobVersion}
	for _, ch := range st.Estimate.Choices {
		blob.Choices = append(blob.Choices, estimateChoiceBlob(ch))
	}
	data, err := encodeBlob("board", blob)
	if err != nil {
		return store.MirrorStructure{}, err
	}
	catalog, err := mirrorCatalog(st.Catalog)
	if err != nil {
		return store.MirrorStructure{}, err
	}
	out := store.MirrorStructure{
		Board: store.MirrorBoard{
			ID: string(st.Board.ID), ProjectID: string(st.Board.Project), Name: st.Board.Name,
			PlansSprints: st.Board.Sprints, EstimateUnit: int(st.Estimate.Unit), EstimateField: st.Estimate.Field, Data: data,
		},
		Catalog: catalog,
		ReadAt:  read,
		// A structure read lists everything it reads to the end or fails:
		// Jira's sprint listing has no limit (jira.SprintList.Complete is
		// false only for a limited one) and Linear's cycles and users are
		// paged to the end. So it is complete, and what the board's rows
		// name that it leaves out is left out on purpose (a sprint closed
		// before the window, a status the site does not list).
		Complete: true,
	}
	for _, col := range st.Columns {
		out.Columns = append(out.Columns, store.MirrorColumn{Name: col.Name, Statuses: idStrings(col.Statuses)})
	}
	for _, sp := range st.Sprints {
		out.Sprints = append(out.Sprints, store.MirrorSprint{
			ID: string(sp.ID), OriginBoardID: string(sp.Board), Name: sp.Name, Goal: sp.Goal, State: int(sp.State),
			StartAt: millis(sp.Start), EndAt: millis(sp.End), CompletedAt: millis(sp.Completed),
		})
	}
	return out, nil
}

// structureOf is the structure m holds with catalog, which the store
// reads apart from it. A board blob that is a miss leaves the estimate
// its unit and field and no choices.
func structureOf(m store.MirrorStructure, catalog tracker.Catalog) tracker.Structure {
	st := tracker.Structure{
		Board: tracker.Board{
			ID: tracker.BoardID(m.Board.ID), Project: tracker.ProjectID(m.Board.ProjectID),
			Name: m.Board.Name, Sprints: m.Board.PlansSprints,
		},
		Estimate: tracker.Estimate{Unit: tracker.EstimateUnit(m.Board.EstimateUnit), Field: m.Board.EstimateField},
		Catalog:  catalog,
	}
	var blob boardBlob
	if decodeBlob(m.Board.Data, boardBlobVersion, &blob, &blob.V) {
		for _, ch := range blob.Choices {
			st.Estimate.Choices = append(st.Estimate.Choices, tracker.EstimateChoice(ch))
		}
	}
	for _, col := range m.Columns {
		c := tracker.Column{Name: col.Name}
		for _, s := range col.Statuses {
			c.Statuses = append(c.Statuses, tracker.StatusID(s))
		}
		st.Columns = append(st.Columns, c)
	}
	for _, sp := range m.Sprints {
		st.Sprints = append(st.Sprints, tracker.Sprint{
			ID: tracker.SprintID(sp.ID), Board: tracker.BoardID(sp.OriginBoardID), Name: sp.Name, Goal: sp.Goal,
			State: tracker.SprintState(sp.State), Start: timeOf(sp.StartAt), End: timeOf(sp.EndAt), Completed: timeOf(sp.CompletedAt),
		})
	}
	return st
}
