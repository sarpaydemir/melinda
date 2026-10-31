CREATE TABLE dost_inflight_runs (
    agent_id         text        NOT NULL,
    generation       text        NOT NULL,
    conversation_key text        NOT NULL,
    buffer           jsonb       NOT NULL,
    taken_at         timestamptz NOT NULL,
    resubmissions    integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (agent_id, generation)
);
