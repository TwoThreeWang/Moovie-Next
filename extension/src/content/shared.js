// 豆瓣页面上几个脚本共用的小工具。几个脚本在同一个隔离环境里运行，这个文件最先加载。
(() => {
  if (globalThis.Moovie) return;

  // send 给插件后台发消息，返回 { ok, ... }。插件刚更新、页面还没刷新时后台连不上，当作失败处理。
  function send(type, payload = {}) {
    return new Promise((resolve) => {
      try {
        chrome.runtime.sendMessage({ ...payload, type }, (response) => {
          if (chrome.runtime.lastError || !response) resolve({ ok: false, error: "插件刚更新过，刷新一下页面" });
          else resolve(response);
        });
      } catch {
        resolve({ ok: false, error: "插件刚更新过，刷新一下页面" });
      }
    });
  }

  async function settings() {
    try {
      const { settings, token } = await chrome.storage.local.get(["settings", "token"]);
      return { connected: Boolean(token), ...(settings || {}) };
    } catch {
      return { connected: false };
    }
  }

  // h 创建元素：h("a", { href, class: "x", onclick }, "文字", child)
  function h(tag, attrs = {}, ...children) {
    const element = document.createElement(tag);
    for (const [key, value] of Object.entries(attrs)) {
      if (value === undefined || value === null || value === false) continue;
      if (key.startsWith("on")) element.addEventListener(key.slice(2), value);
      else if (key === "class") element.className = value;
      else element.setAttribute(key, value === true ? "" : value);
    }
    for (const child of children.flat()) {
      if (child === undefined || child === null || child === false) continue;
      element.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return element;
  }

  // subjectID 从链接里取出豆瓣条目 ID（只认影视条目：movie.douban.com/subject/<id>，或 movie.douban.com 站内的 /subject/<id>）。
  function subjectID(href) {
    if (!href) return "";
    let url;
    try {
      url = new URL(href, location.href);
    } catch {
      return "";
    }
    if (url.hostname !== "movie.douban.com") return "";
    const match = /^\/subject\/(\d{7,9})\/?$/.exec(url.pathname);
    return match ? match[1] : "";
  }

  globalThis.Moovie = { send, settings, h, subjectID };
})();
