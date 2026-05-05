package store

// DesignSnapshot is persisted metadata for a frozen state of a design
// thread's working directory. Snapshots are created on explicit user
// gesture and auto-on-turn-start; a snapshot's dir_path points at a
// directory holding the working files at that moment.
type DesignSnapshot struct {
	ID               string `json:"id"`
	ThreadID         string `json:"threadId"`
	Label            string `json:"label"`
	DirPath          string `json:"dirPath"`
	ParentSnapshotID string `json:"parentSnapshotId,omitempty"`
	Auto             bool   `json:"auto"`
	CreatedAt        int64  `json:"createdAt"`
}
