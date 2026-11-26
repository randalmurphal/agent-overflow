package trackertools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// What set takes. A name is matched without regard to case against the
// names a read shows: the members a read writes at its top (status,
// sprint, estimate, title, assignee, priority, labels, parent,
// description), the members it shows but no write sets (refused), and the
// ticket's fields by the names under fields, a name two fields share
// written with its ID as the read writes it. The estimate also answers to
// the name of the field its board estimates by. A value is text, a
// number, a list of them, or null to clear; a value in the form a read
// prints it is taken back (setValue).

// The names set takes for the members a read writes at its top.
const (
	setStatus      = "status"
	setSprint      = "sprint"
	setEstimate    = "estimate"
	setTitle       = "title"
	setAssignee    = "assignee"
	setPriority    = "priority"
	setLabels      = "labels"
	setParent      = "parent"
	setDescription = "description"
	setType        = "type"
	setComment     = "comment"
)

// standardField is the field a field edit sets for name, a member a read
// writes at its top, ok false for any other name.
func standardField(name string) (tracker.FieldID, bool) {
	switch name {
	case setTitle:
		return tracker.FieldTitle, true
	case setAssignee:
		return tracker.FieldAssignee, true
	case setPriority:
		return tracker.FieldPriority, true
	case setLabels:
		return tracker.FieldLabels, true
	case setParent:
		return tracker.FieldParent, true
	case setDescription:
		return tracker.FieldDescription, true
	}
	return "", false
}

// readOnlyName reports a member a read shows that no write sets.
func readOnlyName(name string) bool {
	switch name {
	case "key", "created", "updated", "resolved", "url", "links", "attachments", "comments", "history", "history_since", "fields", "more":
		return true
	}
	return false
}

// setValue is one value of set as given: null, a text or number, or a
// list of them.
type setValue struct {
	null  bool
	texts []string
	list  bool
}

// parseSetValue reads name's value raw.
func parseSetValue(name string, raw json.RawMessage) (setValue, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return setValue{null: true}, nil
	}
	var list []json.RawMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return setValue{}, setShape(name)
		}
		v := setValue{list: true}
		for _, item := range list {
			text, ok := scalarText(item)
			if !ok {
				return setValue{}, setShape(name)
			}
			v.texts = append(v.texts, text)
		}
		if len(v.texts) == 0 {
			v.null = true
		}
		return v, nil
	}
	text, ok := scalarText(raw)
	if !ok {
		return setValue{}, setShape(name)
	}
	return setValue{texts: []string{text}}, nil
}

func setShape(name string) error {
	return invalidf("set %s takes text, a number, a list of them, or null to clear.", quoteName(name))
}

// scalarText is a JSON string or number as text.
func scalarText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		return n.String(), true
	}
	return "", false
}

// one is v's one value; a list or null is refused for name.
func (v setValue) one(name string) (string, error) {
	if v.null || v.list || len(v.texts) != 1 {
		return "", invalidf("set %s takes one value.", quoteName(name))
	}
	return v.texts[0], nil
}

// quoteName is a name as a refusal quotes it, cut at refCut.
func quoteName(n string) string { return name(n) }

// fieldMatch is the ticket's field set's name names: by its name without
// regard to case, or by the name and ID a read writes for two fields that
// share a name.
func fieldMatch(fields []trackerapp.FieldView, given string) (trackerapp.FieldView, bool) {
	var hits []trackerapp.FieldView
	for _, f := range fields {
		if strings.EqualFold(f.Name, given) || strings.EqualFold(f.Name+" ("+f.ID+")", given) || strings.EqualFold(f.ID, given) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	for _, f := range hits {
		if strings.EqualFold(f.Name+" ("+f.ID+")", given) || f.ID == given {
			return f, true
		}
	}
	return trackerapp.FieldView{}, false
}

// fieldNames lists, for a refusal of a name that matches none, the names
// set takes: the read's top members and the fields', within the answer
// a refusal is, each name cut at refCut.
func fieldNames(fields []trackerapp.FieldView, top []string) string {
	names := slices.Clone(top)
	for _, f := range fields {
		if f.Editable && !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, f.Name) }) {
			names = append(names, name(f.Name))
		}
	}
	const most = 40
	if len(names) > most {
		names = append(names[:most], fmt.Sprintf("%d more", len(names)-most))
	}
	return sayList(names, "and")
}

