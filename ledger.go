package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

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

// PostTransaction posts balanced double-entry postings atomically.
func (s *Service) PostTransaction(ctx context.Context, txReq Transaction) (*Transaction, error) {
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
		// PostgreSQL unique_violation code 23505 (Idempotency key hit)
		return nil, fmt.Errorf("failed to insert transaction (check idempotency): %w", err)
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

// CalculateBalance derives balance strictly from ledger entries up to a given time.
func (s *Service) CalculateBalance(ctx context.Context, accountID uuid.UUID, asOf time.Time) (int64, error) {
	var accType AccountType
	err := s.db.QueryRowContext(ctx, `SELECT type FROM accounts WHERE id = $1`, accountID).Scan(&accType)
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
	if err := s.db.QueryRowContext(ctx, query, accountID, asOf).Scan(&debits, &credits); err != nil {
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

// Transfer performs a wallet transfer with lock ordering to prevent deadlock and negative balances.
func (s *Service) Transfer(ctx context.Context, idempotencyKey string, fromAccountID, toAccountID uuid.UUID, amount int64) error {
	if amount <= 0 {
		return errors.New("transfer amount must be positive")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Prevent Deadlocks: Always lock accounts in deterministic UUID string order
	firstLock, secondLock := fromAccountID, toAccountID
	if fromAccountID.String() > toAccountID.String() {
		firstLock, secondLock = toAccountID, fromAccountID
	}

	_, err = tx.ExecContext(ctx, `SELECT id FROM accounts WHERE id IN ($1, $2) FOR UPDATE`, firstLock, secondLock)
	if err != nil {
		return fmt.Errorf("failed to lock accounts: %w", err)
	}

	// Verify balance inside lock
	balance, err := s.CalculateBalance(ctx, fromAccountID, time.Now().UTC())
	if err != nil {
		return err
	}
	if balance < amount {
		return fmt.Errorf("insufficient funds: available %d, requested %d", balance, amount)
	}

	// Prepare postings
	txID := uuid.New()
	desc := fmt.Sprintf("Transfer from %s to %s", fromAccountID, toAccountID)

	_, err = tx.ExecContext(ctx, `INSERT INTO transactions (id, idempotency_key, description) VALUES ($1, $2, $3)`, txID, idempotencyKey, desc)
	if err != nil {
		return fmt.Errorf("idempotency check or transaction insert failed: %w", err)
	}

	entryQuery := `INSERT INTO entries (transaction_id, account_id, direction, amount) VALUES ($1, $2, $3, $4)`
	// Debit Sender (Liability decreases)
	if _, err := tx.ExecContext(ctx, entryQuery, txID, fromAccountID, Debit, amount); err != nil {
		return err
	}
	// Credit Receiver (Liability increases)
	if _, err := tx.ExecContext(ctx, entryQuery, txID, toAccountID, Credit, amount); err != nil {
		return err
	}

	return tx.Commit()
}
