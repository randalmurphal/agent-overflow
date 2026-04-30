use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Project {
    pub id: String,
    pub path: String,
    pub name: String,
    #[serde(default)]
    pub color: Option<String>,
    #[serde(default)]
    pub sort_position: i64,
    #[serde(default)]
    pub archived: bool,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ProjectWithCounts {
    pub project: Project,
    #[serde(default)]
    pub thread_count: i64,
    #[serde(default)]
    pub last_active: Option<i64>,
}

impl Project {
    pub fn display_name(&self) -> &str {
        if self.name.trim().is_empty() {
            self.path.as_str()
        } else {
            self.name.as_str()
        }
    }
}
