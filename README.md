# 影牛 Moovie

影视资料、观影记录和一个通用播放器。作品资料以豆瓣为主，TMDB、IMDb、Wikidata 补充，不登录也能浏览；用户可以标记想看 / 看过、写短评、做片单、看月度观影报告。

**本站不提供、推荐或保存任何播放源。** 想要播放，用户在设置里填写自己的订阅地址（苹果 CMS 接口，也兼容 TVBox 配置）。查询和播放都在浏览器里完成，服务器只转发查询文本，不经手视频，订阅地址加密后才同步到账号。

当前版本见 [更新记录](internal/app/changelog.go)（站内 `/changelog`）。产品定位、合规边界和设计决策见 [设计](docs/design.md)。

## 功能

- **发现与搜索**：电影、剧集、综艺、动漫的热门榜，热搜榜，筛选浏览，相似推荐（AI 语义向量）；短剧单独成频道。
- **观影记录**：想看、看过、评分、短评和回复，播放进度跨设备同步，月度观影报告和分享卡片。
- **片单**：所有人都能做片单，投稿审核后进入片单广场；已发布的片单可以修改，修改审核期间公开页照常显示旧版。
- **播放器**：hls.js / 系统播放器，众包广告跳过、慢线路测速和换线、弹幕、连播；独立的在线 M3U8 播放器（`/m3u8`）。
- **账号**：只用邮箱注册，工作量证明代替验证码；登录即同步播放源和加密的观看记录；随时导出数据、自助注销。
- **运营**：管理后台（审核、用户、作品、任务、外部服务状态），站点地图和结构化数据，可以添加到手机桌面。

## 技术栈

| 部分 | 选择 |
| --- | --- |
| 后端 | Go 1.25、Gin、pgx v5，模板用标准库 `html/template`，静态文件和模板打包进二进制 |
| 数据库 | PostgreSQL，扩展 `vector`（向量检索）和 `pg_trgm`（模糊搜索）；启动时自动执行迁移 |
| 前端 | 原生 ES 模块，不需要构建；import map 带版本号，Service Worker 只缓存静态文件 |
| 外部服务 | 豆瓣、TMDB、Wikidata、红果短剧官网；OpenAI 格式的大模型和向量接口；SMTP；可选 Telegram 图床、弹幕服务 |
| 部署 | Docker Compose：网站、后台任务、弹幕服务三个容器 |

## 快速开始

需要 Go 1.25+、Node.js（只跑前端测试）和一个装好 `vector`、`pg_trgm` 扩展的 PostgreSQL。

1. 复制配置并按注释修改，至少填好数据库连接（`DATABASE_URL`，或者 `DB_HOST`、`DB_NAME` 等）：

   ```bash
   cp .env.example .env
   ```

2. 启动网站。第一次启动会自动建表；默认在同一个进程里运行后台任务（采集、AI 描述、向量、榜单、豆瓣同步）：

   ```bash
   go run ./cmd/web
   ```

3. 打开 http://localhost:5200 （端口由 `PORT` 决定）注册账号。**第一个注册的账号是管理员。** 开发环境不配 SMTP 时，注册邮件（含登录密码）会打印在日志里。

设置 `JOBS_IN_WEB=false` 后，后台任务要单独运行：

```bash
go run ./cmd/worker
```

### 用演示数据看效果

演示数据写在单独的 schema 里，不碰正式的表：

```bash
DB_SCHEMA=mv_demo go run ./cmd/devseed -reset
```

```bash
DB_SCHEMA=mv_demo JOBS_IN_WEB=false PORT=5200 go run ./cmd/web
```

```bash
go run ./cmd/mocksource 5210
```

打开 http://localhost:5200 ，用 `demo@moovie.local` 登录（密码见 [cmd/devseed/main.go](cmd/devseed/main.go) 里的默认值），在「设置 → 播放源」里填订阅地址 `http://localhost:5210/subscription.json`。

`mocksource` 是一个假资源站，只用于开发和验收：

- 五个源：A、B 是 JSON 接口（B 故意慢 0.7 秒），C 是 XML 接口，D 一直返回错误，E 是 HTTP 地址。
- 视频是公开的 HLS 测试流（Big Buck Bunny）。
- 每部剧第 1 集带"广告"：开头一段已经在演示数据里确认过（会自动跳过），片中另一段出现两次（第一次会问你"这一段是广告吗"）。
- 《深夜便利店》不在作品库里，用来看"未收录作品 → 加密历史"的流程。
- 《深夜便利店》《星陨之地》在两个源里都有（C 源的《星陨之地》没写年份），用来看"原来的源失效时换到其他源"的流程。

