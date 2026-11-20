ALTER TABLE collections
    ADD COLUMN status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'pending', 'published', 'returned')),
    ADD COLUMN published_at timestamptz,
    ADD COLUMN submitted_at timestamptz,
    ADD COLUMN review_reason text NOT NULL DEFAULT '',
    ADD COLUMN link_visible boolean NOT NULL DEFAULT false,
    ADD COLUMN views bigint NOT NULL DEFAULT 0,
    ADD COLUMN legacy_editorial boolean NOT NULL DEFAULT false;

UPDATE collections SET status = 'published', published_at = created_at, legacy_editorial = true;
ALTER TABLE collections DROP COLUMN featured;
ALTER TABLE collections
    ADD CHECK (NOT link_visible OR status IN ('draft', 'returned')),
    ADD CHECK (status <> 'pending' OR submitted_at IS NOT NULL),
    ADD CHECK (status <> 'published' OR published_at IS NOT NULL);

CREATE INDEX collections_public_idx ON collections (published_at DESC, id DESC) WHERE status = 'published';
CREATE INDEX collections_author_public_idx ON collections (owner_user_id, published_at DESC, id DESC) WHERE status = 'published';
CREATE INDEX collections_owner_idx ON collections (owner_user_id, updated_at DESC, id DESC);
CREATE INDEX collections_review_idx ON collections (submitted_at, id) WHERE status = 'pending';

CREATE TABLE collection_favorites (
    collection_id bigint NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    active boolean NOT NULL DEFAULT true,
    PRIMARY KEY (collection_id, user_id)
);
CREATE INDEX collection_favorites_user_idx ON collection_favorites (user_id, created_at DESC) WHERE active;
CREATE INDEX collection_favorites_recent_idx ON collection_favorites (collection_id, created_at) WHERE active;

CREATE TABLE collection_views (
    collection_id bigint NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    day date NOT NULL,
    viewer bytea NOT NULL,
    PRIMARY KEY (collection_id, day, viewer)
);
CREATE INDEX collection_views_day_idx ON collection_views (day);

ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check
    CHECK (kind IN ('like', 'reply', 'follow', 'feedback', 'removed', 'muted', 'collection_approved', 'collection_returned', 'collection_removed'));
ALTER TABLE notifications ADD COLUMN collection_id bigint REFERENCES collections(id) ON DELETE CASCADE;
