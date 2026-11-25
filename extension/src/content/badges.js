// 豆瓣的榜单、搜索结果、豆列、别人的收藏页上，在海报左上角标出你在影牛的想看、看过、看到第几集。
// 页面是动态加载的（比如"选电影"往下翻），新出现的海报过一会儿也会标上。
(async () => {
  const { h, send, settings: loadSettings, subjectID } = globalThis.Moovie;
  const settings = await loadSettings();
  if (settings.badges === false || !settings.connected) return;

  const known = new Map(); // 豆瓣 ID → 标记（null 表示查过、没有标记）
  let timer = 0;

  function label(mark) {
    if (mark.status === "watched") return "看过";
    if (mark.watching) return mark.episode > 0 ? `看到第 ${mark.episode} 集` : "在看";
    if (mark.status === "wish") return "想看";
    return "";
  }

  function decorate(anchor, mark) {
    if (anchor.querySelector(".moovie-badge")) return;
    const text = label(mark);
    if (!text) return;
    if (getComputedStyle(anchor).position === "static") anchor.style.position = "relative";
    anchor.append(h("span", { class: "moovie-badge moovie-badge-" + (mark.status || "watching"), title: "你在影牛的标记" }, text));
  }

  // posters 找出页面上带图片的条目链接（海报），不管主海报（#mainpic 链到的是图片页，subjectID 本来也认不出）。
  function posters() {
    const found = [];
    for (const anchor of document.querySelectorAll('a[href*="/subject/"]')) {
      const img = anchor.querySelector("img");
      // 只认海报：片名旁边那种 16px 的小图标不算
      if (anchor.dataset.moovie || !img || Math.max(img.width, img.getBoundingClientRect().width) < 40) continue;
      const id = subjectID(anchor.getAttribute("href"));
      if (id) found.push([anchor, id]);
    }
    return found;
  }

  async function scan() {
    const items = posters();
    const missing = [...new Set(items.map(([, id]) => id).filter((id) => !known.has(id)))];
    for (let i = 0; i < missing.length; i += 300) {
      const batch = missing.slice(i, i + 300);
      const result = await send("marks", { ids: batch });
      if (!result.ok) return;
      for (const id of batch) known.set(id, (result.marks || {})[id] || null);
    }
    for (const [anchor, id] of items) {
      anchor.dataset.moovie = "1";
      const mark = known.get(id);
      if (mark) decorate(anchor, mark);
    }
  }

  scan();
  new MutationObserver(() => {
    clearTimeout(timer);
    timer = setTimeout(scan, 800);
  }).observe(document.body, { childList: true, subtree: true });
})();
