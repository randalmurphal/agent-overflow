package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtransfer"
	"agent-overflow/internal/transferclient"
	"agent-overflow/internal/transferwire"
	"agent-overflow/internal/transport"
)

// ThreadTransferIntent is a public challenge, not authority to resume a thread.
// The source keeps the activation secret until its retirement is durable.
type ThreadTransferIntent struct {
	OperationID          string `json:"operationId"`
	SourceBackendID      string `json:"sourceBackendId"`
	DestinationBackendID string `json:"destinationBackendId"`
	SourceThreadID       string `json:"sourceThreadId"`
	TargetThreadID       string `json:"targetThreadId"`
	Kind                 string `json:"kind"`
	Provider             string `json:"provider"`
	RuntimeMode          string `json:"runtimeMode"`
	IncludeWorkspace     bool   `json:"includeWorkspace"`
	OwnershipEpoch       int64  `json:"ownershipEpoch"`
	ActivationHash       string `json:"activationHash"`
}

type transferSourceDetails struct {
	Provider         string `json:"provider"`
	RuntimeMode      string `json:"runtimeMode"`
	IncludeWorkspace bool   `json:"includeWorkspace"`
}

type transferDestinationDetails struct {
	Intent        ThreadTransferIntent `json:"intent"`
	ProjectID     string               `json:"projectId"`
	WorkspacePath string               `json:"workspacePath"`
	Branch        string               `json:"branch"`
	Grant         string               `json:"grant"`
}

// BeginThreadTransfer reserves the idle source. The caller supplies a stable
// operation ID so a lost response can retry without creating another copy.
//
//ao:scope threads:operate
func (a *App) BeginThreadTransfer(ctx context.Context, threadID, operationID, destinationBackendID, kind string, includeWorkspace bool) (ThreadTransferIntent, error) {
	release, admitErr := a.workAdmission.begin(ctx)
	if admitErr != nil {
		return ThreadTransferIntent{}, admitErr
	}
	defer release()

	if !entityid.Valid(operationID) || !entityid.Valid(destinationBackendID) || (kind != "move" && kind != "copy") {
		return ThreadTransferIntent{}, errors.New("Choose Move or Copy and a connected destination computer.")
	}
	if err := a.transfers.available(); err != nil {
		return ThreadTransferIntent{}, err
	}
	backendID, _ := a.backendIdentity()
	if destinationBackendID == backendID {
		return ThreadTransferIntent{}, errors.New("Choose another computer for this transfer.")
	}
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	defer unlock()
	if previous, err := a.store.GetThreadTransfer(operationID); err == nil {
		var data threadtransfer.SourceData
		var details transferSourceDetails
		if previous.ThreadID != threadID || previous.PeerBackendID != destinationBackendID || previous.Kind != kind || previous.Direction != "outgoing" ||
			json.Unmarshal(previous.PrivateState, &data) != nil || json.Unmarshal(data.Details, &details) != nil || details.IncludeWorkspace != includeWorkspace {
			return ThreadTransferIntent{}, errors.New("This transfer ID already belongs to a different request.")
		}
		return a.transferIntent(previous, details), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ThreadTransferIntent{}, err
	}
	// Freeze ordinary edits and queue admission while recording the fence.
	// They intentionally do not wait for sends/reverts on the action lock.
	unlockMutations, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	defer unlockMutations()
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		return ThreadTransferIntent{}, err
	}
	if err := a.checkTransferIdle(thread); err != nil {
		return ThreadTransferIntent{}, err
	}
	details := transferSourceDetails{Provider: thread.Provider, RuntimeMode: thread.RuntimeMode, IncludeWorkspace: includeWorkspace}
	encoded, err := json.Marshal(details)
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	secret, hash, err := newTransferSecret()
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	private, err := json.Marshal(threadtransfer.SourceData{ActivationSecret: secret, Details: encoded})
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	row, err := a.store.CreateThreadTransfer(store.ThreadTransfer{ID: operationID, ThreadID: threadID, PeerBackendID: destinationBackendID, Kind: kind, Direction: "outgoing", ActivationHash: hash, PrivateState: private})
	if err != nil {
		return ThreadTransferIntent{}, err
	}
	a.transfers.wake(row.ID)
	a.emit(eventchan.ThreadTransfer, row)
	return a.transferIntent(row, details), nil
}

func (a *App) transferIntent(row store.ThreadTransfer, details transferSourceDetails) ThreadTransferIntent {
	backendID, _ := a.backendIdentity()
	return ThreadTransferIntent{OperationID: row.ID, SourceBackendID: backendID, DestinationBackendID: row.PeerBackendID, SourceThreadID: row.ThreadID, TargetThreadID: row.TargetThreadID, Kind: row.Kind, Provider: details.Provider, RuntimeMode: details.RuntimeMode, IncludeWorkspace: details.IncludeWorkspace, OwnershipEpoch: row.OwnershipEpoch, ActivationHash: row.ActivationHash}
}

