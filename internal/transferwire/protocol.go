// Package transferwire is the bounded computer-to-computer handoff contract.
// It contains no credentials at rest, filesystem behavior or ownership policy.
package transferwire

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const Version = 1

// Epochs must survive the frontend JavaScript number boundary exactly.
const MaxOwnershipEpoch int64 = 1<<53 - 1
const PathPrefix = "/transfers/"
const VersionHeader = "X-AO-Transfer-Version"
const BackendHeader = "X-AO-Transfer-Backend"
const OffsetHeader = "X-AO-Transfer-Offset"
const DigestHeader = "X-AO-Transfer-SHA256"
const MaxChunkBytes int64 = 8 << 20
const MaxUploadBytes int64 = (8 << 30) + 16_384*(16<<10)

type Upload struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (u Upload) Valid() bool {
	return ValidDigest(u.SHA256) && u.Size >= 1024 && u.Size <= MaxUploadBytes && u.Size%512 == 0
}

type Activation struct {
	Secret string `json:"secret"`
}

// State is read from durable operation/upload records, not a second read model.
// It never carries paths, grants or the source activation secret.
type State struct {
	OwnershipEpoch int64  `json:"ownershipEpoch"`
	Phase          string `json:"phase"`
	SHA256         string `json:"sha256,omitempty"`
	Size           int64  `json:"size,omitempty"`
	Received       int64  `json:"received,omitempty"`
	NeedsAttention bool   `json:"needsAttention,omitempty"`
}

// Reply binds every acknowledgment to both the backend and the operation. An
// address being reused by another computer cannot complete the old handoff.
type Reply struct {
	Version     int    `json:"version"`
	BackendID   string `json:"backendId"`
	OperationID string `json:"operationId"`
	State       *State `json:"state,omitempty"`
	Error       string `json:"error,omitempty"`
}

var (
	ErrInvalid     = errors.New("transfer_invalid")
	ErrConflict    = errors.New("transfer_conflict")
	ErrNotReady    = errors.New("transfer_not_ready")
	ErrUnavailable = errors.New("transfer_unavailable")
)

func DecodeSecret(value string) ([]byte, error) {
	if len(value) != 43 {
		return nil, ErrInvalid
	}
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(bytes) != 32 {
		return nil, ErrInvalid
	}
	return bytes, nil
}

func ValidDigest(value string) bool {
	bytes, err := hex.DecodeString(value)
	return err == nil && len(bytes) == 32 && strings.ToLower(value) == value
}
