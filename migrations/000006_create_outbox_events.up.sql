CREATE TABLE outbox_events (
    id              UUID        PRIMARY KEY,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    event_version   INTEGER     NOT NULL,
    partition_key   TEXT        NOT NULL,
    correlation_id  TEXT        NOT NULL,
    causation_id    TEXT,
    payload         JSON        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    last_error      TEXT,
    CONSTRAINT outbox_events_version_positive CHECK (event_version >= 1),
    CONSTRAINT outbox_events_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT outbox_events_lock_shape CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    CONSTRAINT outbox_events_identity_not_blank CHECK (event_type <> '' AND partition_key <> '' AND correlation_id <> '')
);

CREATE INDEX outbox_events_unpublished
    ON outbox_events (next_attempt_at)
    WHERE published_at IS NULL;

CREATE INDEX outbox_events_unpublished_occurred
    ON outbox_events (occurred_at)
    WHERE published_at IS NULL;

CREATE FUNCTION outbox_events_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version, NEW.partition_key,
        NEW.correlation_id, NEW.causation_id, NEW.payload::text, NEW.occurred_at)
       IS DISTINCT FROM
       (OLD.id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version, OLD.partition_key,
        OLD.correlation_id, OLD.causation_id, OLD.payload::text, OLD.occurred_at) THEN
        RAISE EXCEPTION 'outbox event % snapshot is immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_events_snapshot_immutable';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox event % publication is final', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_events_publication_final';
    END IF;
    IF NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'outbox event % attempts cannot decrease', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_events_attempts_monotonic';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER outbox_events_guard_update
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_update();

CREATE FUNCTION outbox_events_guard_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.published_at IS NULL THEN
        RAISE EXCEPTION 'unpublished outbox event % cannot be deleted', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_events_unpublished_retained';
    END IF;
    RETURN OLD;
END
$$;

CREATE TRIGGER outbox_events_guard_delete
    BEFORE DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_delete();

GRANT SELECT, INSERT, DELETE ON outbox_events TO wallet_app;
GRANT UPDATE (attempts, next_attempt_at, locked_by, locked_until, published_at, last_error) ON outbox_events TO wallet_app;
