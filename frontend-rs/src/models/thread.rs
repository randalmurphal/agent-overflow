use serde::Deserialize;

// Thread mirrors store.Thread on the Go side. Only the fields the
// sidebar + header read are deserialized; the rest are tolerated as
// `serde(default)` plus `deny_unknown_fields = false` (default).
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Thread {
    pub id: String,
    pub project_id: String,
    #[serde(default)]
    pub project_path: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub workspace_path: String,
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub archived: bool,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default)]
    pub updated_at: i64,
    #[serde(default)]
    pub latest_turn_completed_at: Option<i64>,
    #[serde(default)]
    pub last_read_at: Option<i64>,
    #[serde(default)]
    pub pinned_at: Option<i64>,
    #[serde(default)]
    pub has_actionable_proposed_plan: bool,
    #[serde(default)]
    pub has_incomplete_turn: bool,
}

impl Thread {
    /// Display title falls back to a placeholder when the backend hasn't
    /// summarised the thread yet — matches the Svelte sidebar's "Untitled"
    /// affordance.
    pub fn display_title(&self) -> &str {
        if self.title.trim().is_empty() {
            "Untitled thread"
        } else {
            self.title.as_str()
        }
    }

    pub fn is_unread(&self) -> bool {
        match (self.latest_turn_completed_at, self.last_read_at) {
            (Some(latest), Some(seen)) => latest > seen,
            (Some(_), None) => false, // pre-tracking rows treated as read
            _ => false,
        }
    }
}
