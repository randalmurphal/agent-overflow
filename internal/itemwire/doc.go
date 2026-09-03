// Package itemwire projects a persisted timeline item onto the wire.
//
// The stored record always stays complete — this is a projection, never
// a storage shape (internal/itemmeta owns the persisted shaping). Only
// the copy a client receives is bounded, and every value the projection
// removes is named by a typed marker whose recovery route
// (App.GetThreadItemProjectionSource) returns the stored value.
package itemwire
