// Package memory is an in-memory implementation of the usecase repositories,
// used by unit tests. A single mutex serialises transactions, which gives
// full isolation; a transaction works on a copy of the data that replaces
// the committed state only if it succeeds.
package memory

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type Store struct {
	mu        *sync.Mutex
	committed *state
	tx        *state // set inside InTx
}

type state struct {
	accounts     map[uuid.UUID]domain.Account
	transactions map[uuid.UUID]domain.Transaction
	entries      []entryRow
	idempotency  map[idempotencyKey]usecase.IdempotencyRecord
	outbox       []outboxRow
	lastEntryID  int64
}

type idempotencyKey struct {
	clientID, key string
}

type entryRow struct {
	accountID uuid.UUID
	line      domain.StatementLine
}

type outboxRow struct {
	event       domain.Event
	publishedAt time.Time // zero while pending
}

var _ usecase.Store = (*Store)(nil)

func New() *Store {
	return &Store{
		mu: &sync.Mutex{},
		committed: &state{
			accounts:     map[uuid.UUID]domain.Account{},
			transactions: map[uuid.UUID]domain.Transaction{},
			idempotency:  map[idempotencyKey]usecase.IdempotencyRecord{},
		},
	}
}

func (s *state) clone() *state {
	return &state{
		accounts:     maps.Clone(s.accounts),
		transactions: maps.Clone(s.transactions),
		entries:      slices.Clone(s.entries),
		idempotency:  maps.Clone(s.idempotency),
		outbox:       slices.Clone(s.outbox),
		lastEntryID:  s.lastEntryID,
	}
}

