// Typed wrappers around the subset of Go App methods we call from the
// spike. Method IDs are computed from FNV-1a 32-bit of
// `main.App.<MethodName>` — see transport/fnv.rs for the hash and pinned
// vectors that prove it matches the Wails generator.
//
// Adding a new RPC: declare a wrapper here, return the deserialized
// shape from models/. No codegen — hand-typing the few methods the
// spike uses is shorter than wiring a generator and easier to read at
// review.

use serde_json::Value;

use crate::models::{PagedItems, ProjectWithCounts, Thread};
use crate::transport::fnv::method_id;
use crate::transport::{Transport, TransportError};

pub async fn list_projects(
    transport: &Transport,
) -> Result<Vec<ProjectWithCounts>, TransportError> {
    let value = transport
        .call_id(method_id("ListProjects"), Vec::new())
        .await?;
    Ok(deserialize_or_default(value))
}

pub async fn list_threads(transport: &Transport) -> Result<Vec<Thread>, TransportError> {
    let value = transport
        .call_id(method_id("ListThreads"), Vec::new())
        .await?;
    Ok(deserialize_or_default(value))
}

pub async fn list_recent_thread_items(
    transport: &Transport,
    thread_id: &str,
    turn_limit: i64,
) -> Result<PagedItems, TransportError> {
    let value = transport
        .call_id(
            method_id("ListRecentThreadItems"),
            vec![Value::String(thread_id.to_string()), Value::from(turn_limit)],
        )
        .await?;
    if matches!(value, Value::Null) {
        return Ok(PagedItems::default());
    }
    serde_json::from_value(value).map_err(TransportError::from)
}

pub async fn get_thread(
    transport: &Transport,
    thread_id: &str,
) -> Result<Thread, TransportError> {
    let value = transport
        .call_id(
            method_id("GetThread"),
            vec![Value::String(thread_id.to_string())],
        )
        .await?;
    serde_json::from_value(value).map_err(TransportError::from)
}

/// Used for shapes where Null is an acceptable empty value. `null`
/// arrays come back as `Value::Null` from the dispatcher when the Go
/// method returns a nil slice — we want `vec![]` in that case rather
/// than a deserialize error.
fn deserialize_or_default<T: serde::de::DeserializeOwned + Default>(value: Value) -> T {
    if matches!(value, Value::Null) {
        return T::default();
    }
    serde_json::from_value(value).unwrap_or_default()
}
