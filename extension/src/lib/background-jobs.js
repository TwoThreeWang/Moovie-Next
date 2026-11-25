// 几件定期做的小事：检查新版本、追剧更新提醒、把影牛上的标记写回豆瓣（实验功能）。

import { api, resolve } from "./api.js";
import { doubanAccount, writeInterest, Throttled } from "./douban.js";
import { compareVersions, getSettings, getState, setState } from "./settings.js";

// ---------- 新版本 ----------

// checkUpdate 问影牛最新的插件版本。有新版本时图标上显示"新"，每个新版本提醒一次。
export async function checkUpdate() {
  const reply = await api("/api/ext/version", { token: "" });
  const info = { ...reply, page: resolve(reply.page), download: resolve(reply.download) };
  const current = chrome.runtime.getManifest().version;
  const newer = compareVersions(info.version, current) > 0;
  await setState("update", newer ? { ...info, checkedAt: Date.now() } : null);
  await setState("updateCheckedAt", Date.now());
  if (newer && (await getState("updateNotified", "")) !== info.version) {
    await setState("updateNotified", info.version);
    await notify(`update-${info.version}`, "影牛插件有新版本", `${info.version} 已经发布（你在用 ${current}），点这里看怎么更新。`, info.page);
  }
  await refreshBadge();
  return { current, ...info, newer };
}

// refreshBadge 更新图标上的小字：有新版本显示"新"，连接失效显示"!"。
export async function refreshBadge() {
  const update = await getState("update", null);
  const token = await getState("token", "");
  const lost = await getState("connectionLost", false);
  let text = "";
  let color = "#e0a030";
  if (update) text = "新";
  else if (!token || lost) {
    text = "!";
    color = "#d04040";
  }
  await chrome.action.setBadgeText({ text });
  if (text) await chrome.action.setBadgeBackgroundColor({ color });
}

// ---------- 通知 ----------

export async function notify(id, title, message, url) {
  const links = await getState("notifyLinks", {});
  if (url) {
    links[id] = url;
    const keys = Object.keys(links);
    for (const key of keys.slice(0, Math.max(0, keys.length - 50))) delete links[key];
    await setState("notifyLinks", links);
  }
  await chrome.notifications.create(id, { type: "basic", iconUrl: "icons/icon-128.png", title, message, priority: 0 });
}

// ---------- 追剧更新 ----------

// checkAiring 和上次看到的比，追的剧最新一集变大了就提醒。第一次只记下来，不提醒。
export async function checkAiring() {
  const settings = await getSettings();
  if (!settings.airing || !(await getState("token", ""))) return;
  const { items } = await api("/api/ext/airing");
  const seen = await getState("airingSeen", null);
  const next = {};
  for (const item of items) {
    next[item.media_id] = item.latest;
    const before = seen ? seen[item.media_id] : undefined;
    if (before !== undefined && item.latest > before) {
      await notify(`airing-${item.media_id}-${item.latest}`, `《${item.title}》更新了`, `已经更新到第 ${item.latest} ${item.unit}`, resolve(item.url));
    }
  }
  await setState("airingSeen", next);
  await setState("airingCheckedAt", Date.now());
}

// ---------- 写回豆瓣 ----------

// runWriteBack 把你在影牛上新做的标记写到豆瓣。第一次打开这个功能时只记下位置，以前的标记不写回去。
// 只写"想看"和"看过"（带评分），不写短评，也不会在豆瓣上删除标记。
export async function runWriteBack() {
  const settings = await getSettings();
  if (!settings.writeBack || !(await getState("token", ""))) return;
  const account = await doubanAccount();
  if (!account) return;
  const cursor = await getState("outboxCursor", 0);
  const data = await api(`/api/ext/outbox?after=${cursor}`);
  if (!cursor) {
    await setState("outboxCursor", Math.max(data.cursor, 1));
    return;
  }
  const log = await getState("writeBack", { written: 0, error: "", at: 0 });
  for (const item of data.items) {
    try {
      await writeInterest(account, item.douban_id, item.status, item.rating);
      log.written++;
      log.error = "";
    } catch (error) {
      log.error = error.message;
      log.at = Date.now();
      await setState("writeBack", log);
      if (error instanceof Throttled) return;
      return; // 写失败多半是豆瓣接口变了或者登录过期，停下来，下次从这一条接着试
    }
    await setState("outboxCursor", item.id);
  }
  log.at = Date.now();
  await setState("writeBack", log);
}
