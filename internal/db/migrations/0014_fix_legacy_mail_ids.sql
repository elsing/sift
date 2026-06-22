-- mails.id encodes its folder as base64 (account|base64(folder)|uid) so folder names
-- with special characters survive being embedded in an id. An older version of the
-- code embedded the folder name literally instead, before this scheme existed. Those
-- rows were never cleaned up, so practically every folder ended up with two cache rows
-- per message: one under the old literal-folder id, one under the current encoded id —
-- both get listed, showing as duplicates everywhere (inbox, folders, tag groups).
-- Safe to delete outright: this is just a sync cache, rebuilt from IMAP on demand, and
-- tags live on message_id (not mails.id), so deleting the stale row doesn't lose a tag.
DELETE FROM mails WHERE split_part(id, '|', 2) = folder;
