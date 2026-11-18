package social_test

import (
	"context"
	"errors"
	"testing"

	"moovie/internal/db"
	"moovie/internal/db/testdb"
	"moovie/internal/social"
)

func setup(t *testing.T) (*db.Pool, *social.Store, int64) {
	pool := testdb.Pool(t)
	testdb.User(t, pool, 1, 2, 3)
	testdb.Media(t, pool, 10, 11)
	var commentID int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO user_movies (user_id, media_id, status, rating, comment)
VALUES (1, 10, 'watched', 5, '很好看') RETURNING id`).Scan(&commentID); err != nil {
		t.Fatal(err)
	}
	return pool, social.NewStore(pool), commentID
}

func TestLikeToggleAndNotification(t *testing.T) {
	_, store, commentID := setup(t)
	ctx := context.Background()

	liked, err := store.ToggleLike(ctx, 2, commentID)
	if err != nil || !liked {
		t.Fatalf("第一次应该是点赞: %v %v", liked, err)
	}
	comment, err := store.Comment(ctx, 2, commentID)
	if err != nil || comment.Likes != 1 || !comment.Liked || comment.Username != "u1" {
		t.Fatalf("短评的点赞数据不对: %+v err=%v", comment, err)
	}
	if unread, _ := store.Unread(ctx, 1); unread != 1 {
		t.Errorf("作者应该收到一条通知，实际 %d", unread)
	}
	// 自己赞自己不发通知。
	store.ToggleLike(ctx, 1, commentID)
	if unread, _ := store.Unread(ctx, 1); unread != 1 {
		t.Errorf("自己赞自己不应该产生通知，实际 %d", unread)
	}
	// 取消点赞，通知也撤回。
	liked, err = store.ToggleLike(ctx, 2, commentID)
	if err != nil || liked {
		t.Fatalf("第二次应该是取消: %v %v", liked, err)
	}
	if unread, _ := store.Unread(ctx, 1); unread != 0 {
		t.Errorf("取消点赞后通知应该撤回，实际 %d", unread)
	}
	if _, err := store.ToggleLike(ctx, 2, 99999); !errors.Is(err, social.ErrNotFound) {
		t.Errorf("不存在的短评应该返回 ErrNotFound，实际 %v", err)
	}
}

func TestRepliesAndNotifications(t *testing.T) {
	_, store, commentID := setup(t)
	ctx := context.Background()
	if err := store.AddReply(ctx, 2, commentID, "  同意  "); err != nil {
		t.Fatal(err)
	}
	if err := store.AddReply(ctx, 2, commentID, "   "); err == nil {
		t.Error("空回复应该被拒绝")
	}
	replies, err := store.Replies(ctx, commentID)
	if err != nil || len(replies) != 1 || replies[0].Content != "同意" || replies[0].Username != "u2" {
		t.Fatalf("回复不对: %+v err=%v", replies, err)
	}
	notes, err := store.Notifications(ctx, 1, 10, 0)
	if err != nil || len(notes) != 1 || notes[0].Kind != "reply" || notes[0].Excerpt != "同意" || notes[0].MediaTitle != "作品10" || notes[0].Read {
		t.Fatalf("通知不对: %+v err=%v", notes, err)
	}
	store.MarkAllRead(ctx, 1)
	if unread, _ := store.Unread(ctx, 1); unread != 0 {
		t.Error("全部标为已读后不应该还有未读")
	}
	// 别人不能删，作者和管理员可以。
	if _, err := store.DeleteReply(ctx, 3, false, replies[0].ID, ""); !errors.Is(err, social.ErrNotFound) {
		t.Errorf("别人删除回复应该失败，实际 %v", err)
	}
	if got, err := store.DeleteReply(ctx, 3, true, replies[0].ID, ""); err != nil || got != commentID {
		t.Errorf("管理员应该能删除: %d %v", got, err)
	}
}

func TestFollowFeedAndProfile(t *testing.T) {
	pool, store, commentID := setup(t)
	ctx := context.Background()
	pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status, comment) VALUES (3, 11, 'watched', '一般')`)
	pool.Exec(ctx, `INSERT INTO user_movies (user_id, media_id, status) VALUES (2, 11, 'wish')`) // 没有短评，不进动态

	if _, err := store.ToggleFollow(ctx, 2, 2); err == nil {
		t.Error("不能关注自己")
	}
	following, err := store.ToggleFollow(ctx, 2, 1)
	if err != nil || !following {
		t.Fatalf("应该关注成功: %v %v", following, err)
	}
	all, err := store.Feed(ctx, 2, false, 10, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("全部动态应该有 2 条短评: %d err=%v", len(all), err)
	}
	mine, err := store.Feed(ctx, 2, true, 10, 0)
	if err != nil || len(mine) != 1 || mine[0].ID != commentID {
		t.Fatalf("只看关注的人时应该只有用户 1 的短评: %+v err=%v", mine, err)
	}
	profile, err := store.Profile(ctx, 2, 1)
	if err != nil || profile.Followers != 1 || !profile.Followed {
		t.Errorf("资料里的关注数据不对: %+v err=%v", profile, err)
	}
	if notes, _ := store.Notifications(ctx, 1, 10, 0); len(notes) != 1 || notes[0].Kind != "follow" {
		t.Errorf("被关注的人应该收到通知: %+v", notes)
	}
	store.ToggleFollow(ctx, 2, 1)
	if profile, _ := store.Profile(ctx, 2, 1); profile.Followers != 0 || profile.Followed {
		t.Errorf("取消关注后数据应该恢复: %+v", profile)
	}
	if comments, _ := store.MediaComments(ctx, 2, 10, 10, 0); len(comments) != 1 {
		t.Errorf("作品 10 下应该有 1 条短评，实际 %d", len(comments))
	}
	if comments, _ := store.UserComments(ctx, 2, 3, 10, 0); len(comments) != 1 || comments[0].MediaID != 11 {
		t.Errorf("用户 3 的短评不对: %+v", comments)
	}
}
