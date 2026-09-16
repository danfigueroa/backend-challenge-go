CREATE TABLE ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL,
    currency             CHAR(3)     NOT NULL,
    transaction_id       UUID        NOT NULL,
    direction            TEXT        NOT NULL,
    amount_minor         BIGINT      NOT NULL,
    balance_before_minor BIGINT      NOT NULL,
    balance_after_minor  BIGINT      NOT NULL,
    wallet_version       BIGINT      NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,
    CONSTRAINT ledger_entries_wallet_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    CONSTRAINT ledger_entries_transaction_fk
        FOREIGN KEY (transaction_id, wallet_id) REFERENCES wager_transactions (id, wallet_id),
    CONSTRAINT ledger_entries_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_entries_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_entries_balances_non_negative CHECK (balance_before_minor >= 0 AND balance_after_minor >= 0),
    CONSTRAINT ledger_entries_version_positive CHECK (wallet_version >= 1),
    CONSTRAINT ledger_entries_arithmetic CHECK (
        (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
        OR (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
    ),
    CONSTRAINT ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT ledger_entries_wallet_version_key UNIQUE (wallet_id, wallet_version)
);

CREATE FUNCTION ledger_entries_forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger entries are append-only (% rejected)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_append_only';
END
$$;

CREATE TRIGGER ledger_entries_forbid_update_delete
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_forbid_mutation();

CREATE TRIGGER ledger_entries_forbid_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_forbid_mutation();

CREATE FUNCTION ledger_entries_check_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    previous_balance BIGINT;
BEGIN
    IF NEW.wallet_version = 1 THEN
        IF NEW.balance_before_minor <> 0 THEN
            RAISE EXCEPTION 'first ledger entry of wallet % must start from zero', NEW.wallet_id
                USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_chain';
        END IF;
        RETURN NEW;
    END IF;

    SELECT balance_after_minor INTO previous_balance
    FROM ledger_entries
    WHERE wallet_id = NEW.wallet_id AND wallet_version = NEW.wallet_version - 1;

    IF NOT FOUND THEN
        IF NEW.wallet_version = 2 AND NEW.balance_before_minor = 0
           AND NOT EXISTS (SELECT 1 FROM ledger_entries WHERE wallet_id = NEW.wallet_id) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'ledger entry version % of wallet % has no predecessor', NEW.wallet_version, NEW.wallet_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_chain';
    END IF;

    IF previous_balance <> NEW.balance_before_minor THEN
        RAISE EXCEPTION 'ledger entry version % of wallet % starts at % but previous entry ended at %',
            NEW.wallet_version, NEW.wallet_id, NEW.balance_before_minor, previous_balance
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_chain';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER ledger_entries_check_chain
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_check_chain();

CREATE FUNCTION ledger_entries_check_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx      wager_transactions%ROWTYPE;
    current wallets%ROWTYPE;
BEGIN
    SELECT * INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;
    IF tx.status <> 'PROCESSED'
       OR tx.amount_minor <> NEW.amount_minor
       OR tx.result_balance_minor <> NEW.balance_after_minor
       OR tx.result_wallet_version <> NEW.wallet_version
       OR (tx.kind = 'BET' AND NEW.direction <> 'DEBIT')
       OR (tx.kind IN ('OPENING', 'WIN', 'REFUND') AND NEW.direction <> 'CREDIT')
       OR tx.kind = 'LOSS' THEN
        RAISE EXCEPTION 'ledger entry % does not match processed transaction %', NEW.id, NEW.transaction_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_transaction_consistency';
    END IF;

    SELECT * INTO current FROM wallets WHERE id = NEW.wallet_id;
    IF current.version < NEW.wallet_version THEN
        RAISE EXCEPTION 'ledger entry % is ahead of wallet % (version % < %)', NEW.id, NEW.wallet_id, current.version, NEW.wallet_version
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_entries_wallet_consistency';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_check_consistency
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_check_consistency();

CREATE FUNCTION wallets_check_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current wallets%ROWTYPE;
    latest  ledger_entries%ROWTYPE;
BEGIN
    SELECT * INTO current FROM wallets WHERE id = NEW.id;

    SELECT * INTO latest FROM ledger_entries
    WHERE wallet_id = NEW.id
    ORDER BY wallet_version DESC
    LIMIT 1;

    IF NOT FOUND THEN
        IF current.version = 1 AND current.balance_minor = 0 THEN
            RETURN NULL;
        END IF;
        RAISE EXCEPTION 'wallet % has balance % at version % without ledger entries', current.id, current.balance_minor, current.version
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_ledger_consistency';
    END IF;

    IF latest.wallet_version <> current.version OR latest.balance_after_minor <> current.balance_minor THEN
        RAISE EXCEPTION 'wallet % (balance %, version %) diverges from its latest ledger entry (balance %, version %)',
            current.id, current.balance_minor, current.version, latest.balance_after_minor, latest.wallet_version
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_ledger_consistency';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER wallets_check_ledger
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_check_ledger();

CREATE FUNCTION wager_transactions_check_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO current FROM wager_transactions WHERE id = NEW.id;
    IF current.status = 'PROCESSED' AND current.kind <> 'LOSS'
       AND NOT EXISTS (SELECT 1 FROM ledger_entries WHERE transaction_id = current.id) THEN
        RAISE EXCEPTION 'processed % transaction % has no ledger entry', current.kind, current.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_ledger_consistency';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER wager_transactions_check_ledger
    AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_check_ledger();

GRANT SELECT, INSERT ON ledger_entries TO wallet_app;
