-- Account-status vocabulary (spec §9.17). A pending sign-up was recorded
-- as 'disabled' although nothing disabled it — the word promised an
-- operator action that does not exist, and an outside audit read it as
-- one. 'unconfirmed' says what is true; 'disabled' is retired to mean
-- exactly one thing: an administrative suspension. 'active' becomes
-- 'enabled' so the three states read as one lifecycle.
UPDATE users SET status = 'unconfirmed' WHERE status = 'disabled';
UPDATE users SET status = 'enabled' WHERE status = 'active';
