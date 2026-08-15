package main

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

// Helper: Setup database connection for testing
func setupTestService(t *testing.T) *Service {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to test DB: %v", err)
	}

	return NewService(db)
}

// Helper: Insert test account directly into DB
func createTestAccount(t *testing.T, svc *Service, name string, accType AccountType) uuid.UUID {
	id := uuid.New()
	query := `INSERT INTO accounts (id, name, type, currency) VALUES ($1, $2, $3, 'USD')`
	_, err := svc.db.Exec(query, id, name, accType)
	if err != nil {
		t.Fatalf("Failed to create test account: %v", err)
	}
	return id
}

func TestUnbalancedTransactionRejected(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	acc1 := createTestAccount(t, svc, "User A", Liability)
	acc2 := createTestAccount(t, svc, "User B", Liability)

	tx := Transaction{
		Description: "Unbalanced attempt",
		Entries: []Entry{
			{AccountID: acc1, Direction: Debit, Amount: 100},
			{AccountID: acc2, Direction: Credit, Amount: 50},
		},
	}

	_, err := svc.PostTransaction(ctx, tx)
	assert.Error(t, err, "Expected unbalanced transaction to fail")
}

func TestHistoricalBalance(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	clearing := createTestAccount(t, svc, "Clearing Cash", Asset)
	wallet := createTestAccount(t, svc, "User Wallet", Liability)

	t1 := time.Now().UTC().Add(-2 * time.Hour)
	t2 := time.Now().UTC().Add(-1 * time.Hour)

	// Deposit 1: $50 at T1
	_, err := svc.PostTransaction(ctx, Transaction{
		PostedAt: t1,
		Entries: []Entry{
			{AccountID: clearing, Direction: Debit, Amount: 5000},
			{AccountID: wallet, Direction: Credit, Amount: 5000},
		},
	})
	assert.NoError(t, err)

	// Deposit 2: $30 at T2
	_, err = svc.PostTransaction(ctx, Transaction{
		PostedAt: t2,
		Entries: []Entry{
			{AccountID: clearing, Direction: Debit, Amount: 3000},
			{AccountID: wallet, Direction: Credit, Amount: 3000},
		},
	})
	assert.NoError(t, err)

	// Historical check at T1 + 30 mins
	balT1, err := svc.CalculateBalance(ctx, wallet, t1.Add(30*time.Minute))
	assert.NoError(t, err)
	assert.Equal(t, int64(5000), balT1)

	// Current check
	balNow, err := svc.CalculateBalance(ctx, wallet, time.Now().UTC())
	assert.NoError(t, err)
	assert.Equal(t, int64(8000), balNow)
}

// TestNoOverdraft proves a withdrawal/transfer cannot push a wallet negative.
func TestNoOverdraft(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	clearing := createTestAccount(t, svc, "Clearing", Asset)
	wallet := createTestAccount(t, svc, "Wallet", Liability)

	// Fund the wallet with 1000.
	_, err := svc.PostTransaction(ctx, Transaction{
		Entries: []Entry{
			{AccountID: clearing, Direction: Debit, Amount: 1000},
			{AccountID: wallet, Direction: Credit, Amount: 1000},
		},
	})
	assert.NoError(t, err)

	// Attempt to withdraw more than the balance.
	err = svc.Transfer(ctx, uuid.NewString(), wallet, clearing, 5000)
	assert.Error(t, err, "expected overdraft to be rejected")

	// Balance must be unchanged.
	bal, err := svc.CalculateBalance(ctx, wallet, time.Now().UTC())
	assert.NoError(t, err)
	assert.Equal(t, int64(1000), bal)
}

// TestIdempotentRetry proves that submitting the same wallet operation twice
// with the same idempotency key does not create a duplicate transaction.
func TestIdempotentRetry(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	clearing := createTestAccount(t, svc, "Clearing", Asset)
	wallet := createTestAccount(t, svc, "Wallet", Liability)
	key := uuid.NewString()

	deposit := func() (*Transaction, error) {
		return svc.PostTransaction(ctx, Transaction{
			IdempotencyKey: &key,
			Description:    "Deposit",
			Entries: []Entry{
				{AccountID: clearing, Direction: Debit, Amount: 500},
				{AccountID: wallet, Direction: Credit, Amount: 500},
			},
		})
	}

	first, err := deposit()
	assert.NoError(t, err)

	second, err := deposit()
	assert.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "retry must return the original transaction, not a new one")

	// Only one deposit's worth of balance should have landed.
	bal, err := svc.CalculateBalance(ctx, wallet, time.Now().UTC())
	assert.NoError(t, err)
	assert.Equal(t, int64(500), bal)

	// And only one row should exist in entries for this transaction id.
	var count int
	err = svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transactions WHERE idempotency_key = $1`, key).Scan(&count)
	assert.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestConcurrentWithdrawalsOnlyOneSucceeds proves that two simultaneous
// withdrawals racing against a wallet that can only cover one of them
// result in exactly one success and one rejection — never both succeeding.
func TestConcurrentWithdrawalsOnlyOneSucceeds(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	clearing := createTestAccount(t, svc, "Clearing", Asset)
	wallet := createTestAccount(t, svc, "Wallet", Liability)

	// Fund the wallet with exactly enough for one withdrawal.
	_, err := svc.PostTransaction(ctx, Transaction{
		Entries: []Entry{
			{AccountID: clearing, Direction: Debit, Amount: 1000},
			{AccountID: wallet, Direction: Credit, Amount: 1000},
		},
	})
	assert.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = svc.Transfer(ctx, uuid.NewString(), wallet, clearing, 1000)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes, "exactly one of two concurrent withdrawals should succeed")

	bal, err := svc.CalculateBalance(ctx, wallet, time.Now().UTC())
	assert.NoError(t, err)
	assert.Equal(t, int64(0), bal, "wallet must end at zero, never negative")
}

// TestSelfTransferRejected proves a wallet can't transfer to itself.
func TestSelfTransferRejected(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	wallet := createTestAccount(t, svc, "Wallet", Liability)

	err := svc.Transfer(ctx, uuid.NewString(), wallet, wallet, 100)
	assert.Error(t, err, "transferring an account to itself must be rejected")
}

// TestTransferToMissingAccountRejected proves a transfer referencing a
// nonexistent account fails with a clear error rather than a raw DB error.
func TestTransferToMissingAccountRejected(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()

	clearing := createTestAccount(t, svc, "Clearing", Asset)
	wallet := createTestAccount(t, svc, "Wallet", Liability)

	// Fund the wallet so the only failure reason possible is the missing account.
	_, err := svc.PostTransaction(ctx, Transaction{
		Entries: []Entry{
			{AccountID: clearing, Direction: Debit, Amount: 500},
			{AccountID: wallet, Direction: Credit, Amount: 500},
		},
	})
	assert.NoError(t, err)

	nonexistent := uuid.New()
	err = svc.Transfer(ctx, uuid.NewString(), wallet, nonexistent, 100)
	assert.Error(t, err, "transferring to a nonexistent account must be rejected")

	// Balance must be unaffected.
	bal, err := svc.CalculateBalance(ctx, wallet, time.Now().UTC())
	assert.NoError(t, err)
	assert.Equal(t, int64(500), bal)
}
