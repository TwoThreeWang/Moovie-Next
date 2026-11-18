#!/usr/bin/env python3
"""
生成 tvui-lab 的静态原型页面。

模拟“服务端渲染”：真实项目里这些 HTML 由 Go Template 输出，
这里用 Python 把假数据渲染成同样结构的静态文件，方便纯静态预览。

用法：python3 tools/gen.py   （在 tvui-lab 目录下执行）
"""
from html import escape as e
import json
import os

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CUR_PAGE = ' aria-current="page"'
CUR_TRUE = ' aria-current="true"'
SEEN = ' class="seen"'
IMG = "https://picsum.photos/seed/{seed}/{w}/{h}.webp"

# ---------------------------------------------------------------- 假数据
# 字段对齐现有 Movie 结构：Title / OriginalTitle / Year / Genres / Rating / Countries / Duration
M = [
    # id, 标题, 原名, 年, 类型, 题材, 分, 主色, 总集数, 已更新, 连载中
    ("wugang", "雾港", "The Harbor in Fog", 2026, "剧集", "悬疑 / 犯罪", 8.9, "#33424d", 24, 8, True),
    ("changye", "长夜将尽", "Before the Long Night Ends", 2026, "电影", "剧情 / 爱情", 8.4, "#4a3328", 0, 0, False),
    ("qixia", "第七个夏天", "The Seventh Summer", 2025, "电影", "青春 / 剧情", 8.1, "#4d4630", 0, 0, False),
    ("xingyun", "星陨之地", "Land of Fallen Stars", 2026, "电影", "科幻 / 冒险", 7.9, "#1f2e47", 0, 0, False),
    ("shanhai", "山海食堂", "Mountain & Sea Canteen", 2026, "剧集", "治愈 / 美食", 9.1, "#3d4a2c", 12, 6, True),
    ("baiye", "白夜追凶者", "White Night Hunter", 2025, "剧集", "刑侦 / 悬疑", 8.7, "#2b2f3a", 32, 32, False),
    ("yunbian", "云边小镇", "Town by the Clouds", 2024, "电影", "家庭 / 喜剧", 7.6, "#46394a", 0, 0, False),
    ("haichao", "潮汐之间", "Between the Tides", 2026, "剧集", "爱情 / 都市", 7.8, "#284248", 18, 11, True),
    ("guiling", "归零", "Zero Point", 2025, "电影", "动作 / 惊悚", 7.4, "#4a2b2b", 0, 0, False),
    ("fengzheng", "风筝线", "Kite String", 2025, "剧集", "谍战 / 年代", 8.8, "#3f3a2c", 40, 40, False),
    ("jiguang", "极光旅馆", "Aurora Inn", 2024, "综艺", "真人秀 / 旅行", 8.2, "#203a3f", 10, 7, True),
    ("mimi", "秘密花园2", "Secret Garden II", 2026, "动漫", "奇幻 / 冒险", 9.0, "#2f4a3a", 26, 14, True),
    ("chenmo", "沉默的证人", "The Silent Witness", 2024, "电影", "犯罪 / 剧情", 8.3, "#30302f", 0, 0, False),
    ("xiaosheng", "小城之春", "Spring in a Small Town", 2025, "电影", "文艺 / 爱情", 8.0, "#4a4136", 0, 0, False),
    ("yeyou", "夜游神", "Night Wanderer", 2026, "剧集", "奇幻 / 古装", 7.7, "#2c2846", 30, 12, True),
    ("liuxing", "流星协奏曲", "Meteor Concerto", 2025, "动漫", "音乐 / 青春", 8.6, "#3a2c46", 12, 12, False),
    ("tianji", "天际线", "Skyline", 2024, "电影", "灾难 / 动作", 7.2, "#2a3946", 0, 0, False),
    ("wanjia", "万家灯火", "Ten Thousand Lights", 2026, "剧集", "家庭 / 都市", 8.5, "#463a28", 36, 20, True),
    ("bingfeng", "冰封", "Frozen Line", 2025, "电影", "悬疑 / 冒险", 7.8, "#2c3a46", 0, 0, False),
    ("youzi", "柚子味的夏天", "Taste of Pomelo", 2026, "综艺", "音乐 / 竞演", 7.5, "#46442a", 12, 9, True),
    ("anxian", "暗线", "Hidden Line", 2025, "剧集", "谍战 / 悬疑", 8.2, "#262626", 28, 28, False),
    ("yuanxing", "远行", "Far Journey", 2024, "电影", "公路 / 剧情", 8.1, "#3c3428", 0, 0, False),
    ("mengjing", "梦境收藏家", "Dream Collector", 2026, "动漫", "奇幻 / 治愈", 8.9, "#33284a", 24, 5, True),
    ("hongchen", "红尘客栈", "Inn of the Red Dust", 2025, "电影", "武侠 / 动作", 7.3, "#4a2c2c", 0, 0, False),
]
KEYS = ["id", "title", "orig", "year", "kind", "genres", "score", "c", "eps", "aired", "airing"]
MOVIES = [dict(zip(KEYS, m)) for m in M]
BY = {m["id"]: m for m in MOVIES}

SUMMARY = {
    "wugang": "台风季的港城，一桩二十年前的旧案在码头重新浮出水面。刚调回老家的女警和退休的老刑警，在潮湿的雾气里一点点拼回被所有人刻意遗忘的那个夜晚。",
    "_": "一个关于选择、代价与和解的故事。导演用克制的镜头语言，把人物放进巨大而安静的环境里，让情绪在留白中慢慢发酵。",
}
HISTORY = [  # 继续观看：(id, 集, 进度%, 剩余)
    ("wugang", 5, 62, "剩 18 分钟"),
    ("shanhai", 3, 24, "剩 31 分钟"),
    ("changye", 0, 81, "剩 22 分钟"),
    ("mimi", 12, 45, "剩 12 分钟"),
    ("fengzheng", 27, 90, "剩 4 分钟"),
]
HERO = ["wugang", "mengjing", "xingyun", "shanhai", "changye"]
TODAY = ["wugang", "shanhai", "mimi", "haichao", "jiguang", "wanjia"]
LISTS = [("东亚悬疑十二夜", "12 部 · 不到最后猜不出凶手", "wugang"), ("下饭治愈系", "18 部 · 适合一个人吃饭时看", "shanhai"),
         ("一个人的公路", "9 部 · 在路上的人", "yuanxing"), ("年度高分动画", "15 部 · 豆瓣 8.5+", "mengjing"),
         ("雨天适合看的电影", "20 部 · 安静又温柔", "xiaosheng")]
