-- Reports of problematic content ("flags") on reports and comments. One row
-- per reporter and target; nothing public shows them. They are deleted with
-- the content, and a maintainer can ignore them, which deletes them too so
-- the reporter may report again.
CREATE TABLE content_flags (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,     -- report | comment
    target_id  INTEGER NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    UNIQUE (kind, target_id, user_id)
);
CREATE INDEX content_flags_target ON content_flags(kind, target_id);
