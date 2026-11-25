// 影牛网站上的脚本：
//  - 告诉网页插件装了（<html data-moovie-ext="版本">），网页据此不再提醒你安装、显示插件状态；
//  - "连接插件"页面签发凭证后，把凭证交给插件后台；
//  - 作品页显示"资料获取中"时，让插件马上去豆瓣取，不用等服务器排队。
(() => {
  const version = chrome.runtime.getManifest().version;
  document.documentElement.dataset.moovieExt = version;

  function send(type, payload = {}) {
    return new Promise((resolve) => {
      try {
        chrome.runtime.sendMessage({ ...payload, type }, (response) => resolve(chrome.runtime.lastError || !response ? { ok: false, error: chrome.runtime.lastError?.message } : response));
      } catch {
        resolve({ ok: false, error: "插件刚更新过，刷新一下页面" });
      }
    });
  }

  function setStatus(text, ok) {
    const status = document.getElementById("ext-connect-status");
    if (!status) return;
    status.textContent = text;
    status.className = "flash " + (ok ? "ok" : "error");
    status.hidden = false;
  }

  async function connect() {
    const holder = document.getElementById("moovie-ext-token");
    const token = holder && holder.dataset.token;
    if (!token || holder.dataset.done) return;
    holder.dataset.done = "1";
    const result = await send("connect", { token });
    if (result.ok) {
      setStatus(`已连接：${result.me.username}。插件正在同步你的豆瓣标记，这个页面可以关掉了。`, true);
      document.documentElement.dataset.moovieExtConnected = "1";
    } else {
      setStatus(`没连接成功：${result.error || "请刷新页面重试"}`, false);
    }
  }

  const fetched = new Set();
  function fetchNow() {
    const banner = document.getElementById("fetching");
    const doubanId = banner && banner.dataset.douban;
    if (!doubanId || fetched.has(doubanId)) return;
    fetched.add(doubanId);
    send("fetchNow", { doubanId, mediaType: banner.dataset.type || "" });
  }

  function check() {
    connect();
    fetchNow();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", check);
  else check();
  // HTMX 换掉页面片段、站内跳转之后也看一眼
  new MutationObserver(() => check()).observe(document.documentElement, { childList: true, subtree: true });
})();
