package jiraprovider

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
)

// kindOf is the neutral kind of a field with schema s and whether it
// holds a list. Rich text is the jira client's own rule
// (jira.Schema.RichText), so a Markdown edit reaches the client only for
// a field it writes as Markdown. The parent is an option whose values are
// issues, picked by ID, as on Linear. A field the app shows and does not
// edit is KindReadOnly: the sprint and rank fields (MoveToSprint and Rank
// change them), other issue link pickers, teams, cascading selects, and
// any type the neutral kinds do not name.
func kindOf(s jira.Schema) (tracker.Kind, bool) {
	custom := s.Custom[strings.LastIndex(s.Custom, ":")+1:]
	switch {
	case s.RichText():
		return tracker.KindRichText, false
	case s.System == "parent":
		return tracker.KindOption, false
	case s.Custom == sprintType || s.Custom == rankType:
		return tracker.KindReadOnly, false
	}
	switch s.Type {
	case "string":
		return tracker.KindText, false
	case "number":
		return tracker.KindNumber, false
	case "date":
		return tracker.KindDate, false
	case "datetime":
		return tracker.KindDateTime, false
	case "user":
		return tracker.KindUser, false
	case "option", "priority", "resolution", "issuetype", "securitylevel", "version", "component":
		return tracker.KindOption, false
	case "array":
		switch s.Items {
		case "user":
			return tracker.KindUser, true
		case "string":
			if s.System == "labels" || custom == "labels" {
				return tracker.KindOption, true
			}
			return tracker.KindText, true
		case "option", "version", "component":
			return tracker.KindOption, true
		}
	}
	return tracker.KindReadOnly, false
}

// offeredField is a field a screen offers, with no value: a create's or a
// move's, required as its screen says and defaulted when Jira fills it
// (jira.FieldMeta.Demanded is the person's part).
func offeredField(f jira.FieldMeta) tracker.ScreenField {
	kind, list := kindOf(f.Schema)
	return tracker.ScreenField{
		Field: tracker.Field{
			ID: neutralField(f.ID), Name: f.Name, Kind: kind, List: list, Required: f.Required,
			Editable: settable(f, kind),
		},
		Defaulted: f.Required && f.HasDefault,
	}
}

// settable reports a field a screen lets the app set.
func settable(f jira.FieldMeta, kind tracker.Kind) bool {
	return kind != tracker.KindReadOnly && (len(f.Operations) == 0 || slices.Contains(f.Operations, "set"))
}

// valueOf reads field id of is as kind, adding the users it names to cat.
// The system fields the jira client decodes into Issue are read from
// there; the rest from their raw values. ok is false for a value that
// does not have its kind's shape, which the caller shows read-only.
func valueOf(is *jira.Issue, id string, kind tracker.Kind, list bool, cat *catalog) (v tracker.Value, ok bool) {
	switch id {
	case "reporter":
		if is.Reporter == nil {
			return tracker.Value{}, true
		}
		cat.user(is.Reporter)
		return tracker.UserValue(tracker.Choice{ID: is.Reporter.AccountID, Name: is.Reporter.DisplayName}), true
	case "components":
		var cs []tracker.Choice
		for _, c := range is.Components {
			cs = append(cs, tracker.Choice{ID: c.ID, Name: c.Name})
		}
		return tracker.OptionValue(cs...), true
	case "fixVersions":
		var cs []tracker.Choice
		for _, ver := range is.FixVersions {
			cs = append(cs, tracker.Choice{ID: ver.ID, Name: ver.Name})
		}
		return tracker.OptionValue(cs...), true
	case "resolution":
		if is.Resolution == nil {
			return tracker.Value{}, true
		}
		return tracker.OptionValue(tracker.Choice{ID: is.Resolution.ID, Name: is.Resolution.Name}), true
	case "duedate":
		if is.DueDate.IsZero() {
			return tracker.Value{}, true
		}
		d, err := tracker.NewDate(is.DueDate.Year, is.DueDate.Month, is.DueDate.Day)
		return tracker.DateValue(d), err == nil
	case "subtasks":
		keys := make([]string, len(is.Subtasks))
		for i, st := range is.Subtasks {
			keys[i] = st.Key
		}
		if len(keys) == 0 {
			return tracker.Value{}, true
		}
		return tracker.ReadOnlyValue(strings.Join(keys, ", ")), true
	}
	raw := is.Field(id)
	if raw.Null() {
		return tracker.Value{}, true
	}
	v, err := rawValue(raw, kind, list, cat)
	return v, err == nil
}

