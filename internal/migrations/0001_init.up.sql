-- 0001: bootstrap migration.
--
-- The migration machinery creates and tracks the schema_migrations table
-- itself. The core data model (sources, documents, claims, contradictions,
-- ledgers) lands in later migrations; this file exists so the chain is real
-- and runs forward on an empty database from day one.
SELECT 1;
