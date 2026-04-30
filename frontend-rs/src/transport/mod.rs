// HTTP+WebSocket transport client.
//
// Mirrors the wire shape declared in /internal/transport/frame.go. The
// Go server is the source of truth; this module's only job is to speak
// the same JSON frames and surface RPC + event subscriptions to the rest
// of the crate as Rust-native types.
//
// Layout:
//   - wire:      Frame structs (Client → Server / Server → Client).
//   - fnv:       FNV-1a 32-bit hash used to compute methodId.
//   - bootstrap: Spawns `agent-overflow --print-url-fd 0`, parses the
//                __AO_BOOTSTRAP__ stdout sentinel, and yields a
//                `Bootstrap { ws_url, token }` for the client.
//   - client:    Long-lived connection: dial, RPC tracking, replay-on-
//                reconnect, channel subscription fanout, transport
//                status snapshot.
//
// Public surface kept narrow on purpose. `Transport` is the only type
// the rest of the app holds — every dependent piece (UI views, RPC
// wrappers) talks through it.

pub mod bootstrap;
pub mod client;
pub mod fnv;
pub mod wire;

pub use bootstrap::{Bootstrap, BootstrapHandle, spawn_backend};
pub use client::{Transport, TransportError, TransportStatus};
