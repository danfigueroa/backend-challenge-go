CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL,
    message_id     TEXT        NOT NULL,
    payload_hash   BYTEA       NOT NULL,
    transaction_id UUID,
    received_at    TIMESTAMPTZ NOT NULL,
    processed_at   TIMESTAMPTZ,
    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_messages_transaction_fk FOREIGN KEY (transaction_id) REFERENCES wager_transactions (id),
    CONSTRAINT inbox_messages_hash_length CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT inbox_messages_identity_not_blank CHECK (consumer_name <> '' AND message_id <> ''),
    CONSTRAINT inbox_messages_processed_after_received CHECK (processed_at IS NULL OR processed_at >= received_at)
);

CREATE FUNCTION inbox_messages_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.consumer_name, NEW.message_id, NEW.payload_hash, NEW.received_at)
       IS DISTINCT FROM (OLD.consumer_name, OLD.message_id, OLD.payload_hash, OLD.received_at) THEN
        RAISE EXCEPTION 'inbox message %/% identity is immutable', OLD.consumer_name, OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'inbox_messages_identity_immutable';
    END IF;
    IF OLD.processed_at IS NOT NULL THEN
        RAISE EXCEPTION 'inbox message %/% is already processed', OLD.consumer_name, OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'inbox_messages_processed_immutable';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER inbox_messages_guard_update
    BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard_update();

GRANT SELECT, INSERT ON inbox_messages TO wallet_app;
GRANT UPDATE (transaction_id, processed_at) ON inbox_messages TO wallet_app;
