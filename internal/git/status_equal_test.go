package git

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Equal compares every field a status says something with, and only Seq,
// which says when it was read, is left out. A field added to GitStatus
// without a comparison fails here.
func TestGitStatusEqualComparesEveryField(t *testing.T) {
	t.Parallel()
	ignored := map[string]bool{"Seq": true}
	fields := reflect.TypeFor[GitStatus]()
	for i := range fields.NumField() {
		field := fields.Field(i)
		var changed GitStatus
		value := reflect.ValueOf(&changed).Elem().Field(i)
		switch value.Kind() {
		case reflect.Bool:
			value.SetBool(true)
		case reflect.String:
			value.SetString("x")
		case reflect.Int:
			value.SetInt(1)
		case reflect.Uint64:
			value.SetUint(1)
		case reflect.Struct:
			if field.Type != reflect.TypeFor[time.Time]() {
				t.Fatalf("field %s has type %s; teach this test to set it", field.Name, field.Type)
			}
			value.Set(reflect.ValueOf(time.Unix(1, 0)))
		default:
			t.Fatalf("field %s has kind %s; teach this test to set it", field.Name, value.Kind())
		}
		if got, want := changed.Equal(GitStatus{}), ignored[field.Name]; got != want {
			t.Errorf("Equal with only %s set = %v, want %v", field.Name, got, want)
		}
	}
}

// Seq travels as the number "seq" and is left out when unsequenced.
func TestGitStatusSeqWireField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		seq  uint64
		want string
	}{{0, ""}, {9007199254740991, `"seq":9007199254740991`}} {
		encoded, err := json.Marshal(GitStatus{Seq: tc.seq})
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(string(encoded), `"seq"`)
		if tc.want == "" && has || tc.want != "" && !strings.Contains(string(encoded), tc.want) {
			t.Errorf("GitStatus{Seq: %d} = %s, want %q", tc.seq, encoded, tc.want)
		}
	}
}
