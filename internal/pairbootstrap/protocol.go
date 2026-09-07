// Package pairbootstrap delivers an ordinary pairing invitation through a
// committed Diffie-Hellman exchange. It owns no device or session credentials.
package pairbootstrap

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

const Version = 1

var (
	ErrClosed  = errors.New("On the other computer, open Remote access → Allow device access → Allow a device to connect, then try again")
	ErrBusy    = errors.New("Another device is pairing; finish or cancel it on the other computer")
	ErrInvalid = errors.New("Pairing verification failed; cancel and try again")
)

type Commitment struct {
	Version    int    `json:"version"`
	Commitment string `json:"commitment"`
}

type Challenge struct {
	ID          string `json:"id"`
	PublicKey   string `json:"publicKey"`
	Nonce       string `json:"nonce"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

type Reveal struct {
	PublicKey string `json:"publicKey"`
	Nonce     string `json:"nonce"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
}

type Invitation struct {
	LinkID string `json:"linkId"`
	URL    string `json:"url"`
}

type SealedInvitation struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Client freezes its response before revealing the committed material. Reusing
// one client with a different challenge would let a relay choose its transcript
// after learning that material and grind the short comparison value offline.
type Client struct {
	key        *ecdh.PrivateKey
	reveal     Reveal
	commitment Commitment
	challenge  *Challenge
	aead       cipher.AEAD
	transcript []byte
	sas        string
}

func Start(label, platform string) (*Client, error) {
	if len(label) > 128 || len(platform) > 128 {
		return nil, ErrInvalid
	}
	key, nonce, err := ephemeral()
	if err != nil {
		return nil, err
	}
	r := Reveal{PublicKey: encode(key.PublicKey().Bytes()), Nonce: nonce, Label: label, Platform: platform}
	return &Client{key: key, reveal: r, commitment: commit(r)}, nil
}

func (c *Client) Commitment() Commitment { return c.commitment }

func (c *Client) Reveal(ch Challenge) (Reveal, error) {
	if c.challenge != nil {
		if *c.challenge != ch {
			return Reveal{}, ErrInvalid
		}
		return c.reveal, nil
	}
	aead, transcript, sas, err := derive(c.key, ch.PublicKey, c.commitment, ch, c.reveal)
	if err != nil {
		return Reveal{}, err
	}
	c.challenge, c.aead, c.transcript, c.sas = &ch, aead, transcript, sas
	return c.reveal, nil
}

func (c *Client) Open(sealed SealedInvitation) (Invitation, string, error) {
	if c.aead == nil {
		return Invitation{}, "", ErrInvalid
	}
	nonce, err := decode(sealed.Nonce, c.aead.NonceSize())
	if err != nil || len(sealed.Ciphertext) > 32768 {
		return Invitation{}, "", ErrInvalid
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return Invitation{}, "", ErrInvalid
	}
	plain, err := c.aead.Open(nil, nonce, ciphertext, c.transcript)
	if err != nil {
		return Invitation{}, "", ErrInvalid
	}
	var invite Invitation
	if json.Unmarshal(plain, &invite) != nil || invite.LinkID == "" || invite.URL == "" {
		return Invitation{}, "", ErrInvalid
	}
	return invite, c.sas, nil
}

func ephemeral() (*ecdh.PrivateKey, string, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return key, encode(random(32)), nil
}

func random(n int) []byte    { b := make([]byte, n); rand.Read(b); return b }
func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string, n int) ([]byte, error) {
	if len(s) != base64.RawURLEncoding.EncodedLen(n) {
		return nil, ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != n {
		return nil, ErrInvalid
	}
	return b, nil
}

func commit(r Reveal) Commitment {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(append([]byte("agent-overflow/pair-bootstrap/commit/1\x00"), b...))
	return Commitment{Version: Version, Commitment: encode(h[:])}
}

func derive(key *ecdh.PrivateKey, peer string, co Commitment, ch Challenge, r Reveal) (cipher.AEAD, []byte, string, error) {
	if co.Version != Version || len(r.Label) > 128 || len(r.Platform) > 128 || ch.ExpiresAtMs <= 0 {
		return nil, nil, "", ErrInvalid
	}
	if _, err := decode(ch.ID, 16); err != nil {
		return nil, nil, "", err
	}
	if _, err := decode(ch.Nonce, 32); err != nil {
		return nil, nil, "", err
	}
	if _, err := decode(r.Nonce, 32); err != nil {
		return nil, nil, "", err
	}
	pub, err := decode(peer, 65)
	if err != nil {
		return nil, nil, "", err
	}
	pk, err := ecdh.P256().NewPublicKey(pub)
	if err != nil {
		return nil, nil, "", ErrInvalid
	}
	shared, err := key.ECDH(pk)
	if err != nil {
		return nil, nil, "", ErrInvalid
	}
	b, _ := json.Marshal(struct {
		Commitment Commitment
		Challenge  Challenge
		Reveal     Reveal
	}{co, ch, r})
	h := sha256.Sum256(b)
	material, err := hkdf.Key(sha256.New, shared, h[:], "agent-overflow/pair-bootstrap/keys/1", 40)
	if err != nil {
		return nil, nil, "", err
	}
	block, err := aes.NewCipher(material[:32])
	if err != nil {
		return nil, nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, "", err
	}
	sas := fmt.Sprintf("%06d", binary.BigEndian.Uint64(material[32:])%1000000)
	return aead, h[:], sas, nil
}
