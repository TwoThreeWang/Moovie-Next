package app

// 临时功能：在 /sub/example.json 提供一份示例订阅（旧版保存的播放源，由 cmd/migrate-legacy -sources 导出），
// 让老用户切过来时有源可用。之后会整块删掉，删的清单见 internal/legacy/sources.go 开头。

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
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