NAMES = ["阿青", "Lin", "小满", "木木", "老周", "Kiko", "一一", "南风"]


def img(seed, w, h):
    return IMG.format(seed=seed, w=w, h=h)


def poster_img(m, eager=False):
    s = m["id"]
    srcset = ", ".join(f"{img(s, w, w * 3 // 2)} {w}w" for w in (160, 240, 320, 420))
    lazy = 'fetchpriority="high"' if eager else 'loading="lazy"'
    return (f'<img src="{img(s, 240, 360)}" srcset="{srcset}" '
            f'sizes="(min-width:1200px) 204px, calc(14vw + 52px)" width="240" height="360" '
            f'{lazy} decoding="async" alt="">')


def wide_img(seed, sizes="(min-width:1200px) 380px, calc(18vw + 168px)"):
    srcset = ", ".join(f"{img(seed, w, w * 9 // 16)} {w}w" for w in (320, 480, 640, 800))
    return (f'<img src="{img(seed, 480, 270)}" srcset="{srcset}" sizes="{sizes}" '
            f'width="480" height="270" loading="lazy" decoding="async" alt="">')


def hero_picture(m, eager):
    s = m["id"]
    land = ", ".join(f"{img(s, w, w * 9 // 16)} {w}w" for w in (960, 1440, 1920))
    port = ", ".join(f"{img(s, w, w * 5 // 4)} {w}w" for w in (480, 720, 960))
    attr = 'fetchpriority="high"' if eager else 'loading="lazy"'
    return (f'<picture><source media="(min-width:720px)" srcset="{land}" sizes="calc(100vw - var(--nav-side, 0px))">'
            f'<img src="{img(s, 720, 900)}" srcset="{port}" sizes="100vw" width="720" height="900" {attr} decoding="async" alt=""></picture>')


def status(m):
    if m["kind"] == "电影":
        return "2 小时 08 分"
    if m["airing"]:
        return f"更新至第 {m['aired']} 集"
    return f"全 {m['eps']} 集"


def meta(m, extra=True):
    parts = [f'<span class="score">豆瓣 {m["score"]}</span>', str(m["year"]), m["genres"]]
    if extra:
        parts.append(status(m))
    return '<div class="meta">' + '<i class="sep"></i>'.join(f"<span>{p}</span>" if not p.startswith("<") else p for p in parts) + "</div>"


def card_attrs(m, history=False):
    a = f'data-id="{m["id"]}" data-title="{e(m["title"])}" data-meta="{m["year"]} · {e(m["genres"])}" data-img="{img(m["id"], 120, 180)}"'
    return a + (' data-history="1"' if history else "")


def poster_card(m, badge="", sub=None, friends=None):
    b = f'<span class="badge{" badge-new" if badge == "新" else ""}">{badge}</span>' if badge else ""
    f = ""
    if friends:
        f = '<span class="friends">' + "".join(f'<img src="{img("u-" + n, 48, 48)}" width="24" height="24" loading="lazy" alt="">' for n in friends) + "</span>"
    sub = sub if sub is not None else f'{m["year"]} · {m["genres"].split(" / ")[0]}'
    return (f'<a class="card" href="detail-{m["id"]}.html" style="--c:{m["c"]}" {card_attrs(m)}>'
            f'<div class="card-art">{poster_img(m)}{b}{f}<span class="score-chip">{m["score"]}</span></div>'
            f'<span class="card-title">{e(m["title"])}</span><span class="card-meta">{e(sub)}</span></a>')


def history_card(h):
    mid, ep, p, left = h
    m = BY[mid]
    sub = (f"第 {ep} 集 · " if ep else "") + left
    href = f"player-{mid}.html" + (f"?ep={ep}" if ep else "")
    return (f'<a class="card card-wide" href="{href}" style="--c:{m["c"]}" {card_attrs(m, True)}>'
            f'<div class="card-art">{wide_img(mid + "-s1")}'
            f'<div class="card-over"><span class="card-title">{e(m["title"])}</span><span class="card-meta">{sub}</span></div>'
            f'<div class="progress"><i style="--p:{p}%"></i></div></div></a>')


def banner_card(l):
    title, sub, seed = l
    m = BY[seed]
    return (f'<a class="card card-banner" href="discover.html" style="--c:{m["c"]}">'
            f'<div class="card-art">{wide_img(seed + "-list", "(min-width:1200px) 620px, calc(30vw + 160px)")}'
            f'<div class="card-over"><span class="card-title">{e(title)}</span><span class="card-meta">{e(sub)}</span></div></div></a>')


def row(title, cards, more="discover.html", sub="", cls="", rid=""):
    s = f'<span class="row-sub">{e(sub)}</span>' if sub else ""
    m = f'<a class="row-more" href="{more}">全部<svg class="i"><use href="#i-chev-r"/></svg></a>' if more else ""
    idattr = f' id="{rid}"' if rid else ""
    return (f'<section class="row"{idattr}><div class="row-head"><h2 class="row-title">{title}{s}</h2>{m}</div>'
            f'<div class="rail {cls}">{"".join(cards)}</div></section>')


def lazy_row(src, title_w=140, cls="", n=8):
    sk = "".join('<div class="card skeleton"><div class="card-art"></div><div class="card-title"></div></div>' for _ in range(n))
    return (f'<section class="row" data-lazy hx-get="{src}" hx-trigger="near once" hx-swap="outerHTML">'
            f'<div class="row-head"><span class="sk-line" style="width:{title_w}px"></span></div>'
            f'<div class="rail {cls}" aria-hidden="true">{sk}</div></section>')


