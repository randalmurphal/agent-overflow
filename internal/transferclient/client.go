// Package transferclient carries one authorized handoff between computers.
// Its grant cannot enroll a device or call ordinary RPCs on the destination.
package transferclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/loopback"
	"agent-overflow/internal/transferwire"
)

// Offer travels only in the explicitly authorized Begin call and private
// recovery state. Never serialize it into ordinary transfer status or logs.
type Offer struct {
	OwnershipEpoch  int64  `json:"ownershipEpoch"`
	Version         int    `json:"version"`
	BackendID       string `json:"backendId"`
	OperationID     string `json:"operationId"`
	Endpoint        string `json:"endpoint"`
	CertFingerprint string `json:"certFingerprint,omitempty"`
	Grant           string `json:"grant"`
}

type Client struct {
	offer     Offer
	http      *http.Client
	transport *http.Transport
}

type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "Conversation transfer: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

func New(offer Offer) (*Client, error) {
	if offer.Version != transferwire.Version || offer.OwnershipEpoch < 0 || offer.OwnershipEpoch > transferwire.MaxOwnershipEpoch || !entityid.Valid(offer.BackendID) || !entityid.Valid(offer.OperationID) {
		return nil, &Error{Code: "invalid_offer"}
	}
	if _, err := transferwire.DecodeSecret(offer.Grant); err != nil {
		return nil, &Error{Code: "invalid_offer"}
	}
	if offer.CertFingerprint != "" && (!strings.HasPrefix(offer.CertFingerprint, "sha256:") || !transferwire.ValidDigest(strings.TrimPrefix(offer.CertFingerprint, "sha256:"))) {
		return nil, &Error{Code: "invalid_certificate"}
	}
	endpoint, err := url.Parse(offer.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.Opaque != "" {
		return nil, &Error{Code: "invalid_endpoint"}
	}
	if endpoint.Scheme == "http" && offer.CertFingerprint != "" {
		endpoint.Scheme = "https"
	}
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !loopback.EndpointHostname(endpoint.Hostname())) {
		return nil, &Error{Code: "encrypted_connection_required"}
	}
	if port := endpoint.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, &Error{Code: "invalid_endpoint"}
		}
	}
	offer.Endpoint = endpoint.Scheme + "://" + endpoint.Host
	transport := deviceclient.NewPinnedTransport(offer.CertFingerprint)
	if endpoint.Scheme == "http" {
		transport.Proxy = nil
		transport.DialContext = loopback.Dialer(10 * time.Second)
	}
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.MaxConnsPerHost = 2
	return &Client{offer: offer, transport: transport, http: &http.Client{Transport: transport, Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) Status(ctx context.Context) (transferwire.State, error) {
	return c.call(ctx, "status", http.MethodGet, nil, nil, 0)
}

func (c *Client) BeginUpload(ctx context.Context, upload transferwire.Upload) (transferwire.State, error) {
	return c.control(ctx, "upload", upload)
}

func (c *Client) Prepare(ctx context.Context) (transferwire.State, error) {
	return c.control(ctx, "prepare", struct{}{})
}

func (c *Client) Activate(ctx context.Context, secret string) (transferwire.State, error) {
	if _, err := transferwire.DecodeSecret(secret); err != nil {
		return transferwire.State{}, &Error{Code: "invalid_activation_secret"}
	}
	return c.control(ctx, "activate", transferwire.Activation{Secret: secret})
}

func (c *Client) Cancel(ctx context.Context, secret string) (transferwire.State, error) {
	if _, err := transferwire.DecodeSecret(secret); err != nil {
		return transferwire.State{}, &Error{Code: "invalid_cancellation_secret"}
	}
	return c.control(ctx, "cancel", transferwire.Activation{Secret: secret})
}

// Chunk makes ONE attempt. The coordinator reads the durable offset after an
// unknown outcome; this client never assumes a failed response means no write.
func (c *Client) Chunk(ctx context.Context, offset int64, digest string, data []byte) (transferwire.State, error) {
	size := int64(len(data))
	if offset < 0 || size < 1 || size > transferwire.MaxChunkBytes || offset > transferwire.MaxUploadBytes-size || !transferwire.ValidDigest(digest) {
		return transferwire.State{}, &Error{Code: "invalid_chunk"}
	}
	input := &chunkBody{reader: bytes.NewReader(data)}
	defer input.Close()
	return c.call(ctx, "chunk", http.MethodPut, map[string]string{
		transferwire.OffsetHeader: strconv.FormatInt(offset, 10), transferwire.DigestHeader: digest,
	}, input, size)
}

// net/http may close a request body asynchronously after Do returns. The
// sender reuses its bounded chunk buffer, so synchronously fence any remaining
// body reader before allowing that reuse, including on timeout/early response.
type chunkBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed bool
}

