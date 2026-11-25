// 插件的后台：处理弹窗、设置页和网页上脚本发来的消息，按时做同步、补全资料、检查更新这些事。

import { api, ApiError, resolve, setToken, siteURL } from "./lib/api.js";
import { doubanAccount, installHeaderRules, NotLoggedIn, pausedUntil } from "./lib/douban.js";
import { backupDouban, syncDouban, syncRunning } from "./lib/sync.js";
import { runTasks, taskStats, uploadSubject } from "./lib/tasks.js";
import { checkAiring, checkUpdate, refreshBadge, runWriteBack } from "./lib/background-jobs.js";
import { DAILY_TASK_LIMIT, getSettings, getState, setSettings, setState } from "./lib/settings.js";

const HOUR = 60 * 60 * 1000;

installHeaderRules();
setState("siteURL", siteURL()); // 给豆瓣页面上的脚本用：它们读不到 config.js

function scheduleAlarms() {
  chrome.alarms.create("tasks", { periodInMinutes: 3, delayInMinutes: 1 });
  chrome.alarms.create("tick", { periodInMinutes: 10, delayInMinutes: 0.5 });
}

chrome.runtime.onInstalled.addListener(async (details) => {
  scheduleAlarms();
  await setState("update", null);
  await refreshBadge();
  if (details.reason === "install") chrome.tabs.create({ url: siteURL("/extension/connect") });
});
chrome.runtime.onStartup.addListener(scheduleAlarms);

chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name === "tasks") {
    await runTasks();
    return;
  }
  if (alarm.name !== "tick") return;
  await quietly(tick());
});

// tick 每 10 分钟一次：到时间了就检查新版本（12 小时）、自动同步（20 小时）、追剧更新（3 小时），每次都看看要不要写回豆瓣。
async function tick() {
  const now = Date.now();
  if (now - (await getState("updateCheckedAt", 0)) > 12 * HOUR) await quietly(checkUpdate());
  if (!(await getState("token", ""))) return;
  const settings = await getSettings();
  if (settings.autoSync && !syncRunning() && now - (await getState("lastSyncAt", 0)) > 20 * HOUR && (await doubanAccount())) {
    await quietly(syncDouban({ reason: "auto" }));
  }
  if (now - (await getState("airingCheckedAt", 0)) > 3 * HOUR) await quietly(checkAiring());
  await quietly(runWriteBack());
}

async function quietly(promise) {
  try {
    return await promise;
  } catch {
    return undefined;
  }
}

chrome.notifications.onClicked.addListener(async (id) => {
  const links = await getState("notifyLinks", {});
  if (links[id]) chrome.tabs.create({ url: links[id] });
  chrome.notifications.clear(id);
});

// ---------- 消息 ----------

const handlers = {
  // 弹窗显示用的全部状态
  async status() {
    const [token, me, account, settings, sync, update, tasks, writeBack, lost] = await Promise.all([
      getState("token", ""), getState("me", null), doubanAccount(), getSettings(), getState("sync", null),
      getState("update", null), taskStats(), getState("writeBack", null), getState("connectionLost", false),
    ]);
    return {
      connected: Boolean(token) && !lost, lost, me, douban: account ? account.uid : "", settings, sync, syncing: syncRunning(), update, writeBack,
      tasks: { ...tasks, limit: DAILY_TASK_LIMIT, pausedUntil: await pausedUntil("anon") },
      ownPausedUntil: await pausedUntil("own"),
      version: chrome.runtime.getManifest().version, site: siteURL(),
    };
  },

  // 影牛"连接插件"页面交来的凭证
  async connect({ token }) {
    const me = await api("/api/ext/me", { token });
    const previous = await getState("token", "");
    if (previous && previous !== token) await quietly(api("/api/ext/disconnect", { method: "POST", body: {}, token: previous }));
    await setToken(token);
    await setState("me", me);
    await setState("connectionLost", false);
    await refreshBadge();
    quietly(syncDouban({ reason: "connect" }));
    return { me };
  },

  async disconnect() {
    await quietly(api("/api/ext/disconnect", { method: "POST", body: {} }));
    await setToken("");
    await setState("me", null);
    await refreshBadge();
    return {};
  },

  async refreshMe() {
    const me = await api("/api/ext/me");
    await setState("me", me);
    return { me };
  },

  async sync({ full }) {
    // 不等同步做完：弹窗会轮询 status 显示进度
    syncDouban({ full: Boolean(full), reason: "manual" }).catch(() => {});
    return {};
  },

  async settings({ patch }) {
    const before = await getSettings();
    const settings = await setSettings(patch || {});
    if (settings.writeBack && !before.writeBack) await setState("outboxCursor", 0); // 从现在开始写回，以前的不写
    return { settings };
  },

  async checkUpdate() {
    return checkUpdate();
  },

  async backup() {
    return backupDouban((progress) => setState("backupProgress", progress));
  },

  // 豆瓣电影页上的影牛卡片
  async media({ doubanId }) {
    return api(`/api/ext/media?douban_id=${encodeURIComponent(doubanId)}`);
  },

  async mark(body) {
    return api("/api/ext/mark", { method: "POST", body });
  },

  async marks({ ids }) {
    return api("/api/ext/marks", { method: "POST", body: { ids } });
  },

  // 豆瓣电影页上摘出的资料
  async contributePage({ doubanId, html, urgent }) {
    return api("/api/ext/douban/page", { method: "POST", body: { douban_id: doubanId, html, urgent: Boolean(urgent) } });
  },

  // 影牛作品页显示"资料获取中"时，让插件马上去豆瓣取
  async fetchNow({ doubanId, mediaType }) {
    const settings = await getSettings();
    if (!settings.contribute) return { skipped: true };
    return uploadSubject(doubanId, mediaType, true);
  },

  async importDoulist(body) {
    return api("/api/ext/collections", { method: "POST", body });
  },

  async open({ url }) {
    await chrome.tabs.create({ url: resolve(url) });
    return {};
  },
};

// openable：网页上的脚本只能让后台打开影牛网站（含站内路径）和豆瓣的页面。
function openable(url) {
  try {
    const target = new URL(String(url), siteURL());
    return target.origin === new URL(siteURL()).origin || target.origin === "https://www.douban.com";
  } catch {
    return false;
  }
}

chrome.runtime.onMessage.addListener((message, sender, respond) => {
  const handler = Object.hasOwn(handlers, message?.type) ? handlers[message.type] : null;
  if (!handler) return false;
  // 网页上的脚本只能用几样，不能改设置、断开连接、导出数据
  const fromPage = sender.tab && !sender.url?.startsWith(chrome.runtime.getURL(""));
  if (fromPage && !["connect", "media", "mark", "marks", "contributePage", "fetchNow", "importDoulist", "open"].includes(message.type)) return false;
  if (message.type === "open" && !openable(message.url)) return false;
  handler(message)
    .then((result) => respond({ ok: true, ...result }))
    .catch((error) => respond({
      ok: false, error: error.message,
      needConnect: error instanceof ApiError && error.status === 401,
      needDoubanLogin: error instanceof NotLoggedIn,
    }));
  return true;
});
