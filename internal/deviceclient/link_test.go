package deviceclient

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// encodeLink renders a payload the way the minting surface does.
func encodeLink(t *testing.T, link Link) string {
	t.Helper()
	buf, err := json.Marshal(link)
	if err != nil {
		t.Fatalf("marshal the link: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func validLink() Link {
	return Link{
		Version:         LinkVersion,
		BackendID:       "backend-0001",
		BackendName:     "Studio",
		Endpoint:        "http://192.168.1.5:8317",
		Token:           "link-token",
		CertFingerprint: "sha256:abcd",
	}
}

// TestDecodeLink_EveryFormAPersonCanPaste — the settings pane shows a full
// URL, a person copying "the part after the address" gets the fragment,
// and a typed or scanned code is the bare payload. All three are things
// the surface actually produces, so all three decode to the same link.
func TestDecodeLink_EveryFormAPersonCanPaste(t *testing.T) {
	want := validLink()
	payload := encodeLink(t, want)
	for name, raw := range map[string]string{
		"full url":  "http://192.168.1.5:8317/#" + LinkFragmentPrefix + payload,
		"fragment":  "#" + LinkFragmentPrefix + payload,
		"bare":      payload,
		"padded":    "  " + payload + "  ",
		"no prefix": "#" + payload,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeLink(raw)
			if err != nil {
				t.Fatalf("DecodeLink: %v", err)
			}
			if got != want {
				t.Fatalf("DecodeLink = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDecodeLink_RefusesWhatItCannotRedeem — each of these is a link that
// would fail somewhere later and less legibly. A version this build does
// not know is refused rather than partially read, because the fields it
// would keep are exactly the ones a version bump changes the meaning of.
func TestDecodeLink_RefusesWhatItCannotRedeem(t *testing.T) {
	future := validLink()
	future.Version = LinkVersion + 1
	noToken := validLink()
	noToken.Token = ""
	noBackend := validLink()
	noBackend.BackendID = ""
	badEndpoint := validLink()
	badEndpoint.Endpoint = "ftp://192.168.1.5:8317"
	noHost := validLink()
	noHost.Endpoint = "http:///nowhere"

	for name, link := range map[string]Link{
		"a later version":  future,
		"no token":         noToken,
		"no backend id":    noBackend,
		"another scheme":   badEndpoint,
		"no host at all":   noHost,
		"an empty payload": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLink(encodeLink(t, link)); err == nil {
				t.Fatalf("DecodeLink accepted %+v", link)
			}
		})
	}
	for name, raw := range map[string]string{
		"nothing":         "",
		"an empty prefix": "#" + LinkFragmentPrefix,
		"not base64":      "not a payload!",
		"not json":        base64.RawURLEncoding.EncodeToString([]byte("plain text")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLink(raw); err == nil {
				t.Fatalf("DecodeLink accepted %q", raw)
			}
		})
	}
}

// TestDialBase_PromotionFollowsTheFingerprintAndNothingElse is the crux of
// the domainless-TLS half. The payload's endpoint is http:// because the
// SPA's share URL is; the SAME port answers TLS when a certificate is
// configured, and a fingerprint in the payload is exactly the signal that
// one is. So a pinned link is dialled over https and an unpinned one over
// the scheme it named. There is deliberately no state in between.
func TestDialBase_PromotionFollowsTheFingerprintAndNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoint    string
		fingerprint string
		want        string
	}{
		"pinned http promotes":     {"http://host:8317", "sha256:abcd", "https://host:8317"},
		"unpinned http stays":      {"http://host:8317", "", "http://host:8317"},
		"https stays https":        {"https://host:8317", "sha256:abcd", "https://host:8317"},
		"unpinned https stays too": {"https://host:8317", "", "https://host:8317"},
		"path and query dropped":   {"http://host:8317/app?x=1#f", "sha256:abcd", "https://host:8317"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := dialBase(tc.endpoint, tc.fingerprint)
			if err != nil {
				t.Fatalf("dialBase: %v", err)
			}
			if got != tc.want {
				t.Fatalf("dialBase = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWebSocketURL_RidesTheSameHalfOfTheListener — a client that dialled
// the cleartext socket after pinning the TLS half would hold an encrypted
// manifest fetch and a plaintext event stream, which is the worse half of
// both. And an empty ticket yields the bare endpoint, which is what a
// caller that mints its own per upgrade needs.
func TestWebSocketURL_RidesTheSameHalfOfTheListener(t *testing.T) {
	got, err := webSocketURL("https://host:8317", "one-shot")
	if err != nil {
		t.Fatalf("webSocketURL: %v", err)
	}
	if got != "wss://host:8317/ws?ticket=one-shot" {
		t.Fatalf("webSocketURL = %q", got)
	}
	if got, err = webSocketURL("http://host:8317", ""); err != nil || got != "ws://host:8317/ws" {
		t.Fatalf("webSocketURL without a ticket = %q (err %v), want the bare endpoint", got, err)
	}
	if _, err := webSocketURL("ftp://host", ""); err == nil {
		t.Fatal("webSocketURL accepted a scheme it cannot upgrade over")
	}
}

// TestLinkRoundTripsThroughItsOwnEncoding guards the field set: a member
// added to the payload without a member here would decode to its zero
// value silently, which for CertFingerprint means an unpinned dial.
func TestLinkRoundTripsThroughItsOwnEncoding(t *testing.T) {
	want := validLink()
	encoded := encodeLink(t, want)
	if strings.ContainsAny(encoded, "+/=") {
		t.Fatalf("the payload is not base64url without padding: %q", encoded)
	}
	got, err := DecodeLink(encoded)
	if err != nil {
		t.Fatalf("DecodeLink: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
