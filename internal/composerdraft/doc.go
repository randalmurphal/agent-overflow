// Package composerdraft builds the composer-draft row a thread should
// be restored to when an existing user_text item is rehydrated into the
// composer: revert-to-message, fork-and-revert, and the
// flush-queue dispatch flush path. Pure helpers — App-bound glue stays
// in `app_draft.go`.
package composerdraft
