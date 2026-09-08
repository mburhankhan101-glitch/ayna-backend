-- +goose Up
-- +goose StatementBegin

-- How long this user's photos are kept (NFR-4).
--
-- Onboarding promises photos are "deleted on a schedule you choose". Until
-- this column existed that sentence was false: the only deletion path was
-- FR-11's account purge, which is a different promise entirely. A privacy
-- claim the code does not keep is worse than no claim, and this app makes it
-- on the second screen anyone sees.
--
-- NULL means keep indefinitely, and it is the ONLY meaning NULL carries here.
-- New rows take the DEFAULT, so NULL never appears by accident -- it is only
-- ever written by a user who deliberately chose "keep them".
--
-- The allowed values are constrained rather than free-form. The sweep turns
-- this straight into an interval, and an unbounded integer there is a way to
-- write 100000 into a column that silently means "never delete anything".
ALTER TABLE users
    ADD COLUMN photo_retention_days INTEGER DEFAULT 30
        CHECK (photo_retention_days IS NULL OR photo_retention_days IN (7, 30, 365));

-- 30 days for everyone who already exists, matching the new default.
--
-- Deliberately the same value rather than NULL. Backfilling "keep forever"
-- would give existing users the weakest policy in the product without them
-- choosing it, and the whole point of this migration is that the default is a
-- real, finite retention.
UPDATE users SET photo_retention_days = 30 WHERE photo_retention_days IS NULL;

-- Lets the sweep find the cohorts to purge without reading every user. Partial
-- on NOT NULL, because "keep forever" rows are exactly the ones it never wants.
CREATE INDEX idx_users_photo_retention
    ON users (photo_retention_days)
    WHERE photo_retention_days IS NOT NULL;

-- The sweep's other half: find scans that still hold an image. Partial again,
-- and heavily so -- once the sweep has run, the overwhelming majority of old
-- rows have a NULL heatmap and indexing them would be waste.
CREATE INDEX idx_scans_heatmap_present
    ON scans (user_id, submitted_at)
    WHERE heatmap IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scans_heatmap_present;
DROP INDEX IF EXISTS idx_users_photo_retention;
ALTER TABLE users DROP COLUMN photo_retention_days;
-- +goose StatementEnd
