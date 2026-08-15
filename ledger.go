package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// isUniqueViolation reports whether err is a Postgres unique_violation (23505),
// which is how we detect a duplicate idempotency key.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Equity    AccountType = "equity"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

type Account struct {
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	Type      AccountType `json:"type"`
	Currency  string      `json:"currency"`
	CreatedAt time.Time   `json:"created_at"`
}

type Entry struct {
	AccountID uuid.UUID `json:"account_id"`
	Direction Direction `json:"direction"`
	Amount    int64     `json:"amount"` // Minor units (e.g. cents)
}

type Transaction struct {
	ID             uuid.UUID `json:"id"`
	IdempotencyKey *string   `json:"idempotency_key,omitempty"`
	Description    string    `json:"description"`
	PostedAt       time.Time `json:"posted_at"`
	Entries        []Entry   `json:"entries"`
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// getByIdempotencyKey returns the previously-posted transaction (with entries)
// for a given idempotency key, or nil if none exists.
func (s *Service) getByIdempotencyKey(ctx context.Context, key string) (*Transaction, error) {
	var tx Transaction
	var k sql.NullString
	row := s.db.QueryRowContext(ctx,
		`SELECT id, idempotency_key, description, posted_at FROM transactions WHERE idempotency_key = $1`, key)
	if err := row.Scan(&tx.ID, &k, &tx.Description, &tx.PostedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if k.Valid {
		tx.IdempotencyKey = &k.String
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, direction, amount FROM entries WHERE transaction_id = $1`, tx.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.AccountID, &e.Direction, &e.Amount); err != nil {
			return nil, err
		}
		tx.Entries = append(tx.Entries, e)
	}
	return &tx, rows.Err()
}

// PostTransaction posts balanced double-entry postings atomically. If the
// request carries an idempotency key that was already used, the original
// transaction is returned instead of creating a duplicate or erroring out —
// this is what makes wallet operations safe to retry.
func (s *Service) PostTransaction(ctx context.Context, txReq Transaction) (*Transaction, error) {
	if txReq.IdempotencyKey != nil && *txReq.IdempotencyKey != "" {
		existing, err := s.getByIdempotencyKey(ctx, *txReq.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	// 1. Validate Double-Entry Invariant: Sum(Debits) == Sum(Credits)
	var totalDebits, totalCredits int64
	for _, e := range txReq.Entries {
		if e.Amount <= 0 {
			return nil, errors.New("entry amount must be greater than zero")
		}
		switch e.Direction {
		case Debit:
			totalDebits += e.Amount
		case Credit:
			totalCredits += e.Amount
		}
	}
	if totalDebits != totalCredits {
		return nil, fmt.Errorf("unbalanced transaction: total debits (%d) != total credits (%d)", totalDebits, totalCredits)
	}

	// 2. Execute DB Transaction
	dbtx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer dbtx.Rollback()

	if txReq.ID == uuid.Nil {
		txReq.ID = uuid.New()
	}
	if txReq.PostedAt.IsZero() {
		txReq.PostedAt = time.Now().UTC()
	}

	// Insert Transaction
	queryTx := `INSERT INTO transactions (id, idempotency_key, description, posted_at) VALUES ($1, $2, $3, $4)`
	_, err = dbtx.ExecContext(ctx, queryTx, txReq.ID, txReq.IdempotencyKey, txReq.Description, txReq.PostedAt)
	if err != nil {
		if isUniqueViolation(err) && txReq.IdempotencyKey != nil {
			// Lost a race with a concurrent retry using the same key: the other
			// request's transaction now exists, so return that instead of erroring.
			dbtx.Rollback()
			existing, lookupErr := s.getByIdempotencyKey(ctx, *txReq.IdempotencyKey)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("failed to insert transaction: %w", err)
	}

	// Insert Entries
	queryEntry := `INSERT INTO entries (transaction_id, account_id, direction, amount) VALUES ($1, $2, $3, $4)`
	for _, entry := range txReq.Entries {
		_, err := dbtx.ExecContext(ctx, queryEntry, txReq.ID, entry.AccountID, entry.Direction, entry.Amount)
		if err != nil {
			return nil, fmt.Errorf("failed to insert entry: %w", err)
		}
	}

	if err := dbtx.Commit(); err != nil {
		return nil, err
	}

	return &txReq, nil
}

// dbtx is satisfied by both *sql.DB and *sql.Tx, letting balance calculation
// run either standalone or inside an existing transaction/row lock.
type dbtx interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// CalculateBalance derives balance strictly from ledger entries up to a given time.
func (s *Service) CalculateBalance(ctx context.Context, accountID uuid.UUID, asOf time.Time) (int64, error) {
	return calculateBalance(ctx, s.db, accountID, asOf)
}

// calculateBalanceTx is CalculateBalance scoped to an in-flight *sql.Tx, so the
// read happens against exactly the state the caller's row lock is holding.
func (s *Service) calculateBalanceTx(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, asOf time.Time) (int64, error) {
	return calculateBalance(ctx, tx, accountID, asOf)
}

func calculateBalance(ctx context.Context, q dbtx, accountID uuid.UUID, asOf time.Time) (int64, error) {
	var accType AccountType
	err := q.QueryRowContext(ctx, `SELECT type FROM accounts WHERE id = $1`, accountID).Scan(&accType)
	if err != nil {
		return 0, fmt.Errorf("account not found: %w", err)
	}

	query := `
		SELECT 
			COALESCE(SUM(CASE WHEN direction = 'debit' THEN amount ELSE 0 END), 0) as debits,
			COALESCE(SUM(CASE WHEN direction = 'credit' THEN amount ELSE 0 END), 0) as credits
		FROM entries e
		JOIN transactions t ON e.transaction_id = t.id
		WHERE e.account_id = $1 AND t.posted_at <= $2
	`
	var debits, credits int64
	if err := q.QueryRowContext(ctx, query, accountID, asOf).Scan(&debits, &credits); err != nil {
		return 0, err
	}

	// Compute based on Account Type Rules
	switch accType {
	case Asset, Expense:
		return debits - credits, nil
	case Liability, Equity, Revenue:
		return credits - debits, nil
	default:
		return 0, errors.New("unknown account type")
	}
}

// Transfer performs a wallet transfer with lock ordering to prevent deadlock,
// negative balances, and duplicate postings on retry.
func (s *Service) Transfer(ctx context.Context, idempotencyKey string, fromAccountID, toAccountID uuid.UUID, amount int64) error {
	if amount <= 0 {
		return errors.New("transfer amount must be positive")
	}
	if fromAccountID == toAccountID {
		return errors.New("cannot transfer to the same account")
	}

	// Idempotency check happens before we touch locks: a retried request with
	// a key we've already committed is a no-op success, not an error.
	if idempotencyKey != "" {
		existing, err := s.getByIdempotencyKey(ctx, idempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Prevent deadlocks: always lock accounts in deterministic UUID string order.
	firstLock, secondLock := fromAccountID, toAccountID
	if fromAccountID.String() > toAccountID.String() {
		firstLock, secondLock = toAccountID, fromAccountID
	}

	_, err = tx.ExecContext(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE`, firstLock, secondLock)
	if err != nil {
		return fmt.Errorf("failed to lock accounts: %w", err)
	}

	// Verify balance INSIDE this transaction (via tx, not s.db) so the check
	// is against the exact state the row lock is holding — a concurrent
	// transfer blocked on the same lock can't sneak a stale read in between.
	balance, err := s.calculateBalanceTx(ctx, tx, fromAccountID, time.Now().UTC())
	if err != nil {
		return err
	}
	if balance < amount {
		return fmt.Errorf("insufficient funds: available %d, requested %d", balance, amount)
	}

	// Prepare postings
	txID := uuid.New()
	desc := fmt.Sprintf("Transfer from %s to %s", fromAccountID, toAccountID)

	var key *string
	if idempotencyKey != "" {
		key = &idempotencyKey
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transactions (id, idempotency_key, description) VALUES ($1, $2, $3)`, txID, key, desc)
	if err != nil {
		if isUniqueViolation(err) {
			// Lost a race with a concurrent retry of the same key: the other
			// request already committed an equivalent transfer, so treat this
			// as a successful no-op rather than surfacing an error.
			return nil
		}
		return fmt.Errorf("failed to insert transaction: %w", err)
	}

	entryQuery := `INSERT INTO entries (transaction_id, account_id, direction, amount) VALUES ($1, $2, $3, $4)`
	// Debit sender (liability decreases)
	if _, err := tx.ExecContext(ctx, entryQuery, txID, fromAccountID, Debit, amount); err != nil {
		return err
	}
	// Credit receiver (liability increases)
	if _, err := tx.ExecContext(ctx, entryQuery, txID, toAccountID, Credit, amount); err != nil {
		return err
	}

	return tx.Commit()
}
