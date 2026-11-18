# tvui-lab · 自适应界面原型

设计思路见 [DESIGN.md](DESIGN.md)，新版 Moovie 的整体重构方案见 [REBUILD.md](REBUILD.md)。这里全是静态文件，和主项目代码完全隔离。

## 运行

```bash
python3 -m http.server 5180 --bind 127.0.0.1 --directory tvui-lab
```

然后打开 <http://localhost:5180/>。必须通过 HTTP 访问，用 `file://` 打开时 HTMX 懒加载会失败。

改了假数据或页面结构后，重新生成页面：

```bash
python3 tvui-lab/tools/gen.py
```

## 文件

| 路径 | 内容 |
| --- | --- |
| `index.html` | 首页：Hero 轮播、继续观看、今日更新（首屏直出），其余内容行懒加载 |
| `detail-*.html` | 详情页（每部作品一个），《雾港》有两季可以切换 |
| `player-*.html` | 播放页，`?ep=N` 指定集数 |
| `discover.html` / `search.html` / `me.html` | 发现、搜索、我的 |
| `partials/` | 模拟 `/api/htmx/*` 返回的 HTML 片段 |
| `css/app.css` | 设计令牌、导航三种形态、卡片、内容行、底部面板 |
| `css/detail.css` / `css/player.css` | 详情页、播放页 |
| `js/app.js` | 懒加载、轮播、视差、长按菜单、预取、想看/看过 |
| `js/focus.js` | 方向键空间导航，第一次按方向键时才加载 |
| `js/player.js` | 轻量播放控制层：手势、跳过片头、下一集、弹幕、倍速、换源 |
| `tools/gen.py` | 用假数据生成上面这些静态页（模拟服务端渲染） |

图片是 picsum.photos 的占位图，视频是公开的 HLS 测试流（Big Buck Bunny）。

## 可以试试

- **手机**：Hero 左右滑；长按任意海报弹出操作面板；在详情页往下滚，底部会出现吸底播放栏；播放页双击画面左右两侧快退/快进，横向拖动调进度；手机横过来视频会铺满。
- **电脑**：鼠标悬停卡片看视差高光；内容行两侧有翻页箭头；按方向键在整页里移动焦点，`/` 跳到搜索；播放页快捷键：空格、←/→、F、M、N、D、T（剧场模式）。
- **拖动窗口宽度**：<720px 底部 Tab 栏，720–1199px 左侧导航栏，≥1200px 顶部导航。

## 体积（gzip）

| 首页首次加载（不含图片） | 约 33KB = HTML 5KB + CSS 7KB + app.js 4.8KB + htmx 16KB |
| --- | --- |
| 播放页额外加载 | player.css 3.4KB + player.js 6KB；hls.js 只在浏览器不能原生播 HLS 时才加载 |
| focus.js | 1.6KB，只给用键盘的人加载 |
