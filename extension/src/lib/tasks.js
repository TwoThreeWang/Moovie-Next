// 补全作品资料：向影牛领一批还没有豆瓣资料的作品（先是你自己标记的、片单里的，再是影牛排着的），
// 不带 Cookie 去豆瓣取详情，原样交回影牛。每次请求间隔 7 秒以上，每天最多 DAILY_TASK_LIMIT 部，
// 豆瓣一拒绝就停。影牛拿到后会取消它自己那边排着的任务，服务器就不用再去请求豆瓣。

import { api, ApiError } from "./api.js";
import { fetchSubject, NotFound, Throttled, pausedUntil } from "./douban.js";
import { DAILY_TASK_LIMIT, getSettings, getState, setState, today } from "./settings.js";

// 一批最多做多久。后台每 3 分钟醒来一次，一批不超过 4 分钟，不会和下一批叠在一起太久。
const BATCH_MS = 4 * 60 * 1000;
let running = false;

export async function taskStats() {
  const stats = await getState("tasks", { day: today(), count: 0, total: 0 });
  if (stats.day !== today()) Object.assign(stats, { day: today(), count: 0 });
  return stats;
}

async function counted() {
  const stats = await taskStats();
  stats.count++;
  stats.total = (stats.total || 0) + 1;
  await setState("tasks", stats);
}

// uploadSubject 去豆瓣取一部作品并交给影牛。urgent 表示有人正等着看这部（影牛页面上显示"资料获取中"）。
export async function uploadSubject(doubanId, type, urgent = false) {
  const { kind, response } = await fetchSubject(doubanId, type);
  const result = await api("/api/ext/douban/subject", { method: "POST", body: { douban_id: doubanId, kind, response, urgent } });
  await counted();
  return result;
}

// runTasks 做一批。没连接账号、关掉了这个功能、今天做够了、豆瓣让暂停时什么都不做。
export async function runTasks() {
  if (running) return;
  const settings = await getSettings();
  if (!settings.contribute || !(await getState("token", ""))) return;
  if (await pausedUntil("anon")) return;
  if ((await taskStats()).count >= DAILY_TASK_LIMIT) return;
  running = true;
  const started = Date.now();
  try {
    const { tasks } = await api(`/api/ext/tasks?limit=20&help=${settings.helpOthers ? 1 : 0}`);
    for (const task of tasks) {
      if (Date.now() - started > BATCH_MS || (await taskStats()).count >= DAILY_TASK_LIMIT) break;
      try {
        await uploadSubject(task.douban_id, task.type);
      } catch (error) {
        if (error instanceof Throttled) break;
        if (error instanceof NotFound) continue;
        if (error instanceof ApiError && (error.status === 401 || error.status === 429 || error.status === 0)) break;
        // 单个条目出错（豆瓣拒绝了这一个、影牛觉得资料对不上）就跳过，服务器到时间会自己做
      }
    }
  } catch {
    // 领任务失败（网络、影牛维护中），下一批再试
  } finally {
    running = false;
  }
}
