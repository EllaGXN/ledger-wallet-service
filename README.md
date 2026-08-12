# Double-Entry General Ledger & Wallet Service

A double-entry, immutable financial ledger written in Go, backed by PostgreSQL,
with a wallet layer (deposit / withdraw / transfer) built on top of it. No
wallet balance is ever stored as a number — every balance is derived by
summing ledger entries at query time.

## Running the project

Requires Docker and Docker Compose.

```bash
docker-compose up --build
```

This starts Postgres (applying `migrations/000001_init_schema.up.sql` on
first boot) and the API server on `http://localhost:8080`.

To run the test suite against the same database:

```bash
docker-compose up -d postgres
DATABASE_URL="postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable" go test ./...
```

## API endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/accounts` | Create an account (asset/liability/equity/revenue/expense). |
| `POST` | `/transactions` | Post a raw, arbitrary balanced transaction (2+ entries). |
| `POST` | `/wallets/deposit` | Move funds from a clearing account into a wallet. |
| `POST` | `/wallets/withdraw` | Move funds from a wallet back to a clearing account. |
| `POST` | `/wallets/transfer` | Move funds between two wallets. |
| `GET` | `/accounts/{id}/balance?as_of=RFC3339` | Current balance, or balance as of a given timestamp. |
| `GET` | `/accounts/{id}/history` | Full entry history for an account. |

`deposit`, `withdraw`, and `transfer` all accept an `idempotency_key` field.

## Design decisions

**Immutability.** The `entries` and `transactions` tables have `BEFORE
UPDATE OR DELETE` triggers that raise an exception unconditionally. This
means immutability is a database-level guarantee, not something the
application code has to remember to respect — even a bug or a stray manual
`UPDATE` in `psql` can't silently rewrite history. The only way to correct a
mistake is to post a new, reversing transaction.

**Idempotency.** `transactions.idempotency_key` has a `UNIQUE` constraint,
so two inserts with the same key can never both land. On top of that, the
service layer checks for an existing transaction with the given key *before*
inserting, and returns that original transaction on a repeat request instead
of erroring — so a client that retries a deposit/withdraw/transfer after a
timeout gets the same successful result back rather than a confusing
duplicate-key error. If two retries race each other, the loser of the unique
constraint falls back to looking up and returning the winner's transaction,
so the caller-facing behavior is the same either way.

**Concurrency safety.** Wallet-affecting operations (`withdraw`, `transfer`)
take `SELECT ... FOR UPDATE` row locks on the account(s) involved before
checking the balance, inside the same database transaction that checks it
and posts the entries. Accounts are always locked in a deterministic order
(lowest UUID first) to prevent deadlocks when two transfers involve the same
pair of accounts in opposite directions. Because the balance read and the
lock live in the same `*sql.Tx`, a second concurrent withdrawal against the
same wallet blocks until the first one commits or rolls back, then re-reads
the balance and correctly sees the first one's effect — so two simultaneous
withdrawals that only one can afford will never both succeed. This was
chosen over `SERIALIZABLE` isolation because it's cheaper (no retry loop
needed on serialization failures) and the lock scope is narrow and explicit.

**Money handling.** All amounts are `int64` minor units (cents), never
floating point, both in Go and in Postgres (`BIGINT`). This avoids rounding
errors entirely; a `CHECK (amount > 0)` constraint also rejects zero/negative
entries at the schema level.

**No overdraft.** Before posting a withdrawal or transfer, the sender's
balance is computed (as above, inside the lock) and the operation is
rejected if it would go negative — no negative-balance entry is ever
written.

## Accounting mapping

The wallet is modeled as a **liability** account (the platform owes the
user their balance) that nets against a **clearing/cash asset** account
representing money that has actually moved.

- **Deposit** — debit the clearing asset account (asset increases), credit
  the wallet liability account (liability increases). Money in.
- **Withdrawal** — debit the wallet liability account (liability decreases),
  credit the clearing asset account (asset decreases). Money out.
- **Transfer** — debit the sending wallet (liability decreases), credit the
  receiving wallet (liability increases). No clearing account involved,
  since no money leaves the platform.

In every case debits equal credits, and every wallet balance is just
`SUM(credits) - SUM(debits)` on that wallet's entries (liability accounts
are credit-normal), computed live from the ledger rather than stored.

## Testing

`ledger_test.go` exercises the invariants directly against Postgres:

- `TestUnbalancedTransactionRejected` — debits ≠ credits is rejected.
- `TestHistoricalBalance` — balance as of a past timestamp matches
  entries posted up to that point, not the current total.
- `TestNoOverdraft` — a withdrawal larger than the balance is rejected and
  leaves the balance unchanged.
- `TestIdempotentRetry` — posting the same operation twice with the same
  key returns the original transaction and only one row is ever written.
- `TestConcurrentWithdrawalsOnlyOneSucceeds` — two goroutines racing to
  withdraw the full balance simultaneously: exactly one succeeds, the
  wallet never goes negative.

## Known limitations / deviations

- The suggested schema is followed as-is, with two additions: a
  `created_at` timestamp for insertion order distinct from the caller-set
  `posted_at`, and the immutability triggers.
- There's no `down` migration yet; the schema is additive-only so far.
- Account existence isn't explicitly pre-checked before posting — an
  invalid account ID is caught by the `entries.account_id` foreign key and
  surfaces as a Postgres error. A friendlier pre-check could be added.
