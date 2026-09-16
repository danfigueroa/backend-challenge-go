CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,
    balance_minor BIGINT      NOT NULL,
    version       BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive CHECK (version >= 1),
    CONSTRAINT wallets_timestamps_ordered CHECK (updated_at >= created_at),
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_id_player_currency_key UNIQUE (id, player_id, currency)
);

CREATE FUNCTION wallets_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.player_id, NEW.currency, NEW.created_at) IS DISTINCT FROM (OLD.id, OLD.player_id, OLD.currency, OLD.created_at) THEN
        RAISE EXCEPTION 'wallet % identity is immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_identity_immutable';
    END IF;
    IF NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'wallet % version may only change with its balance', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_version_follows_balance';
    END IF;
    IF NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'wallet % balance change must increment version by one (% -> %)', OLD.id, OLD.version, NEW.version
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_version_follows_balance';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER wallets_guard_update
    BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();

CREATE FUNCTION wallets_forbid_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallets cannot be deleted'
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_append_only';
END
$$;

CREATE TRIGGER wallets_forbid_delete
    BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_forbid_delete();

CREATE TRIGGER wallets_forbid_truncate
    BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION wallets_forbid_delete();

GRANT SELECT, INSERT ON wallets TO wallet_app;
GRANT UPDATE (balance_minor, version, updated_at) ON wallets TO wallet_app;