# ---------------------------------------------------------------- 外壳
SPRITE = """<svg width="0" height="0" style="position:absolute" aria-hidden="true">
<symbol id="i-home" viewBox="0 0 24 24"><path d="M3 10.5 12 3l9 7.5V20a1 1 0 0 1-1 1h-5v-6h-6v6H4a1 1 0 0 1-1-1z"/></symbol>
<symbol id="i-compass" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="m15.5 8.5-2 5-5 2 2-5z"/></symbol>
<symbol id="i-search" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></symbol>
<symbol id="i-user" viewBox="0 0 24 24"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4 3.6-6 8-6s8 2 8 6"/></symbol>
<symbol id="i-play" viewBox="0 0 24 24"><path d="M7 4.5v15a1 1 0 0 0 1.5.86l12.5-7.5a1 1 0 0 0 0-1.72L8.5 3.64A1 1 0 0 0 7 4.5z" fill="currentColor" stroke="none"/></symbol>
<symbol id="i-pause" viewBox="0 0 24 24"><rect x="6" y="4.5" width="4" height="15" rx="1" fill="currentColor" stroke="none"/><rect x="14" y="4.5" width="4" height="15" rx="1" fill="currentColor" stroke="none"/></symbol>
<symbol id="i-plus" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></symbol>
<symbol id="i-check" viewBox="0 0 24 24"><path d="m5 12.5 4.5 4.5L19 7.5"/></symbol>
<symbol id="i-more" viewBox="0 0 24 24"><circle cx="5" cy="12" r="1.4"/><circle cx="12" cy="12" r="1.4"/><circle cx="19" cy="12" r="1.4"/></symbol>
<symbol id="i-chev-l" viewBox="0 0 24 24"><path d="m15 5-7 7 7 7"/></symbol>
<symbol id="i-chev-r" viewBox="0 0 24 24"><path d="m9 5 7 7-7 7"/></symbol>
<symbol id="i-share" viewBox="0 0 24 24"><path d="M12 3v12M7.5 7.5 12 3l4.5 4.5M5 13v6a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-6"/></symbol>
<symbol id="i-heart" viewBox="0 0 24 24"><path d="M12 20s-7.5-4.6-7.5-10.2A4.3 4.3 0 0 1 12 7a4.3 4.3 0 0 1 7.5 2.8C19.5 15.4 12 20 12 20z"/></symbol>
<symbol id="i-back10" viewBox="0 0 24 24"><path d="M3 4v5h5"/><path d="M3.6 9A9 9 0 1 1 3 12"/><path d="M10 10v5M13 10.5h2.5v4H13z"/></symbol>
<symbol id="i-fwd10" viewBox="0 0 24 24"><path d="M21 4v5h-5"/><path d="M20.4 9A9 9 0 1 0 21 12"/><path d="M9 10v5M12 10.5h2.5v4H12z"/></symbol>
<symbol id="i-next" viewBox="0 0 24 24"><path d="M5 5v14l10-7z" fill="currentColor"/><path d="M19 5v14"/></symbol>
<symbol id="i-vol" viewBox="0 0 24 24"><path d="M4 9h4l5-4v14l-5-4H4z"/><path d="M16.5 8.5a5 5 0 0 1 0 7M19 6a8.5 8.5 0 0 1 0 12"/></symbol>
<symbol id="i-dm" viewBox="0 0 24 24"><path d="M4 5h16v11H9l-5 4z"/><path d="M8 9.5h8M8 12.5h5"/></symbol>
<symbol id="i-pip" viewBox="0 0 24 24"><rect x="3" y="5" width="18" height="14" rx="2"/><rect x="12" y="11" width="7" height="6" rx="1"/></symbol>
<symbol id="i-fs" viewBox="0 0 24 24"><path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/></symbol>
<symbol id="i-theater" viewBox="0 0 24 24"><rect x="2.5" y="6" width="19" height="12" rx="2"/></symbol>
<symbol id="i-trash" viewBox="0 0 24 24"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/></symbol>
<symbol id="i-eye" viewBox="0 0 24 24"><path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></symbol>
<symbol id="i-info" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.5v.5"/></symbol>
<symbol id="i-layers" viewBox="0 0 24 24"><path d="m12 3 9 5-9 5-9-5z"/><path d="m3 13 9 5 9-5"/></symbol>
<symbol id="i-bell" viewBox="0 0 24 24"><path d="M6 9a6 6 0 0 1 12 0c0 6 2.5 7.5 2.5 7.5h-17S6 15 6 9zM10 20a2 2 0 0 0 4 0"/></symbol>
<symbol id="i-cog" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M4.2 4.2l2.1 2.1M17.7 17.7l2.1 2.1M2 12h3M19 12h3M4.2 19.8l2.1-2.1M17.7 6.3l2.1-2.1"/></symbol>
<symbol id="i-sync" viewBox="0 0 24 24"><path d="M20 11a8 8 0 0 0-14.3-4.9L4 8M4 4v4h4M4 13a8 8 0 0 0 14.3 4.9L20 16M20 20v-4h-4"/></symbol>
<symbol id="i-chat" viewBox="0 0 24 24"><path d="M21 12a8 8 0 0 1-11.8 7L4 20l1.1-4.6A8 8 0 1 1 21 12z"/></symbol>
</svg>"""

SPEC = """<script type="speculationrules">{"prerender":[{"where":{"and":[{"href_matches":"/*.html"},{"not":{"selector_matches":"[data-no-prerender]"}}]},"eagerness":"moderate"}]}</script>"""


