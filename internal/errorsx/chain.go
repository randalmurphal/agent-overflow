package errorsx

import (
	"errors"
	"strings"
)

// Chain bounds keep a pathological error from bloating a report.
const (
	chainMaxLayers   = 32
	chainMaxLayerLen = 2000
)

// Chain splits err into its wrap layers, outermost first: the context each
// `fmt.Errorf("context: %w", inner)` added, then the innermost cause. A layer
// whose text does not end with its cause's text (a custom Error method) is
// kept whole and ends the walk. The branches of an errors.Join follow one
// another.
func Chain(err error) []string {
	var layers []string
	appendChain(&layers, err)
	return layers
}

func appendChain(layers *[]string, err error) {
	for err != nil && len(*layers) < chainMaxLayers {
		msg := err.Error()
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, branch := range joined.Unwrap() {
				appendChain(layers, branch)
			}
			return
		}
		inner := errors.Unwrap(err)
		if inner == nil {
			addLayer(layers, msg)
			return
		}
		innerMsg := inner.Error()
		if !strings.HasSuffix(msg, innerMsg) {
			addLayer(layers, msg)
			return
		}
		if context := strings.TrimSuffix(strings.TrimSuffix(msg, innerMsg), ": "); context != "" {
			addLayer(layers, context)
		}
		err = inner
	}
}

func addLayer(layers *[]string, layer string) {
	if len(layer) > chainMaxLayerLen {
		layer = strings.ToValidUTF8(layer[:chainMaxLayerLen], "") + "…"
	}
	*layers = append(*layers, layer)
}
