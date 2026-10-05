//go:build noremote

package push

import (
	"context"

	"agent-overflow/internal/buildvariant"
)

// FCMSender is absent from a build without remote access: NewFCMSender
// refuses and no OAuth client is linked.
type FCMSender struct{}

// NewFCMSender refuses in a build without remote access.
func NewFCMSender(Credential) (*FCMSender, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}

func (s *FCMSender) Send(context.Context, Message) error {
	return buildvariant.ErrRemoteAccessUnavailable
}

var _ Sender = (*FCMSender)(nil)
