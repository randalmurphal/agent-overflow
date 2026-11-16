package adf

import "strings"

// markerWords name the containers Markdown writes with comment markers or
// [TEXT]: macros, custom panels, column layouts and lozenges.
var markerWords = []string{"<!--", "macro", "panel", "layout", "column", "status",
	"lozenge", "date"}

// LossKinds reports which ways of writing a container the Flattens
// warnings in ws name, so a refusal can say how to keep each: details for
// an expand, written <details><summary>...</summary> ... </details>, and
// markers for what is written with comment markers or [TEXT].
func LossKinds(ws []Warning) (markers, details bool) {
	for _, w := range ws {
		if !w.Flattens {
			continue
		}
		expand := strings.Contains(w.Msg, "expand") || strings.Contains(w.Msg, "<details>") ||
			strings.Contains(w.Msg, "</details>")
		details = details || expand
		if !expand {
			markers = true
			continue
		}
		for _, word := range markerWords {
			if strings.Contains(w.Msg, word) {
				markers = true
				break
			}
		}
	}
	return markers, details
}
