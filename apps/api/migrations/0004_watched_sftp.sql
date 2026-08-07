-- P3: SFTP watch connector kind (same schedule/list/cursor abstraction as watched_prefix).
ALTER TYPE connector_kind ADD VALUE IF NOT EXISTS 'watched_sftp';
