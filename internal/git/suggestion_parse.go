package git

import "strings"

// splitContentLines splits forge text into lines without their endings:
// one trailing newline ends the last line rather than adding an empty
// one, and CRLF reads as LF. Empty text is no lines.
func splitContentLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}
