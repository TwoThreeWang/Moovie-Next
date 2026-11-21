# 部署

一台装好 Docker 的服务器，跑三个容器：网站（`moovie-web`）、后台任务（`moovie-worker`）、弹幕服务（`moovie-danmu`）。
数据库、Ollama、Cloudflare WARP 用服务器上已有的，都在外部网络 `postgres_default` 里；豆瓣代理 mihomo 在外部网络 `mihomo_default` 里。两个网络都要先存在，否则 `docker compose up` 会报错。

| 文件 | 作用 |
| --- | --- |
| `Dockerfile` | 一个镜像里放网站、后台任务和几个运维命令（`migrate-legacy`、`mailcheck`、`tgcheck`、`hongguo-seed`）。模板、静态文件、表结构都编进了程序，运行时不需要别的文件 |
| `docker-compose.yml` | 三个容器的配置和变量映射：只读的根文件系统、内存和 CPU 上限、日志轮转、健康检查 |
| `.env` | 唯一的配置入口，网站、后台任务和弹幕服务的变量都从这里来；所有配置项的说明见 `.env.example` |
| `build.sh` | 以后更新用：拉代码、重新构建、重启 |

以下命令都在项目根目录下运行。

## 第一次部署

### 1. 准备数据库

用一个新的库，需要 `vector` 和 `pg_trgm` 两个扩展。程序启动时会自动建扩展和表；如果数据库账号没有建扩展的权限，先用超级用户执行一次：

```sql
CREATE DATABASE moovie;
\c moovie
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

### 2. 写 `.env`

```bash
cp .env.example .env
```

必须改的：

| 配置 | 说明 |
| --- | --- |
| `APP_ENV` | 改成 `production` |
| `JOBS_IN_WEB` | 改成 `false`，后台任务只由独立的 worker 执行；保留 `true` 时网站也会执行后台任务 |
| `SITE_URL` | 正式域名，必须是 HTTPS，比如 `https://moovie.example.com`。站点地图、邮件里的链接、分享卡片都用它 |
| `APP_SECRET` | `openssl rand -hex 32` 生成。改了之后所有人要重新登录 |
| `DB_*` 或 `DATABASE_URL` | 数据库。容器里访问数据库用容器名，比如 `DB_HOST=postgres` |
| `SMTP_*`、`CONTACT_EMAIL` | 注册（密码发到邮箱）、找回密码都靠发信，没配好就没人能注册 |
| `AI_*`、`EMBEDDING_*` | AI 语义描述和向量。容器里访问 Ollama 用 `EMBEDDING_BASE_URL=http://ollama:11434/v1` |
| `TMDB_API_TOKEN` | 剧照、分集 |
| `DANMU_API_TOKEN`、`DANMU_API_BASE` | 弹幕，`DANMU_API_BASE=http://danmu-api:9321/<DANMU_API_TOKEN>` |

网站和 worker 通过 `env_file` 读取根目录 `.env`，`docker-compose.yml` 再把其中的部分变量映射成各容器自己的值：

- `APP_ENV`、`JOBS_IN_WEB` 直接使用 `.env` 里的值；`.env` 里没有时才默认为 `production`、`false`。
- 网站监听、端口映射和健康检查都用 `PORT`。改了 `PORT` 也要同步改反向代理的上游端口。
- 连接池：网站用 `WEB_DB_MAX_CONNS`，worker 用 `WORKER_DB_MAX_CONNS`，没设时都沿用 `DB_MAX_CONNS`。
- Go 内存目标：网站用 `WEB_GO_MEMORY_LIMIT`，worker 用 `WORKER_GO_MEMORY_LIMIT`，没设时沿用 `GOMEMLIMIT`。
- 弹幕容器不读取 `.env` 文件，只拿到它需要的令牌、缓存和限流配置，不会拿到数据库和邮件密钥。

`DB_SCHEMA` 留空（表建在 `public`）。`.env` 由 Compose 在创建容器时读取并注入环境变量，不打进镜像，也不挂载进容器。修改后用 `docker compose up -d` 更新容器，单纯 `restart` 不会重新加载环境变量。

