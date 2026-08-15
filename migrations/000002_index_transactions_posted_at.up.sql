-- The historical/current balance query filters on transactions.posted_at
-- (see calculateBalance in ledger.go), but the only index touching
-- transactions was its primary key. Every balance lookup was doing a
-- sequential scan over transactions to apply that filter. This index makes
-- that filter, and therefore every balance query, an index scan instead.
CREATE INDEX idx_transactions_posted_at ON transactions(posted_at);
