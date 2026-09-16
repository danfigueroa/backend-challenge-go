DROP TRIGGER wager_transactions_check_ledger ON wager_transactions;
DROP FUNCTION wager_transactions_check_ledger();
DROP TRIGGER wallets_check_ledger ON wallets;
DROP FUNCTION wallets_check_ledger();
DROP TABLE ledger_entries;
DROP FUNCTION ledger_entries_check_consistency();
DROP FUNCTION ledger_entries_check_chain();
DROP FUNCTION ledger_entries_forbid_mutation();
