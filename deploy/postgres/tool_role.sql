-- Run once, as the application object owner (with CREATEROLE), against the tool database.
-- Supply AGENTGO_TOOL_DB_PASSWORD in the psql environment. See docs/TOOL_PERMISSIONS.md.
\set ON_ERROR_STOP on
\getenv tool_password AGENTGO_TOOL_DB_PASSWORD
BEGIN;
CREATE ROLE agentgo_tool_reader LOGIN PASSWORD :'tool_password'
    NOSUPERUSER NOINHERIT NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE agentgo_tool_reader SET default_transaction_read_only = on;
ALTER ROLE agentgo_tool_reader SET search_path = pg_catalog;
ALTER ROLE agentgo_tool_reader SET statement_timeout = '10s';

-- PUBLIC grants apply to the tool role too. Restore needed grants to named application roles.
SELECT format('REVOKE CREATE, TEMPORARY ON DATABASE %I FROM PUBLIC', current_database()) \gexec
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM PUBLIC;
REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA public FROM PUBLIC;
-- Repeat for every role that creates functions and every other application schema.
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

CREATE SCHEMA tool_api;
REVOKE ALL ON SCHEMA tool_api FROM PUBLIC;
GRANT USAGE ON SCHEMA tool_api TO agentgo_tool_reader;
COMMIT;
-- No data is exposed here. Create reviewed shared business views, then grant each explicitly.
-- Never GRANT SELECT ON ALL TABLES or add role memberships to agentgo_tool_reader.
