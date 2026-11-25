// 插件的设置和本地状态，都存在 chrome.storage.local 里，只在这台电脑上。

export const DEFAULTS = {
  autoSync: true, // 每天自动增量同步一次豆瓣标记
  card: true, // 豆瓣电影页显示影牛卡片
  badges: true, // 豆瓣列表页的海报角标
  pageUpload: true, // 打开的豆瓣电影页资料过期时，顺手交给影牛
  contribute: true, // 空闲时补全自己标记过的作品资料
  helpOthers: true, // 自己的补完了，再帮影牛补全别人在等的作品
  airing: true, // 追的剧更新时提醒
  writeBack: false, // 实验：把在影牛上的标记写回豆瓣
};

// 每天最多替影牛去豆瓣取多少部作品的详情。豆瓣按 IP 限流，取得太多会让你自己打开豆瓣时也要输验证码。
export const DAILY_TASK_LIMIT = 300;

export async function getSettings() {
  const { settings } = await chrome.storage.local.get("settings");
  return { ...DEFAULTS, ...(settings || {}) };
}

export async function setSettings(patch) {
  const settings = { ...(await getSettings()), ...patch };
  await chrome.storage.local.set({ settings });
  return settings;
}

export async function getState(key, fallback) {
  const value = (await chrome.storage.local.get(key))[key];
  return value === undefined ? fallback : value;
}

export async function setState(key, value) {
  await chrome.storage.local.set({ [key]: value });
}

// today 是本地日期，用来按天计数。
export function today() {
  const now = new Date();
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
}

// compareVersions 比较 1.2.10 这样的版本号：a 新返回正数，一样返回 0。
export function compareVersions(a, b) {
  const left = String(a).split(".").map((part) => parseInt(part, 10) || 0);
  const right = String(b).split(".").map((part) => parseInt(part, 10) || 0);
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const diff = (left[i] || 0) - (right[i] || 0);
    if (diff !== 0) return diff;
  }
  return 0;
}
