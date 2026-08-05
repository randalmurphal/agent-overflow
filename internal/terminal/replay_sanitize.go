package terminal

import "bytes"

// stripReplayableQueries removes terminal query sequences from a replay
// snapshot before it is handed to a reconnecting xterm.
//
// The replay buffer is the raw PTY output history, and interactive
// programs routinely interrogate their terminal — vim sends Primary DA,
// fish and neovim probe the kitty keyboard protocol and the background
// color, TUIs request cursor position reports. When the original bytes
// streamed live, the then-attached xterm answered once and the program
// consumed the answer. Replaying the same bytes makes the new xterm
// answer AGAIN, and this time nobody is waiting: the response lands in
// the shell's input queue and smears `1;2c`-style junk across the
// prompt. Queries are one-shot request/response traffic, not screen
// content — dropping them from replay loses nothing visible.
//
// Live output is deliberately NOT filtered: a currently-attached xterm
// must answer queries for the program to work. Only the snapshot path
// (Session.Replay / Session.ReplaySnapshot) runs this.
//
// Stripped (all one-shot request/response, none paint anything):
//
//	CSI  final c        — Primary/Secondary/Tertiary DA ("", ">", "="
//	                      param prefixes); DECSCUSR-style setters use
//	                      other finals and are untouched.
//	CSI  final n        — DSR / cursor position report requests,
//	                      including the DEC "?" variants.
//	CSI $ final p/y     — DECRQM mode queries and DECRPM replies. The
//	                      "$" intermediate is what separates them from
//	                      DECSTR (`!p`) and DECSCL (`"p`), which are
//	                      state-changing and must replay.
//	CSI > final q       — XTVERSION. DECSCUSR (`SP q`) is kept.
//	CSI ? final u       — kitty keyboard protocol query. Bare `u`
//	                      (restore cursor) and `>`/`<`/`=` forms
//	                      (push/pop/set, which change state) are kept.
//	DCS $q / +q … ST    — DECRQSS and XTGETTCAP queries, plus their
//	                      `[01]$r` / `[01]+r` reply forms. Sixel and
//	                      DECUDK payloads never match the prefix.
//	OSC 4/5/10–19 … ;?  — dynamic-color queries (terminator BEL or ST).
//	                      The final parameter must be exactly "?" and
//	                      every preceding one numeric, so color SETS and
//	                      title writes are untouched.
//
// Only 7-bit ESC-introduced forms are recognized. The 8-bit C1 singles
// (0x90 DCS, 0x9D OSC, 0x9C ST) are deliberately ignored: xterm.js
// decodes its input as UTF-8, where those bytes are continuation bytes
// of multibyte characters, so an 8-bit query could never trigger a
// re-answer — but treating them as control bytes here would corrupt
// legitimate UTF-8 text.
//
// A sequence cut off by the end of the buffer (a query still streaming
// when the snapshot was taken) is passed through unmodified; the live
// stream completes it on the client and at worst one answer fires,
// which is exactly today's behavior.
func stripReplayableQueries(data []byte) []byte {
	esc := bytes.IndexByte(data, 0x1b)
	if esc < 0 {
		return data
	}
	out := make([]byte, 0, len(data))
	pos := 0
	for {
		out = append(out, data[pos:esc]...)
		pos = esc
		if n := queryLen(data[pos:]); n > 0 {
			pos += n
		} else {
			out = append(out, data[pos])
			pos++
		}
		next := bytes.IndexByte(data[pos:], 0x1b)
		if next < 0 {
			return append(out, data[pos:]...)
		}
		esc = pos + next
	}
}

// queryLen reports the byte length of the strippable query sequence
// starting at b[0] (which must be ESC), or 0 when b does not start a
// complete strippable sequence.
func queryLen(b []byte) int {
	if len(b) < 2 || b[0] != 0x1b {
		return 0
	}
	switch b[1] {
	case '[':
		return csiQueryLen(b)
	case 'P':
		return dcsQueryLen(b)
	case ']':
		return oscQueryLen(b)
	}
	return 0
}

