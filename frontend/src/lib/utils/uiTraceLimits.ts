/**
 * Wire mirror of the per-line cap in internal/uitrace/uitrace.go
 * (`MaxLineBytes`, UTF-8 bytes). The Go side rejects a WHOLE batch on a
 * single oversized line, so frontend writers filter client-side first.
 * Shared by uiRenderTrace.ts and frontendErrorCapture.ts so the mirror
 * can't drift between them.
 */
export const UI_TRACE_MAX_LINE_BYTES = 64 * 1024;
