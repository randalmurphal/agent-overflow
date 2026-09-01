package keybindings

import "strings"

// Accelerator is one chord as a native key event reports it: the DOM
// `KeyboardEvent.key` spelling of the key (lowercase; " " for space) plus the
// four modifier states, with `mod` already resolved to the platform's key.
//
// It exists for the embedded browser. A chord pressed while a page's NATIVE
// view holds keyboard focus never reaches the SPA's keydown dispatcher, so the
// engine asks — synchronously, on the UI thread — whether AO binds that chord
// before letting the page have it. Matching happens here, on the Go side of
// the wire, because the answer has to be given before the event is released.
type Accelerator struct {
	Key   string `json:"key"`
	Ctrl  bool   `json:"ctrl,omitempty"`
	Meta  bool   `json:"meta,omitempty"`
	Alt   bool   `json:"alt,omitempty"`
	Shift bool   `json:"shift,omitempty"`
}

// AcceleratorSet is the bound chords of one effective keybinding list.
type AcceleratorSet map[Accelerator]struct{}

// Accelerators reduces an effective keybinding list to the chords a native
// page view must hand back to AO. It is the Go mirror of ONE half of the
// frontend chord grammar (frontend/src/lib/stores/keybindingParser.ts
// tryParseChord): tokens split on "+", the modifier vocabulary, `mod` as
// cmd on macOS and ctrl elsewhere, `space`/`esc` aliases. The frontend
// stays the owner of chord validation; a chord this mirror cannot read is
// simply not claimed, and the page keeps it.
//
// Chords with no ctrl, meta, or alt are never claimed: a bare or shift-only
// key is typing, and stealing it would break every form on every page.
func Accelerators(bindings []Keybinding, mac bool) AcceleratorSet {
	set := make(AcceleratorSet, len(bindings))
	for _, b := range bindings {
		if acc, ok := parseAccelerator(b.Key, mac); ok {
			set[acc] = struct{}{}
		}
	}
	return set
}

func parseAccelerator(chord string, mac bool) (Accelerator, bool) {
	if IsUnbound(Keybinding{Key: chord}) {
		return Accelerator{}, false
	}
	tokens := strings.Split(strings.ToLower(chord), "+")
	for i := range tokens {
		tokens[i] = strings.TrimSpace(tokens[i])
	}
	// "shift++" is shift plus the "+" key: trailing empties are that key.
	trailing := 0
	for len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1]
		trailing++
	}
	if trailing > 0 {
		tokens = append(tokens, "+")
	}
	var acc Accelerator
	mod, haveKey := false, false
	for _, token := range tokens {
		switch token {
		case "":
			return Accelerator{}, false
		case "cmd", "meta":
			acc.Meta = true
		case "ctrl", "control":
			acc.Ctrl = true
		case "shift":
			acc.Shift = true
		case "alt", "option":
			acc.Alt = true
		case "mod":
			mod = true
		default:
			if haveKey || strings.ContainsAny(token, " \t") {
				return Accelerator{}, false
			}
			haveKey = true
			switch token {
			case "space":
				acc.Key = " "
			case "esc":
				acc.Key = "escape"
			default:
				acc.Key = token
			}
		}
	}
	if mod {
		if mac {
			acc.Meta = true
		} else {
			acc.Ctrl = true
		}
	}
	if !haveKey || (!acc.Ctrl && !acc.Meta && !acc.Alt) {
		return Accelerator{}, false
	}
	return acc, true
}

// Match answers whether a pressed chord is bound, and returns the bound
// spelling — which is what the frontend is then asked to dispatch, so the
// chord it matches is exactly the one the user configured.
//
// With Shift held, a native layer may report either the shifted glyph
// ("!") or the unshifted key ("1") — WKWebView strips Shift from the
// character under Cmd, GTK and Win32 report opposite halves — and users
// write either. Both spellings are tried, on the US layout, the same table
// the frontend matcher uses for the same reason.
func (s AcceleratorSet) Match(pressed Accelerator) (Accelerator, bool) {
	if _, ok := s[pressed]; ok {
		return pressed, true
	}
	if !pressed.Shift {
		return Accelerator{}, false
	}
	if alt, ok := shiftedGlyphs[pressed.Key]; ok {
		pressed.Key = alt
	} else if alt, ok := unshiftedGlyphs[pressed.Key]; ok {
		pressed.Key = alt
	} else {
		return Accelerator{}, false
	}
	_, ok := s[pressed]
	return pressed, ok
}

// List is the set as a slice, for shipping to a host that matches remotely.
func (s AcceleratorSet) List() []Accelerator {
	out := make([]Accelerator, 0, len(s))
	for acc := range s {
		out = append(out, acc)
	}
	return out
}

// shiftedGlyphs is the US-layout unshifted → shifted table (the frontend's
// MAC_SHIFTED_GLYPH_BY_CODE, keyed by the glyph instead of the key code).
var shiftedGlyphs = map[string]string{
	"`": "~", "1": "!", "2": "@", "3": "#", "4": "$", "5": "%", "6": "^",
	"7": "&", "8": "*", "9": "(", "0": ")", "-": "_", "=": "+", "[": "{",
	"]": "}", "\\": "|", ";": ":", "'": "\"", ",": "<", ".": ">", "/": "?",
}

var unshiftedGlyphs = func() map[string]string {
	out := make(map[string]string, len(shiftedGlyphs))
	for plain, shifted := range shiftedGlyphs {
		out[shifted] = plain
	}
	return out
}()
