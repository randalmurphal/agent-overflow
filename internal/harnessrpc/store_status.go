package harnessrpc

import "fmt"

// HarnessStoreStatusResult is where the booted instance's database stands
// against its build (store.MigrationStatus), with the deferred phases'
// outcome: pending while the watermark trails the last deferred version,
// and the failure the last run of a pending phase recorded, if any.
type HarnessStoreStatusResult struct {
	SchemaVersion      int                          `json:"schemaVersion"`
	BuildSchemaVersion int                          `json:"buildSchemaVersion"`
	DeferredWatermark  int                          `json:"deferredWatermark"`
	DeferredLatest     int                          `json:"deferredLatest"`
	DeferredPending    bool                         `json:"deferredPending"`
	DeferredFailure    *HarnessStoreDeferredFailure `json:"deferredFailure"`
}

// HarnessStoreDeferredFailure is store.DeferredFailure on the wire.
type HarnessStoreDeferredFailure struct {
	Version    int    `json:"version"`
	Title      string `json:"title"`
	Failures   int    `json:"failures"`
	FirstError string `json:"firstError"`
}

// HarnessStoreStatus reports the database's migration state. The release
// upgrade check polls it after a migrated boot: the applied version must
// be the build's, and the deferred phases must finish without a failure.
func (h *Harness) HarnessStoreStatus() (HarnessStoreStatusResult, error) {
	database := h.store()
	if database == nil {
		return HarnessStoreStatusResult{}, fmt.Errorf("store unavailable")
	}
	status, err := database.MigrationStatus()
	if err != nil {
		return HarnessStoreStatusResult{}, err
	}
	out := HarnessStoreStatusResult{
		SchemaVersion:      status.Applied,
		BuildSchemaVersion: status.Build,
		DeferredWatermark:  status.DeferredWatermark,
		DeferredLatest:     status.DeferredLatest,
		DeferredPending:    status.DeferredWatermark < status.DeferredLatest,
	}
	failure, err := database.DeferredMigrationFailure()
	if err != nil {
		return HarnessStoreStatusResult{}, err
	}
	if failure != nil {
		out.DeferredFailure = &HarnessStoreDeferredFailure{
			Version:    failure.Version,
			Title:      failure.Title,
			Failures:   failure.Failures,
			FirstError: failure.FirstError,
		}
	}
	return out, nil
}
