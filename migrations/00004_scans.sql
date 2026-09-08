-- +goose Up
-- +goose StatementBegin

-- A scan: one submitted photo and whatever the analysis made of it.
--
-- Every row here cost real money. A completed scan is one paid vendor call
-- (ADR-003: AILab Skin Analyze Pro, ~$0.19–0.21) and one of the user's weekly
-- allowance, which is why idempotency and the rejection path are enforced in
-- the schema rather than left to the handler.
CREATE TABLE scans (
    id              TEXT PRIMARY KEY,

    user_id         TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- processing | completed | rejected | failed. Mirrors ScanStatus.status in
    -- api/openapi.yaml; the contract drift test holds the two together.
    status          TEXT        NOT NULL,

    -- received | quality_checked | scoring | writing_report.
    --
    -- Written as the analysis progresses so the client can name the step it is
    -- waiting on. Today the work runs inside the POST, so these transitions all
    -- land before the response and the client cannot observe them mid-flight.
    -- The column exists anyway: when the analysis moves to the worker this is
    -- what makes the progress real, and adding it now costs nothing while
    -- adding it later would be a migration on a live table.
    stage           TEXT,

    -- FALSE for a rejection, always. The vendor does not bill a request it
    -- refused, so a blurry photo must not cost the user a scan either.
    allowance_spent BOOLEAN     NOT NULL DEFAULT FALSE,

    rejection_reason TEXT,

    -- The report, stored whole rather than shredded across tables.
    --
    -- It is an immutable point-in-time fact, never queried by its interior, and
    -- always read as a unit. Normalising six issues into their own table would
    -- buy query flexibility nothing asks for and cost a join on the hottest
    -- read in the feature.
    report          JSONB,

    -- Which SeverityRuleSet produced those severities (PD-2). Denormalised out
    -- of the JSON so "which scans were graded under the old thresholds?" is a
    -- plain indexed query rather than a JSON scan -- and under FR-7 and NFR-6
    -- that question has to be answerable.
    ruleset_version TEXT,

    -- The vendor and endpoint that produced this, recorded per scan.
    --
    -- ADR-003 selected Pro conditionally, with an open risk that acne is
    -- under-reported. If that resolves against the vendor and the provider
    -- changes, this column is what separates old readings from new ones instead
    -- of silently mixing two scales in the same trend line.
    provider        TEXT,

    submitted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

-- The home screen and the trend both ask "this user's scans, newest first".
CREATE INDEX idx_scans_user_recent ON scans (user_id, submitted_at DESC);

-- Idempotency, enforced where it can actually hold.
--
-- POST /scans requires an Idempotency-Key because a retry over a flaky
-- connection would otherwise buy a second vendor call and burn a second scan
-- from the user's week. Checking in the handler is a race; a unique index is
-- not. Scoped per user so two people cannot collide on a client-generated key.
CREATE TABLE scan_idempotency (
    user_id         TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    idempotency_key TEXT        NOT NULL,
    scan_id         TEXT        NOT NULL REFERENCES scans (id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, idempotency_key)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE scan_idempotency;
DROP TABLE scans;
-- +goose StatementEnd
