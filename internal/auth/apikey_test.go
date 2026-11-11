package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yohadrian21/go-ledger-service/internal/auth"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func newKey(t *testing.T) string {
	t.Helper()
	key, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func entry(t *testing.T, clientID string, role usecase.Role, key string) string {
	t.Helper()
	e, err := auth.Entry(clientID, role, key)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestNewKey(t *testing.T) {
	a, b := newKey(t), newKey(t)
	if a == b {
		t.Fatal("keys must be random")
	}
	if !strings.HasPrefix(a, "lk_") || len(a) != 46 {
		t.Fatalf("unexpected key format: length %d", len(a))
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	aliceKey, aliceRotated, opsKey := newKey(t), newKey(t), newKey(t)
	keys, err := auth.ParseAPIKeys(" " + entry(t, "alice", usecase.RoleClient, aliceKey) + " ,," +
		entry(t, "alice", usecase.RoleClient, aliceRotated) + "," +
		entry(t, "ops", usecase.RoleOperator, opsKey))
	if err != nil {
		t.Fatal(err)
	}
	if keys.Len() != 3 {
		t.Fatalf("Len = %d, want 3", keys.Len())
	}

	tests := []struct {
		name  string
		token string
		want  usecase.Principal
	}{
		{"client key", aliceKey, usecase.Principal{ClientID: "alice", Role: usecase.RoleClient}},
		{"second key of the same client", aliceRotated, usecase.Principal{ClientID: "alice", Role: usecase.RoleClient}},
		{"operator key", opsKey, usecase.Principal{ClientID: "ops", Role: usecase.RoleOperator}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keys.Authenticate(ctx, tt.token)
			if err != nil || got != tt.want {
				t.Fatalf("Authenticate = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}

	modified := []byte(aliceKey)
	modified[len(modified)-1] ^= 1
	for name, token := range map[string]string{
		"empty":       "",
		"unknown key": newKey(t),
		"too short":   aliceKey[:20],
		"too long":    aliceKey + strings.Repeat("x", 300),
		"modified":    string(modified),
	} {
		t.Run(name, func(t *testing.T) {
			if p, err := keys.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidCredentials) || p != (usecase.Principal{}) {
				t.Fatalf("Authenticate = %+v, %v; want ErrInvalidCredentials", p, err)
			}
		})
	}
}

func TestParseAPIKeysRejectsInvalidConfig(t *testing.T) {
	key := newKey(t)
	valid := entry(t, "alice", usecase.RoleClient, key)
	digest := valid[strings.LastIndex(valid, ":")+1:]

	tests := map[string]string{
		"empty":              "",
		"only separators":    " , ,",
		"missing field":      "alice:" + digest,
		"extra field":        "alice:client:x:" + digest,
		"invalid client id":  "al ice:client:" + digest,
		"blank client id":    ":client:" + digest,
		"unknown role":       "alice:admin:" + digest,
		"short digest":       "alice:client:abcd",
		"non-hex digest":     "alice:client:" + strings.Repeat("z", 64),
		"duplicate digest":   valid + ",bob:client:" + digest,
		"conflicting roles":  valid + "," + entry(t, "alice", usecase.RoleOperator, newKey(t)),
		"one invalid of two": valid + ",bob:client:nope",
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := auth.ParseAPIKeys(spec)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), digest) {
				t.Fatalf("error must not echo key material: %v", err)
			}
		})
	}
}

func TestEntryValidatesInput(t *testing.T) {
	key := newKey(t)
	if _, err := auth.Entry("bad id", usecase.RoleClient, key); err == nil {
		t.Error("invalid client id accepted")
	}
	if _, err := auth.Entry("alice", "admin", key); err == nil {
		t.Error("invalid role accepted")
	}
	if _, err := auth.Entry("alice", usecase.RoleClient, "short"); err == nil {
		t.Error("short key accepted")
	}
}
