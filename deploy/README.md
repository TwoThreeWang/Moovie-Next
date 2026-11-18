# 部署

一台装好 Docker 的服务器，跑三个容器：网站（`moovie-web`）、后台任务（`moovie-worker`）、弹幕服务（`moovie-danmu`）。
数据库、Ollama、Cloudflare WARP 用服务器上已有的，和旧系统一样都在外部网络 `postgres_default` 里。

| 文件 | 作用 |
| --- | --- |
| `Dockerfile` | 一个镜像里放网站、后台任务和几个运维命令（`migrate-legacy`、`mailcheck`、`tgcheck`、`hongguo-seed`）。模板、静态文件、表结构都编进了程序，运行时不需要别的文件 |
| `docker-compose.yml` | 三个容器的配置：只读的根文件系统、内存和 CPU 上限、日志轮转、健康检查 |
| `update.sh` | 以后更新用：拉代码、重新构建、重启 |
| `.env.example` | 所有配置项的说明 |

以下命令都在 `new/` 目录下运行。

## 第一次部署

### 1. 准备数据库

新系统用一个新的库（不要和旧系统的 `moovie_v2` 混在一起），需要 `vector` 和 `pg_trgm` 两个扩展。程序启动时会自动建扩展和表；如果数据库账号没有建扩展的权限，先用超级用户执行一次：

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
| `SITE_URL` | 正式域名，必须是 HTTPS，比如 `https://moovie.example.com`。站点地图、邮件里的链接、分享卡片都用它 |
| `APP_SECRET` | `openssl rand -hex 32` 生成。改了之后所有人要重新登录 |
| `DB_*` 或 `DATABASE_URL` | 数据库。容器里访问数据库用容器名，比如 `DB_HOST=postgres` |
| `SMTP_*`、`CONTACT_EMAIL` | 注册（密码发到邮箱）、找回密码都靠发信，没配好就没人能注册 |
| `AI_*`、`EMBEDDING_*` | AI 语义描述和向量。容器里访问 Ollama 用 `EMBEDDING_BASE_URL=http://ollama:11434/v1` |
| `TMDB_API_TOKEN` | 剧照、分集 |
| `DANMU_API_TOKEN`、`DANMU_API_BASE` | 弹幕，`DANMU_API_BASE=http://danmu-api:9321/<DANMU_API_TOKEN>` |

`APP_ENV` 不用写，compose 里固定为 `production`。`DB_SCHEMA` 留空（表建在 `public`）。

可选的：`TELEGRAM_*`（图片备份）、`DOUBAN_PROXY`（豆瓣出站代理，见 `deploy/mihomo/`）、`TRUSTED_PROXIES`、`WEB_BIND`、`OUTBOUND_PROXY`，见 `.env.example`。

### 3. 导入旧版的数据（可选）

旧版采集过的影视资料和广告指纹（账号、观看数据不迁移）。要在新库还没有作品时导入，先试运行看报告：

```bash
docker compose build
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 web ./migrate-legacy -dry-run
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 web ./migrate-legacy
```

临时的示例订阅（旧版保存的播放源）：

```bash
mkdir -p data
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 -v "$PWD/data:/app/out" web ./migrate-legacy -sources /app/out/example-sources.json
```

然后在 `.env` 里加 `EXAMPLE_SUBSCRIPTION_FILE=data/example-sources.json`。

### 4. 启动

```bash
./update.sh          # 第一次也可以直接 docker compose up -d --build
docker compose logs -f web worker
```

检查：

```bash
curl -s http://127.0.0.1:5008/healthz                      # ok
docker compose exec web ./mailcheck -dial                   # SMTP 能连上并登录（不发信）
docker compose exec web ./tgcheck                           # 配了 Telegram 时
```

### 5. 反向代理

网站端口默认只绑在 `127.0.0.1:5008`，由本机的 Nginx / Caddy 对外提供 HTTPS。反向代理要带上 `X-Forwarded-For` 和 `X-Forwarded-Proto`（登录 Cookie 只在 HTTPS 下发送，靠后者判断）：

```nginx
server {
    listen 443 ssl http2;
    server_name moovie.example.com;
    # ssl_certificate / ssl_certificate_key …

    client_max_body_size 2m;
    location / {
        proxy_pass http://127.0.0.1:5008;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Caddy 一行就够：`moovie.example.com { reverse_proxy 127.0.0.1:5008 }`。

反向代理是 `postgres_default` 网络里的另一个容器时，可以不开端口，直接 `proxy_pass http://moovie-web:5008`。
网站前面还有 CDN（比如 Cloudflare）时，要把 CDN 回源的地址段加进 `TRUSTED_PROXIES`，否则所有访客的 IP 都会被当成 CDN 的，按 IP 的限流会误伤。

### 6. 第一个账号

**网站上第一个注册的账号自动成为管理员。** 上线后马上用你自己的邮箱注册。

## 从旧系统切换

旧系统的容器也叫 `danmu-api` 服务、也用 5008 端口，两套不能同时跑：

1. 新系统先按上面的 1–3 步准备好（数据库、`.env`、导入），这时旧系统还在服务。
2. 停旧系统：在旧系统目录下 `docker compose down`。
3. 启动新系统：`./update.sh`。反向代理指向的还是 `127.0.0.1:5008`，不用改。
4. 有问题要回退：`docker compose down`，再到旧系统目录 `docker compose up -d`。

## 出站网络

| 请求 | 怎么走 |
| --- | --- |
| 豆瓣、TMDB、Wikidata、AI、Telegram、作品图片 | `OUTBOUND_PROXY`（默认 `socks5://cloudflare-warp:1080`，和旧系统一样）。写成空值就直连 |
| 豆瓣（配了 `DOUBAN_PROXY` 时） | 走 `DOUBAN_PROXY`，比如 mihomo；网站和后台任务的容器要加入 `moovie-egress` 网络，见 `deploy/mihomo/README.md` |
| 播放源查询的转发（`/api/relay`） | 直连，不走代理；只允许连公网地址 |
| 数据库、Ollama、弹幕服务、内网地址 | 直连 |
| 视频 | 不经过服务器，由用户的浏览器直接加载 |

## 资源

默认上限：网站 512 MB / 2 核，后台任务 256 MB / 1 核，弹幕服务 512 MB。实测网站空闲约 30 MB，200 个并发请求时约 120 MB。
要调整就在 `.env` 里写 `WEB_MEMORY_LIMIT`、`WEB_CPU_LIMIT`、`WORKER_MEMORY_LIMIT` 这些（见 `docker-compose.yml`）。

## 日常

```bash
./update.sh                          # 更新
docker compose logs --tail 200 web   # 看日志（只记方法、路径、状态码和耗时，不记查询参数）
docker compose restart worker        # 重启后台任务
```

后台的"任务"页能看到排队、失败的任务，失败的可以一键重试。数据库请自行备份。
