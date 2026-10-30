-- Identity -------------------------------------------------------------------

-- Email addresses are never stored: email_hash is the keyed Argon2id
-- identity (auth.Keys.HashEmail). Deleting an account keeps the row, marked
-- deleted, so posts that survive deletion stay traceable for moderation and a
-- ban still applies if the same address signs in again.
CREATE TABLE users (
    id             INTEGER PRIMARY KEY,
    email_hash     TEXT NOT NULL UNIQUE,
    display_name   TEXT NOT NULL UNIQUE COLLATE NOCASE,
    is_banned      INTEGER NOT NULL DEFAULT 0,
    spam_strikes   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    last_login_at  INTEGER,
    last_export_at INTEGER,
    deleted_at     INTEGER
);

-- Sign-in codes are bound to the browser that asked for them: request_hash
-- is the hash of a secret that lives only in that browser's cookie, and
-- code_hash covers secret and code together, so a database dump yields no
-- usable code. Wrong guesses are counted and void the code.
CREATE TABLE login_codes (
    id           INTEGER PRIMARY KEY,
    request_hash BLOB NOT NULL UNIQUE,
    code_hash    BLOB NOT NULL,
    email_hash   TEXT NOT NULL,
    ip           TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    used_at      INTEGER
);
CREATE INDEX login_codes_hash ON login_codes(email_hash, created_at);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   BLOB NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

-- UTM versions ---------------------------------------------------------------

CREATE TABLE utm_versions (
    id            INTEGER PRIMARY KEY,
    version       TEXT NOT NULL UNIQUE,
    v_major       INTEGER NOT NULL,
    v_minor       INTEGER NOT NULL,
    v_patch       INTEGER NOT NULL,
    is_prerelease INTEGER NOT NULL DEFAULT 0,
    released_at   INTEGER,
    created_at    INTEGER NOT NULL
);

-- IGDB mirror ----------------------------------------------------------------

CREATE TABLE igdb_games (
    id                 INTEGER PRIMARY KEY,
    checksum           TEXT,
    name               TEXT NOT NULL,
    slug               TEXT NOT NULL,
    summary            TEXT,
    url                TEXT,
    first_release_date INTEGER,
    game_type_id       INTEGER,
    cover_id           INTEGER,
    created_at         INTEGER,
    updated_at         INTEGER,
    genres_json        TEXT NOT NULL DEFAULT '[]',
    game_engines_json  TEXT NOT NULL DEFAULT '[]',
    raw_json           TEXT NOT NULL,
    synced_at          INTEGER NOT NULL
);
CREATE INDEX igdb_games_slug ON igdb_games(slug);
CREATE INDEX igdb_games_updated ON igdb_games(updated_at);