func (s *Store) InTx(_ context.Context, fn func(tx usecase.Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.committed.clone()
	if err := fn(&Store{mu: s.mu, committed: s.committed, tx: work}); err != nil {
		return err
	}
	*s.committed = *work
	return nil
}

// with runs f against the transaction's working copy, or against the
// committed state when called outside a transaction.
func (s *Store) with(f func(st *state) error) error {
	if s.tx != nil {
		return f(s.tx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return f(s.committed)
}

func (s *Store) Accounts() usecase.AccountRepository         { return accounts{s} }
func (s *Store) Transactions() usecase.TransactionRepository { return transactions{s} }
func (s *Store) Idempotency() usecase.IdempotencyRepository  { return idempotency{s} }
func (s *Store) Outbox() usecase.OutboxRepository            { return outbox{s} }

type accounts struct{ s *Store }

func (r accounts) Create(_ context.Context, a domain.Account) error {
	return r.s.with(func(st *state) error {
		if _, ok := st.accounts[a.ID]; ok {
			return errors.New("memory: duplicate account id")
		}
		st.accounts[a.ID] = a
		return nil
	})
}

func (r accounts) EnsureExternal(_ context.Context, a domain.Account) error {
	return r.s.with(func(st *state) error {
		if _, ok := external(st, a.Currency); !ok {
			st.accounts[a.ID] = a
		}
		return nil
	})
}

func (r accounts) Get(_ context.Context, id uuid.UUID) (domain.Account, error) {
	var acc domain.Account
	err := r.s.with(func(st *state) error {
		a, ok := st.accounts[id]
		if !ok || a.Kind != domain.AccountKindCustomer {
			return domain.ErrAccountNotFound
		}
		acc = a
		return nil
	})
	return acc, err
}

func (r accounts) GetForUpdate(_ context.Context, ids ...uuid.UUID) ([]domain.Account, error) {
	var out []domain.Account
	err := r.s.with(func(st *state) error {
		for _, id := range ids {
			a, ok := st.accounts[id]
			if ok && a.Kind == domain.AccountKindCustomer && !slices.ContainsFunc(out, func(x domain.Account) bool { return x.ID == id }) {
				out = append(out, a)
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b domain.Account) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out, err
}

func (r accounts) GetExternalForUpdate(_ context.Context, currency domain.Currency) (domain.Account, error) {
	var acc domain.Account
	err := r.s.with(func(st *state) error {
		a, ok := external(st, currency)
		if !ok {
			return domain.ErrAccountNotFound
		}
		acc = a
		return nil
	})
	return acc, err
}

func (r accounts) UpdateBalances(_ context.Context, list ...domain.Account) error {
	return r.s.with(func(st *state) error {
		for _, a := range list {
			cur, ok := st.accounts[a.ID]
			if !ok {
				return domain.ErrAccountNotFound
			}
			cur.Balance, cur.UpdatedAt = a.Balance, a.UpdatedAt
			st.accounts[a.ID] = cur
		}
		return nil
	})
}

func external(st *state, currency domain.Currency) (domain.Account, bool) {
	for _, a := range st.accounts {
		if a.Kind == domain.AccountKindExternal && a.Currency == currency {
			return a, true
		}
	}
	return domain.Account{}, false
}

type transactions struct{ s *Store }

func (r transactions) Create(_ context.Context, t domain.Transaction) error {
	return r.s.with(func(st *state) error {
		st.transactions[t.ID] = t
		for _, e := range t.Entries {
			st.lastEntryID++
			st.entries = append(st.entries, entryRow{
				accountID: e.AccountID,
				line: domain.StatementLine{
					EntryID:       st.lastEntryID,
					TransactionID: t.ID,
					Type:          t.Type,
					Amount:        e.Amount,
					BalanceAfter:  e.BalanceAfter,
					Currency:      t.Currency,
					CreatedAt:     t.CreatedAt,
				},
			})
		}
		return nil
	})
}

func (r transactions) Get(_ context.Context, id uuid.UUID) (domain.Transaction, error) {
	var t domain.Transaction
	err := r.s.with(func(st *state) error {
		var ok bool
		if t, ok = st.transactions[id]; !ok {
			return domain.ErrTransactionNotFound
		}
		return nil
	})
	return t, err
}

func (r transactions) ListByAccount(_ context.Context, accountID uuid.UUID, before int64, limit int) ([]domain.StatementLine, error) {
	var out []domain.StatementLine
	err := r.s.with(func(st *state) error {
		for i := len(st.entries) - 1; i >= 0 && len(out) < limit; i-- {
			e := st.entries[i]
			if e.accountID == accountID && e.line.EntryID < before {
				out = append(out, e.line)
			}
		}
		return nil
	})
	return out, err
}

type idempotency struct{ s *Store }

func (r idempotency) Reserve(_ context.Context, rec usecase.IdempotencyRecord) (usecase.IdempotencyRecord, bool, error) {
	created := false
	err := r.s.with(func(st *state) error {
		k := idempotencyKey{clientID: rec.ClientID, key: rec.Key}
		if existing, ok := st.idempotency[k]; ok {
			rec = existing
			return nil
		}
		st.idempotency[k] = rec
		created = true
		return nil
	})
	return rec, created, err
}

func (r idempotency) DeleteExpired(_ context.Context, createdBefore time.Time, limit int) (int, error) {
	deleted := 0
	err := r.s.with(func(st *state) error {
		for k, rec := range st.idempotency {
			if deleted == limit {
				break
			}
			if rec.CreatedAt.Before(createdBefore) {
				delete(st.idempotency, k)
				deleted++
			}
		}
		return nil
	})
	return deleted, err
}

type outbox struct{ s *Store }

func (r outbox) Add(_ context.Context, events ...domain.Event) error {
	return r.s.with(func(st *state) error {
		for _, e := range events {
			st.outbox = append(st.outbox, outboxRow{event: e})
		}
		return nil
	})
}

func (r outbox) FetchPending(_ context.Context, limit int) ([]domain.Event, error) {
	var out []domain.Event
	err := r.s.with(func(st *state) error {
		for _, row := range st.outbox {
			if len(out) == limit {
				break
			}
			if row.publishedAt.IsZero() {
				out = append(out, row.event)
			}
		}
		return nil
	})
	return out, err
}

func (r outbox) MarkPublished(_ context.Context, ids []uuid.UUID, at time.Time) error {
	return r.s.with(func(st *state) error {
		for i := range st.outbox {
			if slices.Contains(ids, st.outbox[i].event.ID) {
				st.outbox[i].publishedAt = at
			}
		}
		return nil
	})
}

func (r outbox) DeletePublished(_ context.Context, publishedBefore time.Time, limit int) (int, error) {
	deleted := 0
	err := r.s.with(func(st *state) error {
		st.outbox = slices.DeleteFunc(st.outbox, func(row outboxRow) bool {
			if deleted == limit || row.publishedAt.IsZero() || !row.publishedAt.Before(publishedBefore) {
				return false
			}
			deleted++
			return true
		})
		return nil
	})
	return deleted, err
}