// rawValue reads raw as kind.
func rawValue(raw jira.FieldValue, kind tracker.Kind, list bool, cat *catalog) (tracker.Value, error) {
	switch kind {
	case tracker.KindText:
		if list {
			var ss []string
			err := json.Unmarshal(raw.JSON(), &ss)
			return tracker.TextValue(ss...), err
		}
		s, err := raw.Text()
		return tracker.TextValue(s), err
	case tracker.KindRichText:
		doc, err := raw.RichText()
		return tracker.RichTextValue(adf.ToMarkdown(doc)), err
	case tracker.KindNumber:
		n, err := raw.Number()
		return tracker.NumberValue(n), err
	case tracker.KindDate:
		d, err := raw.Date()
		if err != nil {
			return tracker.Value{}, err
		}
		day, err := tracker.NewDate(d.Year, d.Month, d.Day)
		return tracker.DateValue(day), err
	case tracker.KindDateTime:
		t, err := raw.DateTime()
		return tracker.DateTimeValue(t), err
	case tracker.KindUser:
		return userValue(raw, list, cat)
	case tracker.KindOption:
		return optionValue(raw, list)
	case tracker.KindReadOnly:
		// Its JSON as text, below.
	}
	return tracker.ReadOnlyValue(display(raw.JSON())), nil
}

func userValue(raw jira.FieldValue, list bool, cat *catalog) (tracker.Value, error) {
	var us []jira.User
	if list {
		var err error
		if us, err = raw.Users(); err != nil {
			return tracker.Value{}, err
		}
	} else {
		u, err := raw.User()
		if err != nil {
			return tracker.Value{}, err
		}
		us = []jira.User{u}
	}
	cs := make([]tracker.Choice, 0, len(us))
	for _, u := range us {
		if u.AccountID == "" {
			return tracker.Value{}, errors.New("a user without an account ID")
		}
		cat.user(&u)
		cs = append(cs, tracker.Choice{ID: u.AccountID, Name: u.DisplayName})
	}
	return tracker.UserValue(cs...), nil
}

func optionValue(raw jira.FieldValue, list bool) (tracker.Value, error) {
	if !list {
		o, err := raw.Option()
		return tracker.OptionValue(tracker.Choice{ID: o.ID, Name: o.Value}), err
	}
	var labels []string
	if json.Unmarshal(raw.JSON(), &labels) == nil {
		cs := make([]tracker.Choice, len(labels))
		for i, l := range labels {
			cs[i] = tracker.Choice{ID: l, Name: l}
		}
		return tracker.OptionValue(cs...), nil
	}
	os, err := raw.Options()
	cs := make([]tracker.Choice, len(os))
	for i, o := range os {
		cs[i] = tracker.Choice{ID: o.ID, Name: o.Value}
	}
	return tracker.OptionValue(cs...), err
}

// display is a value the app shows read-only, as a person reads it: text
// as it is, an object by its name, a list joined.
func display(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return displayAny(v)
}

func displayAny(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			if s := displayAny(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		for _, k := range []string{"displayName", "name", "value", "key", "title"} {
			if s, ok := v[k].(string); ok && s != "" {
				if child, ok := v["child"].(map[string]any); ok {
					return s + " / " + displayAny(child)
				}
				return s
			}
		}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
