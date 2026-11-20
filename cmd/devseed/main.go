// devseed 往数据库里写一批虚构的演示数据，用来在浏览器里看效果。
//
// 只应该对演示用的 schema 运行（设置 DB_SCHEMA），不要对正式数据运行。
// 片名、人名都是虚构的；图片是 picsum.photos 的占位图。
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"

	"moovie/internal/adskip"
	"moovie/internal/auth"
	"moovie/internal/collections"
	"moovie/internal/config"
	"moovie/internal/db"
	"moovie/internal/library"
	"moovie/internal/media"
	"moovie/internal/playback"
	"moovie/internal/social"
)

type item struct {
	title    string
	kind     string
	year     int
	season   int
	episodes int
	rating   float64
	genres   []string
	summary  string
}

var items = []item{
	{"雾港 第二季", media.TypeTV, 2026, 2, 12, 8.9, []string{"悬疑", "犯罪", "剧情"}, "台风季的港城，一桩二十年前的旧案重新浮出水面。刚调回老家的女警和退休的老刑警，在潮湿的雾气里一点点拼出当年的真相。"},
	{"雾港", media.TypeTV, 2024, 1, 12, 8.6, []string{"悬疑", "犯罪"}, "港城码头发现一具无名尸体，所有线索都指向一艘二十年前就已沉没的渡轮。"},
	{"山海食堂", media.TypeTV, 2025, 1, 10, 8.4, []string{"剧情", "家庭"}, "海边小镇的一间食堂，只在深夜营业。每一道菜背后，都是一个舍不得离开的人。"},
	{"白夜追凶者", media.TypeTV, 2025, 1, 16, 8.2, []string{"悬疑", "犯罪", "动作"}, "极昼的北方小城，太阳不落，凶手也不睡。"},
	{"风筝线", media.TypeTV, 2023, 1, 24, 7.9, []string{"剧情", "爱情"}, "九十年代的南方小城，一封寄错的信，把两个家庭的命运缠在了一起。"},
	{"梦境收藏家", media.TypeTV, 2026, 1, 8, 8.1, []string{"科幻", "悬疑"}, "他能进入别人的梦，却找不到自己丢失的那一个。"},
	{"回声", media.TypeMovie, 2024, 1, 0, 8.7, []string{"剧情", "悬疑"}, "一位调音师在老录音带里听到了一段不该存在的对话。"},
	{"回声", media.TypeMovie, 2010, 1, 0, 7.4, []string{"恐怖"}, "搬进山谷小屋的一家人发现，这里的回声总是慢半拍。"},
	{"长夜将尽", media.TypeMovie, 2025, 1, 0, 8.3, []string{"动作", "犯罪"}, "一个通宵，一辆出租车，一座即将被拆掉的城中村。"},
	{"极光之下", media.TypeMovie, 2023, 1, 0, 8.0, []string{"爱情", "剧情"}, "两个互不相识的人，为了同一场极光，在世界尽头的小旅馆里等了七天。"},
	{"秘密花园 2", media.TypeMovie, 2026, 1, 0, 7.6, []string{"奇幻", "家庭"}, "花园的门再次打开，这一次走进来的是长大了的她。"},
	{"云边小卖部", media.TypeMovie, 2022, 1, 0, 7.8, []string{"喜剧", "家庭"}, "外婆的小卖部要拆了，全村人都来买最后一根冰棍。"},
	{"冰封航线", media.TypeMovie, 2025, 1, 0, 7.7, []string{"科幻", "冒险"}, "一艘破冰船在北极收到了来自五十年前的求救信号。"},
	{"星陨之地", media.TypeAnime, 2026, 1, 12, 8.8, []string{"动画", "奇幻", "冒险"}, "流星坠落的地方长出了一座城。少年要在它消失之前，找到回家的路。"},
	{"夜游记", media.TypeAnime, 2024, 1, 13, 8.3, []string{"动画", "奇幻"}, "每到午夜，城里的招牌就会活过来。"},
	{"小小的我", media.TypeAnime, 2025, 1, 26, 8.5, []string{"动画", "家庭", "喜剧"}, "一年级小学生的日常，比大人想的要忙得多。"},
	{"周末厨房", media.TypeShow, 2026, 3, 10, 7.9, []string{"真人秀"}, "六位不会做饭的嘉宾，要在周末撑起一间小餐馆。"},
	{"旅行的意义", media.TypeShow, 2025, 1, 12, 8.2, []string{"真人秀"}, "不做攻略，不订酒店，只带一张单程票。"},
}

