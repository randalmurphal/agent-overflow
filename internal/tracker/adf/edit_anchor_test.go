package adf

import (
	"slices"
	"testing"
)

func TestEmptyAnchorsExhaustive(t *testing.T) {
	for size, combinations := 0, 1; size <= 8; size, combinations = size+1, combinations*3 {
		for pattern := range combinations {
			n := normalized{blocks: make([][]*Node, size), broken: make([]bool, size)}
			for i, bits := 0, pattern; i < size; i, bits = i+1, bits/3 {
				switch bits % 3 {
				case 1:
					n.blocks[i] = []*Node{{Type: "text", Text: "x"}}
				case 2:
					n.broken[i] = true
				}
			}
			want, content := anchorOracle(n)
			got, hasContent := anchorEmpty(n)
			if !slices.Equal(got, want) || hasContent != content {
				t.Fatalf("size %d pattern %d: %v, %v; want %v, %v",
					size, pattern, got, hasContent, want, content)
			}
		}
	}
}

func anchorOracle(n normalized) ([]int, bool) {
	out := make([]int, len(n.blocks))
	content := false
	for i := range out {
		out[i] = -1
		if !n.empty(i) {
			content = true
			continue
		}
		for j := range n.blocks {
			if n.empty(j) {
				continue
			}
			if j < i || out[i] < 0 {
				out[i] = j
			}
			if j > i {
				break
			}
		}
	}
	return out, content
}

func BenchmarkEmptyAnchors(b *testing.B) {
	n := normalized{blocks: make([][]*Node, 2048), broken: make([]bool, 2048)}
	n.blocks[len(n.blocks)-1] = []*Node{{Type: "text", Text: "last"}}
	b.ReportAllocs()
	for b.Loop() {
		anchorEmpty(n)
	}
}
