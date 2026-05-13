// Package dirbrowse lists directory contents for the project-picker
// UI. The binding does not stat per-keystroke results into errors — a
// missing path or "path is a file" is reported as Exists=false so the
// frontend can prefix-filter without flooding the server log.
package dirbrowse
