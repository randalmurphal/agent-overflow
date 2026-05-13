// Package keybindings owns the on-disk keybindings config (the shipped
// defaults plus the user's overrides) and the merge that produces the
// effective list. Frontend palette + Settings UI read the merged list
// via the Get/Update/Reset bindings.
package keybindings
