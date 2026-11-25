package extapi

import "context"

// Tier 是用插件补全资料得到的称号：补全够 Min 部作品得到这一级。
type Tier struct {
	Level int
	Title string
	Min   int
}

// tiers 从低到高。插件一天最多补全 300 部，最高一级要坚持两个多月。
var tiers = []Tier{
	{1, "拾片人", 1},
	{2, "场记", 50},
	{3, "资料员", 200},
	{4, "档案官", 1000},
	{5, "片库守护者", 5000},
	{6, "影史编修", 20000},
}

// TierFor 返回补全了 count 部作品对应的称号；一部都没有时 Level 是 0。
func TierFor(count int) Tier {
	var tier Tier
	for _, t := range tiers {
		if count >= t.Min {
			tier = t
		}
	}
	return tier
}

// Contributor 是贡献榜上的一个人。
type Contributor struct {
	UserID   int64
	Username string
	Avatar   string
	Count    int
	Tier     Tier
}

// Leaderboard 按补全数量列出前 limit 名。只列公开了主页的人：上榜就是把主页亮给大家看，没公开的不拿出来。
// 数量相同时，先开始补全的排前面。
func (s *Store) Leaderboard(ctx context.Context, limit int) ([]Contributor, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.username, u.avatar, count(*) FROM media_contributions c
JOIN users u ON u.id = c.user_id WHERE u.is_public
GROUP BY u.id ORDER BY count(*) DESC, min(c.created_at), u.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Contributor{}
	for rows.Next() {
		var item Contributor
		if err := rows.Scan(&item.UserID, &item.Username, &item.Avatar, &item.Count); err != nil {
			return nil, err
		}
		item.Tier = TierFor(item.Count)
		list = append(list, item)
	}
	return list, rows.Err()
}
