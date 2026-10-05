package push

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

// Credential is a Google service-account key, reduced to the fields a send
// needs plus the raw bytes the token exchange is built from.
//
// SHAPE ONLY, VALIDATED WITHOUT A NETWORK. `ParseCredential` answers
// whether this document could possibly be a service-account key — the right
// type, a project, an account, and a private key that actually parses — and
// nothing more. Whether Google ACCEPTS it is a question only a real send can
// ask, and the answer to that one is reported through the sender status the
// owner reads, not through the paste field. Reaching the network from a
// setter would also put a network call in `make go-test`, which is banned.
type Credential struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`

	// raw is the document as pasted. Kept because the OAuth flow is built
	// from the whole key file (`google.JWTConfigFromJSON`), and re-encoding
	// the parsed fields would drop anything a future key format added.
	raw []byte
}

// Raw is the credential document as it was pasted. Backend-local, stored
// beside the signing keys and never on a read wire shape.
func (c Credential) Raw() []byte { return c.raw }

// ParseCredential reads and shape-checks a service-account key.
func ParseCredential(raw []byte) (Credential, error) {
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return Credential{}, fmt.Errorf("push: that is not a JSON key file: %w", err)
	}
	if cred.Type != "service_account" {
		return Credential{}, fmt.Errorf(
			"push: a sender credential must be a service account key, not %q", cred.Type)
	}
	if cred.ProjectID == "" {
		return Credential{}, errors.New("push: the key file names no project_id")
	}
	if cred.ClientEmail == "" {
		return Credential{}, errors.New("push: the key file names no client_email")
	}
	if _, err := parsePrivateKey(cred.PrivateKey); err != nil {
		return Credential{}, err
	}
	cred.raw = bytes.Clone(raw)
	return cred, nil
}

// parsePrivateKey is the parseability half of the shape check: the same PEM
// and PKCS#1/PKCS#8 pair the OAuth library will do at token time, done now
// so a typo in the paste field is refused where a person can see it rather
// than an hour later on the first turn that completes.
func parsePrivateKey(key string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, errors.New("push: the key file's private_key is not PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("push: the key file's private_key is not an RSA key")
		}
		return rsaKey, nil
	}
	parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("push: the key file's private_key did not parse: %w", err)
	}
	return parsed, nil
}
