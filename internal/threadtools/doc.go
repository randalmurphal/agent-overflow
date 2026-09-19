// Package threadtools owns the ao-thread-tools contract: the thirteen tool
// schemas, the server instructions, argument validation, thread id
// resolution, transcript rendering and paging, item range reads, the
// message footers and wake headers, and the thread state derivation.
//
// It depends on the App interface declared in app.go and on nothing in
// internal/app or internal/store, so the same code runs in the calling
// thread's process and, unchanged, on the computer that owns a thread
// reached over a peer call.
//
// Two shapes exist. With no paired computers the schemas carry no
// computers/computer_id parameter, no result row carries a computer field,
// and the instructions omit the "Other computers" paragraph. Shape is
// recomputed per tools/list and per initialize, so pairing a computer
// changes the shape of a running session without adding or removing a tool.
package threadtools
