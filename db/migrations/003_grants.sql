-- 003_grants.sql — course ch. 6: grant privileges to a role (least privilege).
SET ROLE relief_owner;

-- Lock everything down first, then grant only what each role needs.
REVOKE ALL ON SCHEMA core, ops, audit, reporting FROM PUBLIC;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA core, ops, audit, reporting FROM PUBLIC;

-- relief_app: the web application.
GRANT USAGE ON SCHEMA core, ops, reporting TO relief_app;
GRANT SELECT                         ON ALL TABLES IN SCHEMA core TO relief_app;
GRANT UPDATE (occupancy, status)     ON core.shelters             TO relief_app;  -- column-level grant
GRANT SELECT, INSERT, UPDATE         ON ALL TABLES IN SCHEMA ops  TO relief_app;
GRANT DELETE                         ON ops.shifts                TO relief_app;  -- cancel a shift; nothing else is deletable
GRANT SELECT                         ON ALL TABLES IN SCHEMA reporting TO relief_app;
GRANT EXECUTE ON FUNCTION ops.fulfill_request(bigint, integer),
                          reporting.match_volunteers(integer, integer),
                          reporting.refresh_region_summary() TO relief_app;
-- Audit is read-only for the app (rows are written by the SECURITY DEFINER trigger).
GRANT USAGE  ON SCHEMA audit TO relief_app;
GRANT SELECT ON audit.changes TO relief_app;

-- relief_analyst: reporting views only. No core, no ops, no audit, no contact details.
GRANT USAGE  ON SCHEMA reporting TO relief_analyst;
GRANT SELECT ON ALL TABLES IN SCHEMA reporting TO relief_analyst;
GRANT EXECUTE ON FUNCTION reporting.match_volunteers(integer, integer) TO relief_analyst;

-- Objects created later inherit the same privileges.
ALTER DEFAULT PRIVILEGES IN SCHEMA reporting GRANT SELECT ON TABLES TO relief_app, relief_analyst;
ALTER DEFAULT PRIVILEGES IN SCHEMA ops       GRANT SELECT, INSERT, UPDATE ON TABLES TO relief_app;

RESET ROLE;
