package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtransfer"
)

// GetThreadTransferStatus follows an exact operation, independent of recent-list limits.
//
//ao:scope threads:read
func (a *App) GetThreadTransferStatus(threadID, operationID string) (store.ThreadTransfer, error) {
	row, err := a.store.GetThreadTransferStatus(operationID)
	if err != nil {
		return store.ThreadTransfer{}, err
	}
	if row.ThreadID != threadID {
		return store.ThreadTransfer{}, errors.New("The transfer belongs to another thread.")
	}
	return row, nil
}

// ThreadRecurringJob is one recurring scheduled job armed in a thread's live
// provider session (Claude CronCreate).
type ThreadRecurringJob struct {
	ID   string `json:"id"`
	Cron string `json:"cron"`
}

// ListThreadRecurringJobs names the recurring jobs the thread's live session
// still holds. A transfer stops that session and the jobs end with it, so
// the transfer dialog names them instead of the transfer being refused. A
// job whose deadline has passed without a fire is no longer counted, as the
// idle reaper no longer counts it.
//
//ao:scope threads:read
func (a *App) ListThreadRecurringJobs(threadID string) ([]ThreadRecurringJob, error) {
	jobs := []ThreadRecurringJob{}
	if a.triage == nil {
		return jobs, nil
	}
	now := time.Now()
	for _, job := range a.triage.RecurringScheduledJobs(threadID) {
		if now.Before(time.UnixMilli(job.FireByUnixMs).Add(a.wakeupReapGrace())) {
			jobs = append(jobs, ThreadRecurringJob{ID: job.ID, Cron: job.Cron})
		}
	}
	return jobs, nil
}

// GetThreadTransfers returns a bounded recent status list on this computer.
//
//ao:scope threads:read
//ao:route selected
func (a *App) GetThreadTransfers() ([]store.ThreadTransfer, error) {
	return a.store.ListRecentThreadTransfers()
}

// GetThreadTransferDestinationProject recovers an accepted destination choice
// after a lost offer response. It returns no transfer grant or private paths.
// An empty answer means this computer has not accepted the offer yet.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) GetThreadTransferDestinationProject(operationID string) (string, error) {
	row, err := a.store.GetThreadTransfer(operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var data threadtransfer.DestinationData
	var details transferDestinationDetails
	if row.Direction != "incoming" || json.Unmarshal(row.PrivateState, &data) != nil || json.Unmarshal(data.Details, &details) != nil {
		return "", errors.New("This computer is not the destination of that transfer.")
	}
	return details.ProjectID, nil
}

// GetThreadTransferIntent recovers the public half of an interrupted two-host
// setup. It never exposes the source's activation secret.
//
//ao:scope threads:operate
func (a *App) GetThreadTransferIntent(threadID, operationID string) (ThreadTransferIntent, error) {
	row, err := a.store.GetThreadTransfer(operationID)
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	var data threadtransfer.SourceData
	var details transferSourceDetails
	if row.Direction != "outgoing" || row.ThreadID != threadID || json.Unmarshal(row.PrivateState, &data) != nil || json.Unmarshal(data.Details, &details) != nil {
		return ThreadTransferIntent{}, errors.New("This computer is not the source of that transfer.")
	}
	return a.transferIntent(row, details), nil
}