可选的：`SITE_NAME`（站点名，默认 `Moovie 影牛`，网页标题后缀、首页标题、分享卡片、邮件和桌面图标名都用它）、`TELEGRAM_*`（图片备份）、`DOUBAN_PROXY`（豆瓣出站代理）、`TRUSTED_PROXIES`、`WEB_BIND`、`OUTBOUND_PROXY`、`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`，见 `.env.example`。

### 3. 导入已有的数据（可选）

从已有数据库导入影视资料和广告指纹（账号、观看数据不导入），步骤见 [从已有数据初始化](bootstrap.md)。

### 4. 启动

```bash
./build.sh          # 第一次也可以直接 docker compose up -d --build
docker compose logs -f web worker
```

检查（把 `5200` 换成你的 `PORT`）：

```bash
curl -s http://127.0.0.1:5200/healthz                      # ok
docker compose exec web ./mailcheck -dial                   # SMTP 能连上并登录（不发信）
docker compose exec web ./tgcheck                           # 配了 Telegram 时
```

### 5. 反向代理

网站端口默认只绑在 `127.0.0.1:<PORT>`，由本机的 Nginx / Caddy 对外提供 HTTPS。反向代理要带上 `X-Forwarded-For` 和 `X-Forwarded-Proto`（登录 Cookie 只在 HTTPS 下发送，靠后者判断）。下面以 `PORT=5200` 为例：

```nginx
server {
    listen 443 ssl http2;
    server_name moovie.example.com;
    # ssl_certificate / ssl_certificate_key …

    client_max_body_size 2m;
    location / {
        proxy_pass http://127.0.0.1:5200;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Caddy 一行就够：`moovie.example.com { reverse_proxy 127.0.0.1:5200 }`。

反向代理是 `postgres_default` 网络里的另一个容器时，可以不开端口，直接 `proxy_pass http://moovie-web:5200`。反向代理在别的机器上时把 `WEB_BIND` 改成 `0.0.0.0`。
网站前面还有 CDN（比如 Cloudflare）时，要把 CDN 回源的地址段加进 `TRUSTED_PROXIES`，否则所有访客的 IP 都会被当成 CDN 的，按 IP 的限流会误伤。

### 6. 第一个账号

**网站上第一个注册的账号自动成为管理员。** 上线后马上用你自己的邮箱注册。

## 出站网络

