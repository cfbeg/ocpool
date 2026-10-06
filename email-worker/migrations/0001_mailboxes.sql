CREATE TABLE IF NOT EXISTS inboxes (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS inbox_expiry ON inboxes(expires_at);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  received_at INTEGER NOT NULL,
  sender TEXT NOT NULL,
  subject TEXT NOT NULL,
  urls_json TEXT NOT NULL,
  codes_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS message_inbox_time ON messages(inbox_id, received_at);
