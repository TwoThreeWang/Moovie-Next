// tgcheck 检查 Telegram 图床的配置。只读，不发消息。
//
//	go run ./cmd/tgcheck
//
// 还没有 TELEGRAM_CHAT_ID 时，它会列出机器人最近收到消息的会话，帮你找到 chat_id：
// 最简单的是和机器人私聊——在 Telegram 里搜到你的机器人，发一句 /start，再运行这个命令。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"moovie/internal/config"
	"moovie/internal/telegram"
)

func main() {
	config.LoadDotEnv()
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	if cfg.TelegramToken == "" {
		fail(fmt.Errorf("没有配置 TELEGRAM_BOT_TOKEN（找 @BotFather 新建一个机器人就能拿到）"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := telegram.New(cfg.TelegramToken, cfg.TelegramChatID, cfg.TelegramAPI, nil)
	if cfg.TelegramChatID == "" {
		chats, err := client.RecentChats(ctx)
		if err != nil {
			fail(err)
		}
		if len(chats) == 0 {
			fmt.Println("机器人最近没有收到消息。先在 Telegram 里给你的机器人发一句 /start（或者在你的频道 / 群里发一条），再运行一次。")
			return
		}
		fmt.Println("机器人最近收到消息的会话，把其中一个的 ID 填到 TELEGRAM_CHAT_ID：")
		for _, chat := range chats {
			fmt.Printf("  %d\t%s\t%s\n", chat.ID, chat.Type, chat.Title)
		}
		fmt.Println("private 是和你的私聊（最简单，每秒能存一张）；channel / supergroup 要先把机器人设为管理员。")
		return
	}
	bot, chat, err := client.Check(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "提示：频道和群要先把机器人加进去并设为管理员；私聊要先给机器人发一句 /start。")
		fail(err)
	}
	fmt.Printf("机器人 @%s 可以访问：%s\n", bot, chat)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "失败:", err)
	os.Exit(1)
}