// CreateThreadTransferOffer authorizes only one incoming conversation into the
// selected local project. The returned one-operation grant is private: do not
// persist it in frontend preferences, log it, or attach it to status events.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) CreateThreadTransferOffer(ctx context.Context, intent ThreadTransferIntent, projectID, workspacePath, branch string) (transferclient.Offer, error) {
	release, admitErr := a.workAdmission.begin(ctx)
	if admitErr != nil {
		return transferclient.Offer{}, admitErr
	}
	defer release()

	if err := a.transfers.available(); err != nil {
		return transferclient.Offer{}, err
	}
	backendID, _ := a.backendIdentity()
	if intent.DestinationBackendID != backendID || !entityid.Valid(intent.SourceBackendID) || intent.SourceBackendID == backendID || !entityid.Valid(intent.OperationID) ||
		intent.SourceThreadID == "" || intent.TargetThreadID == "" || !transferwire.ValidDigest(intent.ActivationHash) || (intent.Kind != "move" && intent.Kind != "copy") ||
		(intent.Provider != "codex" && intent.Provider != "claude") || (intent.Kind == "move" && intent.SourceThreadID != intent.TargetThreadID) || (intent.Kind == "copy" && intent.SourceThreadID == intent.TargetThreadID) {
		return transferclient.Offer{}, errors.New("The transfer request does not match this computer.")
	}
	mode, err := threadmode.ParseRuntime(intent.RuntimeMode)
	if err != nil {
		return transferclient.Offer{}, err
	}
	if err := a.requireAutonomy(ctx, string(mode)); err != nil {
		return transferclient.Offer{}, err
	}
	if intent.IncludeWorkspace {
		if err := a.requireScope(ctx, transport.ScopeGitOperate, "Create the transferred workspace"); err != nil {
			return transferclient.Offer{}, err
		}
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return transferclient.Offer{}, err
	}
	defaultWorkspace, defaultBranch := workspacePath == "", branch == ""
	if intent.IncludeWorkspace {
		if branch == "" {
			branch = "conversation-" + intent.OperationID[:8]
		}
		if err := gitops.ValidateBranchName(branch); err != nil {
			return transferclient.Offer{}, err
		}
		if workspacePath == "" {
			workspacePath, err = a.defaultWorktreePath(project.Path, branch)
			if err != nil {
				return transferclient.Offer{}, err
			}
		}
	}
	if workspacePath == "" {
		workspacePath = project.Path
	}
	if !filepath.IsAbs(workspacePath) {
		return transferclient.Offer{}, errors.New("Choose an absolute destination workspace path.")
	}
	workspacePath = filepath.Clean(workspacePath)
	if !intent.IncludeWorkspace && workspacePath != filepath.Clean(project.Path) {
		// Existing linked worktrees are checked through Git, not path prefixes.
		if err := a.validateTransferCheckout(ctx, project.Path, workspacePath); err != nil {
			return transferclient.Offer{}, err
		}
	}
	unlock, err := a.threadLocks().LockCtx(ctx, intent.TargetThreadID)
	if err != nil {
		return transferclient.Offer{}, err
	}
	defer unlock()
	details := transferDestinationDetails{Intent: intent, ProjectID: projectID, WorkspacePath: workspacePath, Branch: branch}
	if row, err := a.store.GetThreadTransfer(intent.OperationID); err == nil {
		var data threadtransfer.DestinationData
		var previous transferDestinationDetails
		if row.Direction != "incoming" || json.Unmarshal(row.PrivateState, &data) != nil || json.Unmarshal(data.Details, &previous) != nil {
			return transferclient.Offer{}, errors.New("This transfer ID is already in use.")
		}
		details.Grant = previous.Grant
		if defaultWorkspace {
			details.WorkspacePath = previous.WorkspacePath
		}
		if defaultBranch {
			details.Branch = previous.Branch
		}
		if details != previous {
			return transferclient.Offer{}, errors.New("This transfer already names a different destination.")
		}
		return a.makeTransferOffer(row, details.Grant)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return transferclient.Offer{}, err
	}
	grant, hash, err := newTransferSecret()
	if err != nil {
		return transferclient.Offer{}, err
	}
	details.Grant = grant
	encoded, err := json.Marshal(details)
	if err != nil {
		return transferclient.Offer{}, err
	}
	private, err := json.Marshal(threadtransfer.DestinationData{GrantHash: hash, Details: encoded})
	if err != nil {
		return transferclient.Offer{}, err
	}
	row, err := a.store.CreateThreadTransfer(store.ThreadTransfer{ID: intent.OperationID, ThreadID: intent.TargetThreadID, TargetThreadID: intent.TargetThreadID, PeerBackendID: intent.SourceBackendID, ProjectID: projectID, Kind: intent.Kind, Direction: "incoming", OwnershipEpoch: intent.OwnershipEpoch, ActivationHash: intent.ActivationHash, PrivateState: private})
	if err != nil {
		return transferclient.Offer{}, err
	}
	a.emit(eventchan.ThreadTransfer, row)
	return a.makeTransferOffer(row, grant)
}

