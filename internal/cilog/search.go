package cilog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/stringsx"
)

// SearchHit is a line of a log holding Count matches of a search.
type SearchHit struct {
	Line  int `json:"line"`
	Count int `json:"count"`
}

// Search is the answer of a search over one published version of a log:
// the lines holding matches, in line order, and their total. Capped is
// true when Matches stopped at the search's limit.
type Search struct {
	Version    uint64      `json:"version"`
	TotalLines int         `json:"totalLines"`
	Hits       []SearchHit `json:"hits"`
	Matches    int         `json:"matches"`
	Capped     bool        `json:"capped"`
}

// searchCheckLines is how many lines a search reads between checks of its
// context.
const searchCheckLines = 1024

// What the log view does not show of a line (frontend utils/ansiText.ts):
// OSC and APC strings, SGR sequences, and NUL, which the HTML parser drops.
// A search matches the text around them. stripHidden is the view's one
// left-to-right scan: at each ESC the next byte names the only form that
// can start there, read forward to its end or to the first byte it cannot
// hold; a form that does not end leaves its ESC as text and the scan goes
// on after it. Every byte is read at most twice. The vectors in
// frontend/src/test/fixtures/ansiShownVectors.json hold both sides to it.

// hiddenEnd is the end (exclusive) of the OSC, APC or SGR starting at the
// ESC at at, else -1.
func hiddenEnd(line []byte, at int) int {
	if at+1 >= len(line) {
		return -1
	}
	i := at + 2
	switch line[at+1] {
	case ']':
		for ; i < len(line); i++ {
			switch line[i] {
			case 0x07:
				return i + 1
			case 0x1b:
				if i+1 < len(line) && line[i+1] == '\\' {
					return i + 2
				}
				return -1
			case '\n':
				return -1
			}
		}
	case '_':
		for ; i < len(line); i++ {
			switch line[i] {
			case 0x1b:
				if i+1 < len(line) && line[i+1] == '\\' {
					return i + 2
				}
				return -1
			case '\n':
				return -1
			}
		}
	case '[':
		for ; i < len(line); i++ {
			c := line[i]
			if (c >= '0' && c <= '9') || c == ';' {
				continue
			}
			if c == 'm' {
				return i + 1
			}
			return -1
		}
	}
	return -1
}

// stripHidden appends what the view shows of line to dst.
func stripHidden(dst, line []byte) []byte {
	for i := 0; i < len(line); {
		switch line[i] {
		case 0:
			i++
			continue
		case 0x1b:
			if end := hiddenEnd(line, i); end >= 0 {
				i = end
				continue
			}
		}
		dst = append(dst, line[i])
		i++
	}
	return dst
}

// Search counts the matches of query in key's published log as the log
// view shows each line: its first lineMax bytes, without the escapes the
// view hides. The rule is the find bar's (stringsx.FoldForFind): the query
// trimmed, matched literally with each code point folded to its simple
// lower case, matches counted left to right without overlap. The search stops at limit matches and when
// ctx ends; it holds one line at a time.
func (s *Store) Search(ctx context.Context, key Key, query string, limit, lineMax int) (Search, error) {
	needle := stringsx.FoldForFind(strings.TrimSpace(query))
	if needle == "" || limit <= 0 || lineMax <= 0 {
		return Search{}, fmt.Errorf("search CI log: empty query or bound")
	}
	_, v, file, err := s.open(key)
	if err != nil {
		return Search{}, err
	}
	defer s.closeRead(v, file)
	out := Search{Version: v.number, TotalLines: v.idx.lines, Hits: []SearchHit{}}
	if v.idx.lines == 0 {
		return out, nil
	}
	br, _, err := lineReader{file: file, idx: v.idx}.at(0)
	if err != nil {
		return Search{}, fmt.Errorf("search CI log: %w", err)
	}
	line := make([]byte, 0, min(lineMax, readBlock))
	var stripped []byte
	for i := 0; i < v.idx.lines; i++ {
		if i%searchCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return Search{}, err
			}
		}
		line, err = readShownLine(br, line[:0], lineMax)
		if err != nil {
			return Search{}, fmt.Errorf("search CI log: %w", err)
		}
		text := line
		if bytes.IndexByte(text, 0x1b) >= 0 || bytes.IndexByte(text, 0) >= 0 {
			stripped = stripHidden(stripped[:0], text)
			text = stripped
		}
		shown := string(text)
		if !utf8.Valid(text) {
			// The view receives each invalid byte as U+FFFD (encoding/json).
			shown = string([]rune(shown))
		}
		count := strings.Count(stringsx.FoldForFind(shown), needle)
		if count == 0 {
			continue
		}
		if room := limit - out.Matches; count >= room {
			out.Hits = append(out.Hits, SearchHit{Line: i, Count: room})
			out.Matches = limit
			out.Capped = true
			return out, nil
		}
		out.Hits = append(out.Hits, SearchHit{Line: i, Count: count})
		out.Matches += count
	}
	return out, nil
}

// readShownLine appends the first lineMax bytes of the next line, without
// its newline and cut on a rune boundary, to dst, and reads past the rest.
func readShownLine(br *bufio.Reader, dst []byte, lineMax int) ([]byte, error) {
	read := 0
	for {
		chunk, err := br.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		if !full && err != nil && (!errors.Is(err, io.EOF) || len(chunk) == 0) {
			return dst, unexpectedEnd(err)
		}
		read += len(chunk)
		if !full && len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			chunk = chunk[:len(chunk)-1]
		}
		if room := lineMax - len(dst); room > 0 {
			dst = append(dst, chunk[:min(room, len(chunk))]...)
		}
		if !full {
			break
		}
	}
	if read > lineMax {
		dst = trimPartialRune(dst)
	}
	return dst, nil
}
