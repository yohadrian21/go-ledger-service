package usecase

import (
	"errors"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

// Role determines what an authenticated client may do.
type Role string

const (
	// RoleClient may open accounts, read its own accounts and transfer money
	// out of them.
	RoleClient Role = "client"
	// RoleOperator is a trusted back-office role. In addition to what a
	// client may do, it can deposit into and withdraw from any customer
	// account and read any account.
	RoleOperator Role = "operator"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return r == RoleClient || r == RoleOperator
}

// ErrForbidden is returned when the caller is authenticated but its role does
// not allow the operation.
var ErrForbidden = errors.New("not allowed to perform this operation")

// Principal is the authenticated API client a use case runs on behalf of.
type Principal struct {
	ClientID string
	Role     Role
}

func (p Principal) valid() bool {
	return p.ClientID != "" && p.Role.Valid()
}

func (p Principal) isOperator() bool {
	return p.valid() && p.Role == RoleOperator
}

// canRead reports whether p may see acc. Accounts the caller may not see are
// reported as not found, so their IDs cannot be probed.
func (p Principal) canRead(acc domain.Account) bool {
	return p.isOperator() || (p.valid() && acc.OwnedBy(p.ClientID))
}

func requireCaller(p Principal) error {
	if !p.valid() {
		return ErrForbidden
	}
	return nil
}

func requireOperator(p Principal) error {
	if !p.isOperator() {
		return ErrForbidden
	}
	return nil
}
