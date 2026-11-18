package feedback_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/db/testdb"
	"moovie/internal/feedback"
)

func TestFeedbackLifecycle(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	store := feedback.NewStore(pool)
	ctx := context.Background()

	if err := store.Create(ctx, 1, "dmca", "这部作品的海报侵权", "/m/10"); err != nil {
		t.Fatal(err)
	}
	store.Create(ctx, 2, "bug", "搜索打不开", "")
	if err := store.Create(ctx, 1, "spam", "x", ""); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("未知类型应该被拒绝，实际 %v", err)
	}
	if err := store.Create(ctx, 1, "bug", "   ", ""); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("空内容应该被拒绝，实际 %v", err)
	}
	if open, _ := store.OpenCount(ctx); open != 2 {
		t.Errorf("待处理应该有 2 条，实际 %d", open)
	}
	mine, err := store.List(ctx, 1, "", 10, 0)
	if err != nil || len(mine) != 1 || mine[0].KindLabel() != "版权投诉" || mine[0].Username != "u1" {
		t.Fatalf("用户 1 的反馈不对: %+v err=%v", mine, err)
	}
	if userID, replied, err := store.Respond(ctx, mine[0].ID, "resolved", "已下架"); err != nil || userID == 0 || !replied {
		t.Fatal(err)
	}
	if _, replied, _ := store.Respond(ctx, mine[0].ID, "resolved", "已下架"); replied {
		t.Error("回复没变时不应该再通知")
	}
	if _, _, err := store.Respond(ctx, 999999, "resolved", "x"); !errors.Is(err, feedback.ErrNotFound) {
		t.Errorf("不存在的反馈: %v", err)
	}
	if _, _, err := store.Respond(ctx, mine[0].ID, "weird", ""); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("未知状态应该被拒绝，实际 %v", err)
	}
	resolved, _ := store.List(ctx, 0, "resolved", 10, 0)
	if len(resolved) != 1 || resolved[0].Reply != "已下架" || resolved[0].RepliedAt == nil {
		t.Errorf("回复没有保存: %+v", resolved)
	}
	// 用户注销后反馈保留，只是不再关联用户。
	pool.Exec(ctx, `DELETE FROM users WHERE id = 2`)
	all, _ := store.List(ctx, 0, "", 10, 0)
	if len(all) != 2 {
		t.Errorf("用户注销后反馈应该保留，实际 %d 条", len(all))
	}
}
