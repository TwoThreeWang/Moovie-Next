package app

// 临时功能：在 /sub/example.json 提供一份示例订阅（旧版保存的播放源，由 cmd/migrate-legacy -sources 导出），
// 让老用户切过来时有源可用。从旧版迁移过来的账号第一次登录时，浏览器自动填入它（见 pendingExampleSubscription）。
// 之后会整块删掉，删的清单见 internal/legacy/sources.go 开头。

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
)

const exampleSubscriptionPath = "/sub/example.json"

// loadExampleSubscription 读取 EXAMPLE_SUBSCRIPTION_FILE。没配置、读不了或者不是带 sources 的 JSON 时返回 nil，功能关闭。
func (a *App) loadExampleSubscription() []byte {
	path := a.Config.ExampleSubscription
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(path)
	var parsed struct {
		Sources []json.RawMessage `json:"sources"`
	}
	if err == nil {
		err = json.Unmarshal(body, &parsed)
	}
	if err != nil || len(parsed.Sources) == 0 {
		a.Log.Warn("示例订阅文件不可用，已关闭", "path", path, "error", err)
		return nil
	}
	return body
}

func (a *App) exampleSubscription(c *gin.Context) {
	if a.exampleSub == nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/json; charset=utf-8", a.exampleSub)
}

// pendingExampleSubscription 告诉刚登录的浏览器要不要自动填入示例订阅：从旧版迁移过来的账号，
// 并且账号还没有钥匙圈（第一次在新站登录）时返回地址，其余情况 404。
// 钥匙圈建好以后就不再返回，所以用户之后清除或换掉订阅，不会再被填回来。
func (a *App) pendingExampleSubscription(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if a.exampleSub == nil {
		c.Status(http.StatusNotFound)
		return
	}
	var pending bool
	err := a.Pool.QueryRow(c.Request.Context(), `SELECT EXISTS (SELECT 1 FROM example_subscription_users WHERE user_id = $1)
AND NOT EXISTS (SELECT 1 FROM keyrings WHERE user_id = $1)`, auth.Current(c).ID).Scan(&pending)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	if !pending {
		c.Status(http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": exampleSubscriptionPath})
}
