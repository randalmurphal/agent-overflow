package orphanreaper

import "fmt"

// The control protocol is newline-delimited text on the pipe: one
// `watch <pgid>` or `release <pgid>` command per line, read by
// sidecarScript. Lines stay whole because the parent is the pipe's sole
// writer and serializes every send under a mutex (see Client.send); being
// far under PIPE_BUF, they wouldn't tear even if that weren't so.

func formatWatch(pgid int) string   { return fmt.Sprintf("watch %d\n", pgid) }
func formatRelease(pgid int) string { return fmt.Sprintf("release %d\n", pgid) }
