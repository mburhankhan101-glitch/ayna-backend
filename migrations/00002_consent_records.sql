-- +goose Up
-- +goose StatementBegin

-- FR-1: explicit, timestamped consent to photo processing. A scan is refused
-- until a granting record exists for the live policy version.
--
-- This table is APPEND-ONLY, and that is the whole design:
--
--   * Revocation inserts a new row with granted = FALSE. It never updates or
--     deletes an existing row.
--   * Re-consenting after a policy change inserts another row.
--
-- An audit trail you can edit is not an audit trail. If someone later asks
-- "was this user consented when that scan ran?", the answer has to be
-- reconstructible from what is stored, not from what the current state happens
-- to be.
CREATE TABLE consent_records (
    id             TEXT PRIMARY KEY,

    user_id        TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Consent is version-scoped. When the wording changes, prior consent does
    -- not silently cover the new terms and the user is asked again.
    policy_version TEXT        NOT NULL,

    granted        BOOLEAN     NOT NULL,

    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The question asked on every scan submission is "what is this user's most
-- recent record for the current policy version?". DESC on recorded_at means
-- that answer is the first row read.
CREATE INDEX idx_consent_user_policy_recent
    ON consent_records (user_id, policy_version, recorded_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE consent_records;
-- +goose StatementEnd
