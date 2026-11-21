package app

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"moovie/internal/ads"
	"moovie/internal/httpx"
)

// adminAds 是广告设置页：播放页弹幕下方、详情页信息下方共用的广告位，可以有多条，竖着往下排，每条高 60px。
func (a *App) adminAds(c *gin.Context) {
	a.renderAdminAds(c, nil, ads.Ad{Slot: ads.Sidebar, Kind: "image", Enabled: true}, "", 0)
}

// renderAdminAds 显示广告列表和添加表单。表单填错时 errorFor 是出错的那条（0 表示新加的那条），
// 那一条显示刚填的内容和错误，别的照常。
func (a *App) renderAdminAds(c *gin.Context, edited *ads.Ad, draft ads.Ad, message string, errorFor int64) {
	list, err := a.Ads.List(c.Request.Context(), ads.Sidebar)
	if err != nil {
		a.fail(c, err)
		return
	}
	if edited != nil {
		for i := range list {
			if list[i].ID == edited.ID {
				list[i] = *edited
			}
		}
	}
	a.adminPage(c, "admin_ads.html", "广告", "ads", gin.H{"Ads": list, "Draft": draft, "Error": message, "ErrorFor": errorFor})
}

func adFromForm(c *gin.Context) ads.Ad {
	position, _ := strconv.Atoi(strings.TrimSpace(c.PostForm("position")))
	return ads.Ad{
		Slot: ads.Sidebar, Position: position, Enabled: c.PostForm("enabled") == "on", Kind: c.PostForm("kind"),
		ImageURL: c.PostForm("image_url"), Text: c.PostForm("text"), Link: c.PostForm("link"),
	}
}

func (a *App) adminAdCreate(c *gin.Context) {
	ad := adFromForm(c)
	if _, err := a.Ads.Create(c.Request.Context(), ad); err != nil {
		if isAdInputError(err) {
			ads.Normalize(&ad)
			a.renderAdminAds(c, nil, ad, err.Error(), 0)
			return
		}
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/ads", "已添加")
}

func (a *App) adminAdUpdate(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	ad := adFromForm(c)
	ad.ID = id
	if err := a.Ads.Update(c.Request.Context(), ad); err != nil {
		if isAdInputError(err) {
			ads.Normalize(&ad)
			a.renderAdminAds(c, &ad, ads.Ad{Slot: ads.Sidebar, Kind: "image", Enabled: true}, err.Error(), id)
			return
		}
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/ads", "已保存")
}

func (a *App) adminAdDelete(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := a.Ads.Delete(c.Request.Context(), ads.Sidebar, id); err != nil && !errors.Is(err, ads.ErrNotFound) {
		a.fail(c, err)
		return
	}
	adminBack(c, "/admin/ads", "已删除")
}

// isAdInputError 区分"填错了"（给管理员看）和数据库出错（按服务器错误处理）。
func isAdInputError(err error) bool {
	var input *ads.InputError
	return errors.As(err, &input) || errors.Is(err, ads.ErrNotFound)
}
