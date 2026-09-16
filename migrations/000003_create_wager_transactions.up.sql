CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL,
    kind                              TEXT        NOT NULL,
    status                            TEXT        NOT NULL,
    wallet_id                         UUID        NOT NULL,
    player_id                         UUID        NOT NULL,
    currency                          CHAR(3)     NOT NULL,
    amount_minor                      BIGINT      NOT NULL,
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      BYTEA,
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID,
    failure_code                      TEXT,
    result_balance_minor              BIGINT,
    result_wallet_version             BIGINT,
    attempts                          INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at                   TIMESTAMPTZ,
    expires_at                        TIMESTAMPTZ,
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,
    completed_at                      TIMESTAMPTZ,
    CONSTRAINT wager_transactions_wallet_fk
        FOREIGN KEY (wallet_id, player_id, currency) REFERENCES wallets (id, player_id, currency),
    CONSTRAINT wager_transactions_reference_fk
        FOREIGN KEY (reference_transaction_id) REFERENCES wager_transactions (id),
    CONSTRAINT wager_transactions_origin_valid CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wager_transactions_kind_valid CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_transactions_status_valid CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wager_transactions_amount_policy CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    CONSTRAINT wager_transactions_internal_shape CHECK (
        origin <> 'INTERNAL' OR (
            kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL AND idempotency_key IS NULL
            AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL
        )
    ),
    CONSTRAINT wager_transactions_external_shape CHECK (
        origin <> 'EXTERNAL' OR (
            kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL AND idempotency_key IS NOT NULL
            AND octet_length(payload_hash) = 32 AND round_id IS NOT NULL AND game_id IS NOT NULL
        )
    ),
    CONSTRAINT wager_transactions_reference_shape CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR kind = 'WIN'
        OR (kind IN ('OPENING', 'BET', 'LOSS') AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
    ),
    CONSTRAINT wager_transactions_status_shape CHECK (
        (status = 'PENDING' AND failure_code IS NULL AND result_balance_minor IS NULL AND completed_at IS NULL)
        OR (status = 'PENDING_REFERENCE' AND failure_code IS NULL AND result_balance_minor IS NULL
            AND next_attempt_at IS NOT NULL AND expires_at IS NOT NULL AND completed_at IS NULL)
        OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL AND completed_at IS NOT NULL)
        OR (status IN ('REJECTED', 'FAILED') AND failure_code IS NOT NULL AND completed_at IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_result_valid CHECK (
        (result_balance_minor IS NULL AND result_wallet_version IS NULL)
        OR (result_balance_minor >= 0 AND result_wallet_version >= 1)
    ),
    CONSTRAINT wager_transactions_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT wager_transactions_timestamps_ordered CHECK (
        updated_at >= created_at AND (completed_at IS NULL OR completed_at >= created_at)
    ),
    CONSTRAINT wager_transactions_id_wallet_key UNIQUE (id, wallet_id),
    CONSTRAINT wager_transactions_provider_external_key UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_transactions_provider_idempotency_key UNIQUE (provider_id, idempotency_key)
);

CREATE UNIQUE INDEX wager_transactions_single_opening
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

CREATE UNIQUE INDEX wager_transactions_single_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

CREATE INDEX wager_transactions_due_pending
    ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

CREATE INDEX wager_transactions_waiting_reference
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';

CREATE INDEX wager_transactions_wallet_created
    ON wager_transactions (wallet_id, created_at);

CREATE FUNCTION wager_transactions_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'transaction % is terminal (%)', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_terminal_immutable';
    END IF;
    IF (NEW.id, NEW.origin, NEW.kind, NEW.wallet_id, NEW.player_id, NEW.currency, NEW.amount_minor,
        NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.round_id, NEW.game_id, NEW.reference_external_transaction_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.origin, OLD.kind, OLD.wallet_id, OLD.player_id, OLD.currency, OLD.amount_minor,
        OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.round_id, OLD.game_id, OLD.reference_external_transaction_id, OLD.created_at) THEN
        RAISE EXCEPTION 'transaction % request data is immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_request_immutable';
    END IF;
    IF NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
        OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
    ) THEN
        RAISE EXCEPTION 'transaction % cannot move from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_status_transition';
    END IF;
    IF OLD.expires_at IS NOT NULL AND NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'transaction % expiration is immutable once set', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_expiration_immutable';
    END IF;
    IF NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'transaction % attempts cannot decrease', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_attempts_monotonic';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER wager_transactions_guard_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard_update();

CREATE FUNCTION wager_transactions_forbid_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wager transactions cannot be deleted'
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_transactions_append_only';
END
$$;

CREATE TRIGGER wager_transactions_forbid_delete
    BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_forbid_delete();

CREATE TRIGGER wager_transactions_forbid_truncate
    BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION wager_transactions_forbid_delete();

GRANT SELECT, INSERT ON wager_transactions TO wallet_app;
GRANT UPDATE (
    status, reference_transaction_id, failure_code, result_balance_minor, result_wallet_version,
    attempts, next_attempt_at, expires_at, updated_at, completed_at
) ON wager_transactions TO wallet_app;
