-- A user's Mac (host) configuration, kept on the profile so the report and
-- comment forms can be prefilled and updated from them ("Save to profile").
ALTER TABLE users ADD COLUMN host_macos_version TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN host_device_model  TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN host_chip          TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN host_ram_gb        INTEGER NOT NULL DEFAULT 0;
