-- queue_messages was superseded; dropping it means a code path still writing it is
-- drift (the `relation does not exist` hazard).
DROP TABLE queue_messages;
