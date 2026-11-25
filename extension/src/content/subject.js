// 豆瓣电影详情页（movie.douban.com/subject/<id>/）：右侧栏顶部插一张影牛卡片，
// 影牛上这部作品的资料缺了或者过期了，就把页面上的资料摘几块交给影牛（设置里可以关）。
(async () => {
  const match = /^\/subject\/(\d{7,9})\/?$/.exec(location.pathname);
  if (!match) return;
  const doubanId = match[1];
  const { h, send, settings: loadSettings } = globalThis.Moovie;
  const settings = await loadSettings();
  if (settings.card === false && settings.pageUpload === false) return;

  // 网站地址由插件后台记在 storage 里（内容脚本读不到 config.js）。影牛接口给的链接是站内路径，在这里拼成完整链接。
  const { siteURL = "" } = await chrome.storage.local.get("siteURL");
  const site = (path) => (siteURL ? new URL(path, siteURL).href : path);
  const connectURL = site("/extension/connect");

  const pageTitle = (document.querySelector('[property="v:itemreviewed"]')?.textContent || "").trim();
  const pageYear = parseInt((document.querySelector("#content h1 .year")?.textContent || "").replace(/\D/g, ""), 10) || 0;
  const pagePoster = document.querySelector("#mainpic img")?.getAttribute("src") || "";
  const seed = { douban_id: doubanId, title: pageTitle, year: pageYear, poster: pagePoster };

  // pageExtract 只摘资料所在的几块（片名、海报、信息栏、评分、简介、结构化数据），不带页面上你的账号信息。
  function pageExtract() {
    const pick = (selector) => document.querySelector(selector)?.outerHTML || "";
    const ld = pick('script[type="application/ld+json"]');
    const title = document.title.replace(/[<>]/g, "");
    return `<!DOCTYPE html><html><head><title>${title}</title>${ld}</head><body><div id="content">` +
      pick("#content h1") + pick("#mainpic") + pick("#info") + pick(".rating_self") + pick("#link-report-intra") +
      "</div></body></html>";
  }

  async function contribute(urgent) {
    if (!pageTitle) return { ok: false, error: "页面上没读到片名" };
    return send("contributePage", { doubanId, html: pageExtract(), urgent });
  }

  if (settings.card === false) {
    // 只交资料，不显示卡片
    if (settings.connected) {
      const info = await send("media", { doubanId });
      if (info.ok && info.needs_data) contribute(false);
    }
    return;
  }

  const aside = document.querySelector("#content .aside") || document.querySelector(".aside");
  const card = h("section", { class: "moovie-card", "aria-label": "影牛" });
  if (aside) aside.prepend(card);
  else (document.querySelector("#info")?.parentElement || document.body).append(card);

  const header = () => h("div", { class: "moovie-card-head" },
    h("img", { src: chrome.runtime.getURL("icons/icon-32.png"), alt: "", width: 20, height: 20 }), h("b", {}, "影牛"));

  function render(...children) {
    card.replaceChildren(header(), ...children.filter(Boolean));
  }

  function siteLink(url, text, primary) {
    return h("a", { class: "moovie-btn" + (primary ? " moovie-primary" : ""), href: site(url), target: "_blank", rel: "noopener" }, text);
  }

  async function load() {
    render(h("p", { class: "moovie-dim" }, "正在读取…"));
    const info = await send("media", { doubanId });
    if (!info.ok) {
      if (info.needConnect) {
        render(
          h("p", {}, "连接影牛账号以后，这里会显示你在影牛的标记、看到第几集和相似推荐。"),
          h("button", { class: "moovie-btn moovie-primary", type: "button", onclick: () => send("open", { url: connectURL }) }, "连接影牛账号"),
        );
        return;
      }
      render(h("p", { class: "moovie-dim" }, info.error || "影牛暂时连不上"));
      return;
    }
    if (info.needs_data && settings.pageUpload !== false) {
      const uploaded = await contribute(!info.found || !info.complete);
      if (uploaded.ok && uploaded.applied && (!info.found || !info.complete)) return load(); // 刚收录或刚补全，重新读一遍卡片
    }
    if (!info.found) {
      render(
        h("p", {}, "影牛还没有收录这部作品。"),
        h("button", {
          class: "moovie-btn moovie-primary", type: "button",
          onclick: async (event) => {
            event.target.disabled = true;
            const result = await contribute(true);
            if (result.ok) load();
            else {
              event.target.disabled = false;
              event.target.textContent = result.error || "没收录成功，再试一次";
            }
          },
        }, "收录到影牛"),
      );
      return;
    }
    renderFound(info);
  }

  function renderFound(info) {
    const status = info.status || "";
    const markButton = (value, label, activeLabel) => h("button", {
      class: "moovie-btn" + (status === value ? " moovie-on" : ""), type: "button",
      onclick: async (event) => {
        event.target.disabled = true;
        const next = status === value ? "" : value;
        const result = await send("mark", { ...seed, status: next, rating: 0 });
        if (result.ok) {
          info.status = next;
          renderFound(info);
        } else {
          event.target.disabled = false;
          event.target.title = result.error || "";
        }
      },
    }, status === value ? activeLabel : label);

    const lines = [];
    if (info.progress) lines.push(h("p", { class: "moovie-progress" }, info.progress));
    else if (status) lines.push(h("p", { class: "moovie-progress" }, status === "watched" ? "你在影牛标记了看过" : "你在影牛标记了想看"));
    if (!info.complete) lines.push(h("p", { class: "moovie-dim" }, "影牛正在补全这部作品的资料。"));
    const lists = (info.collections || []).map((list) => h("li", {}, h("a", { href: site(list.url), target: "_blank", rel: "noopener" }, list.title)));
    const similar = (info.similar || []).map((item) => h("li", {}, h("a", { href: site(item.url), target: "_blank", rel: "noopener" }, item.title), item.year ? h("span", { class: "moovie-dim" }, ` ${item.year}`) : null));
    render(
      ...lines,
      h("div", { class: "moovie-actions" }, siteLink(info.play_url, "去影牛播放", true), siteLink(info.url, "影牛详情")),
      h("div", { class: "moovie-actions" }, markButton("wish", "想看", "已想看"), markButton("watched", "看过", "已看过")),
      info.rating_tmdb > 0 ? h("p", { class: "moovie-dim" }, `TMDB 评分 ${info.rating_tmdb.toFixed(1)}`) : null,
      lists.length ? h("div", { class: "moovie-block" }, h("h4", {}, "收录它的片单"), h("ul", {}, lists)) : null,
      similar.length ? h("div", { class: "moovie-block" }, h("h4", {}, "影牛的相似推荐"), h("ul", {}, similar)) : null,
    );
  }

  load();
})();
