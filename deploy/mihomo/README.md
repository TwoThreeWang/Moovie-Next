# 用 Clash 订阅给豆瓣请求做出站代理（mihomo）

网站采集豆瓣资料时，所有请求都从服务器自己的 IP 发出。请求多了，豆瓣会对这个 IP 限流。
这里用 [mihomo](https://github.com/MetaCubeX/mihomo)（Clash.Meta 内核）把你买的 Clash 订阅变成一个普通的 HTTP / SOCKS5 代理，
让豆瓣的请求轮流从订阅里的不同节点出去。

- **只影响豆瓣的接口请求。** TMDB、Wikidata、发信、Telegram、图片代理都不走它。
- **单独的容器，单独的网络。** 不用 host 网络，不建虚拟网卡，不要额外权限，不向宿主机开放端口，宿主机的网络不受影响。
- **不是必须的。** 不配代理时，网站被限流会自动放慢、必要时暂停 10 分钟再继续。先看后台概览里的"豆瓣 · 请求节奏"，经常被限流再考虑用它。

## 这个目录里有什么

| 文件 | 作用 |
| --- | --- |
| `docker-compose.yml` | 启动 mihomo 容器，并创建 `moovie-egress` 网络 |
| `config.yaml.example` | mihomo 的配置模板。复制成 `config.yaml` 再填订阅链接 |
| `config.yaml` | 你填好的配置。里面有订阅链接，已经在 `.gitignore` 里，不要提交 |
| `providers/` | mihomo 自动下载的节点列表缓存，同样不提交 |

## 第一次使用

以下命令都在项目根目录（`new/`）下运行。

**1. 填订阅链接**

```bash
cp deploy/mihomo/config.yaml.example deploy/mihomo/config.yaml
```

打开 `deploy/mihomo/config.yaml`，把 `proxy-providers → airport → url` 换成你的 Clash 订阅链接（机场后台里"Clash 订阅"或"Clash.Meta 订阅"那一个，不是 V2Ray / Shadowrocket 的）。

**2. 启动**

```bash
docker compose -f deploy/mihomo/docker-compose.yml up -d
```

**3. 确认节点拉下来了**

```bash
docker logs mihomo --tail 30
```

没有 `error` 就行。`deploy/mihomo/providers/airport.yaml` 里应该已经有节点了。

**4. 确认代理能用**（临时起一个容器，加入同一个网络去访问豆瓣）

```bash
docker run --rm --network moovie-egress curlimages/curl -s -o /dev/null -w '%{http_code}\n' -x http://mihomo:7890 https://m.douban.com/
```

输出 `200` 说明通了。想看出口 IP 是不是在变，多跑几次这条：

```bash
docker run --rm --network moovie-egress curlimages/curl -s -x http://mihomo:7890 https://api.ipify.org
```

**5. 让网站用它**

网站和 worker 的容器要加入 `moovie-egress` 网络。在它们的 compose 文件里：

```yaml
services:
  web:
    networks: [default, moovie-egress]
    environment:
      DOUBAN_PROXY: http://mihomo:7890
  worker:
    networks: [default, moovie-egress]
    environment:
      DOUBAN_PROXY: http://mihomo:7890

networks:
  moovie-egress:
    external: true
```

也可以把 `DOUBAN_PROXY=http://mihomo:7890` 写在 `.env` 里。改完重启网站和 worker。

**6. 看效果**

打开后台概览，"外部来源"里豆瓣那一行会显示"通过 DOUBAN_PROXY 出站"和当前的请求节奏（正常 / 放慢 / 暂停）。

## 网站不是跑在容器里的情况

本机开发、或者直接在服务器上运行二进制时，网站够不到 Docker 的内部网络。
打开 `docker-compose.yml` 里注释掉的 `ports` 两行（只绑定 `127.0.0.1`，外网访问不到），重新 `up -d`，然后：

```bash
DOUBAN_PROXY=http://127.0.0.1:7890
```

## 其他容器也想用

任何容器加入 `moovie-egress` 网络后，都可以把 `http://mihomo:7890`（HTTP 代理）或 `socks5://mihomo:7890` 当代理用。
注意 `config.yaml` 里只有一条规则：经过它的请求**全部**走订阅节点，没有国内直连的分流。

## 常用配置

都在 `config.yaml` 里，改完运行 `docker restart mihomo`。

| 想做的事 | 改哪里 |
| --- | --- |
| 只用某些地区的节点 | `proxy-providers → airport → filter`，正则，例如 `"(?i)香港\|日本\|新加坡"` |
| 排除高倍率、流量提示之类的"节点" | `exclude-filter`，例如 `"(?i)x[3-9]\|过期\|剩余\|官网"` |
| 不轮换，固定用最快的一个节点 | `proxy-groups` 里把 `type: load-balance` 和 `strategy` 两行换成 `type: url-test` |
| 同一个目标尽量用同一个节点 | `strategy: consistent-hashing`（默认的 `round-robin` 是每个新连接换一个） |
| 订阅多久更新一次 | `proxy-providers → airport → interval`，单位秒，默认 6 小时 |
| 给代理加用户名密码 | 打开 `authentication` 两行，然后 `DOUBAN_PROXY=http://用户名:密码@mihomo:7890` |

## 排查

| 现象 | 多半的原因 |
| --- | --- |
| 日志里有 `pull error` / `can't fetch provider` | 订阅链接不对或过期；服务器访问不了机场的订阅地址 |
| 第 4 步不是 `200`，或者超时 | 节点都不可用，或者被 `filter` 筛得一个不剩。先去掉 `filter` 试试 |
| 第 4 步通了，但网站还是直连 | `DOUBAN_PROXY` 没生效：看后台概览有没有"通过 DOUBAN_PROXY 出站"；容器是否加入了 `moovie-egress` 网络 |
| 用了代理反而更容易被豆瓣拒绝 | 机场的 IP 多在境外，而且很多人共用，豆瓣不一定更友好。换离国内近的地区，或者干脆不用代理 |
| 网站日志里有 `proxyconnect tcp: dial tcp: lookup mihomo` | 网站容器不在 `moovie-egress` 网络里，或者 mihomo 容器没在运行 |

## 停用和更新

```bash
# 停用：先把网站的 DOUBAN_PROXY 去掉并重启，再停掉容器
docker compose -f deploy/mihomo/docker-compose.yml down

# 更新 mihomo
docker compose -f deploy/mihomo/docker-compose.yml pull
docker compose -f deploy/mihomo/docker-compose.yml up -d
```

上线后建议把 `docker-compose.yml` 里的镜像从 `latest` 固定成一个你测试过的版本号。

## 要知道的两点

- 这份配置按 mihomo 的文档写的，YAML 格式校验过，但写的时候手边没有 Docker，没有实际启动过。第一次用请按上面的步骤逐步确认。
- 有些机场的服务条款不允许爬取或自动化请求，用之前看一下。豆瓣请求的总量仍然由网站控制（默认每秒最多一次）。
