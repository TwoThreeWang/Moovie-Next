// 同步豆瓣标记：插件用这个浏览器的豆瓣登录状态一页一页读你的"看过""想看"，原样交给影牛导入。
// 和服务器那边的同步规则一样：影牛上已有的标记不会被覆盖，只有"想看"会升级成"看过"。
//
// 增量同步：豆瓣的标记按时间倒序，读到一整页都已经导入过就停。第一次同步、换了豆瓣账号、
// 或者上一次有没导入成功的，就完整地读一遍。

import { api } from "./api.js";
import { doubanAccount, fetchInterests, NotLoggedIn } from "./douban.js";
import { getState, setState } from "./settings.js";

const PAGE = 50;
let running = null;

export function syncRunning() {
  return running !== null;
}

// syncDouban 同步一次。已经在同步时返回正在进行的那一次。
export function syncDouban(options = {}) {
  if (!running) {
    running = doSync(options).finally(() => {
      running = null;
    });
  }
  return running;
}

async function doSync({ full = false, reason = "manual" } = {}) {
  const account = await doubanAccount();
  if (!account) throw new NotLoggedIn();
  const me = await api("/api/ext/me");
  if (me.douban_user_id !== account.uid) {
    await api("/api/ext/douban/bind", { method: "POST", body: { douban_user_id: account.uid } });
    full = true;
  }
  if (!me.synced_at || (await getState("syncFailed", 0)) > 0) full = true;

  const progress = { status: "running", reason, full, total: 0, processed: 0, imported: 0, failed: 0, startedAt: Date.now() };
  await setState("sync", progress);
  try {
    for (const status of ["done", "mark"]) {
      for (let start = 0; start < 20000; start += PAGE) {
        const response = await fetchInterests(account, status, start, PAGE);
        const result = await api("/api/ext/douban/interests", { method: "POST", body: { status, response } });
        if (start === 0) progress.total += result.total;
        progress.processed += result.processed;
        progress.imported += result.imported;
        progress.failed += result.failed;
        await setState("sync", progress);
        const count = (response.interests || []).length;
        if (count < PAGE || start + PAGE >= result.total || (!full && result.changed === 0)) break;
      }
    }
    await api("/api/ext/douban/synced", { method: "POST", body: { failed: progress.failed } });
    await setState("syncFailed", progress.failed);
    Object.assign(progress, { status: "done", finishedAt: Date.now() });
    await setState("sync", progress);
    await setState("lastSyncAt", Date.now());
    return progress;
  } catch (error) {
    Object.assign(progress, { status: "failed", error: error.message, finishedAt: Date.now() });
    await setState("sync", progress);
    throw error;
  }
}

// backupDouban 读出你在豆瓣上的全部影视标记（看过、在看、想看，含评分、短评和标记时间），给设置页导出成文件。
export async function backupDouban(onProgress) {
  const account = await doubanAccount();
  if (!account) throw new NotLoggedIn();
  const entries = [];
  for (const [status, label] of [["done", "看过"], ["doing", "在看"], ["mark", "想看"]]) {
    for (let start = 0; start < 20000; start += PAGE) {
      const response = await fetchInterests(account, status, start, PAGE);
      const list = response.interests || [];
      for (const item of list) {
        const subject = item.subject || {};
        entries.push({
          status: label,
          douban_id: String(subject.id || ""),
          title: subject.title || "",
          year: subject.year || "",
          type: subject.type || "",
          url: subject.url || (subject.id ? `https://movie.douban.com/subject/${subject.id}/` : ""),
          rating: item.rating ? Math.round(item.rating.value) : "",
          comment: item.comment || "",
          tags: (item.tags || []).join(" "),
          marked_at: item.create_time || "",
        });
      }
      if (onProgress) await onProgress({ status: label, count: entries.length });
      if (list.length < PAGE || start + PAGE >= (response.total || 0)) break;
    }
  }
  return { account: account.uid, exportedAt: new Date().toISOString(), entries };
}
