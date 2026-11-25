// 豆列页面（www.douban.com/doulist/<id>/）：标题旁加一个"导入到影牛片单"按钮。
// 把豆列里的影视条目（最多 200 部，影牛片单的上限）读出来，在影牛建一个私密片单，打开编辑页给你检查。
(async () => {
  const match = /^\/doulist\/(\d+)\/?$/.exec(location.pathname);
  if (!match) return;
  const { h, send, subjectID } = globalThis.Moovie;
  const heading = document.querySelector("#content h1");
  if (!heading) return;

  const LIMIT = 200;
  const PAGE = 25;
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  // parse 从一页豆列里取出影视条目：豆瓣 ID、片名、年份、海报。
  function parse(root) {
    const items = [];
    for (const item of root.querySelectorAll(".doulist-item")) {
      const link = item.querySelector(".title a");
      const id = subjectID(link?.getAttribute("href"));
      if (!id) continue; // 书、音乐、小组话题这些不是影视条目
      const year = /年份[:：]\s*(\d{4})/.exec(item.querySelector(".abstract")?.textContent || "");
      items.push({
        douban_id: id,
        title: (link.textContent || "").replace(/\s+/g, " ").trim(),
        year: year ? parseInt(year[1], 10) : 0,
        poster: item.querySelector(".post img")?.getAttribute("src") || "",
      });
    }
    return items;
  }

  const status = h("span", { class: "moovie-doulist-status" });
  const button = h("button", { class: "moovie-btn moovie-primary moovie-doulist-btn", type: "button", onclick: run }, "导入到影牛片单");
  heading.append(button, status);

  async function run() {
    button.disabled = true;
    const pages = parseInt(document.querySelector(".paginator .thispage")?.dataset.totalPage || "1", 10) || 1;
    const seen = new Set();
    const items = [];
    const add = (list) => {
      for (const item of list) {
        if (items.length < LIMIT && !seen.has(item.douban_id)) {
          seen.add(item.douban_id);
          items.push(item);
        }
      }
    };
    // 从第一页开始读，不管你现在停在第几页
    for (let page = 0; page < pages && items.length < LIMIT; page++) {
      status.textContent = ` 正在读第 ${page + 1} / ${pages} 页…`;
      const url = `${location.origin}${location.pathname}?start=${page * PAGE}&sort=seq&playable=0&sub_type=`;
      try {
        const response = await fetch(url, { credentials: "include" });
        if (!response.ok) throw new Error(String(response.status));
        add(parse(new DOMParser().parseFromString(await response.text(), "text/html")));
      } catch {
        status.textContent = ` 第 ${page + 1} 页没读出来，先导入已经读到的 ${items.length} 部`;
        break;
      }
      if (page + 1 < pages) await sleep(1200 + Math.random() * 800);
    }
    if (!items.length) {
      status.textContent = " 这个豆列里没有影视条目";
      button.disabled = false;
      return;
    }
    status.textContent = ` 读到 ${items.length} 部，正在导入…`;
    const about = (document.querySelector(".doulist-about")?.textContent || "").replace(/\s+/g, " ").trim().slice(0, 800);
    const title = (heading.querySelector("span")?.textContent || heading.firstChild?.textContent || "").trim();
    const description = [about, `从豆列导入：${location.origin}${location.pathname}`].filter(Boolean).join("\n\n");
    const result = await send("importDoulist", { title, description, items });
    if (!result.ok) {
      status.textContent = result.needConnect ? " 请先在插件里连接影牛账号" : ` 导入失败：${result.error}`;
      button.disabled = false;
      return;
    }
    status.textContent = ` 已导入 ${result.count} 部，片单是私密的，可以在编辑页检查后再投稿`;
    send("open", { url: result.edit_url });
  }
})();
