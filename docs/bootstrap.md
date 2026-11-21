# 从已有数据初始化（可选）

上线前可以从已有的旧数据库导入公共的影视资料，或者临时提供一份示例订阅。新部署不需要这些步骤。

旧库用 `LEGACY_DATABASE_URL` 指定，或者只用 `LEGACY_DB_NAME` 写库名（主机和账号沿用新库的 `DB_*`）。下面以旧库 `moovie_v2` 为例。

## 导入已有的影视资料

可以导入的只有公共数据：采集过的影视元数据（作品资料、外部 ID、别名、分集，以及 AI 写的语义描述和向量，新旧生成方式一样，导入后不用重新调用 AI），和众包确认过的广告指纹（连同票数）。账号、标记、短评、片单、进度、弹幕、反馈、屏蔽词都不迁移。

旧库只读；新库的写入在一个事务里完成，出错时整体回滚，可以反复运行。先试运行看报告：

```bash
LEGACY_DB_NAME=moovie_v2 go run ./cmd/migrate-legacy -dry-run
```

确认后去掉 `-dry-run` 正式导入。作品 ID 保持不变，所以要在新库还没有作品时导入（上线前）。

在 Docker 部署的服务器上，用镜像里的命令运行（先 `docker compose build`）：

```bash
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 web ./migrate-legacy -dry-run
```

```bash
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 web ./migrate-legacy
```

## 临时：示例订阅

为了让老用户切过来时有源可用，临时在 `/sub/example.json` 提供一份示例订阅，内容是旧版保存的播放源；设置页的播放源一栏会出现"使用示例订阅"，点一下就填好并读取。之后会删掉。

```bash
LEGACY_DB_NAME=moovie_v2 go run ./cmd/migrate-legacy -sources data/example-sources.json   # 从旧库导出（只读，不迁移任何数据）
```

在 Docker 部署的服务器上：

```bash
mkdir -p data
docker compose run --rm --no-deps -e LEGACY_DB_NAME=moovie_v2 -v "$PWD/data:/app/out" web ./migrate-legacy -sources /app/out/example-sources.json
```

然后在 `.env` 里设置 `EXAMPLE_SUBSCRIPTION_FILE=data/example-sources.json` 并重启。同一个地址只留一个（HTTP 的也留着，查询由本站转发），key 用接口的域名。`data/` 已经加进 `.gitignore`。不配置这个变量时，接口返回 404，设置页也没有按钮。

实测（2026-11-17）：旧版 35 个源，设置页检测下来 31 个可用，4 个是对方关站或拦截。

删掉这个功能时一起删：`internal/legacy/sources.go`、`cmd/migrate-legacy` 的 `-sources` 参数、`internal/app/example_subscription.go`（以及 `app.go` 里的路由和 `exampleSub` 字段）、设置页的"使用示例订阅"按钮和 `settings.js` 里对应的几行、配置项 `EXAMPLE_SUBSCRIPTION_FILE`、相关测试。

## 替换同一台服务器上的旧站

旧站的弹幕容器也叫 `danmu-api`、网站也用 5008 端口，两套不能同时运行：

1. 先按 [部署](../deploy/README.md) 的 1–3 步准备好数据库、`.env` 和导入，这时旧站还在服务。
2. 停旧站：在旧站目录下 `docker compose down`。
3. 启动影牛：`./update.sh`。反向代理指向的还是 `127.0.0.1:5008`，不用改。
4. 有问题要回退：`docker compose down`，再到旧站目录 `docker compose up -d`。
