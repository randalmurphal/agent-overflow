package trackerapi

import (
	"bytes"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
)

// Every way a JSON body can spell a byte of a credential form other than
// as itself reaches the caller scrubbed: each subtest spells part of a
// form one way, and the strings the scrubbed body decodes to hold no form.
// Each spelling is let through only when mayHold misses it, so each
// subtest names the rule of mayHold that catches it.
func TestEveryJSONSpellingOfAFormIsScrubbed(t *testing.T) {
	t.Parallel()
	u := func(hex string) string { return bs + "u" + hex }
	e := func(letter string) string { return bs + letter }
	replacement := string(rune(0xFFFD))
	for _, tc := range []struct {
		name   string
		cred   *Credential
		secret string // the secret as the tracker spells it in the body
	}{
		// A \u escape below U+0080 decodes to the byte it names.
		{"u-escape-lowercase-hex", Bearer(secret.New([]byte("tok-J-value-0001"))), "tok-" + u("004a") + "-value-0001"},
		{"u-escape-uppercase-hex", Bearer(secret.New([]byte("tok-J-value-0001"))), "tok-" + u("004A") + "-value-0001"},
		{"u-escape-of-a-digit", Bearer(secret.New([]byte("tok-J-value-0001"))), "tok-J-value-000" + u("0031")},
		// The one-letter escapes decode to one byte each.
		{"escape-slash", Bearer(secret.New([]byte("tok/value-0001"))), "tok" + e("/") + "value-0001"},
		{"escape-quote", Bearer(secret.New([]byte(`tok"value-0001`))), "tok" + e(`"`) + "value-0001"},
		{"escape-backslash", Bearer(secret.New([]byte("tok" + bs + "value-0001"))), "tok" + e(bs) + "value-0001"},
		{"escape-b", Basic("user-1", secret.New([]byte("tok\bvalue-0001"))), "tok" + e("b") + "value-0001"},
		{"escape-f", Basic("user-1", secret.New([]byte("tok\fvalue-0001"))), "tok" + e("f") + "value-0001"},
		{"escape-n", Basic("user-1", secret.New([]byte("tok\nvalue-0001"))), "tok" + e("n") + "value-0001"},
		{"escape-r", Basic("user-1", secret.New([]byte("tok\rvalue-0001"))), "tok" + e("r") + "value-0001"},
		{"escape-t", Basic("user-1", secret.New([]byte("tok\tvalue-0001"))), "tok" + e("t") + "value-0001"},
		// A \u escape at or above U+0080 decodes to bytes of 0x80 and
		// above: a letter, a surrogate pair, and a lone surrogate, which
		// decodes to U+FFFD.
		{"u-escape-non-ascii", Bearer(secret.New([]byte("tok-" + string(rune(0xE9)) + "-value-0001"))), "tok-" + u("00e9") + "-value-0001"},
		{"u-escape-surrogate-pair", Bearer(secret.New([]byte("tok-" + string(rune(0x1F600)) + "-value"))), "tok-" + u("d83d") + u("DE00") + "-value"},
		{"u-escape-lone-surrogate", Bearer(secret.New([]byte("tok-" + replacement + "-value-0001"))), "tok-" + u("d800") + "-value-0001"},
		// An invalid UTF-8 byte in a string decodes to U+FFFD.
		{"invalid-utf8", Bearer(secret.New([]byte("tok-" + replacement + "-value-0001"))), "tok-\xff-value-0001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.cred.check(); err != nil {
				t.Fatalf("the credential is not one a request carries: %v", err)
			}
			s := scrubberFor(tc.cred)
			defer s.clear()
			body := []byte(`{"a":"x ` + tc.secret + ` y","` + tc.secret + `":1}`)
			if bytes.Contains(body, s.form(s.n-1)) {
				t.Fatal("the body spells the secret as itself")
			}
			out, err := s.body(body)
			if err != nil {
				t.Fatal(err)
			}
			values, ok := jsonValues(out)
			if !ok {
				t.Fatalf("the scrubbed body %q does not decode", out)
			}
			var texts []string
			collectStrings(values[0], &texts)
			decoded := strings.Join(texts, " ")
			for i := range s.n {
				if strings.Contains(decoded, string(s.form(i))) {
					t.Fatalf("the decoded strings %q hold form %d", decoded, i)
				}
			}
			if strings.Count(decoded, secret.Redacted) != 2 {
				t.Errorf("the decoded strings %q, want both secrets replaced", decoded)
			}
		})
	}
}

// An escape no JSON decoder accepts is not shown to be clean, so the body
// takes the decode.
func TestMalformedEscapeTakesTheDecode(t *testing.T) {
	t.Parallel()
	s := scrubberFor(Bearer(secret.New([]byte("tok-value-0001"))))
	defer s.clear()
	for name, body := range map[string]string{
		"unknown-letter":    `{"a":"` + bs + `x"}`,
		"uppercase-U":       `{"a":"` + bs + `U0041"}`,
		"short-u":           `{"a":"` + bs + `u00"}`,
		"non-hex-u":         `{"a":"` + bs + `u00g1"}`,
		"backslash-at-end":  `{"a":"x` + bs,
		"u-at-end-of-body":  `{"a":"` + bs + `u`,
		"after-clean-ones":  `{"a":"` + bs + `n` + bs + `u00e9` + bs + `q"}`,
		"escape-is-at-zero": bs + `q`,
	} {
		t.Run(name, func(t *testing.T) {
			if !s.mayHold([]byte(body)) {
				t.Errorf("%q passed as clean", body)
			}
		})
	}
}

// An escape that decodes to no byte a form holds leaves the body as read,
// with no copy and no allocation: the common escapes of a real answer,
// against an ASCII credential without those bytes.
func TestCleanEscapesSkipTheDecode(t *testing.T) {
	s := scrubberFor(Basic("user1@example.com", secret.New([]byte("ATATT3xFfGF0-invented-token-value-0001"))))
	defer s.clear()
	held := s.heldBytes()
	for name, escape := range map[string]string{
		"quote":            bs + `"`,
		"backslash":        bs + bs,
		"newline":          bs + "n",
		"tab":              bs + "t",
		"less-than":        bs + "u003c",
		"ampersand":        bs + "u0026",
		"non-ascii-letter": bs + "u00e9",
		"uppercase-hex":    bs + "u00E9",
		"surrogate-pair":   bs + "ud83d" + bs + "ude00",
		"lone-surrogate":   bs + "udc00",
		"line-separator":   bs + "u2028",
	} {
		t.Run(name, func(t *testing.T) {
			n, decoded, ok := jsonEscape([]byte(escape))
			if !ok || decoded < 0x80 && held.has(byte(decoded)) || n > len(escape) {
				t.Fatalf("%q is not a clean escape for this credential", escape)
			}
			body := []byte(`{"a":"x ` + escape + ` y","n":1}`)
			out, err := s.body(body)
			if err != nil || &out[0] != &body[0] {
				t.Fatalf("got a copy or an error: %q %v", out, err)
			}
			if allocs := testing.AllocsPerRun(20, func() { _, _ = s.body(body) }); allocs != 0 {
				t.Errorf("%v allocations, want none", allocs)
			}
		})
	}
}