def head(title, extra_css=(), ambient="#2a2a35", body_cls=""):
    css = "".join(f'<link rel="stylesheet" href="css/{c}.css">' for c in extra_css)
    cls = f' class="{body_cls}"' if body_cls else ""
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#0a0a0c">
<title>{e(title)}</title>
<link rel="preconnect" href="https://picsum.photos">
<link rel="preconnect" href="https://fastly.picsum.photos">
<link rel="stylesheet" href="css/app.css">{css}
<script src="js/htmx.min.js" defer></script>
<script src="js/app.js" defer></script>
{SPEC}
</head>
<body{cls} style="--ambient:{ambient}">
{SPRITE}
"""


def nav(current):
    items = [("home", "index.html", "首页", "i-home"), ("discover", "discover.html", "发现", "i-compass"),
             ("search", "search.html", "搜索", "i-search"), ("me", "me.html", "我的", "i-user")]
    li = "".join(
        f'<li><a class="nav-link" data-tab="{k}" href="{h}"{CUR_PAGE if k == current else ""}>'
        f'<svg class="i"><use href="#{i}"/></svg><span>{t}</span></a></li>' for k, h, t, i in items)
    return (f'<nav class="nav" aria-label="主导航"><a class="nav-brand" href="index.html" aria-label="Moovie 首页">'
            f'<span class="brand-m">M</span><span class="brand-t">Moovie</span></a><ul class="nav-links">{li}</ul>'
            f'<a class="nav-avatar" href="me.html" aria-label="我的"><img class="avatar" src="{img("u-me", 72, 72)}" width="36" height="36" alt=""></a></nav>')


SHEET = """<dialog class="sheet" id="sheet">
<div class="sheet-grab"></div>
<div class="sheet-head"><img alt="" id="sheet-img"><div><strong id="sheet-title"></strong><span id="sheet-meta"></span></div></div>
<a class="sheet-item" id="sheet-play" href="#"><svg class="i"><use href="#i-play"/></svg>播放</a>
<button class="sheet-item" data-sheet="wish"><svg class="i"><use href="#i-plus"/></svg>加入想看</button>
<button class="sheet-item" data-sheet="seen"><svg class="i"><use href="#i-eye"/></svg>标记为看过</button>
<a class="sheet-item" id="sheet-detail" href="#"><svg class="i"><use href="#i-info"/></svg>查看详情</a>
<button class="sheet-item danger" data-sheet="remove"><svg class="i"><use href="#i-trash"/></svg>移除观看记录</button>
</dialog>
<div class="toast" id="toast" role="status" aria-live="polite"></div>
"""

FOOT = """<footer class="footer">Moovie tvui-lab 原型 · 图片为占位图</footer>
</main>
""" + SHEET + "</body>\n</html>\n"


def write(path, html):
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w", encoding="utf-8") as f:
        f.write(html)


# ---------------------------------------------------------------- 首页
def page_index():
    slides, dots = [], []
    for i, mid in enumerate(HERO):
        m = BY[mid]
        kicker = '<span class="dot"></span>今日更新' if m["airing"] else ("新片上线" if m["kind"] == "电影" else "热门" + m["kind"])
        summ = SUMMARY.get(mid, SUMMARY["_"])
        play = f"player-{mid}.html"
        slides.append(
            f'<div class="slide" style="--c:{m["c"]}" data-c="{m["c"]}" aria-roledescription="slide" aria-label="{i + 1} / {len(HERO)}">'
            f'<div class="slide-media">{hero_picture(m, i == 0)}</div>'
            f'<div class="slide-body"><p class="kicker">{kicker} · {m["kind"]}</p>'
            f'<h2 class="slide-title"><a href="detail-{mid}.html">{e(m["title"])}</a></h2>{meta(m)}'
            f'<p class="slide-summary">{e(summ)}</p>'
            f'<div class="slide-actions"><a class="btn btn-primary" href="{play}" data-no-prerender><svg class="i"><use href="#i-play"/></svg>播放</a>'
            f'<button class="btn btn-glass btn-icon" data-wish="{mid}" aria-pressed="false" aria-label="加入想看"><svg class="i"><use href="#i-plus"/></svg></button>'
            f'<a class="btn btn-glass btn-icon" href="detail-{mid}.html" aria-label="详情"><svg class="i"><use href="#i-info"/></svg></a></div></div></div>')
        dots.append(f'<button class="hero-dot" data-i="{i}" aria-label="第 {i + 1} 张"{CUR_TRUE if i == 0 else ""}></button>')

    hist = row("继续观看", [history_card(h) for h in HISTORY], more="me.html", cls="rail-wide")
    today = row("今日更新", [poster_card(BY[i], "新", f"更新至第 {BY[i]['aired']} 集") for i in TODAY], sub="你在追的 6 部", more="me.html")

    html = head("Moovie 影牛 · 发现你的下一部", ambient=BY[HERO[0]]["c"]) + nav("home") + f"""<main>
