CREATE TABLE event_types (
    id   bigserial PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE queue_messages (
    id   bigserial PRIMARY KEY,
    body text NOT NULL
);

-- archived_events is defined but the code never writes it: an advisory lower-bound
-- signal, not drift.
CREATE TABLE archived_events (
    id bigserial PRIMARY KEY
);
