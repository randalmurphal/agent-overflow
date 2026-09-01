package browser

import (
	"strings"
	"testing"
)

// These bodies are shared by both engines, so a change here changes what a
// locator action MEANS on every platform at once.

func TestNodeReadFunctionValidatesItsVocabulary(t *testing.T) {
	for _, kind := range []string{"attribute", "innerText", "textContent", "enabled", "visible"} {
		fn, err := nodeReadFunction(kind, "href")
		if err != nil || !strings.HasPrefix(fn, "function(){") {
			t.Fatalf("%s = %q %v", kind, fn, err)
		}
	}
	if _, err := nodeReadFunction("outerHTML", ""); err == nil {
		t.Fatal("an unknown read kind must be rejected once, here, not per engine")
	}
}

func TestNodeReadAttributeBoundsItsResult(t *testing.T) {
	fn, err := nodeReadFunction("attribute", `data-x"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fn, jsonString(`data-x"`)) {
		t.Fatalf("attribute name must cross as a JSON string: %s", fn)
	}
	if !strings.Contains(fn, "slice(0,") {
		t.Fatalf("attribute read must be bounded: %s", fn)
	}
}

func TestNodeFillAndSelectRefuseTheWrongElement(t *testing.T) {
	if !strings.Contains(nodeFillFunction("x"), "element is not fillable") {
		t.Fatal("fill must refuse a non-fillable element in the page")
	}
	if !strings.Contains(nodeSelectOptionFunction(nil), "element is not a select") {
		t.Fatal("select_option must refuse a non-select element in the page")
	}
}
