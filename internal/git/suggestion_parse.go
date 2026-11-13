package git

import "strings"

// splitContentLines splits forge text (GitLab's from_content and
// to_content) into lines without their endings: empty text is no lines;
// otherwise one trailing line ending ends the last line and the rest
// split, so "\n" is one empty line. CRLF reads as LF.
func splitContentLines(text string) []string {
	if text == "" {
		return []string{}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
