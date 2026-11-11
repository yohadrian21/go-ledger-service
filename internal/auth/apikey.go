// Package auth authenticates API clients with API keys.
//
// An API key is a random 256-bit secret that the client sends as a bearer
// token. The service only knows the SHA-256 digest of each key, configured
// together with the client ID and role it grants:
//
//	client_id:role:sha256_hex[,client_id:role:sha256_hex...]
//
// Because keys are high-entropy random values, a fast hash is enough: a
// leaked digest cannot be brute-forced back into a usable key. A client can
// have several keys at once, which allows rotation without downtime.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

const (
	keyPrefix = "lk_"
	// Presented keys outside these bounds are rejected before hashing. The
	// minimum also means that a digest of a short, guessable key can never
	// authenticate anyone.
	minKeyLength = 32
	maxKeyLength = 256
)

// ErrInvalidCredentials is returned for a missing, malformed or unknown key.
var ErrInvalidCredentials = errors.New("invalid API key")

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type apiKey struct {
	digest    [sha256.Size]byte
	principal usecase.Principal
}

// APIKeys verifies API keys against a fixed set of digests.
type APIKeys struct {
	keys []apiKey
}

// ParseAPIKeys parses the AUTH_API_KEYS format described in the package
// documentation. Errors refer to entries by position and never echo their
// content.
func ParseAPIKeys(spec string) (*APIKeys, error) {
	var (
		keys    []apiKey
		roles   = map[string]usecase.Role{}
		digests = map[[sha256.Size]byte]bool{}
	)
	for i, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		pos := i + 1
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("entry %d: want client_id:role:sha256_hex", pos)
		}
		clientID, role := parts[0], usecase.Role(parts[1])
		if !clientIDPattern.MatchString(clientID) {
			return nil, fmt.Errorf("entry %d: client id must be 1-64 letters, digits, '.', '_' or '-'", pos)
		}
		if !role.Valid() {
			return nil, fmt.Errorf("entry %d: role must be %q or %q", pos, usecase.RoleClient, usecase.RoleOperator)
		}
		raw, err := hex.DecodeString(parts[2])
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("entry %d: digest must be 64 hex characters (SHA-256)", pos)
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		if digests[digest] {
			return nil, fmt.Errorf("entry %d: duplicate key digest", pos)
		}
		if prev, ok := roles[clientID]; ok && prev != role {
			return nil, fmt.Errorf("entry %d: client %q is configured with more than one role", pos, clientID)
		}
		digests[digest], roles[clientID] = true, role
		keys = append(keys, apiKey{digest: digest, principal: usecase.Principal{ClientID: clientID, Role: role}})
	}
	if len(keys) == 0 {
		return nil, errors.New("no API keys configured")
	}
	return &APIKeys{keys: keys}, nil
}

// Len returns the number of configured keys.
func (k *APIKeys) Len() int { return len(k.keys) }

// Authenticate returns the client that owns token.
func (k *APIKeys) Authenticate(_ context.Context, token string) (usecase.Principal, error) {
	if len(token) < minKeyLength || len(token) > maxKeyLength {
		return usecase.Principal{}, ErrInvalidCredentials
	}
	digest := sha256.Sum256([]byte(token))
	// Compare against every key without stopping early, so the response time
	// does not depend on which key, if any, matched.
	match := -1
	for i := range k.keys {
		if subtle.ConstantTimeCompare(digest[:], k.keys[i].digest[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return usecase.Principal{}, ErrInvalidCredentials
	}
	return k.keys[match].principal, nil
}

// NewKey returns a new random API key.
func NewKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Entry returns the AUTH_API_KEYS entry that grants key to clientID with role.
func Entry(clientID string, role usecase.Role, key string) (string, error) {
	if !clientIDPattern.MatchString(clientID) {
		return "", errors.New("client id must be 1-64 letters, digits, '.', '_' or '-'")
	}
	if !role.Valid() {
		return "", fmt.Errorf("role must be %q or %q", usecase.RoleClient, usecase.RoleOperator)
	}
	if len(key) < minKeyLength || len(key) > maxKeyLength {
		return "", fmt.Errorf("key must be %d-%d characters", minKeyLength, maxKeyLength)
	}
	digest := sha256.Sum256([]byte(key))
	return clientID + ":" + string(role) + ":" + hex.EncodeToString(digest[:]), nil
}
