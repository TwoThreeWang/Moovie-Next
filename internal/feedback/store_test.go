package feedback_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/db/testdb"
	"moovie/internal/feedback"
)

func TestFeedbackThread(t *testing.T) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2)
	store := feedback.NewStore(pool)
	ctx := context.Background()

	dmca, err := store.Create(ctx, 1, "dmca", "这部作品的海报侵权", "/m/10")
	if err != nil {
		t.Fatal(err)
	}
	bug, _ := store.Create(ctx, 2, "bug", "搜索打不开", "")
	if _, err := store.Create(ctx, 1, "spam", "x", ""); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("未知类型应该被拒绝，实际 %v", err)
	}
	if _, err := store.Create(ctx, 1, "bug", "   ", ""); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("空内容应该被拒绝，实际 %v", err)
	}
	if open, _ := store.OpenCount(ctx); open != 2 {
		t.Errorf("待回复应该有 2 条，实际 %d", open)
	}
	mine, err := store.List(ctx, 1, "", 10, 0)
	if err != nil || len(mine) != 1 || mine[0].KindLabel() != "版权投诉" || mine[0].Username != "u1" ||
		mine[0].Preview != "这部作品的海报侵权" || mine[0].State != feedback.StateStaff || mine[0].Count != 1 {
		t.Fatalf("用户 1 的反馈不对: %+v err=%v", mine, err)
	}

	// 管理员追问，状态变成等用户回复
	if userID, err := store.Reply(ctx, dmca, 0, 2, true, "请提供权利证明"); err != nil || userID != 1 {
		t.Fatalf("管理员回复: userID=%d err=%v", userID, err)
	}
	if f, _ := store.Get(ctx, dmca, 1); f.State != feedback.StateUser || f.UserStateLabel() != "已回复" || len(f.Messages) != 2 || !f.Messages[1].Staff {
		t.Errorf("管理员回复后状态不对: %+v", f)
	}
	if open, _ := store.OpenCount(ctx); open != 1 {
		t.Errorf("回复后待回复应该剩 1 条，实际 %d", open)
	}
	// 别人不能回复、关闭、查看不属于自己的反馈
	if _, err := store.Reply(ctx, dmca, 2, 2, false, "插一句"); !errors.Is(err, feedback.ErrNotFound) {
		t.Errorf("回复别人的反馈应该被拒绝，实际 %v", err)
	}
	if _, err := store.Close(ctx, dmca, 2, ""); !errors.Is(err, feedback.ErrNotFound) {
		t.Errorf("关闭别人的反馈应该被拒绝，实际 %v", err)
	}
	if _, err := store.Get(ctx, dmca, 2); !errors.Is(err, feedback.ErrNotFound) {
		t.Errorf("看别人的反馈应该被拒绝，实际 %v", err)
	}
	// 用户补充材料，又回到待回复
	if _, err := store.Reply(ctx, dmca, 1, 1, false, "证明附在这里"); err != nil {
		t.Fatal(err)
	}
	if f, _ := store.Get(ctx, dmca, 0); f.State != feedback.StateStaff || f.Count != 3 {
		t.Errorf("用户回复后应该回到待回复: %+v", f)
	}
	// 管理员关闭并标结果
	if _, err := store.Close(ctx, dmca, 0, "weird"); !errors.Is(err, feedback.ErrInvalid) {
		t.Errorf("未知结果应该被拒绝，实际 %v", err)
	}
	if _, err := store.Close(ctx, 999999, 0, "done"); !errors.Is(err, feedback.ErrNotFound) {
		t.Errorf("不存在的反馈: %v", err)
	}
	if _, err := store.Close(ctx, dmca, 0, "done"); err != nil {
		t.Fatal(err)
	}
	closed, _ := store.List(ctx, 0, feedback.StateClosed, 10, 0)
	if len(closed) != 1 || closed[0].UserStateLabel() != "已处理" || closed[0].AdminStateLabel() != "已关闭 · 已处理" || closed[0].ClosedAt == nil {
		t.Errorf("关闭后状态不对: %+v", closed)
	}
	// 关闭后用户再回复会重新打开，结果清空
	store.Reply(ctx, dmca, 1, 1, false, "又出现了")
	if f, _ := store.Get(ctx, dmca, 1); f.State != feedback.StateStaff || f.Outcome != "" || f.ClosedAt != nil {
		t.Errorf("再回复应该重新打开: %+v", f)
	}

	// 管理员回复后用户超过 14 天不回，自动算关闭；用户再回复又打开
	store.Reply(ctx, bug, 0, 1, true, "请问是哪个浏览器？")
	pool.Exec(ctx, `UPDATE feedback_messages SET created_at = now() - interval '15 days' WHERE feedback_id = $1`, bug)
	if f, _ := store.Get(ctx, bug, 2); f.State != feedback.StateClosed || f.UserStateLabel() != "已关闭" {
		t.Errorf("超过 14 天没回复应该自动关闭: %+v", f)
	}
	store.Reply(ctx, bug, 2, 2, false, "Safari")
	if f, _ := store.Get(ctx, bug, 2); f.State != feedback.StateStaff {
		t.Errorf("自动关闭后用户回复应该重新打开: %+v", f)
	}
	// 用户自己关闭
	if _, err := store.Close(ctx, bug, 2, ""); err != nil {
		t.Fatal(err)
	}
	if f, _ := store.Get(ctx, bug, 2); f.State != feedback.StateClosed {
		t.Errorf("用户关闭后状态不对: %+v", f)
	}

	// 用户注销后反馈保留，只是不再关联用户。
	pool.Exec(ctx, `DELETE FROM users WHERE id = 2`)
	all, _ := store.List(ctx, 0, "", 10, 0)
	if len(all) != 2 {
		t.Errorf("用户注销后反馈应该保留，实际 %d 条", len(all))
	}
	if all[0].ID != bug {
		t.Errorf("最近有消息的应该排在前面: %+v", all)
	}
}
