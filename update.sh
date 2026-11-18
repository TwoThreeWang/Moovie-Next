#!/bin/sh
# 在服务器上更新：拉最新代码，重新构建镜像并重启网站、后台任务和弹幕服务。表结构的变化启动时自动执行。
set -eu
cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "没有 .env：先照 .env.example 和 deploy/README.md 写好正式环境的配置。" >&2
    exit 1
fi
mkdir -p data/danmu-cache

echo "拉取最新代码…"
git pull --ff-only

echo "重新构建并启动…"
docker compose up -d --build
docker compose ps