func picture(seed string, width, height int) string {
	return fmt.Sprintf("https://picsum.photos/seed/%s/%d/%d.webp", seed, width, height)
}

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.DBSchema == "" {
		log.Fatal("为了不把演示数据写进正式的表，请设置 DB_SCHEMA（例如 DB_SCHEMA=mv_demo）后再运行")
	}
	// 加上 -reset 参数：先删掉整个演示 schema 再重新生成。只会删 DB_SCHEMA 指定的那一个。
	if len(os.Args) > 1 && os.Args[1] == "-reset" {
		admin, err := db.Open(ctx, cfg.DatabaseURL, db.Options{MaxConns: 1})
		must(err)
		if cfg.DBSchema == "public" {
			log.Fatal("不能重置 public")
		}
		_, err = admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgIdent(cfg.DBSchema)+` CASCADE`)
		admin.Close()
		must(err)
	}
	pool, err := db.OpenApp(ctx, cfg.DatabaseURL, cfg.DBSchema, 4)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
	users := auth.NewStore(pool)
	if _, err := users.ByEmail(ctx, "demo@moovie.local"); err == nil {
		fmt.Println("演示数据已经存在，跳过。要重新生成，先删除 schema", cfg.DBSchema)
		return
	}
	password := os.Getenv("DEMO_PASSWORD")
	if password == "" {
		password = "moovie-demo"
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	demo, err := users.Create(ctx, "demo@moovie.local", "演示账号", string(hash))
	must(err)
	lin, err := users.Create(ctx, "lin@moovie.local", "Lin", string(hash))
	must(err)
	must(users.Update(ctx, lin.ID, "is_public", true))
	must(users.Update(ctx, lin.ID, "avatar", "🦊"))

	store := media.NewStore(pool)
	ids := map[string]int64{}
	byKind := map[string][]int64{}
	today := media.Today()
	for index, entry := range items {
		id, err := store.Create(ctx)
		must(err)
		seed := fmt.Sprintf("mv%d", index)
		patch := media.Patch{
			"media_type": entry.kind, "title": entry.title, "year": entry.year, "season": entry.season, "rating_douban": entry.rating,
			"genres": entry.genres, "summary": entry.summary, "countries": []string{"中国大陆"},
			"directors": []string{"陈默"}, "actors": []string{"周予", "张望", "王景", "袁青"},
			"poster":     picture(seed, 600, 900),
			"backdrops":  []string{picture(seed+"a", 1600, 900), picture(seed+"b", 1600, 900), picture(seed+"c", 1600, 900)},
			"votes_tmdb": 1000 - index*40,
		}
		if entry.episodes > 0 {
			patch["episode_count"] = entry.episodes
			patch["series_status"] = "ended"
		} else {
			patch["duration_minutes"] = 96 + index*3
		}
		must(store.ApplyPatch(ctx, id, media.ProviderManual, patch))
		must(store.MarkSynced(ctx, id, "complete"))
		key := entry.title
		if _, taken := ids[key]; taken {
			key = fmt.Sprintf("%s %d", entry.title, entry.year)
		}
		ids[key] = id
		byKind[entry.kind] = append(byKind[entry.kind], id)
	}
	for kind, list := range byKind {
		must(store.ReplaceChart(ctx, kind, list))
	}
	// 《雾港 第二季》正在更新：前 8 集已播（第 8 集今天更新），第 9 集下周。
	second := ids["雾港 第二季"]
	must(store.ApplyPatch(ctx, second, media.ProviderManual, media.Patch{"series_status": "ongoing"}))
	titles := []string{"雾起", "旧码头", "第二个证人", "潮声", "沉船", "灯塔守夜人", "回南天", "失踪的账本", "无人认领", "台风眼", "风停之后", "归港"}
	units := []media.Unit{}
	for number, title := range titles {
		aired := today.AddDate(0, 0, (number-7)*7)
		units = append(units, media.Unit{Season: 2, Episode: number + 1, Title: title, AirDate: &aired, RuntimeMinutes: 45})
	}
	must(store.ReplaceUnits(ctx, second, 2, units))

	lib := library.NewStore(pool)
	_, err = lib.Save(ctx, demo.ID, second, library.StatusWish, 0, "")
	must(err)
	_, err = lib.Save(ctx, demo.ID, ids["回声"], library.StatusWatched, 5, "镜头相信观众，观众也就愿意相信它。")
	must(err)
	_, err = lib.Save(ctx, demo.ID, ids["山海食堂"], library.StatusWatched, 4, "")
	must(err)
	_, err = lib.Save(ctx, lin.ID, ids["回声"], library.StatusWatched, 4, "前半段很好，结尾有点刻意。但声音设计真的是影院级的。")
	must(err)
	_, err = lib.Save(ctx, lin.ID, ids["雾港"], library.StatusWatched, 5, "配乐和雾气一样黏在皮肤上。第五集码头那场戏，没有一句台词却把我看哭了。")
	must(err)
	_, err = lib.Save(ctx, lin.ID, ids["星陨之地"], library.StatusWatched, 5, "今年最好的原创动画。")
	must(err)
	soc := social.NewStore(pool)
	_, err = soc.ToggleFollow(ctx, demo.ID, lin.ID)
	must(err)
	comments, err := soc.UserComments(ctx, demo.ID, demo.ID, 1, 0)
	must(err)
	if len(comments) > 0 {
		_, err = soc.ToggleLike(ctx, lin.ID, comments[0].ID)
		must(err)
		must(soc.AddReply(ctx, lin.ID, comments[0].ID, "同意，尤其是录音棚那场戏。"))
	}

	positions := playback.NewStore(pool)
	_, err = positions.Upsert(ctx, demo.ID, []playback.Position{
		{MediaID: ids["山海食堂"], Season: 1, Episode: 2, Position: 240, Duration: 634, At: time.Now().Add(-2 * time.Hour).UnixMilli()},
		{MediaID: ids["山海食堂"], Season: 1, Episode: 1, Position: 630, Duration: 634, Completed: true, At: time.Now().Add(-26 * time.Hour).UnixMilli()},
		{MediaID: ids["长夜将尽"], Season: 1, Episode: 1, Position: 120, Duration: 634, At: time.Now().Add(-50 * time.Hour).UnixMilli()},
	})
	must(err)

	lists := collections.NewStore(pool)
	for _, draft := range []collections.Draft{
		{OwnerID: demo.ID, Slug: "harbor-noir", Title: "潮湿的悬疑", Description: "雾、码头、旧案。适合下雨天看。", Submit: true,
			MediaIDs: []int64{ids["雾港"], second, ids["白夜追凶者"], ids["回声"], ids["梦境收藏家"]}, Notes: map[int64]string{second: "比第一季更稳"}},
		{OwnerID: demo.ID, Slug: "family-night", Title: "一家人一起看", Description: "不吵不闹，老少皆宜。", Submit: true,
			MediaIDs: []int64{ids["云边小卖部"], ids["小小的我"], ids["秘密花园 2"], ids["山海食堂"]}},
	} {
		id, err := lists.Save(ctx, draft)
		must(err)
		list, err := lists.ByID(ctx, id)
		must(err)
		must(lists.Review(ctx, id, demo.ID, list.UpdatedAt, "approve", ""))
	}

	// 假资源站第 1 集开头那段"广告"的指纹：预先确认 5 次，这样打开就能看到自动跳过。
	// 片中的另一段不预置，用来看"这一段是广告吗"的询问。
	fingerprint := sha256.Sum256([]byte("10000|/hls/ad/0.ts\n10000|/hls/ad/1.ts"))
	ads := adskip.NewStore(pool)
	for range adskip.MinConfirmCount {
		must(ads.Vote(ctx, fingerprint[:], true))
	}

	fmt.Printf("演示数据已写入 schema %s：%d 部作品、2 个账号。\n登录邮箱 demo@moovie.local，密码见 cmd/devseed/main.go 里的默认值（或环境变量 DEMO_PASSWORD）。\n", cfg.DBSchema, len(items))
}

// pgIdent 给 schema 名加上引号。名字的合法性由 db.OpenApp 检查，这里再防一手。
func pgIdent(name string) string {
	for _, r := range name {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			log.Fatalf("schema 名不合法: %q", name)
		}
	}
	return `"` + name + `"`
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
