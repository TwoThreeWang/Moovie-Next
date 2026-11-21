package legacy

// 账号和用户写下的东西。和作品资料不同，这些数据上线后用户会在新站上继续改，所以：
//   - 只插入新库里没有的，已经有的保留新库的版本；进度只在旧的那条更新时才覆盖。
//   - 账号和标记沿用旧 ID（关注、点赞、回复靠它们对上人和短评）。新库里同一个 ID 已经是别人的，
//     就跳过并记进报告，不能把旧数据挂到别人名下。

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"moovie/internal/auth"
)

// 新库的长度限制（见 0001_init.sql 里 user_movies.comment 和 comment_replies.content 的 CHECK）。
const (
	maxCommentRunes = 2000
	maxReplyRunes   = 300
)

func (m *migrator) migrateUsers() error {
	stats := m.report.table("users")
	// 先记下新库里已有的账号：ID 归谁、邮箱和用户名被谁占了。这些冲突都在排进批次之前判断掉。
	idEmail, emailOwner, nameOwner := map[int64]string{}, map[string]int64{}, map[string]int64{}
	rows, err := m.tx.Query(m.ctx, `SELECT id, email, username FROM users`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var email, username string
		if err := rows.Scan(&id, &email, &username); err != nil {
			rows.Close()
			return err
		}
		email = strings.ToLower(email)
		idEmail[id], emailOwner[email], nameOwner[username] = email, id, id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return m.each("users", `SELECT id, email, username, password_hash, COALESCE(role, ''), COALESCE(douban_user_id, ''), is_public,
COALESCE(avatar, ''), ad_skip_enabled, created_at FROM users ORDER BY id`, func(rows pgx.Rows) error {
		var id int64
		var email, username, hash, role, doubanID, avatar string
		var public, adSkip bool
		var created time.Time
		if err := rows.Scan(&id, &email, &username, &hash, &role, &doubanID, &public, &avatar, &adSkip, &created); err != nil {
			return err
		}
		stats.Read++
		email = strings.ToLower(strings.TrimSpace(email)) // 新版的邮箱一律小写
		if existing, ok := idEmail[id]; ok {
			if existing != email {
				stats.Skipped["ID 已被新库里的其他账号占用"]++
				return nil
			}
			// 以前迁移过（或者本人已经在新站上），账号保留新库的版本，后面的数据照常对上。
			stats.Skipped["新库里已经有了（保留新库的版本）"]++
			m.users[id] = true
			return m.markExampleSubscription(id)
		}
		if email == "" || hash == "" {
			stats.Skipped["没有邮箱或密码"]++
			return nil
		}
		if _, taken := emailOwner[email]; taken {
			stats.Skipped["邮箱已被其他账号使用"]++
			return nil
		}
		username = strings.TrimSpace(username)
		if username == "" {
			username, _, _ = strings.Cut(email, "@")
			stats.Adjusted["没有用户名，取邮箱 @ 前面的部分"]++
		}
		if _, taken := nameOwner[username]; taken {
			username += "_" + strconv.FormatInt(id, 10)
			if _, taken := nameOwner[username]; taken {
				stats.Skipped["用户名已被占用"]++
				return nil
			}
			stats.Adjusted["用户名已被占用，后面加上 _ID"]++
		}
		if role != "admin" {
			role = "user"
		}
		if !auth.ValidAvatar(avatar) {
			avatar = "🎬"
			stats.Adjusted["头像不是单个 emoji，换成默认的"]++
		}
		idEmail[id], emailOwner[email], nameOwner[username] = email, id, id
		m.users[id] = true
		if err := m.exec(stats, `INSERT INTO users (id, email, username, password_hash, role, avatar, is_public, douban_user_id, ad_skip_enabled, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
			id, email, username, hash, role, avatar, public, strings.TrimSpace(doubanID), adSkip, created); err != nil {
			return err
		}
		return m.markExampleSubscription(id)
	})
}

// markExampleSubscription 记下这是从旧版迁移过来的账号：第一次在新站登录时，浏览器自动填入示例订阅
// （临时功能，见 internal/app/example_subscription.go）。
func (m *migrator) markExampleSubscription(userID int64) error {
	return m.write(nil, "", `INSERT INTO example_subscription_users (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userID)
}

func (m *migrator) migrateFollows() error {
	stats := m.report.table("user_follows")
	return m.each("user_follows", `SELECT follower_id, followee_id, created_at FROM user_follows`, func(rows pgx.Rows) error {
		var follower, followee int64
		var created time.Time
		if err := rows.Scan(&follower, &followee, &created); err != nil {
			return err
		}
		stats.Read++
		if !m.users[follower] || !m.users[followee] || follower == followee {
			stats.Skipped["账号没有迁移"]++
			return nil
		}
		return m.exec(stats, `INSERT INTO user_follows (follower_id, followee_id, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			follower, followee, created)
	})
}

// migrateMarks 迁移想看和看过，评分、短评在同一行里。
func (m *migrator) migrateMarks() error {
	stats := m.report.table("user_movies")
	type mark struct{ user, media int64 }
	idMark, markID := map[int64]mark{}, map[mark]int64{}
	rows, err := m.tx.Query(m.ctx, `SELECT id, user_id, media_id FROM user_movies`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var key mark
		if err := rows.Scan(&id, &key.user, &key.media); err != nil {
			rows.Close()
			return err
		}
		idMark[id], markID[key] = key, id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return m.each("user_movies", `SELECT id, user_id, media_id, status, COALESCE(rating, 0), COALESCE(comment, ''), created_at, updated_at
FROM user_movies ORDER BY id`, func(rows pgx.Rows) error {
		var id, userID int64
		var mediaID *int64
		var status, comment string
		var rating int
		var created, updated time.Time
		if err := rows.Scan(&id, &userID, &mediaID, &status, &rating, &comment, &created, &updated); err != nil {
			return err
		}
		stats.Read++
		switch {
		case !m.users[userID]:
			stats.Skipped["账号没有迁移"]++
			return nil
		case mediaID == nil || !m.media[*mediaID]:
			stats.Skipped["作品没有迁移"]++
			return nil
		case status != "wish" && status != "watched":
			stats.Skipped["状态不是想看或看过"]++
			return nil
		}
		key := mark{userID, *mediaID}
		if existing, ok := idMark[id]; ok {
			if existing != key {
				stats.Skipped["ID 已被新库里的其他标记占用"]++
				return nil
			}
			stats.Skipped["新库里已经有了（保留新库的版本）"]++
			m.marks[id] = true
			return nil
		}
		if _, ok := markID[key]; ok {
			stats.Skipped["新库里已经标记过这部作品（保留新库的版本）"]++
			return nil
		}
		if rating < 0 || rating > 5 {
			rating = min(max(rating, 0), 5)
			stats.Adjusted["评分不在 0–5 之间，已截到范围内"]++
		}
		comment = strings.TrimSpace(comment)
		if utf8.RuneCountInString(comment) > maxCommentRunes {
			comment = string([]rune(comment)[:maxCommentRunes])
			stats.Adjusted["短评超过 2000 字，已截断"]++
		}
		idMark[id], markID[key] = key, id
		m.marks[id] = true
		return m.exec(stats, `INSERT INTO user_movies (id, user_id, media_id, status, rating, comment, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`, id, userID, *mediaID, status, rating, comment, created, updated)
	})
}

func (m *migrator) migrateLikes() error {
	stats := m.report.table("comment_likes")
	return m.each("comment_likes", `SELECT user_movie_id, user_id, created_at FROM comment_likes`, func(rows pgx.Rows) error {
		var markID, userID int64
		var created time.Time
		if err := rows.Scan(&markID, &userID, &created); err != nil {
			return err
		}
		stats.Read++
		if !m.marks[markID] || !m.users[userID] {
			stats.Skipped["短评或账号没有迁移"]++
			return nil
		}
		return m.exec(stats, `INSERT INTO comment_likes (user_movie_id, user_id, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			markID, userID, created)
	})
}

// migrateReplies 迁移短评的回复。回复的 ID 由新库生成，按"同一条短评、同一个人、同一时间、同样内容"判断有没有迁移过。
func (m *migrator) migrateReplies() error {
	stats := m.report.table("comment_replies")
	return m.each("comment_replies", `SELECT user_movie_id, user_id, content, created_at FROM comment_replies ORDER BY id`, func(rows pgx.Rows) error {
		var markID, userID int64
		var content string
		var created time.Time
		if err := rows.Scan(&markID, &userID, &content, &created); err != nil {
			return err
		}
		stats.Read++
		if !m.marks[markID] || !m.users[userID] {
			stats.Skipped["短评或账号没有迁移"]++
			return nil
		}
		content = strings.TrimSpace(content)
		if length := utf8.RuneCountInString(content); length == 0 || length > maxReplyRunes {
			stats.Skipped["内容为空或超过 300 字"]++
			return nil
		}
		return m.exec(stats, `INSERT INTO comment_replies (user_movie_id, user_id, content, created_at) SELECT $1, $2, $3, $4
WHERE NOT EXISTS (SELECT 1 FROM comment_replies WHERE user_movie_id = $1 AND user_id = $2 AND created_at = $4 AND content = $3)`,
			markID, userID, content, created)
	})
}

// oldEpisodeKey 是旧版的集键，比如 S01E03，后面可能带着上下集的后缀（S01E03:上）。
var oldEpisodeKey = regexp.MustCompile(`^S(\d+)E(\d+)`)

// migratePlayback 迁移作品库里有的作品的播放进度。新版按"作品、季、集"记：季取作品在新库里的季数
// （和播放页一样），集号优先用旧版分集表里的集号，没有就从集键里解析；电影固定是第 1 季第 1 集。
// 来源、资源站的 vod_id 一律不要。每条进度当天记为看过东西的一天（连续观影天数靠它）。
func (m *migrator) migratePlayback() error {
	stats := m.report.table("playback_positions")
	return m.each("playback_positions", `SELECT p.user_id, p.media_id, COALESCE(u.unit_type, ''), COALESCE(u.episode_number, 0), COALESCE(p.episode_key, ''),
COALESCE(p.position_seconds, 0), COALESCE(p.duration_seconds, 0), COALESCE(p.completed, false), p.activity_at
FROM playback_positions p LEFT JOIN media_units u ON u.id = p.media_unit_id
WHERE p.deleted_at IS NULL ORDER BY p.id`, func(rows pgx.Rows) error {
		var userID int64
		var mediaID *int64
		var unitType, key string
		var unitEpisode int
		var position, duration float64
		var completed bool
		var activity time.Time
		if err := rows.Scan(&userID, &mediaID, &unitType, &unitEpisode, &key, &position, &duration, &completed, &activity); err != nil {
			return err
		}
		stats.Read++
		switch {
		case mediaID == nil:
			stats.Skipped["作品库里没有的片子（新版由浏览器加密保存）"]++
			return nil
		case !m.users[userID]:
			stats.Skipped["账号没有迁移"]++
			return nil
		case !m.media[*mediaID]:
			stats.Skipped["作品没有迁移"]++
			return nil
		}
		season, episode := m.seasons[*mediaID], 0
		if m.movies[*mediaID] {
			season, episode = 1, 1
		} else if unitType == "episode" && unitEpisode > 0 {
			episode = unitEpisode
		} else if match := oldEpisodeKey.FindStringSubmatch(strings.ToUpper(key)); match != nil {
			episode, _ = strconv.Atoi(match[2])
		}
		if episode <= 0 {
			stats.Skipped["认不出是第几集（按日期排的期数等）"]++
			return nil
		}
		if err := m.write(stats, "新库里已经有更新的进度", `INSERT INTO playback_positions
(user_id, media_id, season, episode, position_seconds, duration_seconds, completed, activity_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id, media_id, season, episode) DO UPDATE SET
  position_seconds = EXCLUDED.position_seconds, duration_seconds = EXCLUDED.duration_seconds,
  completed = EXCLUDED.completed, activity_at = EXCLUDED.activity_at
WHERE playback_positions.activity_at < EXCLUDED.activity_at`,
			userID, *mediaID, season, episode, max(position, 0), max(duration, 0), completed, activity); err != nil {
			return err
		}
		return m.write(nil, "", `INSERT INTO watch_days (user_id, day) VALUES ($1, ($2::timestamptz)::date) ON CONFLICT DO NOTHING`, userID, activity)
	})
}
