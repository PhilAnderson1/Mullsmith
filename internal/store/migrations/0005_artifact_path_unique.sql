-- audit2 finding 4: "at most one artifact row per on-disk path" was an
-- argument in a comment; this index makes it a fact. Path ALIASES
-- ("./work/x" vs "work/x") and concurrent identical shares used to create
-- several rows over the same bytes, so expiring or deleting one row unlinked
-- files another live row still pointed at. Registration now canonicalizes
-- paths and upserts atomically; the index is the backstop that no future
-- code path can silently reintroduce the alias pair.
--
-- Partial by kind: uploads live in their own server-managed namespace
-- (uploads/<file-id>.ext) and must not take artifact uniqueness.
CREATE UNIQUE INDEX idx_files_artifact_path ON files(user_id, rel_path)
  WHERE kind = 'artifact';
