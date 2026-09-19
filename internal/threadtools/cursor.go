package threadtools

import (
	"encoding/base64"
	"encoding/json"
)

// A cursor is opaque to the model: it is base64url of a small JSON record
// this package minted. Nothing in it is a promise to the agent, so its
// shape can change without changing a tool schema, and a model cannot
// construct one by hand and page somewhere it was never shown.
//
// A thread_show cursor carries the window, its bounds, the last position
// rendered and the high-water position that existed when the first page
// was taken. The bounds are what makes a head or around page keep its
// window across calls; the high-water is what makes streaming growth
// unable to shift a page, because every later page is clipped to it.
type cursor struct {
	// Version guards against a cursor from an older build.
	Version int    `json:"v"`
	Kind    string `json:"k"`
	Thread  string `json:"t,omitempty"`
	// Window and its resolved bounds.
	Window string `json:"w,omitempty"`
	From   int64  `json:"f,omitempty"`
	To     int64  `json:"to,omitempty"`
	// Position is the last item position already rendered.
	Position int64 `json:"p,omitempty"`
	// High is the snapshot high-water item position.
	High int64 `json:"h,omitempty"`
	// Include is the thread_show include list, so a continued page keeps
	// the content the first page asked for.
	Include []string `json:"i,omitempty"`
	// Offset is a byte offset for thread_item and a status answer, and a
	// row offset for a single-computer listing.
	Offset int64 `json:"o,omitempty"`
	// Offsets is the per-computer row offset of a grouped search page.
	Offsets map[string]int `json:"co,omitempty"`
	// Query is the literal thread_item search a match page continues.
	Query string `json:"q,omitempty"`
	// Token names the request whose answer a status page continues.
	Token string `json:"tk,omitempty"`
}

const cursorVersion = 1

// Cursor kinds, one per paging tool.
const (
	cursorShow   = "show"
	cursorSearch = "search"
	cursorItem   = "item"
	cursorStatus = "status"
	cursorList   = "list"
)

func encodeCursor(c cursor) (string, error) {
	c.Version = cursorVersion
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// decodeCursor refuses a cursor from another tool or another build rather
// than paging somewhere the agent did not ask for.
func decodeCursor(raw, kind string) (cursor, error) {
	if raw == "" {
		return cursor{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, invalidf("The cursor is not one this tool returned. Pass back the cursor from the previous %s result, unchanged, or omit it to start over.", kind)
	}
	var c cursor
	if err := json.Unmarshal(data, &c); err != nil || c.Version != cursorVersion || c.Kind != kind {
		return cursor{}, invalidf("The cursor is not one this tool returned. Pass back the cursor from the previous %s result, unchanged, or omit it to start over.", kind)
	}
	return c, nil
}
