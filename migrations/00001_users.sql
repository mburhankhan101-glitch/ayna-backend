-- +goose Up
-- +goose StatementBegin

-- The local profile. Auth0 answers "who is this person"; this table records
-- what they are allowed to do here, which is the part Auth0 has no opinion on.
CREATE TABLE users (
    id                    TEXT PRIMARY KEY,

    -- The Auth0 `sub` claim. This is the join between the identity provider
    -- and everything else, and it is deliberately NOT the email: people change
    -- their email address, and a join key that changes is not a key.
    auth0_sub             TEXT        NOT NULL UNIQUE,

    -- PD-1: the YEAR only, never a full date of birth. The year satisfies both
    -- the 18+ gate and FR-5's skin-age comparison, and a full date is
    -- meaningfully more identifying for no added benefit.
    --
    -- The 18+ rule itself is NOT expressible as a CHECK constraint: it depends
    -- on today's date, and Postgres requires CHECK expressions to be immutable.
    -- So the invariant lives in the domain (iam.NewUser), and this constraint
    -- only catches values that are nonsense in any year.
    birth_year            INTEGER     NOT NULL CHECK (birth_year > 1900 AND birth_year < 2100),

    -- PD-5: streak weeks are computed in the user's own timezone. UTC
    -- boundaries would credit a 2am scan in Karachi to the previous week,
    -- which is invisible to the user and reads as a bug.
    timezone              TEXT        NOT NULL,

    display_name          TEXT,

    -- FR-11 is *deletion*, not archival, so there is no `deleted_at` here:
    -- a soft-deleted row is data you promised to destroy and kept. This column
    -- marks a purge in flight, and the row is genuinely removed when it
    -- completes.
    deletion_requested_at TIMESTAMPTZ,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every authenticated request resolves the caller by this. It is the hottest
-- lookup in the system, so it gets its own index rather than relying on the
-- UNIQUE constraint's implicit one being enough for the query planner.
CREATE INDEX idx_users_auth0_sub ON users (auth0_sub);

-- Lets the purge worker find outstanding deletion requests without scanning
-- the whole table. Partial, because the overwhelming majority of rows have
-- NULL here and indexing them would be waste.
CREATE INDEX idx_users_pending_deletion
    ON users (deletion_requested_at)
    WHERE deletion_requested_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE users;
-- +goose StatementEnd
