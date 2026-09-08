-- +goose Up
-- +goose StatementBegin

-- The redness overlay (FR-4), stored as the composited JPEG.
--
-- ONLY the composite is kept, never a clean face photograph. The alternative --
-- storing the original and blending on the client -- would mean holding an
-- unmarked photo of every user's face for every scan, which is a materially
-- heavier thing to be responsible for than an image that is already annotated.
-- It also halves the bytes.
--
-- In Postgres rather than object storage, deliberately and temporarily. The
-- observed size is ~30KB per scan, which at this volume is nothing, and R2 is
-- an entire piece of infrastructure to stand up for a feature nobody has used
-- yet. `heatmapUrl` in the contract is already a URL, so moving to object
-- storage later changes what that URL points at and nothing else.
--
-- Deletion is inherited: this column lives on `scans`, which cascades from
-- `users`, so FR-11's account deletion takes the overlays with it and cannot
-- forget to.
ALTER TABLE scans ADD COLUMN heatmap BYTEA;

-- NOT NULL is deliberately absent. Rejected scans, failed scans, and any scan
-- where the overlay could not be built safely all have no heatmap, and that
-- absence is a fact the report screen already knows how to render.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE scans DROP COLUMN heatmap;
-- +goose StatementEnd