func (a *App) makeTransferOffer(row store.ThreadTransfer, grant string) (transferclient.Offer, error) {
	_, endpoint, fingerprint, err := a.pairingPageURL()
	if err != nil {
		return transferclient.Offer{}, err
	}
	backendID, _ := a.backendIdentity()
	offer := transferclient.Offer{Version: transferwire.Version, OwnershipEpoch: row.OwnershipEpoch, BackendID: backendID, OperationID: row.ID, Endpoint: endpoint, CertFingerprint: fingerprint, Grant: grant}
	client, err := transferclient.New(offer)
	if err != nil {
		return transferclient.Offer{}, err
	}
	client.Close()
	return offer, nil
}

// BindThreadTransferDestination accepts the destination offer once. Transfer
// work then belongs to the host and continues when the frontend disconnects.
//
//ao:scope threads:operate
func (a *App) BindThreadTransferDestination(ctx context.Context, threadID string, offer transferclient.Offer) (store.ThreadTransfer, error) {
	release, admitErr := a.workAdmission.begin(ctx)
	if admitErr != nil {
		return store.ThreadTransfer{}, admitErr
	}
	defer release()

	if err := a.transfers.available(); err != nil {
		return store.ThreadTransfer{}, err
	}
	row, err := a.store.GetThreadTransfer(offer.OperationID)
	if err != nil {
		return row, err
	}
	if row.ThreadID != threadID || row.Direction != "outgoing" || row.PeerBackendID != offer.BackendID || row.OwnershipEpoch != offer.OwnershipEpoch {
		return row, errors.New("The destination offer belongs to another transfer.")
	}
	client, err := transferclient.New(offer)
	if err != nil {
		return row, err
	}
	client.Close()
	encoded, err := json.Marshal(offer)
	if err != nil {
		return row, err
	}
	row, err = a.store.BindThreadTransferPeer(row.ID, encoded)
	if err == nil {
		a.transfers.wake(row.ID)
		a.emit(eventchan.ThreadTransfer, row)
	}
	return row, err
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

// RetryThreadTransfer wakes a durable operation without minting another copy.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) RetryThreadTransfer(operationID string) error {
	release, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return admitErr
	}
	defer release()

	if err := a.transfers.available(); err != nil {
		return err
	}
	if _, err := a.store.GetThreadTransfer(operationID); err != nil {
		return err
	}
	a.transfers.wake(operationID)
	return nil
}

// DiscardUnpreparedThreadTransfer removes an orphaned destination offer. After
// preparation, only the source can decide whether retirement already occurred.
//
//ao:scope threads:operate
//ao:route selected
func (a *App) DiscardUnpreparedThreadTransfer(ctx context.Context, operationID string) error {
	release, admitErr := a.workAdmission.begin(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()

	if err := a.transfers.available(); err != nil {
		return err
	}
	if err := a.transfers.live.Load().destination.DiscardUnprepared(ctx, operationID); err != nil {
		return err
	}
	row, err := a.store.GetThreadTransfer(operationID)
	if err != nil {
		return err
	}
	a.emit(eventchan.ThreadTransfer, row)
	return nil
}

// CancelThreadTransfer keeps the source fenced until the destination confirms
// it cannot activate. A committed move can only finish, never reopen its source.
//
//ao:scope threads:operate
func (a *App) CancelThreadTransfer(threadID, operationID string) error {
	release, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return admitErr
	}
	defer release()

	if err := a.transfers.available(); err != nil {
		return err
	}
	row, err := a.store.GetThreadTransfer(operationID)
	if err != nil {
		return err
	}
	if row.ThreadID != threadID || row.Direction != "outgoing" {
		return errors.New("Cancel this transfer from its source computer.")
	}
	row, err = a.store.RequestThreadTransferCancellation(operationID)
	if err != nil {
		return err
	}
	a.transfers.wake(operationID)
	a.emit(eventchan.ThreadTransfer, row)
	return nil
}

func newTransferSecret() (string, string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(secret)
	return base64.RawURLEncoding.EncodeToString(secret), hex.EncodeToString(hash[:]), nil
}
