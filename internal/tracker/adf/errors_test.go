package adf

import (
	"fmt"
	"testing"
)

// TestPublicErrHasNoNUL proves no error leaving the package names an
// internal node type by its NUL-prefixed name.
func TestPublicErrHasNoNUL(t *testing.T) {
	err := publicErr(fmt.Errorf("adf: edit built invalid ADF: doc cannot contain %s", tMarker))
	if err.Error() != "adf: edit built invalid ADF: doc cannot contain an HTML comment" {
		t.Errorf("%q", err)
	}
}

// TestPublicWarningsHaveNoNUL proves warnings leaving the package carry
// no NUL either.
func TestPublicWarningsHaveNoNUL(t *testing.T) {
	ws := publicWarnings([]Warning{{Msg: "replaced a block that had unsupported content (" + tMarker + ")"}})
	if ws[0].Msg != "replaced a block that had unsupported content (an HTML comment)" {
		t.Errorf("%q", ws[0].Msg)
	}
}