CREATE VIRTUAL TABLE igdb_games_fts USING fts5(
    name,
    content='igdb_games',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER igdb_games_ai AFTER INSERT ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(rowid, name) VALUES (new.id, new.name);
END;
CREATE TRIGGER igdb_games_ad AFTER DELETE ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(igdb_games_fts, rowid, name) VALUES ('delete', old.id, old.name);
END;
CREATE TRIGGER igdb_games_au AFTER UPDATE OF name ON igdb_games BEGIN
    INSERT INTO igdb_games_fts(igdb_games_fts, rowid, name) VALUES ('delete', old.id, old.name);
    INSERT INTO igdb_games_fts(rowid, name) VALUES (new.id, new.name);
END;

CREATE TABLE igdb_game_types    (id INTEGER PRIMARY KEY, type TEXT NOT NULL);
CREATE TABLE igdb_genres        (id INTEGER PRIMARY KEY, name TEXT NOT NULL, slug TEXT);
CREATE TABLE igdb_game_engines  (id INTEGER PRIMARY KEY, name TEXT NOT NULL, slug TEXT);

CREATE TABLE igdb_covers (
    id         INTEGER PRIMARY KEY,
    game_id    INTEGER NOT NULL,
    image_id   TEXT NOT NULL,
    width      INTEGER,
    height     INTEGER,
    local_big  TEXT,
    local_small TEXT,
    fetched_at INTEGER NOT NULL
);
CREATE INDEX igdb_covers_game ON igdb_covers(game_id);

CREATE TABLE igdb_websites (
    id            INTEGER PRIMARY KEY,
    game_id       INTEGER NOT NULL,
    type_id       INTEGER,
    url           TEXT NOT NULL,
    trusted       INTEGER NOT NULL DEFAULT 0,
    storefront    TEXT,
    storefront_id TEXT
);
CREATE INDEX igdb_websites_game ON igdb_websites(game_id);
CREATE INDEX igdb_websites_store ON igdb_websites(storefront, storefront_id);

CREATE TABLE igdb_sync_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Community ------------------------------------------------------------------

-- Only games with at least one report are "active" and appear here.
CREATE TABLE games (
    igdb_game_id     INTEGER PRIMARY KEY REFERENCES igdb_games(id),
    activated_at     INTEGER NOT NULL,
    details_fetched_at INTEGER,
    status           TEXT NOT NULL DEFAULT 'unknown',
    status_version_id INTEGER REFERENCES utm_versions(id),
    report_count     INTEGER NOT NULL DEFAULT 0,
    star_count       INTEGER NOT NULL DEFAULT 0,
    triage_available INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX games_updated ON games(updated_at);
CREATE INDEX games_stars ON games(star_count);
CREATE INDEX games_reports ON games(report_count);

CREATE TABLE stars (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    game_id    INTEGER NOT NULL REFERENCES games(igdb_game_id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, game_id)
);
CREATE INDEX stars_game ON stars(game_id);

CREATE TABLE configurations (
    id               INTEGER PRIMARY KEY,
    utm_version_id   INTEGER NOT NULL REFERENCES utm_versions(id),
    guest_os         TEXT NOT NULL,          -- windows | linux | other
    linux_distro     TEXT,
    linux_version    TEXT,
    other_os_name    TEXT,
    graphics_backend TEXT,                   -- unknown | dxmt | d3dmetal (windows only)
    macos_version    TEXT,
    device_model     TEXT,
    chip             TEXT,
    ram_gb           INTEGER,
    created_at       INTEGER NOT NULL
);

CREATE TABLE reports (
    id                   INTEGER PRIMARY KEY,
    game_id              INTEGER NOT NULL REFERENCES games(igdb_game_id),
    user_id              INTEGER NOT NULL REFERENCES users(id),
    rating               TEXT NOT NULL,      -- unknown | does_not_launch | in_game_issues | playable | gold
    played_to_completion INTEGER NOT NULL DEFAULT 0,
    config_id            INTEGER NOT NULL REFERENCES configurations(id),
    issue_type           TEXT,               -- unknown | hang_crash | graphics | performance | workaround | other
    description_md       TEXT NOT NULL DEFAULT '',
    repro_md             TEXT NOT NULL DEFAULT '',
    wine_same_issue      TEXT,               -- unknown | no | yes
    visibility           TEXT NOT NULL DEFAULT 'visible', -- visible | hidden | removed
    anonymized           INTEGER NOT NULL DEFAULT 0,      -- author deleted their account
    triage_status        TEXT,               -- NULL (not triaged) | pending | in_progress | needs_clarification | fix_proposed | wont_fix | cannot_reproduce
    triage_updated_at    INTEGER,
    plus_one_count       INTEGER NOT NULL DEFAULT 0,
    comment_count        INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    edited_at            INTEGER,
    ip                   TEXT,
    user_agent           TEXT
);
CREATE INDEX reports_game ON reports(game_id, visibility, created_at);
CREATE INDEX reports_user ON reports(user_id, created_at);
CREATE INDEX reports_created ON reports(created_at);
CREATE INDEX reports_updated ON reports(updated_at, id);
CREATE INDEX reports_config ON reports(config_id);

CREATE TABLE comments (
    id          INTEGER PRIMARY KEY,
    report_id   INTEGER NOT NULL REFERENCES reports(id) ON DELETE CASCADE,
    user_id     INTEGER REFERENCES users(id),
    author_kind TEXT NOT NULL DEFAULT 'user', -- user | bot
    body_md     TEXT NOT NULL,
    config_id   INTEGER REFERENCES configurations(id),
    visibility  TEXT NOT NULL DEFAULT 'visible',
    created_at  INTEGER NOT NULL,
    ip          TEXT,
    user_agent  TEXT
);
CREATE INDEX comments_report ON comments(report_id, created_at);
CREATE INDEX comments_config ON comments(config_id);

CREATE TABLE plus_ones (
    report_id  INTEGER NOT NULL REFERENCES reports(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    config_id  INTEGER REFERENCES configurations(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (report_id, user_id)
);
CREATE INDEX plus_ones_user ON plus_ones(user_id);
CREATE INDEX plus_ones_config ON plus_ones(config_id);

CREATE TABLE attachments (
    id           INTEGER PRIMARY KEY,
    report_id    INTEGER NOT NULL REFERENCES reports(id) ON DELETE CASCADE,
    user_id      INTEGER NOT NULL REFERENCES users(id),
    filename     TEXT NOT NULL,
    mime         TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    deleted_at   INTEGER
);
CREATE INDEX attachments_report ON attachments(report_id);
CREATE INDEX attachments_user ON attachments(user_id, created_at);

CREATE TABLE upload_ledger (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day     TEXT NOT NULL,                   -- YYYY-MM-DD (UTC)
    bytes   INTEGER NOT NULL DEFAULT 0,
    files   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

CREATE TABLE spam_log (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    kind           TEXT NOT NULL,            -- report | comment | triage_request | deletion_request
    payload_json   TEXT NOT NULL,
    akismet_result TEXT NOT NULL,            -- spam | discard | error
    feedback       TEXT,                     -- spam | ham (sent back to Akismet)
    created_at     INTEGER NOT NULL
);

-- Auto-Triage ----------------------------------------------------------------

-- Raw list pushed by the owned-games service; matched lazily to IGDB ids.
CREATE TABLE triage_owned (
    id            INTEGER PRIMARY KEY,
    storefront    TEXT NOT NULL,             -- steam | gog | epic | itch | igdb
    storefront_id TEXT NOT NULL,
    free          INTEGER NOT NULL DEFAULT 0,
    igdb_game_id  INTEGER,
    received_at   INTEGER NOT NULL,
    UNIQUE(storefront, storefront_id)
);
CREATE INDEX triage_owned_game ON triage_owned(igdb_game_id);

CREATE TABLE triage_availability (
    game_id       INTEGER PRIMARY KEY,
    source        TEXT NOT NULL,             -- owned | free | manual
    storefront    TEXT,
    storefront_id TEXT,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE triage_requests (
    id         INTEGER PRIMARY KEY,
    game_id    INTEGER NOT NULL REFERENCES games(igdb_game_id),
    user_id    INTEGER NOT NULL REFERENCES users(id),
    kind       TEXT NOT NULL,                -- gift | demo
    storefront TEXT,
    demo_url   TEXT,
    notes      TEXT,
    status     TEXT NOT NULL DEFAULT 'open', -- open | done | rejected
    created_at INTEGER NOT NULL
);
CREATE INDEX triage_requests_game ON triage_requests(game_id);

-- Authors can delete a report directly only while it is fresh and has no
-- comments; otherwise they file a request that a maintainer reviews.
CREATE TABLE deletion_requests (
    id         INTEGER PRIMARY KEY,
    report_id  INTEGER NOT NULL REFERENCES reports(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id),
    reason     TEXT,
    status     TEXT NOT NULL DEFAULT 'open', -- open | done | rejected
    created_at INTEGER NOT NULL
);
CREATE INDEX deletion_requests_report ON deletion_requests(report_id, status);
