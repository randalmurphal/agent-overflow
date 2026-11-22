package jira

import "time"

// timeField reads a record's timestamp field name from text into dst. An
// absent value (empty) is no problem; one that does not parse leaves dst
// zero and is recorded in probs, as an issue's field is, so the rest of
// the record stays usable and nothing the answer held is dropped
// silently.
func timeField(name, text string, dst *time.Time, probs *[]FieldProblem) {
	if text == "" {
		return
	}
	t, err := parseTime(text)
	if err != nil {
		*probs = append(*probs, FieldProblem{Field: name, Err: err})
		return
	}
	*dst = t
}

// problemIn is the decode error recorded for field in probs, or nil.
func problemIn(probs []FieldProblem, field string) error {
	for _, p := range probs {
		if p.Field == field {
			return p.Err
		}
	}
	return nil
}
