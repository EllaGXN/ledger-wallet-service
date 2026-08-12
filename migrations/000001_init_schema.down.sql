DROP TRIGGER IF EXISTS no_update_delete_transactions ON transactions;
DROP TRIGGER IF EXISTS no_update_delete_entries ON entries;
DROP FUNCTION IF EXISTS prevent_ledger_modification();

DROP INDEX IF EXISTS idx_entries_transaction;
DROP INDEX IF EXISTS idx_entries_account_created;

DROP TABLE IF EXISTS entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;

DROP TYPE IF EXISTS entry_direction;
DROP TYPE IF EXISTS account_type;
