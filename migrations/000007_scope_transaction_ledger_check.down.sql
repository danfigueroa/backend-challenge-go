CREATE OR REPLACE FUNCTION wager_transactions_check_ledger() RETURNS trigger
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
