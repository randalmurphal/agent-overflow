package cilog

import "io"

// escapeMax bounds an escape sequence held across writes: a run of
// parameters longer than this is not one the log view renders, and passes
// through as text.
const escapeMax = 64

// PlainWriter passes a log's text through with its escape sequences (ESC
// [ parameters letter) removed: the text a reader copies or sends to a
// chat, as the log view shows it without color. A sequence split across
// writes is held until it completes; Close writes what is held.
type PlainWriter struct {
	w    io.Writer
	held []byte
	out  []byte
}

func NewPlainWriter(w io.Writer) *PlainWriter {
	return &PlainWriter{w: w}
}

func (p *PlainWriter) Write(b []byte) (int, error) {
	data := b
	if len(p.held) > 0 {
		data = append(p.held, b...)
		p.held = nil
	}
	p.out = p.out[:0]
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			p.out = append(p.out, data[i])
			i++
			continue
		}
		j := i + 1
		if j < len(data) && data[j] == '[' {
			j++
			for j < len(data) && j-i < escapeMax && (isDigit(data[j]) || data[j] == ';' || data[j] == '?') {
				j++
			}
		}
		switch {
		case j == len(data) && j-i < escapeMax:
			// The sequence may end in the next write.
			p.held = append([]byte(nil), data[i:]...)
			i = j
		case j > i+1 && j < len(data) && isLetter(data[j]):
			i = j + 1
		default:
			p.out = append(p.out, data[i])
			i++
		}
	}
	if _, err := p.w.Write(p.out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close writes an escape sequence the text ended inside, as text.
func (p *PlainWriter) Close() error {
	if len(p.held) == 0 {
		return nil
	}
	_, err := p.w.Write(p.held)
	p.held = nil
	return err
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
