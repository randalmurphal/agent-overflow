package trackerapi

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// jsonValues decodes body with encoding/json as one or more JSON values,
// numbers kept as written; ok is false when it is not JSON throughout.
// The tests read a scrubbed body through it: a decoder the scrub does not
// use.
func jsonValues(body []byte) (values []any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return values, len(values) > 0
		}
		if err != nil {
			return nil, false
		}
		values = append(values, v)
	}
}

// collectStrings appends every string in a decoded JSON value, object
// keys included.
func collectStrings(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		for k, e := range v {
			*out = append(*out, k)
			collectStrings(e, out)
		}
	case []any:
		for _, e := range v {
			collectStrings(e, out)
		}
	}
}

// streamStrings is every string a token read of body yields, repeated
// names included: what a type's own UnmarshalJSONFrom is handed.
func streamStrings(t *testing.T, body []byte) []string {
	t.Helper()
	dec := jsontext.NewDecoder(bytes.NewReader(body), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	var out []string
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("the body %q does not read as a token stream: %v", body, err)
		}
		if tok.Kind() == '"' {
			out = append(out, tok.String())
		}
	}
}

// firstOfRepeated decodes its own member "v" from the first one a body
// holds, as a type that reads the token stream may.
type firstOfRepeated struct{ v string }

func (f *firstOfRepeated) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		name := tok.String()
		var value string
		if err := jsonv2.UnmarshalDecode(dec, &value); err != nil {
			return err
		}
		if name == "v" && f.v == "" {
			f.v = value
		}
	}
	_, err := dec.ReadToken()
	return err
}

// A name that repeats is scrubbed at every member: a decode into a map
// keeps only the last, and a decoder that reads the stream sees them all.
func TestRepeatedNameIsScrubbedAtEveryMember(t *testing.T) {
	t.Parallel()
	const token = "tok-J-value-0001"
	spelled := "tok-" + bs + "u004a-value-0001"
	for name, body := range map[string]string{
		"first value":          `{"v":"` + spelled + `","v":"x"}`,
		"last value":           `{"v":"x","v":"` + spelled + `"}`,
		"both values":          `{"v":"` + spelled + `","v":"` + spelled + `"}`,
		"repeated name itself": `{"` + spelled + `":1,"` + spelled + `":2}`,
		"nested":               `[{"a":{"v":"` + spelled + `","v":"x"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := scrubberFor(Key(secret.New([]byte(token))))
			defer s.clear()
			if bytes.Contains([]byte(body), []byte(token)) {
				t.Fatal("the body spells the token as itself")
			}
			out, err := s.body([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range streamStrings(t, out) {
				if strings.Contains(text, token) {
					t.Fatalf("a token read of %q yields the credential", out)
				}
			}
			if name == "repeated name itself" || name == "nested" {
				return
			}
			var first firstOfRepeated
			if err := jsonv2.Unmarshal(out, &first, jsontext.AllowDuplicateNames(true)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(first.v, token) {
				t.Fatalf("a type that reads the stream was handed the credential from %q", out)
			}
		})
	}
}

// The walk keeps what it does not replace: numbers as written, the order
// of members, repeated names, and several values in one body.
func TestScrubKeepsTheRestOfTheBody(t *testing.T) {
	t.Parallel()
	const token = "tok-J-value-0001"
	s := scrubberFor(Key(secret.New([]byte(token))))
	defer s.clear()
	body := `{"z":1.50,"a":[12345678901234567890,true,null],"v":"` + token + `","v":"kept"} {"n":-0.0e10}`
	out, err := s.body([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z":1.50,"a":[12345678901234567890,true,null],"v":"` + secret.Redacted + `","v":"kept"}` + "\n" + `{"n":-0.0e10}`
	if string(out) != want {
		t.Fatalf("scrubbed body\n got %s\nwant %s", out, want)
	}
}

// Whatever encoding/json decodes, the walk reads, and it visits every
// string that decode yields: a body the walk refused would be scrubbed as
// bytes only, and a string it missed would not be scrubbed at all.
func FuzzWalkStringsSeesWhatADecodeSees(f *testing.F) {
	for _, seed := range []string{
		`{"a":"b"}`, `[]`, `"s"`, `1`, `null`, ` {"a":{"b":["c",{"d":"e"}]}} `,
		`{"a":"x","a":"y"}`, `{"a":"` + bs + `u004a` + bs + `n` + bs + `/"}`,
		`{"a":"` + bs + `ud83d` + bs + `ude00"}`, `{"a":"` + bs + `ud800"}`,
		"{\"a\":\"\xff\xfe\"}", "{\"\xff\":1}", `{"a":1}{"b":2}`, `{"a":1} [2] "3"`,
		`{"a":1e400}`, `{"a":-0}`, `{"":""}`, `[[[[[[[[[["deep"]]]]]]]]]]`,
		`{"a":"` + bs + bs + `u004a"}`, "\t\r\n{\"a\":\"b\"}\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		values, ok := jsonValues(body)
		if !ok {
			return
		}
		var want []string
		for _, v := range values {
			collectStrings(v, &want)
		}
		var got []string
		if !walkStrings(body, nil, func(v string) string {
			got = append(got, v)
			return v
		}) {
			t.Fatalf("encoding/json decodes %q and the walk does not read it", body)
		}
		for _, text := range want {
			if !slices.Contains(got, text) {
				t.Fatalf("encoding/json yields %q from %q and the walk did not visit it (visited %q)", text, body, got)
			}
		}
	})
}
