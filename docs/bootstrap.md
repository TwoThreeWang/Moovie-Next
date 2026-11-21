# 从已有数据初始化（可选）

上线前可以从已有的旧数据库导入影视资料和老用户的账号数据，或者临时提供一份示例订阅。新部署不需要这些步骤。

旧库用 `LEGACY_DATABASE_URL` 指定，或者只用 `LEGACY_DB_NAME` 写库名（主机和账号沿用新库的 `DB_*`）。下面以旧库 `moovie_v2` 为例。

## 导入旧库的数据

导入两类数据：

- **公共数据**：采集过的影视元数据（作品资料、外部 ID、别名、分集，以及 AI 写的语义描述和向量，新旧生成方式一样，导入后不用重新调用 AI），和众包确认过的广告指纹（连同票数）。
- **用户的数据**：账号（邮箱、用户名、密码哈希、角色、头像、是否公开、豆瓣 ID、跳过广告开关）、关注、想看和看过（含评分、短评）、短评的点赞和回复、播放进度，以及由进度推出的观影日期。两边的密码都是 bcrypt，老用户用原来的密码登录，只是要重新登录一次。

不迁移的：片单（旧版没有开放）、弹幕、反馈、通知、屏蔽词；作品库里没有的片子的进度（新版由浏览器加密保存，服务器替用户生成不了）；播放源、资源站的 `vod_id` 等一切和资源站有关的数据。老用户的订阅见下面的[示例订阅](#临时示例订阅)。

用户的数据按新版的规则处理，报告里会列出每一类的条数：

- 邮箱转成小写；同一个邮箱只留 ID 最小的账号。用户名被占用时后面加 `_ID`，头像不是单个 emoji 时换成默认的 🎬。
- 账号、标记沿用旧 ID。新库里同一个 ID 已经是别人的（上线后有人注册了），这一条就跳过，不会挂到别人名下。
- 评分截到 0–5，短评截到 2000 字，超过 300 字的回复不要；账号或作品没有迁移过来的标记、点赞、回复一起跳过。
- 进度的季取作品在新库里的季数，集号先看旧版的分集表，再从集键（`S01E03`）解析，认不出的（按日期排的期数）跳过；同一集的上下集合成一条，留最新的。

旧库只读；新库的写入在一个事务里完成，出错时整体回滚，可以反复运行。再次运行时作品资料会更新，用户的数据只补新库里没有的：账号、标记保留新库的版本，进度只有旧的更新时才覆盖，所以上线后用户在新站上做的修改不会被盖掉。先试运行看报告：

```bash
LEGACY_DB_NAME=moovie_v2 go run ./cmd/migrate-legacy -dry-run
```

确认后去掉 `-dry-run` 正式导入。作品、账号、标记的 ID 保持不变，所以要在新库还没有作品和用户时导入（上线前、开放注册之前）。

在 Docker 部署的服务器上，用镜像里的命令运行（先 `docker compose build`）：

```bash
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 --entrypoint ./migrate-legacy web -dry-run
```

```bash
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 --entrypoint ./migrate-legacy web
```

## 临时：示例订阅

为了让老用户切过来时有源可用，临时在 `/sub/example.json` 提供一份示例订阅，内容是旧版保存的播放源；设置页的播放源一栏会出现"使用示例订阅"，点一下就填好并读取。之后会删掉。

从旧版迁移过来的账号（导入时记在 `example_subscription_users` 表里）第一次在新站登录时，浏览器会自动填入示例订阅，和账号里的其他配置一起加密保存，换设备登录也有。只在账号还没有钥匙圈时填一次，之后用户清除或换掉订阅都不会再填回来；本机已经有订阅的不覆盖。

```bash
LEGACY_DB_NAME=moovie_v2 go run ./cmd/migrate-legacy -sources data/example-sources.json   # 从旧库导出（只读，不迁移任何数据）
```

在 Docker 部署的服务器上：

```bash
mkdir -p data
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 -v "$PWD/data:/app/out" --entrypoint ./migrate-legacy web -sources /app/out/example-sources.json
```

然后在 `.env` 里设置 `EXAMPLE_SUBSCRIPTION_FILE=data/example-sources.json` 并重启。同一个地址只留一个（HTTP 的也留着，查询由本站转发），key 用接口的域名。`data/` 已经加进 `.gitignore`。不配置这个变量时，接口返回 404，设置页也没有按钮。

实测（2026-11-17）：旧版 35 个源，设置页检测下来 31 个可用，4 个是对方关站或拦截。

删掉这个功能时一起删：`internal/legacy/sources.go`、`cmd/migrate-legacy` 的 `-sources` 参数、`internal/app/example_subscription.go`（以及 `app.go` 里的两个路由和 `exampleSub` 字段）、设置页的"使用示例订阅"按钮和 `settings.js` 里对应的几行、`keyring.js` 里的 `pendingExampleSubscription`、`internal/legacy/users.go` 里的 `markExampleSubscription`、配置项 `EXAMPLE_SUBSCRIPTION_FILE`、相关测试，再加一个迁移删掉 `example_subscription_users` 表。

## 替换同一台服务器上的旧站

旧站的弹幕容器也叫 `danmu-api`、网站也用 5008 端口，两套不能同时运行：

1. 先按 [部署](deploy.md) 的 1–3 步准备好数据库、`.env` 和导入，这时旧站还在服务。
2. 停旧站：在旧站目录下 `docker compose down`。
3. 在 `.env` 里把 `PORT` 设成旧站用的 `5008`，启动影牛：`./build.sh`。反向代理指向的还是 `127.0.0.1:5008`，不用改。
4. 有问题要回退：`docker compose down`，再到旧站目录 `docker compose up -d`。
