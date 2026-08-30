package compare

import (
	"crypto/sha256"
	"encoding/hex"
)

func legIndex(l Leg) int {
	if l == LegB {
		return 1
	}
	return 0
}

func cloneMetrics(in map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func semanticDigest(text string) string {
	s := sha256.Sum256([]byte(text))
	return hex.EncodeToString(s[:])
}

func CompareText(a, b string) TextGate {
	gate := TextGate{Equal: a == b, DigestA: semanticDigest(a), DigestB: semanticDigest(b)}
	if gate.Equal {
		return gate
	}
	gate.ComparedPairs = 1
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	offset := 0
	for offset < limit && a[offset] == b[offset] {
		offset++
	}
	gate.FirstDifference = &TextDifference{OffsetA: offset, OffsetB: offset, Line: 1, Column: 1, A: snippet(a, offset), B: snippet(b, offset)}
	for i := 0; i < offset; i++ {
		if a[i] == '\n' {
			gate.FirstDifference.Line++
			gate.FirstDifference.Column = 1
		} else {
			gate.FirstDifference.Column++
		}
	}
	return gate
}

func snippet(s string, at int) string {
	if at >= len(s) {
		return "<eof>"
	}
	end := at + 32
	if end > len(s) {
		end = len(s)
	}
	return s[at:end]
}
