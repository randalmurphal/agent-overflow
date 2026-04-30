// JSON wire frames. Names + tags are pinned to the Go side
// (internal/transport/frame.go) — every field name, every error code,
// every type discriminant must match exactly or the dispatcher rejects
// the frame.
//
// We use serde's `tag = "type"` to match the Go side's untagged-string
// approach: each frame on the wire carries a `"type": "rpc"|"event"|
// "replay"` discriminator. serde's "internally tagged" enums emit
// + parse exactly that shape.

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum ClientFrame {
    /// rpc — invoke a method. Either MethodID (preferred) or Method (by
    /// name) must be set; Params is the positional argument array.
    Rpc {
        id: String,
        #[serde(rename = "methodId", skip_serializing_if = "Option::is_none")]
        method_id: Option<u32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        method: Option<String>,
        params: Vec<Value>,
    },
    /// replay — request missed events on reconnect.
    Replay {
        #[serde(rename = "lastSeqByChannel")]
        last_seq_by_channel: std::collections::HashMap<String, u64>,
    },
}

#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum ServerFrame {
    Rpc {
        id: String,
        #[serde(default)]
        result: Option<Value>,
        #[serde(default)]
        error: Option<FrameError>,
    },
    Event {
        channel: String,
        seq: u64,
        #[serde(default)]
        data: Value,
        #[serde(default)]
        gap: bool,
    },
}

#[derive(Debug, Clone, Deserialize)]
pub struct FrameError {
    pub code: String,
    pub message: String,
}

/// Stable error codes exposed by the Go dispatcher. Documented in
/// frame.go. Kept as `&str` constants so call sites can match without
/// pulling in the full enum-of-strings ceremony.
pub mod codes {
    pub const METHOD_NOT_FOUND: &str = "method_not_found";
    pub const BAD_PARAMS: &str = "bad_params";
    pub const METHOD_ERROR: &str = "method_error";
    pub const INTERNAL: &str = "internal";
    pub const SHUTTING_DOWN: &str = "shutting_down";
}
