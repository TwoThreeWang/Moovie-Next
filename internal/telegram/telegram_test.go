package telegram

import "testing"

func TestPrivateChat(t *testing.T) {
	if !New("t", "123456", "", nil).Private() || New("t", "-1001234", "", nil).Private() {
		t.Error("正数 chat_id 是和机器人的私聊，负数是频道或群")
	}
}
