// 插件弹窗：连接状态、同步状态和按钮、补全资料的进度、新版本提醒。同步进行中时每秒刷新一次。

const $ = (id) => document.getElementById(id);
const send = (type, payload = {}) => chrome.runtime.sendMessage({ ...payload, type });

function ago(at) {
  if (!at) return "";
  const minutes = Math.round((Date.now() - at) / 60000);
  if (minutes < 1) return "刚刚";
  if (minutes < 60) return `${minutes} 分钟前`;
  if (minutes < 60 * 24) return `${Math.round(minutes / 60)} 小时前`;
  return `${Math.round(minutes / 60 / 24)} 天前`;
}

function clock(at) {
  const date = new Date(at);
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

let timer = 0;

async function render() {
  const s = await send("status");
  if (!s || !s.ok) return;
  $("version").textContent = "v" + s.version;
  $("guide").href = s.site + "/extension";

  $("update").hidden = !s.update;
  if (s.update) {
    $("update-version").textContent = s.update.version;
    $("update-download").href = s.update.download;
    $("update-guide").href = s.update.page + "#update";
  }

  $("account-on").hidden = !s.connected;
  $("account-off").hidden = s.connected;
  $("account-lost").hidden = !s.lost;
  if (s.me) {
    $("username").textContent = s.me.username;
    $("contributions").textContent = s.me.contributions ? `影牛「${s.me.title}」· 已经补全了 ${s.me.contributions} 部作品的资料` : "";
    $("board").hidden = !s.me.contributions;
    $("board").href = s.site + "/contributors";
  }

  $("douban").textContent = s.douban ? `这个浏览器登录着豆瓣账号 ${s.douban}` : "这个浏览器还没有登录豆瓣。先在 douban.com 登录，再回来同步。";
  const sync = s.sync;
  let text = "";
  if (s.syncing && sync) text = `正在同步… 已处理 ${sync.processed} / ${sync.total} 条，新增 ${sync.imported} 条`;
  else if (sync && sync.status === "done") text = `上次同步：${ago(sync.finishedAt)}，处理 ${sync.processed} 条，新增 ${sync.imported} 条` + (sync.failed ? `，${sync.failed} 条没导入成功，下次会重试` : "");
  else if (sync && sync.status === "failed") text = `上次同步没完成：${sync.error}`;
  else if (s.connected) text = "还没有同步过。";
  if (s.ownPausedUntil) text += `（豆瓣暂时拒绝了请求，${clock(s.ownPausedUntil)} 以后再试）`;
  $("sync-status").textContent = text;
  const canSync = s.connected && s.douban && !s.syncing;
  $("sync").disabled = !canSync;
  $("sync-full").disabled = !canSync;

  const t = s.tasks;
  if (!s.settings.contribute) $("tasks").textContent = "已在设置里关闭。";
  else if (t.pausedUntil) $("tasks").textContent = `豆瓣暂时拒绝了请求，${clock(t.pausedUntil)} 以后再继续。今天补全了 ${t.count} 部。`;
  else $("tasks").textContent = `今天补全了 ${t.count} / ${t.limit} 部，累计 ${t.total || 0} 部。浏览器开着时每隔几分钟做一点，请求很慢，不影响你上网。`;

  $("write-back").hidden = !(s.settings.writeBack && s.writeBack && s.writeBack.error);
  if (s.writeBack && s.writeBack.error) $("write-back").textContent = `写回豆瓣失败：${s.writeBack.error}`;

  clearTimeout(timer);
  if (s.syncing) timer = setTimeout(render, 1000);
}

$("connect").addEventListener("click", async () => {
  const s = await send("status");
  chrome.tabs.create({ url: s.site + "/extension/connect" });
});
$("sync").addEventListener("click", async () => {
  await send("sync", { full: false });
  setTimeout(render, 300);
});
$("sync-full").addEventListener("click", async () => {
  await send("sync", { full: true });
  setTimeout(render, 300);
});

render();
send("refreshMe").then(render);
