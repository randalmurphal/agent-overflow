package store

// DesignArtifact is persisted metadata for a design-mode HTML artifact.
type DesignArtifact struct {
	ID          string `json:"id"`
	ThreadID    string `json:"threadId"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	HTMLPath    string `json:"htmlPath"`
	CreatedAt   int64  `json:"createdAt"`
}
