CREATE TABLE dost_schema_migrations (
    version    integer     PRIMARY KEY,
    name       text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);
