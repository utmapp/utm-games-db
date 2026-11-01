-- IGDB's main games carry game_type 0, which the first imports stored as
-- NULL. Make it explicit so searches can rank base games first.
UPDATE igdb_games SET game_type_id = 0 WHERE game_type_id IS NULL;

-- Alternative titles from IGDB (renamed games, acronyms, translations) are
-- searchable alongside the name, so "Overwatch 2" finds the entry IGDB now
-- calls "Overwatch". The search index is rebuilt over both columns, and the
-- sync watermark is cleared so the next sync re-imports every game with its
-- alternative names.
ALTER TABLE igdb_games ADD COLUMN alt_names TEXT NOT NULL DEFAULT '';

DROP TRIGGER igdb_games_ai;
DROP TRIGGER igdb_games_ad;
DROP TRIGGER igdb_games_au;
DROP TABLE igdb_games_fts;

CREATE VIRTUAL TABLE igdb_games_fts USING fts5(
    name,
    alt_names,
    content='igdb_games',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER igdb_games_ai AFTER INSERT ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(rowid, name, alt_names) VALUES (new.id, new.name, new.alt_names);
END;
CREATE TRIGGER igdb_games_ad AFTER DELETE ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(igdb_games_fts, rowid, name, alt_names) VALUES ('delete', old.id, old.name, old.alt_names);
END;
CREATE TRIGGER igdb_games_au AFTER UPDATE ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(igdb_games_fts, rowid, name, alt_names) VALUES ('delete', old.id, old.name, old.alt_names);
    INSERT INTO igdb_games_fts(rowid, name, alt_names) VALUES (new.id, new.name, new.alt_names);
END;
INSERT INTO igdb_games_fts(igdb_games_fts) VALUES ('rebuild');

DELETE FROM igdb_sync_state WHERE key = 'watermark';
