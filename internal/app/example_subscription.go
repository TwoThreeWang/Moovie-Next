package app

// 临时功能：示例订阅，让老用户切过来时有源可用。设置页的"使用示例订阅"和从旧版迁移过来的账号第一次登录时
// 自动填入的（见 pendingExampleSubscription），都是 exampleSubscriptionURL 这个外部地址。
// 订阅内容是旧版保存的播放源，由 cmd/migrate-legacy -sources 导出后放到外部地址。
// 本站以前的 /sub/example.json 现在跳到外部地址，已经填了它的用户还能继续用。
// 之后会整块删掉，删的清单见 internal/legacy/sources.go 开头。

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"moovie/internal/auth"
)

const exampleSubscriptionPath = "/sub/example.json"

// exampleSubscriptionURL 是给用户填的示例订阅地址。不在本站，浏览器读取时走 /api/relay 转发。
const exampleSubscriptionURL = "https://static.error.workers.dev/moovie/examplesub.json"

// exampleSubscription 把本站以前提供的 /sub/example.json 跳到外部地址，之前填了这个地址的用户不用改也能继续用。
// 本站地址由浏览器直接读取（不走转发），跟着跳过去就是跨域请求，外部地址的响应要带 Access-Control-Allow-Origin。
func (a *App) exampleSubscription(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=3600")
	c.Redirect(http.StatusFound, exampleSubscriptionURL)
}

// pendingExampleSubscription 告诉刚登录的浏览器要不要自动填入示例订阅：从旧版迁移过来的账号，
// 并且账号还没有钥匙圈（第一次在新站登录）时返回地址，其余情况 404。
// 钥匙圈建好以后就不再返回，所以用户之后清除或换掉订阅，不会再被填回来。
func (a *App) pendingExampleSubscription(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
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
	c.JSON(http.StatusOK, gin.H{"url": exampleSubscriptionURL})
}
