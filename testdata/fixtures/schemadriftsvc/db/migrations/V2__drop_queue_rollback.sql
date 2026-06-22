-- Rollback (undo) script: it re-creates queue_messages. The forward replay MUST
-- exclude *_rollback.sql, so this must NOT re-add the table to the defined schema.
CREATE TABLE queue_messages (
    id   bigserial PRIMARY KEY,
    body text NOT NULL
);
