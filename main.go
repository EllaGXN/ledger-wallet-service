package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

type Server struct {
	svc *Service
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Database unreachable: %v", err)
	}

	server := &Server{svc: NewService(db)}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /accounts", server.handleCreateAccount)
	mux.HandleFunc("POST /transactions", server.handlePostTransaction)
	mux.HandleFunc("POST /wallets/deposit", server.handleDeposit)
	mux.HandleFunc("POST /wallets/withdraw", server.handleWithdraw)
	mux.HandleFunc("POST /wallets/transfer", server.handleTransfer)
	mux.HandleFunc("GET /accounts/{id}/balance", server.handleGetBalance)
	mux.HandleFunc("GET /accounts/{id}/history", server.handleGetHistory)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server listening on port %s...", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// HTTP Request payloads
type CreateAccountReq struct {
	Name     string      `json:"name"`
	Type     AccountType `json:"type"`
	Currency string      `json:"currency"`
}

type WalletOpReq struct {
	WalletID       uuid.UUID `json:"wallet_id"`
	ClearingID     uuid.UUID `json:"clearing_id"`
	Amount         int64     `json:"amount"`
	IdempotencyKey string    `json:"idempotency_key"`
}

type TransferReq struct {
	FromWalletID   uuid.UUID `json:"from_wallet_id"`
	ToWalletID     uuid.UUID `json:"to_wallet_id"`
	Amount         int64     `json:"amount"`
	IdempotencyKey string    `json:"idempotency_key"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req CreateAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	acc := Account{
		ID:        uuid.New(),
		Name:      req.Name,
		Type:      req.Type,
		Currency:  req.Currency,
		CreatedAt: time.Now().UTC(),
	}

	query := `INSERT INTO accounts (id, name, type, currency, created_at) VALUES ($1, $2, $3, $4, $5)`
	_, err := s.svc.db.ExecContext(r.Context(), query, acc.ID, acc.Name, acc.Type, acc.Currency, acc.CreatedAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(acc)
}

func (s *Server) handlePostTransaction(w http.ResponseWriter, r *http.Request) {
	var tx Transaction
	if err := json.NewDecoder(r.Body).Decode(&tx); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	res, err := s.svc.PostTransaction(r.Context(), tx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(res)
}

func (s *Server) handleDeposit(w http.ResponseWriter, r *http.Request) {
	var req WalletOpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Deposit: Debit Asset (Clearing), Credit Liability (Wallet)
	tx := Transaction{
		IdempotencyKey: &req.IdempotencyKey,
		Description:    "Wallet Deposit",
		Entries: []Entry{
			{AccountID: req.ClearingID, Direction: Debit, Amount: req.Amount},
			{AccountID: req.WalletID, Direction: Credit, Amount: req.Amount},
		},
	}

	res, err := s.svc.PostTransaction(r.Context(), tx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	var req WalletOpReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Lock sender wallet and verify non-negative balance before posting
	err := s.svc.Transfer(r.Context(), req.IdempotencyKey, req.WalletID, req.ClearingID, req.Amount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"success"}`))
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req TransferReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := s.svc.Transfer(r.Context(), req.IdempotencyKey, req.FromWalletID, req.ToWalletID, req.Amount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"success"}`))
}

func (s *Server) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	accID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}

	asOf := time.Now().UTC()
	if asOfQuery := r.URL.Query().Get("as_of"); asOfQuery != "" {
		if parsedTime, err := time.Parse(time.RFC3339, asOfQuery); err == nil {
			asOf = parsedTime
		}
	}

	balance, err := s.svc.CalculateBalance(r.Context(), accID, asOf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"account_id": accID,
		"balance":    balance,
		"as_of":      asOf,
	})
}

func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	accID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}

	query := `
		SELECT e.id, e.transaction_id, e.direction, e.amount, e.created_at, t.description 
		FROM entries e
		JOIN transactions t ON e.transaction_id = t.id
		WHERE e.account_id = $1
		ORDER BY e.created_at DESC
	`
	rows, err := s.svc.db.QueryContext(r.Context(), query, accID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type HistoryItem struct {
		EntryID       uuid.UUID `json:"entry_id"`
		TxID          uuid.UUID `json:"transaction_id"`
		Direction     Direction `json:"direction"`
		Amount        int64     `json:"amount"`
		CreatedAt     time.Time `json:"created_at"`
		TxDescription string    `json:"description"`
	}

	var history []HistoryItem
	for rows.Next() {
		var item HistoryItem
		if err := rows.Scan(&item.EntryID, &item.TxID, &item.Direction, &item.Amount, &item.CreatedAt, &item.TxDescription); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		history = append(history, item)
	}

	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}
