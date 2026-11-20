-- 已发布片单的修改稿。公开内容仍是 collections + collection_items，
-- revision 只保存作者提出、尚未通过的新内容：{"title", "description", "items": [{"media_id", "note"}]}。
-- 通过后合并进正式内容并清空；退回时保留修改稿和原因，作者可以继续修改。
ALTER TABLE collections
    ADD COLUMN revision jsonb,
    ADD COLUMN revision_submitted_at timestamptz,
    ADD CHECK (revision IS NULL OR status = 'published'),
    ADD CHECK (revision_submitted_at IS NULL OR revision IS NOT NULL);

CREATE INDEX collections_revision_review_idx ON collections (revision_submitted_at, id) WHERE revision_submitted_at IS NOT NULL;
