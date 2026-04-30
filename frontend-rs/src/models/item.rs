use serde::Deserialize;

// Item is the canonical timeline row. The Go side is intentionally loose
// here — the union of every kind across both providers — so we keep the
// payload-shaped fields optional. Renderers branch on `kind`.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Item {
    pub id: String,
    pub thread_id: String,
    #[serde(default)]
    pub turn_index: i64,
    #[serde(default)]
    pub item_index: i64,
    pub kind: String,
    #[serde(default)]
    pub role: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub summary: String,
    #[serde(default)]
    pub payload_id: Option<String>,
    #[serde(default)]
    pub payload_kind: Option<String>,
    #[serde(default)]
    pub payload_meta: Option<String>,
    #[serde(default)]
    pub parent_id: Option<String>,
    #[serde(default)]
    pub tool_name: Option<String>,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default)]
    pub updated_at: i64,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PagedItems {
    #[serde(default)]
    pub items: Vec<Item>,
    #[serde(default)]
    pub oldest_turn_index: i64,
    #[serde(default)]
    pub has_more: bool,
}

impl Item {
    /// Returns one of {user, assistant, tool, system, other} so renderers
    /// can pick a row variant without branching on the long backend list.
    pub fn lane(&self) -> ItemLane {
        match (self.kind.as_str(), self.role.as_str()) {
            (_, "user") => ItemLane::User,
            (_, "assistant") => ItemLane::Assistant,
            ("tool_call" | "tool_result" | "tool_completion", _) => ItemLane::Tool,
            ("notification" | "system" | "session_lifecycle", _) => ItemLane::System,
            _ => ItemLane::Other,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ItemLane {
    User,
    Assistant,
    Tool,
    System,
    Other,
}