## 测试

```bash
go test ./...
```

```bash
cd web && npm test
```

```bash
node --test extension/test/
```

Go 测试需要数据库：每个包在自己的临时 schema 里运行，不碰 `public`；用 `TEST_DATABASE_URL` 可以指定单独的测试库。前端测试用 Node 自带的测试框架，覆盖 `web/static/js/lib` 和 `core` 里的逻辑。测试留下的临时 schema 可以这样清理：

```bash
go run ./cmd/testclean
```

## 配置

外部元数据接口（豆瓣、TMDB、Wikidata、红果）的单次请求超时为 60 秒，AI 语义描述为 10 分钟，向量接口为 3 分钟。后台任务租约为 15 分钟，防止慢 AI 请求在执行中被回收后重复调用。

根目录 `.env` 是运行和部署的唯一配置入口，完整列表和说明在 [.env.example](.env.example)。Compose 直接使用其中的 `APP_ENV`、`PORT`、`JOBS_IN_WEB`；正式部署时设置 `APP_ENV=production`，只让独立 worker 执行任务时设置 `JOBS_IN_WEB=false`。常用的几组：

| 用途 | 变量 |
| --- | --- |
| 站点 | `SITE_NAME`、`SITE_URL`（正式环境必须是 HTTPS 域名）、`APP_SECRET`（会话和图片签名，必须修改）、`TRUSTED_PROXIES` |
| 数据库 | `DATABASE_URL` 或 `DB_*`、`DB_SCHEMA`、`TEST_DATABASE_URL` |
| 后台任务 | `JOBS_IN_WEB`、`WORKER_CONCURRENCY`、`HONGGUO_SYNC` |
| 资料来源 | `TMDB_API_TOKEN`、`WIKIDATA_*`、`DOUBAN_PROXY` |
| AI | `AI_BASE_URL`、`AI_API_KEY`、`AI_MODEL`、`EMBEDDING_*`；不配时不写描述，相似推荐改按类型重合度 |
| 邮件 | `SMTP_*`、`CONTACT_EMAIL`、`MAIL_HOURLY_LIMIT` |
| 可选服务 | `TELEGRAM_*`（图床）、`DANMU_API_*`（弹幕）、`POW_BITS`（人机校验难度） |

## 部署

正式环境用 Docker Compose，步骤、反向代理、CDN 规则和资源上限见 [部署](docs/deploy.md)。日常更新在服务器上运行：

```bash
./build.sh
```

