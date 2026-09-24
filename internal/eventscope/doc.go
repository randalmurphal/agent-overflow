// Package eventscope extracts attribution from an event payload flowing
// through the App's transport bus: the thread a frame is about, used by the
// watch filter and the observability fan-out, and the transcript scope root
// of the row a frame describes, used by the watch filter's scope narrowing.
package eventscope
