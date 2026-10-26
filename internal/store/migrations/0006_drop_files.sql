-- Disk-as-truth refactor: the files registry is removed (a file exists iff
-- its bytes exist on disk — spec §4, §8). The owner sanctioned discarding
-- every pre-refactor row and byte; no migration of data.
--
-- Drop order matters: attachments FK-references files and foreign_keys is
-- ON for every connection. The files indexes (idx_files_user,
-- idx_files_expiry, idx_files_artifact_path from 0005) drop with the table.

DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS files;

-- Attachments survive, re-keyed by PATH: with the registry gone the path
-- IS the identity of a file. The row records only the conversation fact
-- "this message was sent with this upload" — no size, kind, expiry or any
-- other claim about the filesystem. Paths are stored canonical
-- (scratch.ParseRel segments rejoined), so a stored path matches the disk
-- by exact string comparison. Cascade with the message: deleting a thread
-- removes its pairings; the upload bytes survive (deleting chat is not a
-- file-management action). A file deleted later leaves the row behind on
-- purpose — the chip is a historical record and its download 404s.

CREATE TABLE attachments (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  PRIMARY KEY (message_id, path)
);