<section class="hero" aria-roledescription="carousel" aria-label="精选">
<div class="hero-track" id="hero-track">{"".join(slides)}</div>
<div class="hero-dots" role="tablist">{"".join(dots)}</div>
</section>
<div class="rows">
{hist}
{today}
{lazy_row("partials/row-foryou.html", 220)}
{lazy_row("partials/row-hot-movie.html")}
{lazy_row("partials/row-lists.html", 120, "rail-banner", 4)}
{lazy_row("partials/row-hot-tv.html")}
{lazy_row("partials/row-friends.html")}
{lazy_row("partials/row-anime.html")}
</div>
""" + FOOT
    write("index.html", html)

    films = [m for m in MOVIES if m["kind"] == "电影"]
    tv = [m for m in MOVIES if m["kind"] == "剧集"]
    anime = [m for m in MOVIES if m["kind"] in ("动漫", "综艺")]
    write("partials/row-foryou.html", row("为你推荐", [poster_card(m, sub=r) for m, r in zip(
        [BY[x] for x in ["baiye", "chenmo", "anxian", "bingfeng", "fengzheng", "guiling", "yeyou", "tianji"]],
        ["同为港城刑侦", "同导演作品", "悬疑高分", "冷峻风格", "你可能喜欢", "节奏紧凑", "奇幻悬疑", "同类高分"])],
        sub="因为你看过《雾港》", more="discover.html"))
    write("partials/row-hot-movie.html", row("热门电影", [poster_card(m) for m in films]))
    write("partials/row-hot-tv.html", row("热门剧集", [poster_card(m, "更新" if m["airing"] else "") for m in tv]))
    write("partials/row-anime.html", row("动漫与综艺", [poster_card(m) for m in anime]))
    write("partials/row-lists.html", row("精选片单", [banner_card(l) for l in LISTS], cls="rail-banner"))
    fr = []
    for i, mid in enumerate(["qixia", "baiye", "liuxing", "yunbian", "xiaosheng", "jiguang", "yuanxing", "hongchen"]):
        who = NAMES[i % len(NAMES)], NAMES[(i + 3) % len(NAMES)]
        fr.append(poster_card(BY[mid], sub=f"{who[0]}、{who[1]} 在看", friends=who))
    write("partials/row-friends.html", row("朋友在看", fr, more="me.html"))


# ---------------------------------------------------------------- 详情页
def ep_rail(m, season=1, rid="ep-rail"):
    n = m["eps"] if season == 1 or m["id"] != "wugang" else 12
    aired = m["aired"] if m["airing"] or season == 1 else n
    if m["id"] == "wugang" and season == 1:
        n, aired = 12, 12
    if m["id"] == "wugang" and season == 2:
        n, aired = 12, 8
    cur = 5 if m["id"] == "wugang" and season == 2 else 0
    cards = []
    for k in range(1, aired + 1):
        seen = (season == 1 and m["id"] == "wugang") or (cur and k < cur)
        chk = '<span class="ep-check"><svg class="i"><use href="#i-check"/></svg></span>' if seen else ""
        prog = '<div class="progress"><i style="--p:62%"></i></div>' if k == cur else ""
        cards.append(
            f'<a class="ep{" is-current" if k == cur else ""}" href="player-{m["id"]}.html?ep={k}" data-no-prerender>'
            f'<div class="card-art">{wide_img(m["id"] + f"-e{season}-{k}", "(min-width:1200px) 320px, calc(16vw + 160px)")}{chk}{prog}</div>'
            f'<span class="ep-n">第 {k} 集 · 45 分钟</span><span class="ep-t">{e(EP_TITLES[(k + season) % len(EP_TITLES)])}</span>'
            f'<span class="ep-d">{e(EP_DESC)}</span></a>')
    if m["airing"] and (season == 2 or m["id"] != "wugang"):
        cards.append(f'<div class="ep ep-soon"><div class="card-art"><div><b>第 {aired + 1} 集</b><span>11 月 12 日 周三 20:00</span></div></div>'
                     f'<span class="ep-n">即将上线</span></div>')
    return f'<div class="rail rail-ep" id="{rid}">{"".join(cards)}</div>'


EP_TITLES = ["雾起", "旧码头", "第二个证人", "潮声", "沉船", "灯塔守夜人", "回南天", "失踪的账本", "无人认领", "台风眼", "风停之后", "归港"]
EP_DESC = "老刑警在码头仓库发现一张泛黄的船票，日期正是案发当晚；而她母亲的名字，赫然出现在乘客名单上。"


def page_detail(m):
    mid = m["id"]
    series = m["kind"] != "电影"
    hist = next((h for h in HISTORY if h[0] == mid), None)
    if hist and series:
        play_l, play_s = "继续播放", f"第 {hist[1]} 集 · {hist[3]}"
        play_href = f"player-{mid}.html?ep={hist[1]}"
    elif hist:
        play_l, play_s, play_href = "继续播放", hist[3], f"player-{mid}.html"
    else:
        play_l, play_s, play_href = "播放", "第 1 集" if series else "正片 · 2 小时 08 分", f"player-{mid}.html"
    sched = (f'<p class="d-schedule"><svg class="i"><use href="#i-bell"/></svg><span>下集 <b>11 月 12 日 周三</b> 更新</span></p>'
             if m["airing"] else "")

    eps = ""
    if series:
        seg = ""
        if mid == "wugang":
            seg = ('<div class="seg" role="tablist" hx-target="#ep-rail" hx-swap="outerHTML">'
                   '<button role="tab" aria-selected="false" hx-get="partials/ep-wugang-s1.html">第 1 季</button>'
                   '<button role="tab" aria-selected="true" hx-get="partials/ep-wugang-s2.html">第 2 季</button></div>')
        eps = (f'<section class="d-sec" id="episodes"><div class="row-head"><h2 class="row-title">选集'
               f'<span class="row-sub">{status(m)}</span></h2>{seg}</div>'
               f'{ep_rail(m, 2 if mid == "wugang" else 1)}</section>')

    stills = row("剧照", [f'<div class="card card-wide"><div class="card-art">{wide_img(f"{mid}-s{k}")}</div></div>' for k in range(1, 7)],
                 more="", cls="rail-wide")
    info = f"""<section class="info"><h2>信息</h2><dl>
<div><dt>导演</dt><dd>陈默</dd></div><div><dt>编剧</dt><dd>许知远 / 林夕</dd></div>
<div><dt>主演</dt><dd>周冬雨 / 张译 / 王景春</dd></div><div><dt>类型</dt><dd>{e(m["genres"])}</dd></div>
<div><dt>地区</dt><dd>中国大陆</dd></div><div><dt>语言</dt><dd>普通话 / 粤语</dd></div>
<div><dt>首播</dt><dd>{m["year"]}-10-08</dd></div><div><dt>{"集数" if series else "片长"}</dt><dd>{f'{m["eps"]} 集' if series else "128 分钟"}</dd></div>
<div><dt>又名</dt><dd>{e(m["orig"])}</dd></div><div><dt>IMDb</dt><dd>tt3{sum(map(ord, mid)) * 7919 % 10 ** 6:06d}</dd></div>
</dl></section>"""

    html = head(f'{m["title"]} - Moovie', ["detail"], m["c"]) + nav("home") + f"""<div class="d-top">