func (b *chunkBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}

func (b *chunkBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (c *Client) control(ctx context.Context, action string, value any) (transferwire.State, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return transferwire.State{}, err
	}
	return c.call(ctx, action, http.MethodPost, nil, bytes.NewReader(body), int64(len(body)))
}

func (c *Client) call(ctx context.Context, action, method string, headers map[string]string, input io.Reader, size int64) (transferwire.State, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.offer.Endpoint+transferwire.PathPrefix+c.offer.OperationID+"/"+action, input)
	if err != nil {
		return transferwire.State{}, &Error{Code: "invalid_request"}
	}
	request.ContentLength = size
	request.Header.Set("Authorization", "Bearer "+c.offer.Grant)
	request.Header.Set(transferwire.VersionHeader, strconv.Itoa(transferwire.Version))
	request.Header.Set(transferwire.BackendHeader, c.offer.BackendID)
	request.Header.Set("Content-Type", "application/json")
	if action == "chunk" {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		code := "connection_failed"
		if errors.Is(err, deviceclient.ErrCertificateMismatch) {
			code = "certificate_changed"
		}
		return transferwire.State{}, &Error{Code: code, cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return transferwire.State{}, &Error{Code: "redirect_refused"}
	}
	if response.StatusCode == http.StatusNotFound {
		return transferwire.State{}, &Error{Code: "offer_unavailable"}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return transferwire.State{}, &Error{Code: "busy"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil {
		return transferwire.State{}, &Error{Code: "response_incomplete", cause: err}
	}
	var reply transferwire.Reply
	if len(data) > 16<<10 || json.Unmarshal(data, &reply) != nil {
		return transferwire.State{}, &Error{Code: "invalid_response"}
	}
	if reply.Version != transferwire.Version {
		return transferwire.State{}, &Error{Code: "unsupported_transfer_version"}
	}
	if reply.BackendID != c.offer.BackendID || reply.OperationID != c.offer.OperationID {
		return transferwire.State{}, &Error{Code: "destination_changed"}
	}
	if reply.Error != "" || response.StatusCode != http.StatusOK {
		code := "transfer_failed"
		switch reply.Error {
		case "transfer_invalid", "transfer_conflict", "transfer_not_ready", "transfer_unavailable", "transfer_timeout", "unsupported_transfer_version":
			code = reply.Error
		}
		return transferwire.State{}, &Error{Code: code}
	}
	if reply.State == nil || !validState(*reply.State) || reply.State.OwnershipEpoch != c.offer.OwnershipEpoch {
		return transferwire.State{}, &Error{Code: "invalid_response"}
	}
	return *reply.State, nil
}

func validState(state transferwire.State) bool {
	switch state.Phase {
	case "preparing", "prepared", "committed", "complete", "canceled":
	default:
		return false
	}
	if state.Size == 0 {
		return (state.Phase == "preparing" || state.Phase == "canceled") && state.Received == 0 && state.SHA256 == ""
	}
	if state.Size < 1024 || state.Size > transferwire.MaxUploadBytes || state.Size%512 != 0 || state.Received < 0 || state.Received > state.Size || !transferwire.ValidDigest(state.SHA256) {
		return false
	}
	return state.Phase == "preparing" || state.Phase == "canceled" || state.Received == state.Size
}
