package adf

import (
	"errors"
	"strings"
)

// internalNames are the package's internal node types as messages name
// them: no user-facing text carries a NUL byte.
var internalNames = strings.NewReplacer(tMarker, "an HTML comment",
	tDetailsOpen, "<details>", tDetailsClose, "</details>", tImage, "an image",
	pendingID, "a pending reference", "\x00", "")

// publicErr rewrites an error whose text names an internal node type, and
// returns an error in the Markdown itself as a *SyntaxError.
func publicErr(err error) error {
	if se := syntaxError(err); se != nil {
		return se
	}
	if err == nil || !strings.Contains(err.Error(), "\x00") {
		return err
	}
	return errors.New(internalNames.Replace(err.Error()))
}

// publicWarnings rewrites warnings that name an internal node type.
func publicWarnings(ws []Warning) []Warning {
	for i, w := range ws {
		if strings.Contains(w.Msg, "\x00") {
			ws[i].Msg = internalNames.Replace(w.Msg)
		}
	}
	return ws
}
