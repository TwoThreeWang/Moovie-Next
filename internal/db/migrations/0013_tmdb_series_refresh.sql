ALTER TABLE media
  ADD COLUMN tmdb_series_checked_at timestamptz,
  ADD COLUMN tmdb_season_unmatched boolean NOT NULL DEFAULT false;

CREATE INDEX media_tmdb_series_due_idx ON media (tmdb_series_checked_at NULLS FIRST, id)
WHERE media_type IN ('tv', 'show', 'anime') AND tmdb_id LIKE 'tv/%'
  AND series_status <> 'ended' AND NOT hidden AND metadata_status <> 'not_found';