<a class="btn btn-icon" href="index.html" data-back aria-label="返回"><svg class="i"><use href="#i-chev-l"/></svg></a>
<button class="btn btn-icon" data-share aria-label="分享"><svg class="i"><use href="#i-share"/></svg></button>
</div>
<main class="detail" style="--c:{m["c"]}">
<section class="d-hero">
<div class="d-media">{hero_picture(m, True)}</div>
<div class="d-body">
<p class="kicker">{m["kind"]} · {status(m)}</p>
<h1 class="d-title">{e(m["title"])}</h1>
<p class="d-orig">{e(m["orig"])}</p>
{meta(m, False)}
<div class="d-actions" id="d-actions">
<a class="btn btn-primary" href="{play_href}" data-no-prerender><span style="display:flex;gap:8px;align-items:center"><svg class="i"><use href="#i-play"/></svg>{play_l}</span><small>{play_s}</small></a>
<button class="btn btn-glass btn-icon" data-wish="{mid}" aria-pressed="false" aria-label="加入想看"><svg class="i"><use href="#i-plus"/></svg></button>
<button class="btn btn-glass btn-icon" data-seen="{mid}" aria-pressed="false" aria-label="标记看过"><svg class="i"><use href="#i-eye"/></svg></button>
</div>
<p class="d-summary" id="d-summary">{e(SUMMARY.get(mid, SUMMARY["_"]) * 2)}</p>
<button class="d-more" data-more="d-summary">更多</button>
{sched}
</div>
</section>
<div class="d-grid">
<div class="d-main">
{eps}
{stills}
{lazy_row("partials/d-reviews.html", 80, "rail-review", 4)}
</div>
<aside class="d-aside">
{info}
{lazy_row("partials/d-cast.html", 80, "rail-cast", 8)}
</aside>
</div>
{lazy_row("partials/d-similar.html", 100)}
<div class="d-bar" id="d-bar">
<a class="btn btn-primary" href="{play_href}" data-no-prerender><svg class="i"><use href="#i-play"/></svg>{play_l} · {play_s}</a>
<button class="btn btn-glass btn-icon" data-wish="{mid}" aria-pressed="false" aria-label="加入想看"><svg class="i"><use href="#i-plus"/></svg></button>
</div>
""" + FOOT
    write(f"detail-{mid}.html", html)


def detail_partials():
    write("partials/ep-wugang-s1.html", ep_rail(BY["wugang"], 1))
    write("partials/ep-wugang-s2.html", ep_rail(BY["wugang"], 2))
    cast = [("周冬雨", "饰 林汐"), ("张译", "饰 老陈"), ("王景春", "饰 船老大"), ("袁泉", "饰 母亲"), ("陈默", "导演"),
            ("黄觉", "饰 周律师"), ("谭卓", "饰 阿梅"), ("许知远", "编剧"), ("段奕宏", "特别出演")]
    people = "".join(f'<div class="person"><img src="{img("p-" + str(i), 144, 144)}" width="72" height="72" loading="lazy" alt=""><b>{n}</b><span>{r}</span></div>'
                     for i, (n, r) in enumerate(cast))
    write("partials/d-cast.html", row("演职员", [people], more="", cls="rail-cast"))
    revs = [("阿青", 5, "配乐和雾气一样黏在皮肤上。第五集码头那场戏，没有一句台词却把我看哭了。", 1203),
            ("Lin", 4, "节奏比第一季稳很多，人物终于落地了。唯一的问题是每集结尾钩子有点刻意。", 864),
            ("老周", 5, "国产悬疑终于学会了「不解释」。镜头相信观众，观众也就愿意相信它。", 652),
            ("小满", 3, "前半段很好，后面感情线有点拖。但摄影真的是电影级的。", 211),
            ("Kiko", 4, "回南天那一集的潮湿感是怎么拍出来的？隔着屏幕都觉得墙在出水。", 188)]
    cards = "".join(
        f'<article class="review"><div class="review-head"><img src="{img("u-" + n, 64, 64)}" width="32" height="32" loading="lazy" alt="">'
        f'<div><b>{n}</b><div class="stars">{"★" * s}{"☆" * (5 - s)}</div></div></div><p>{t}</p>'
        f'<div class="review-foot"><span>11 月 {3 + i} 日</span><button class="like" aria-pressed="false" data-like>'
        f'<svg class="i"><use href="#i-heart"/></svg><span>{c}</span></button></div></article>'
        for i, (n, s, t, c) in enumerate(revs))
    write("partials/d-reviews.html", f'<section class="row"><div class="row-head"><h2 class="row-title">短评<span class="row-sub">2.3 万条</span></h2>'
          f'<a class="row-more" href="#">全部<svg class="i"><use href="#i-chev-r"/></svg></a></div><div class="rail rail-review">{cards}</div></section>')
    write("partials/d-similar.html", row("相似推荐", [poster_card(BY[x], sub=r) for x, r in [
        ("baiye", "同为港城刑侦"), ("anxian", "同编剧"), ("chenmo", "旧案翻查"), ("fengzheng", "年代悬疑"),
        ("bingfeng", "冷峻风格"), ("guiling", "节奏紧凑"), ("yeyou", "奇幻悬疑"), ("changye", "同主演")]], more="discover.html"))


# ---------------------------------------------------------------- 播放页
def page_player(m):
    mid = m["id"]
    series = m["kind"] != "电影"
    total = m["aired"] if m["airing"] else (m["eps"] or 1)
    if mid == "wugang":
        total = 8
    hist = next((h for h in HISTORY if h[0] == mid), None)
    seen = hist[1] if hist and series else 0
    grid = ""
    if series:
        grid = "".join(
            f'<a href="player-{mid}.html?ep={k}" data-ep="{k}"{SEEN if k < seen else ""}>{k}'
            f'{"<i class=eq></i>" if m["airing"] and k == total else ""}</a>' for k in range(1, total + 1))
        grid = (f'<section><h3 class="pl-h">选集<small>{status(m)}{" · 下集 11/12 周三" if m["airing"] else ""}</small></h3>'
                f'<div class="ep-grid" id="ep-grid">{grid}</div></section>')
    srcs = [("自动选择 · 最佳线路", "38ms", True), ("线路 A · 量子", "52ms", False), ("线路 B · 非凡", "96ms", False), ("线路 C · 红牛", "180ms", False)]
    src = "".join(
        f'<button role="radio" aria-checked="{"true" if a else "false"}" data-src="{n}"><svg class="i"><use href="#i-layers"/></svg>{n}'
        f'<span class="lat{" good" if int(l[:-2]) < 60 else ""}">{l}</span></button>' for n, l, a in srcs)
    data = json.dumps({"id": mid, "title": m["title"], "series": series, "total": total, "intro": 12,
                       "src": "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8"}, ensure_ascii=False)
    poster = img(mid + "-s1", 1280, 720)
    html = head(f'{m["title"]} - 播放 - Moovie', ["player"], m["c"], "player-page") + nav("home") + f"""<main class="pl" id="pl">
