// Package memory owns the campaign memory a run tree accumulates: what one
// note is, what a valid one looks like, where the log lives on disk, how a line
// is appended and read back, and how the promoted digest is rendered into a
// prompt.
//
// It is stdlib-only and holds no app, engine, store, or provider concern. The
// app resolves which tree a note belongs to and stamps its provenance; this
// package decides nothing about ownership and never learns a run's shape.
package memory
