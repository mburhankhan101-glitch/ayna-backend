-- +goose Up
-- +goose StatementBegin

-- Align the index with the query.
--
-- LatestFor now orders by (recorded_at DESC, id DESC). The tie-break exists
-- because two consent records can share a recorded_at — a double-tap, a retry,
-- or any two writes inside the same clock tick — and without it a grant and a
-- revocation written in the same instant resolve in whatever order the index
-- happens to return. The same user could then read as consented or not on
-- alternate requests.
--
-- Adding id to the index lets Postgres satisfy the whole ORDER BY from it
-- rather than sorting afterwards. The sort would be cheap at this row count,
-- but an index that does not match its query is a trap for whoever reads the
-- plan later and assumes it does.
--
-- Written as a new migration rather than by editing 00002. That file has been
-- applied, and an applied migration is history: editing it means two databases
-- can report the same schema version with different schemas.

DROP INDEX idx_consent_user_policy_recent;

CREATE INDEX idx_consent_user_policy_recent
    ON consent_records (user_id, policy_version, recorded_at DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX idx_consent_user_policy_recent;

CREATE INDEX idx_consent_user_policy_recent
    ON consent_records (user_id, policy_version, recorded_at DESC);

-- +goose StatementEnd
