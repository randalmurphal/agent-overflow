package cilog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
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

// The escapes the log view does not show (AnsiText.svelte): OSC and APC
// strings and SGR sequences. A search matches the text around them.
var hiddenEscapes = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b_[^\x1b]*\x1b\\|\x1b\[[0-9;]*m`)

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
		if bytes.IndexByte(text, 0x1b) >= 0 {
			text = hiddenEscapes.ReplaceAll(text, nil)
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
