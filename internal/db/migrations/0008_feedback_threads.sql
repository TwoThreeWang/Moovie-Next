-- 反馈改成对话：用户和管理员可以来回回复，内容都存在 feedback_messages 里，第一条就是用户提交的内容。
-- 处理状态不再手动设置，而是看最后一条消息是谁发的：用户发的就是"待回复"，管理员发的就是"等用户回复"，
-- 等用户回复超过 14 天自动算作关闭。closed_at 只记管理员或用户主动关闭的时间，任何一方再发消息都会重新打开。
-- outcome 是关闭时可选的结果：done（已修复 / 已上线 / 已采纳 / 已处理）、declined（不处理 / 找不到 / 未采纳 / 已驳回），空表示不标结果。
CREATE TABLE feedback_messages (
    id          bigserial PRIMARY KEY,
    feedback_id bigint NOT NULL REFERENCES feedbacks(id) ON DELETE CASCADE,
    author_id   bigint REFERENCES users(id) ON DELETE SET NULL,
    staff       boolean NOT NULL DEFAULT false,
    content     text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 5000),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX feedback_messages_thread_idx ON feedback_messages (feedback_id, id);

INSERT INTO feedback_messages (feedback_id, author_id, staff, content, created_at)
SELECT id, user_id, false, content, created_at FROM feedbacks;
INSERT INTO feedback_messages (feedback_id, author_id, staff, content, created_at)
SELECT id, NULL, true, left(reply, 5000), COALESCE(replied_at, created_at) FROM feedbacks WHERE reply <> '';

ALTER TABLE feedbacks
    ADD COLUMN closed_at timestamptz,
    ADD COLUMN outcome text NOT NULL DEFAULT '' CHECK (outcome IN ('', 'done', 'declined'));
UPDATE feedbacks SET closed_at = COALESCE(replied_at, created_at),
    outcome = CASE status WHEN 'resolved' THEN 'done' ELSE 'declined' END
WHERE status <> 'open';
ALTER TABLE feedbacks DROP COLUMN content, DROP COLUMN reply, DROP COLUMN replied_at, DROP COLUMN status;
CREATE INDEX feedbacks_user_idx ON feedbacks (user_id, id DESC);
