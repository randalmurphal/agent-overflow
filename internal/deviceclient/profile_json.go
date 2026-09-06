package deviceclient

import (
	"encoding/json"
	"reflect"
	"strings"
)

type sessionJSON Session

// Derive the known keys from the struct so adding a field cannot leave its old
// value in the unknown-field bag when a later write deliberately clears it.
var sessionJSONKeys = func() []string {
	typeOf := reflect.TypeFor[sessionJSON]()
	keys := make([]string, 0, typeOf.NumField())
	for i := 0; i < typeOf.NumField(); i++ {
		name := strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}()

func (s *Session) UnmarshalJSON(data []byte) error {
	var known sessionJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		for _, known := range sessionJSONKeys {
			// encoding/json accepts case-insensitive field names. Such an
			// alias is known too; keeping it could resurrect a cleared field.
			if strings.EqualFold(key, known) {
				delete(fields, key)
				break
			}
		}
	}
	*s = Session(known)
	if len(fields) != 0 {
		s.extraFields = fields
	}
	return nil
}

func (s Session) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal(sessionJSON(s))
	if err != nil || len(s.extraFields) == 0 {
		return known, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	for key, value := range s.extraFields {
		fields[key] = value
	}
	return json.Marshal(fields)
}
