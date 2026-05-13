// Package network owns the wire shape + helpers behind the LAN-bind
// toggle in Settings: the public Settings struct, the bind host /
// origin allow-list / share-URL formatters, and the deterministic
// local-IP discovery. The App-side orchestration (settings persist +
// transport rebind with rollback) stays in main package.
package network