// The estimate as a read prints it: points, or a time in minutes or
// hours. Compiled on first use: every app process links the package.
var (
	pointsForm = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(?i)\s*([0-9]+(?:\.[0-9]+)?)\s*(?:points?|pts?|sp)?\s*$`)
	})
	timeForm = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(?i)\s*(?:([0-9]+(?:\.[0-9]+)?)\s*(?:h|hours?))?\s*(?:([0-9]+(?:\.[0-9]+)?)\s*(?:m|mins?|minutes?))?\s*$`)
	})
)

// estimateValue is text as an estimate in unit ("points", or "time" in
// seconds), refused for board when it is not one.
func estimateValue(text, unit, board string) (float64, error) {
	switch unit {
	case "points":
		if m := pointsForm().FindStringSubmatch(text); m != nil {
			n, err := strconv.ParseFloat(m[1], 64)
			if err == nil && n >= 0 && !math.IsInf(n, 0) {
				return n, nil
			}
		}
		return 0, invalidf("Board %s estimates in points; give a number, such as 5 or \"5 points\".", name(board))
	case "time":
		if m := timeForm().FindStringSubmatch(text); m != nil && (m[1] != "" || m[2] != "") {
			hours, _ := strconv.ParseFloat(cmpOr(m[1], "0"), 64)
			minutes, _ := strconv.ParseFloat(cmpOr(m[2], "0"), 64)
			return math.Round(hours*3600 + minutes*60), nil
		}
		return 0, invalidf("Board %s estimates in time; give it as 45m, 2h or 1h 30m.", name(board))
	}
	return 0, invalidf("Board %s has no estimates.", name(board))
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// estimateText is n in unit as a read prints it, for an approval.
func estimateText(n float64, unit string) string {
	v := n
	rows := []trackerapp.TicketRowView{{Estimate: &v, Unit: unit}}
	return estimate(rows)
}

// stampForm is a date-time as a read prints it (stamp).
const stampForm = "2006-01-02T15:04Z"

// typedOf is v as typed text for a field of kind: a date-time a read
// printed becomes RFC 3339, and a parent a read printed with its title
// ("WEB-4 Login page") its key.
func typedOf(kind string, field tracker.FieldID, v setValue) []string {
	out := slices.Clone(v.texts)
	for i, t := range out {
		t = strings.TrimSpace(t)
		switch {
		case kind == tracker.KindDateTime.String():
			if at, err := time.Parse(stampForm, t); err == nil {
				t = at.Format(time.RFC3339)
			}
		case field == tracker.FieldParent:
			if key, _, ok := strings.Cut(t, " "); ok && len(TicketKeysIn(key)) == 1 {
				t = key
			}
		}
		out[i] = t
	}
	return out
}

// TicketKeysIn is the ticket keys text names (trackerapp.TicketKeys).
func TicketKeysIn(text string) []string { return trackerapp.TicketKeys(text) }

// userValue is v for a person field on a connection whose account is
// account: me is that account, picked by ID; any other is a name or an
// email the tracker resolves.
func userValue(v setValue, account string) trackerapp.FieldEditInput {
	if len(v.texts) == 1 && strings.EqualFold(strings.TrimSpace(v.texts[0]), "me") && account != "" {
		return trackerapp.FieldEditInput{Value: &trackerapp.FieldValueView{Kind: tracker.KindUser.String(), Choices: []trackerapp.ChoiceView{{ID: account}}}}
	}
	return trackerapp.FieldEditInput{Typed: slices.Clone(v.texts)}
}

// shown is v as an approval shows it: each value, cut, or "nothing" for
// a clear.
func (v setValue) shown() string {
	if v.null {
		return "nothing"
	}
	out := make([]string, len(v.texts))
	for i, t := range v.texts {
		out[i], _ = textClip(t, 0, 80)
	}
	return strings.Join(out, ", ")
}