<div class="pl-stage">
<div class="vp" id="vp" data-state="paused" data-dm="on" data-cfg='{e(data)}'>
<video playsinline preload="none" poster="{poster}"></video>
<div class="vp-bright"></div>
<div class="vp-dm" aria-hidden="true"></div>
<div class="vp-ripple" data-side="-1">« 10 秒</div><div class="vp-ripple" data-side="1">10 秒 »</div>
<div class="vp-spin"></div>
<div class="vp-ui">
 <div class="vp-top">
  <a class="vbtn" href="detail-{mid}.html" aria-label="返回"><svg class="i"><use href="#i-chev-l"/></svg></a>
  <div class="t"><b>{e(m["title"])}</b><span id="vp-sub">{"第 1 集" if series else "正片"}</span></div>
  <button class="vbtn" data-act="pip" aria-label="画中画"><svg class="i"><use href="#i-pip"/></svg></button>
 </div>
 <div class="vp-mid">
  <button class="vbtn vp-jump" data-act="back" aria-label="后退 10 秒"><svg class="i"><use href="#i-back10"/></svg></button>
  <button class="vbtn vp-big" data-act="toggle" aria-label="播放/暂停"><svg class="i i-play-on"><use href="#i-play"/></svg><svg class="i i-pause-on"><use href="#i-pause"/></svg></button>
  <button class="vbtn vp-jump" data-act="fwd" aria-label="前进 10 秒"><svg class="i"><use href="#i-fwd10"/></svg></button>
 </div>
 <div class="vp-bottom">
  <div class="vp-bar" id="vp-bar" role="slider" aria-label="进度" tabindex="0"><div class="track"><div class="buf"></div><div class="done"></div><div class="knob"></div></div><span class="tip">0:00</span></div>
  <div class="vp-row">
   <button class="vbtn" data-act="toggle" aria-label="播放/暂停"><svg class="i i-play-on"><use href="#i-play"/></svg><svg class="i i-pause-on"><use href="#i-pause"/></svg></button>
   {'<button class="vbtn" data-act="next" aria-label="下一集"><svg class="i"><use href="#i-next"/></svg></button>' if series else ""}
   <button class="vbtn vp-vol" data-act="mute" aria-label="静音"><svg class="i"><use href="#i-vol"/></svg></button>
   <span class="vp-time"><span id="vp-cur">0:00</span> / <span id="vp-dur">0:00</span></span>
   <span class="grow"></span>
   <button class="vbtn" data-act="dm" aria-pressed="true" aria-label="弹幕"><svg class="i"><use href="#i-dm"/></svg></button>
   <button class="vbtn txt" data-act="speed" aria-label="倍速" id="vp-speed">1.0×</button>
   <button class="vbtn only-wide" data-act="theater" aria-label="剧场模式"><svg class="i"><use href="#i-theater"/></svg></button>
   <button class="vbtn" data-act="fs" aria-label="全屏"><svg class="i"><use href="#i-fs"/></svg></button>
  </div>
 </div>
</div>
<div class="vp-menu" id="vp-menu" role="menu">{"".join(f'<button role="menuitemradio" aria-checked="{"true" if r == 1 else "false"}" data-rate="{r}">{r}×</button>' for r in (0.5, 0.75, 1, 1.25, 1.5, 2))}</div>
<div class="vp-hint" id="vp-hint"></div>
<button class="vp-skip" id="vp-skip" data-act="skip">跳过片头</button>
<button class="vp-next" id="vp-next" data-act="next"><img src="{img(mid + '-s2', 192, 108)}" width="96" height="54" loading="lazy" alt=""><span><small>即将播放</small>下一集</span><span class="ring"><svg class="i"><use href="#i-play"/></svg></span></button>
</div>
</div>
<section class="pl-side">
<div class="full"><h1 class="pl-title">{e(m["title"])}</h1><p class="pl-meta">{m["year"]} · {e(m["genres"])} · 豆瓣 {m["score"]}</p></div>
<div class="pl-acts full">
<button class="chip" data-wish="{mid}" aria-pressed="false"><svg class="i"><use href="#i-plus"/></svg>想看</button>
<button class="chip" data-seen="{mid}" aria-pressed="false"><svg class="i"><use href="#i-eye"/></svg>看过</button>
<a class="chip" href="detail-{mid}.html"><svg class="i"><use href="#i-info"/></svg>详情</a>
<button class="chip" data-share><svg class="i"><use href="#i-share"/></svg>分享</button>
</div>
{grid}
<section><h3 class="pl-h">来源<small>播放失败会在同一集内自动切换</small></h3><div class="src" role="radiogroup" id="src">{src}</div></section>
<section><h3 class="pl-h">弹幕<small>2,418 条</small></h3><form class="dm-form" id="dm-form"><input name="t" maxlength="40" placeholder="发一条友善的弹幕" autocomplete="off" enterkeyhint="send"><button class="btn btn-primary" style="height:44px;padding:0 18px">发送</button></form></section>
</section>
</main>
""" + SHEET + '<script src="js/player.js" defer></script>\n</body>\n</html>\n'
    write(f"player-{mid}.html", html)


# ---------------------------------------------------------------- 发现 / 搜索 / 我的
def chips(items, active=0):
    return '<div class="chips">' + "".join(
        f'<a class="chip-l" href="#"{CUR_TRUE if i == active else ""}>{t}</a>' for i, t in enumerate(items)) + "</div>"


def page_discover():
    grid = "".join(poster_card(m) for m in MOVIES)
    html = head("发现 - Moovie", body_cls="") + nav("discover") + f"""<main>
