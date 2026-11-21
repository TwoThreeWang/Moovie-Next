-- 临时：从旧版迁移过来的账号。这些人在账号还没有钥匙圈时（也就是在新站第一次登录时），
-- 浏览器自动填入示例订阅（/sub/example.json），老用户切过来就有源可用。
-- 由 cmd/migrate-legacy 写入；删掉示例订阅功能时，加一个迁移把这张表删掉（清单见 internal/legacy/sources.go 开头）。
CREATE TABLE example_subscription_users (
    user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE
);