| 请求 | 怎么走 |
| --- | --- |
| 豆瓣、TMDB、Wikidata、AI、Telegram、作品图片 | `OUTBOUND_PROXY`（默认 `socks5://cloudflare-warp:1080`）。写成空值就直连；需要按协议区分时用 `HTTP_PROXY`、`HTTPS_PROXY` |
| 豆瓣（配了 `DOUBAN_PROXY` 时） | 走 `DOUBAN_PROXY`，比如自建的 mihomo；`docker-compose.yml` 已让网站和 worker 加入 `mihomo_default` 网络，见 [作品资料](metadata.md#豆瓣请求节奏) |
| 播放源查询的转发（`/api/relay`） | 直连，不走代理；只允许连公网地址 |
| 数据库、Ollama、弹幕服务、内网地址 | 直连（列在 `NO_PROXY` 里） |
| 视频 | 不经过服务器，由用户的浏览器直接加载 |

## 图片与 CDN

应用不做图片内容缓存，也不需要增加磁盘卷。`/img/<签名素材标识>?size=thumb|display|original&sig=...` 返回对应规格的图片，片名、用途、来源和规格都受签名保护。缩略图和展示图地址固定，公开页面不要求图片请求带登录 Cookie、Referer 或特定 User-Agent，搜索引擎和分享预览可以直接抓取。

CDN 需要设置以下规则（具体选项名取决于提供商）：

1. 只缓存成功的图片响应：状态 200 且 Content-Type 为 image/*；允许正常的 304 条件校验。尊重源站 `Cache-Control`，成功图当前为 30 天。不要把 HTML 错误页作为图片缓存。
2. 错误缓存时间设为 0，尤其是 400、403、429 和 5xx；尊重 `Cache-Control: no-store`，不要用强制缓存规则覆盖。若之前缓存过错误响应，要主动清除对应 URL 的缓存。
3. 缓存键保留完整路径及全部查询参数，至少不能忽略 `size` 和 `sig`；回源也必须完整转发这些参数。否则会混用规格，或让签名校验失效。
4. 要禁止外站嵌图时，在 CDN 查找缓存之前执行来源判断。同站请求、直接访问、无 Referer 的抓取请求应保持可用；不要只凭 User-Agent 中含 Googlebot 就豁免全部规则。只在应用回源时检查来源，无法限制 CDN 已缓存的图片。
5. 不把 CDN 域名无差别设为 `Cross-Origin-Resource-Policy: same-origin`：图片在独立域名时可能阻止本站展示。默认不加此头，不按 Referer 缓存不同响应，也不做来源相关的 Vary 变体。

应用回源请求按真实访客 IP 限流：所有规格每分钟 600 次，原图另外每分钟 60 次；CDN 命中不消耗这些额度。务必按反向代理一节配置 `TRUSTED_PROXIES` 并正确转发访客 IP，否则可能把整个 CDN 节点当成一个访客。图片来源域名在每次重定向时也会检查，不能跳到任意地址或内网。

修改 `APP_SECRET` 后要刷新 CDN 缓存的旧 HTML 页面，让页面引用新的签名链接；已被别人下载或被 CDN 缓存的公开图片，不会因为源站密钥变化立刻失效。

检查时分别测试：正常页面图片返回 200 且可缓存；修改 `size` 或 `sig` 返回 403 / no-store；缺少 Referer 的有效签名请求返回 200；外站嵌图即使缓存命中也被 CDN 规则拦截；错误请求后再次请求有效链接仍能正常显示。用搜索引擎站长工具检查页面渲染和图片抓取。这里仅提供配置要求，应用更新不会自动修改 CDN 设置。

## 资源

容器上限在 `.env` 里用 `WEB_MEMORY_LIMIT`、`WEB_CPU_LIMIT`、`WORKER_MEMORY_LIMIT`、`WORKER_CPU_LIMIT`、`DANMU_MEMORY_LIMIT` 调整：

| 容器 | `.env.example` 里的值 | 不设置时 Compose 的默认值 |
| --- | --- | --- |
| 网站 | 512 MB / 2 核 | 2048 MB / 2 核 |
| 后台任务 | 256 MB / 1 核 | 1024 MB / 1.5 核 |
| 弹幕服务 | 512 MB | 512 MB |

实测网站空闲约 30 MB，200 个并发请求时约 120 MB。

数据库连接池和 Go 内存目标的取值顺序见上面第 2 步。都不设置时，网站最多 12 个连接、内存目标 400 MiB，worker 最多 6 个连接、内存目标 200 MiB。这些都是运行时设置，不限制镜像构建时的资源。

保留独立 worker：默认普通任务 2 个执行位，另有豆瓣 3 个、红果 2 个、AI 2 个，共 9 个执行位。独立的进程、连接池和资源上限可以减少采集任务繁忙时对网页请求的影响；两个进程仍共享同一个镜像和 PostgreSQL 任务队列。worker 不依赖网站端口，启动时自行连接数据库、执行带锁的表结构迁移，因此不需要等待 web 容器。

## 构建缓存

`.dockerignore` 只放行 Go 源码、迁移 SQL、模板和静态资源；`.git`、`.env`、文档、设计 demo、测试和本地数据不发送给 builder。只改 `.env`、Compose 或文档时，应用编译层可以复用。

Dockerfile 先处理 `go.mod`、`go.sum`，再复制源码，并保留 Go 模块缓存 `/go/pkg/mod` 和编译缓存 `/root/.cache/go-build`。在同一台服务器、同一个 BuildKit builder 上更新时使用普通的 `./build.sh` 即可；日常不要加 `--no-cache` 或执行 `docker builder prune`、`docker buildx prune` 清掉缓存。首次构建、换 builder、缓存被清理或回收后仍需重新下载或编译；运行镜像里不包含这些构建缓存。

需要定位耗时时，运行 `docker compose --progress plain build web`，分别看下载依赖、编译、导出镜像花了多久。网站和 worker 共用一次构建结果，六个程序仍顺序编译，避免同时链接多个程序增加服务器的内存压力。

## 日常

```bash
./build.sh                           # 更新
docker compose logs --tail 200 web   # 看日志（只记方法、路径、状态码和耗时，不记查询参数）
docker compose restart worker        # 重启后台任务（改了 .env 要用 docker compose up -d）
```

后台的"任务"页能看到排队、失败的任务，失败的可以一键重试。数据库请自行备份。