// csiQueryLen parses one CSI sequence (params 0x30–0x3F, intermediates
// 0x20–0x2F, final 0x40–0x7E) and reports its length when it is one of
// the query shapes documented on stripReplayableQueries.
func csiQueryLen(b []byte) int {
	i := 2
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params := b[2:i]
	interStart := i
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	inters := b[interStart:i]
	if i >= len(b) || b[i] < 0x40 || b[i] > 0x7e {
		return 0
	}
	final := b[i]
	length := i + 1

	strip := false
	switch final {
	case 'c':
		strip = len(inters) == 0 && isDAParams(params)
	case 'n':
		strip = len(inters) == 0 && isNumericParams(params, true)
	case 'p', 'y':
		strip = string(inters) == "$" && isNumericParams(params, true)
	case 'q':
		strip = len(inters) == 0 && len(params) > 0 && params[0] == '>' &&
			isNumericParams(params[1:], false)
	case 'u':
		strip = len(inters) == 0 && string(params) == "?"
	}
	if !strip {
		return 0
	}
	return length
}

// isDAParams matches the Device Attributes request parameter shapes: an
// optional ">" (DA2) or "=" (DA3) prefix followed by digits/semicolons.
func isDAParams(params []byte) bool {
	if len(params) > 0 && (params[0] == '>' || params[0] == '=') {
		params = params[1:]
	}
	return isNumericParams(params, false)
}

// isNumericParams reports whether params is digits and semicolons only,
// with an optional leading "?" when allowPrivate is set.
func isNumericParams(params []byte, allowPrivate bool) bool {
	if allowPrivate && len(params) > 0 && params[0] == '?' {
		params = params[1:]
	}
	for _, c := range params {
		if (c < '0' || c > '9') && c != ';' {
			return false
		}
	}
	return true
}

// dcsQueryLen matches DCS strings whose content opens with the
// DECRQSS/XTGETTCAP query or reply prefix (`[01]?[$+][qr]`), through
// their ESC \ terminator.
func dcsQueryLen(b []byte) int {
	i := 2
	if i < len(b) && (b[i] == '0' || b[i] == '1') {
		i++
	}
	if i >= len(b) || (b[i] != '$' && b[i] != '+') {
		return 0
	}
	i++
	if i >= len(b) || (b[i] != 'q' && b[i] != 'r') {
		return 0
	}
	st := bytes.Index(b[i:], []byte{0x1b, '\\'})
	if st < 0 {
		return 0
	}
	return i + st + 2
}

// oscColorQueryCodes are the OSC commands xterm answers when the final
// parameter is "?": 4 (palette), 5 (special colors), 10–19 (dynamic
// colors: foreground, background, cursor, …).
func isOSCColorQueryCode(code int) bool {
	return code == 4 || code == 5 || (code >= 10 && code <= 19)
}

// oscQueryLen matches OSC color-query strings — a query-capable command
// number, any further numeric parameters, and a final parameter that is
// exactly "?" — through their BEL or ESC \ terminator.
func oscQueryLen(b []byte) int {
	i := 2
	code := -1
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		if code < 0 {
			code = 0
		}
		code = code*10 + int(b[i]-'0')
		if code > 99 {
			return 0
		}
		i++
	}
	if code < 0 || !isOSCColorQueryCode(code) {
		return 0
	}
	lastParamStart := i + 1
	for i < len(b) {
		switch {
		case b[i] == 0x07:
			if string(b[lastParamStart:i]) != "?" {
				return 0
			}
			return i + 1
		case b[i] == 0x1b:
			if i+1 >= len(b) || b[i+1] != '\\' {
				return 0
			}
			if string(b[lastParamStart:i]) != "?" {
				return 0
			}
			return i + 2
		case b[i] == ';':
			lastParamStart = i + 1
		case b[i] >= '0' && b[i] <= '9', b[i] == '?':
			// Digits and "?" are the only payload bytes a query can
			// hold; anything else (a color spec, title text) is a set.
		default:
			return 0
		}
		i++
	}
	return 0
}
