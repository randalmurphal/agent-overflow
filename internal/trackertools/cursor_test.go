package trackertools

import (
	"strings"
	"testing"
)

// A next carrying the largest page token a tracker writes is accepted
// back: a Linear listing's token names up to 100 issue IDs of a tie, about
// 4.3 KB before the cursor's own encoding.
func TestACursorTakesTheLargestPageToken(t *testing.T) {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = `"00000000-0000-4000-8000-000000000000"`
	}
	page := `{"v":1,"team":"00000000-0000-4000-8000-000000000000","bound":"2026-01-02T03:04:05.678Z","at":[` +
		strings.Join(ids, ",") + `],"drain":"00000000-0000-4000-8000-000000000000"}`
	next := encodeCursor(findCursor{Connection: "00000000-0000-4000-8000-000000000000", Page: page, Query: "AAAAAAAA"})
	var back findCursor
	if err := decodeCursor("next", next, &back); err != nil {
		t.Fatalf("a %d-byte next is refused: %v", len(next), err)
	}
	if back.Page != page {
		t.Fatal("the page token did not survive the cursor")
	}
	// A cursor these tools could have written, past the bound, is refused.
	over := encodeCursor(findCursor{Connection: "c", Page: strings.Repeat("x", maxCursorBytes), Query: "AAAAAAAA"})
	if err := decodeCursor("next", over, &back); err == nil {
		t.Fatalf("a %d-byte cursor, past the bound of %d, is accepted", len(over), maxCursorBytes)
	}
}