<header class="page-head"><h1 class="page-title">发现</h1></header>
<div class="sticky-filters">
{chips(["全部", "电影", "剧集", "综艺", "动漫", "趋势"])}
{chips(["全部类型", "悬疑", "剧情", "爱情", "科幻", "喜剧", "动作", "犯罪", "治愈", "纪录片", "奇幻", "家庭"], 0)}
</div>
<div class="grid" id="grid">{grid}
<div class="grid-more" data-lazy hx-get="partials/grid-more.html" hx-trigger="near once" hx-swap="outerHTML"></div>
</div>
""" + FOOT
    write("discover.html", html)
    write("partials/grid-more.html", "".join(poster_card(m) for m in reversed(MOVIES)))


def page_search():
    grid = "".join(poster_card(m) for m in MOVIES)
    hot = ["雾港", "山海食堂", "梦境收藏家", "风筝线", "长夜将尽", "白夜追凶者", "星陨之地", "秘密花园2"]
    html = head("搜索 - Moovie") + nav("search") + f"""<main class="search-page">
<header class="page-head"><h1 class="page-title">搜索</h1></header>
<form class="search-box" action="search.html" role="search" onsubmit="return false">
<svg class="i"><use href="#i-search"/></svg>
<input id="q" name="kw" type="search" placeholder="电影、剧集、演员" autocomplete="off" enterkeyhint="search" autofocus>
</form>
<div id="search-empty">
<section class="search-sec" id="recent-wrap" hidden><div class="row-head"><h2 class="row-title">最近搜索</h2><button class="row-more" id="recent-clear">清除</button></div><div class="chips wrap" id="recent"></div></section>
<section class="search-sec"><div class="row-head"><h2 class="row-title">大家都在搜</h2></div>
<ol class="hot">{"".join(f'<li><a href="detail-{BY_T[t]}.html"><b>{i + 1}</b>{t}</a></li>' for i, t in enumerate(hot))}</ol></section>
</div>
<div class="grid" id="results" hidden>{grid}</div>
<p class="empty" id="no-result" hidden>没有找到相关内容，换个关键词试试</p>
<script>
(() => {{
  // 原型：在已渲染的卡片里过滤；生产环境换成 hx-get="/api/v2/media/suggest" hx-trigger="input changed delay:150ms"
  const q = document.getElementById('q'), res = document.getElementById('results'), empty = document.getElementById('search-empty'), none = document.getElementById('no-result');
  const cards = [...res.children];
  const KEY = 'mv:recent';
  const get = () => {{ try {{ return JSON.parse(localStorage.getItem(KEY) || '[]'); }} catch {{ return []; }} }};
  const save = (list) => {{ try {{ localStorage.setItem(KEY, JSON.stringify(list.slice(0, 10))); }} catch {{}} }};
  const renderRecent = () => {{
    const list = get(), wrap = document.getElementById('recent-wrap'), box = document.getElementById('recent');
    wrap.hidden = !list.length;
    box.replaceChildren(...list.map((t) => {{ const a = document.createElement('a'); a.className = 'chip-l'; a.href = '#'; a.textContent = t; a.onclick = (e) => {{ e.preventDefault(); q.value = t; run(); }}; return a; }}));
  }};
  let timer;
  const run = () => {{
    const k = q.value.trim().toLowerCase();
    empty.hidden = !!k; res.hidden = !k;
    let n = 0;
    for (const c of cards) {{ const hit = k && (c.dataset.title + c.dataset.meta).toLowerCase().includes(k); c.hidden = !hit; n += hit; }}
    none.hidden = !k || n > 0;
    clearTimeout(timer);
    if (k && n) timer = setTimeout(() => save([q.value.trim(), ...get().filter((x) => x !== q.value.trim())]), 1200);
  }};
  q.addEventListener('input', run);
  document.getElementById('recent-clear').onclick = () => {{ save([]); renderRecent(); }};
  renderRecent();
  const kw = new URLSearchParams(location.search).get('kw'); if (kw) {{ q.value = kw; run(); }}
}})();
</script>
""" + FOOT
    write("search.html", html)


BY_T = {m["title"]: m["id"] for m in MOVIES}


def page_me():
    wish = [BY[x] for x in ["xingyun", "qixia", "liuxing", "jiguang", "yunbian", "tianji", "hongchen", "youzi"]]
    seen = [BY[x] for x in ["baiye", "fengzheng", "anxian", "chenmo", "yuanxing", "xiaosheng", "bingfeng", "guiling"]]
    html = head("我的 - Moovie") + nav("me") + f"""<main>
<header class="page-head me-head">
<img class="avatar" src="{img("u-me", 160, 160)}" width="72" height="72" alt="">
<div><h1 class="page-title">Evan</h1><p class="me-stats"><b>214</b> 看过 · <b>32</b> 想看 · <b>6</b> 在追</p></div>
<a class="btn btn-glass btn-icon" href="#" aria-label="通知"><svg class="i"><use href="#i-bell"/></svg></a>
</header>
<a class="report" href="#" style="--c:#3a2c46">
<span class="kicker">10 月观影报告</span>
<b>这个月你看了 12 部，最爱悬疑</b>
<span>共 31 小时 · 比 9 月多 4 部 · 超过 82% 的用户</span>
</a>
<div class="rows">
{row("观看历史", [history_card(h) for h in HISTORY], more="#", cls="rail-wide")}
{row("想看", [poster_card(m) for m in wish], more="#", sub="32")}
{row("看过", [poster_card(m, sub="★★★★★"[: 3 + i % 3]) for i, m in enumerate(seen)], more="#", sub="214")}
</div>
<ul class="menu">
<li><a href="#"><svg class="i"><use href="#i-bell"/></svg>通知<span class="pill">3</span></a></li>
<li><a href="#"><svg class="i"><use href="#i-sync"/></svg>豆瓣同步<span class="dim">上次同步 2 小时前</span></a></li>
<li><a href="#"><svg class="i"><use href="#i-cog"/></svg>设置</a></li>
<li><a href="#"><svg class="i"><use href="#i-chat"/></svg>反馈与建议</a></li>
</ul>
""" + FOOT
    write("me.html", html)


if __name__ == "__main__":
    page_index()
    for m in MOVIES:
        page_detail(m)
        page_player(m)
    detail_partials()
    page_discover()
    page_search()
    page_me()
    print("ok")
