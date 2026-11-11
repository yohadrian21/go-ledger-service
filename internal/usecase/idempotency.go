package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const maxIdempotencyKeyLength = 255

var (
	ErrIdempotencyKeyMissing = errors.New("an idempotency key is required")
	ErrIdempotencyKeyInvalid = errors.New("idempotency key must be at most 255 characters")
	ErrIdempotencyKeyReused  = errors.New("idempotency key was already used with a different request")
)

func validateIdempotencyKey(key string) error {
	switch {
	case key == "":
		return ErrIdempotencyKeyMissing
	case len(key) > maxIdempotencyKeyLength:
		return ErrIdempotencyKeyInvalid
	}
	return nil
}

// fingerprint identifies a request so that a reused key can be told apart
// from a genuine retry.
func fingerprint(operation string, fields ...any) string {
	b, _ := json.Marshal(append([]any{operation}, fields...))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// reserveKey claims the caller's key for a new resource inside tx. Because
// the key is written in the same transaction as the ledger changes, either
// both commit or neither does: a request that fails leaves the key unused,
// and a request that succeeded can never be applied twice.
//
// If the caller already used the key for the same request, the ID of the
// resource it created is returned with fresh=false. Keys of other clients are
// never visible.
func reserveKey(ctx context.Context, tx Store, caller Principal, key, requestHash string, newID uuid.UUID, now time.Time) (id uuid.UUID, fresh bool, err error) {
	rec, created, err := tx.Idempotency().Reserve(ctx, IdempotencyRecord{
		ClientID:    caller.ClientID,
		Key:         key,
		RequestHash: requestHash,
		ResourceID:  newID,
		CreatedAt:   now,
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	if !created && rec.RequestHash != requestHash {
		return uuid.Nil, false, ErrIdempotencyKeyReused
	}
	return rec.ResourceID, created, nil
}
