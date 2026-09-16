DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'wallet_app') THEN
        CREATE ROLE wallet_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO wallet_app;
