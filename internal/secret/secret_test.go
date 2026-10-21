package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const raw = "sk-live-0123456789abcdef"

type holder struct {
	key   *Bytes
	val   Bytes
	Key   *Bytes
	Label string
}

func TestNoFormatterRevealsTheSecret(t *testing.T) {
	s := New([]byte(raw))
	h := holder{key: s, val: *s, Key: s, Label: "x"}
	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v"} {
		outputs = append(outputs,
			fmt.Sprintf(verb, s),
			fmt.Sprintf(verb, *s),
			fmt.Sprintf(verb, h),
			fmt.Sprintf(verb, &h),
			fmt.Sprintf(verb, []any{s, h}),
			fmt.Sprintf(verb, map[string]*Bytes{"k": s}),
		)
	}
	outputs = append(outputs, fmt.Errorf("wrap: %v", s).Error(), fmt.Sprint(s), fmt.Sprintln(h))

	encoded, err := json.Marshal(map[string]any{"holder": h, "direct": s, "value": *s})
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(encoded))

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("event", "secret", s, "holder", h, "value", *s)
	textLogger := slog.New(slog.NewTextHandler(&logged, nil))
	textLogger.Info("event", "secret", s, "holder", h)
	outputs = append(outputs, logged.String())

	hexRaw := fmt.Sprintf("%x", []byte(raw))
	decimalRaw := fmt.Sprint([]byte(raw))
	for _, out := range outputs {
		for _, leak := range []string{raw, hexRaw, strings.ToUpper(hexRaw), strings.Trim(decimalRaw, "[]")} {
			if strings.Contains(out, leak) {
				t.Fatalf("formatted output leaked the secret: %q", out)
			}
		}
	}
	var decoded struct {
		Holder struct{ Key string }
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Holder.Key != Redacted {
		t.Fatalf("JSON should carry the redaction marker for the exported field: %s (%v)", encoded, err)
	}
}

func TestRevealAndZero(t *testing.T) {
	source := []byte(raw)
	s := New(source)
	source[0] = 'X'
	if string(s.Reveal()) != raw || s.Len() != len(raw) {
		t.Fatalf("New must copy: got %q", s.Reveal())
	}
	backing := s.Reveal()
	s.Zero()
	if s.Reveal() != nil || s.Len() != 0 {
		t.Fatal("Zero must drop the secret")
	}
	if !bytes.Equal(backing, make([]byte, len(raw))) {
		t.Fatal("Zero must overwrite the backing bytes")
	}

	adopted := []byte(raw)
	a := Adopt(adopted)
	a.Zero()
	if !bytes.Equal(adopted, make([]byte, len(raw))) {
		t.Fatal("Adopt must take ownership without copying")
	}

	var none *Bytes
	if none.Reveal() != nil || none.Len() != 0 {
		t.Fatal("a nil secret reveals nothing")
	}
	none.Zero()
}
