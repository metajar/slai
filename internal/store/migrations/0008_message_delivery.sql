CREATE INDEX IF NOT EXISTS idx_messages_wake ON messages(delivered_at, woken_at);
CREATE INDEX IF NOT EXISTS idx_messages_owed ON messages(to_agent, kind, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to ON messages(reply_to);
CREATE INDEX IF NOT EXISTS idx_messages_answer ON messages(from_agent, to_agent, id);