上线前建议做一次[发信检查](docs/accounts.md#上线前的发信检查)。需要从已有数据库导入影视资料时，见 [从已有数据初始化](docs/bootstrap.md)。

## 目录

| 路径 | 内容 |
| --- | --- |
| `cmd/web`、`cmd/worker` | 网站和后台任务 |
| `cmd/devseed`、`cmd/mocksource` | 演示数据和假资源站（只用于开发） |
| `cmd/hongguo-seed` | 在本地抓取全量短剧并导入（见 [短剧](docs/short-drama.md)） |
| `cmd/mailcheck`、`cmd/tgcheck`、`cmd/dbinfo`、`cmd/testclean` | 检查发信、Telegram、数据库情况，清理测试 schema |
| `cmd/migrate-legacy` | 从旧版数据库导入影视资料和老用户的账号数据（可选） |
| `internal/app` | 路由和页面处理；`internal/ui` 模板渲染；`internal/httpx` 通用的请求工具 |
| `internal/db` | 连接和迁移（`migrations/`） |
| `internal/media`、`ingest`、`providers`、`jobs` | 作品库、采集流程、外部来源客户端、任务队列 |
| `internal/auth`、`mail`、`pow` | 账号、邮件、人机校验 |
| `internal/library`、`social`、`collections`、`feedback`、`report`、`recommend`、`trends`、`doubansync` | 观影记录、社区、片单、反馈、月度报告、推荐、热搜、豆瓣标记同步 |
| `internal/playback`、`vault`、`adskip`、`danmaku`、`relay` | 进度同步、加密历史、广告指纹、弹幕、播放源查询的转发 |
| `internal/imageproxy`、`telegram` | 签名图片代理和 Telegram 图床 |
| `web/templates`、`web/static` | 模板、样式、脚本 |
| `web/static/js/lib` | 可单独测试的前端逻辑：解析资源站和 TVBox 配置、匹配作品、广告指纹、加密、二维码 |
| `web/static/js/core`、`pages` | 查源引擎、同步、本地存储；各页面的脚本 |
| `web/jstest` | 前端测试 |
| `docs` | 设计决策和各模块的详细设计 |
| `extension` | 浏览器插件（`src/` 是插件本身，网站打包提供下载；`releases.json` 是插件的发布记录） |
| `前端设计demo` | 早期的静态界面原型，只作参考，不参与构建 |

## 文档

| 文档 | 内容 |
| --- | --- |
| [产品定位与设计决策](docs/design.md) | 定位、合规边界、整体架构、关键设计和决定记录 |
| [播放源与播放器](docs/playback.md) | 订阅格式、TVBox 兼容、查询转发、M3U8 播放器、广告跳过、慢线路 |
| [账号、同步与邮件](docs/accounts.md) | 登录即同步、人机校验、邮件和防刷、通知、导出和注销 |
| [片单与管理后台](docs/collections.md) | 片单的状态和审核、修改稿、收藏和热门；后台各页面 |
| [作品资料：采集与榜单](docs/metadata.md) | 采集的四个步骤、失败重试、豆瓣请求节奏、热门榜、热搜榜 |
| [短剧](docs/short-drama.md) | 数据来源、全量导入教程、线上增量、展示规则 |
| [图片代理与 Telegram 图床](docs/images.md) | 签名链接、尺寸、缓存、Telegram 备份 |
| [公开页面、SEO 与安装](docs/web.md) | 不登录能看什么、搜索引擎优化、添加到桌面、分享卡片 |
| [数据库](docs/database.md) | 迁移、每张表的用途、schema |
| [从已有数据初始化](docs/bootstrap.md) | 导入已有的影视资料、临时示例订阅 |
| [部署](docs/deploy.md) | Docker Compose、反向代理、CDN、资源和构建缓存 |
| [浏览器插件](docs/extension.md) | 豆瓣同步、补全资料、豆瓣页面上的卡片和角标；连接、打包和更新 |

## 版本与更新记录

版本号和更新内容都写在 [internal/app/changelog.go](internal/app/changelog.go)，站内 `/changelog` 页面按它显示。发布新版本时：

1. 在 `releases` 最前面加一条：版本号、日期、一句话概括，按类别（新增、变更、界面、性能、修复、移除、破坏性变更）列出每一项。
2. 改动会影响已有用户时（比如需要重新配置），写进"破坏性变更"，大版本设 `Major: true`。
3. 更新 `internal/app/app_test.go` 里检查更新记录页面的断言。

版本号按 `主版本.次版本.修订号`：新功能加次版本，只有修复和小改动加修订号。

## 已知限制

- 豆瓣的抓取、TMDB 和 Wikidata 的同步只用假服务器测过，没有对真实接口跑过完整流程。
- 第三方弹幕库（dandanplay 协议）只用假服务器测过。
- 浏览器插件里需要豆瓣登录的部分（同步标记、备份、写回豆瓣）没有用真实的豆瓣账号跑过；写回豆瓣用的是豆瓣网页按钮的接口，没有验证过。
- 没有在真机上测过 iPhone 的系统播放器、触屏手势、长按三倍速和锁定按钮，也没有测过 AirPlay。
- 没有用真实的 SMTP 服务器发过信；正式上线前要配好 SPF、DKIM、DMARC 再实测各家邮箱（QQ、163、Gmail、Outlook）能否收到。
- 分享卡片只在 Chromium 里看过效果，没有在 iPhone Safari 和微信内置浏览器里实测；TMDB 的海报跨域加载失败时卡片上会是占位色块。
- 慢线路的测速和自动换线只在本机演示环境里跑过，没有在真实的慢源上验证效果。
- "原来的源还在，但视频都播放不了"时换源的流程只看过代码，演示源模拟不了这种情况。
- TVBox 兼容导入只用测试数据验证过，没有用真实的 TVBox 配置地址实测；返回类型是二进制或图片的配置地址会被转发拒绝。
- 第三方下载链接指向外部网站，那个网站能不能用、会不会改地址，本站控制不了。
- Service Worker 只在模拟环境里测过，没有在真机上验证过安装流程。
- Chrome 的控制台偶尔会出现一条 `Transition was aborted` 的报错：这是浏览器在预渲染页面和页面过渡动画同时生效时自己打的，不影响使用。
