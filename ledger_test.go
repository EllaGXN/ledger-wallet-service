package main

import (
	"context"
	"database/sql"
	"os"
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
