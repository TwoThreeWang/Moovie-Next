// 在你的浏览器里请求豆瓣。三条通道各有各的节奏，互不影响：
//   own   用你的登录状态读你自己的标记（同步、备份），1.5 秒左右一页，像人往下翻
//   anon  不带 Cookie 取作品详情（补全资料），7 秒以上一次，不算在你的豆瓣账号头上
//   write 把影牛上的标记写回豆瓣（实验功能），3 秒以上一次
// 豆瓣按 IP 限流。一旦看到拒绝（403/418/429、跳到验证页、该是数据的地方给了网页），这条通道就停下来：
// 宁可少做，也不能让你自己打开豆瓣时要输验证码。插件不会去碰验证码。

import { getState, setState } from "./settings.js";

const MOBILE = "https://m.douban.com";
const MOVIE = "https://movie.douban.com";

export class Throttled extends Error {
  constructor(channel, until) {
    super(`豆瓣暂时拒绝了请求，${formatTime(until)} 以后再试`);
    this.channel = channel;
    this.until = until;
  }
}

export class NotLoggedIn extends Error {
  constructor() {
    super("这个浏览器还没有登录豆瓣，请先在 douban.com 登录");
  }
}

export class NotFound extends Error {}

function formatTime(at) {
  const date = new Date(at);
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// 每条通道的基准间隔、被拒绝后暂停多久，以及"上一次成功之后被拒绝了几个不同的对象就算限流"。
// 有的条目豆瓣对谁都返回 403，单个对象的拒绝不能让整条通道停下（和服务器的判断一样，见 internal/providers/douban.go）。
const CHANNELS = {
  own: { base: 1500, pause: 30 * 60 * 1000, refusals: 2 },
  anon: { base: 7000, pause: 6 * 60 * 60 * 1000, refusals: 3 },
  write: { base: 3000, pause: 60 * 60 * 1000, refusals: 2 },
};
const state = { own: { next: 0, refused: new Set() }, anon: { next: 0, refused: new Set() }, write: { next: 0, refused: new Set() } };

export async function pausedUntil(channel) {
  const pauses = await getState("doubanPause", {});
  return pauses[channel] && pauses[channel] > Date.now() ? pauses[channel] : 0;
}

async function pause(channel) {
  const pauses = await getState("doubanPause", {});
  pauses[channel] = Date.now() + CHANNELS[channel].pause;
  await setState("doubanPause", pauses);
  state[channel].refused.clear();
  return pauses[channel];
}

// pace 等到这条通道可以发下一次请求。间隔带 0~50% 的随机抖动，偶尔多歇一会儿：分毫不差的间隔一看就是程序。
async function pace(channel) {
  const until = await pausedUntil(channel);
  if (until) throw new Throttled(channel, until);
  const base = CHANNELS[channel].base;
  let gap = base * (1 + Math.random() * 0.5);
  if (Math.random() < 1 / 30) gap += base * (3 + Math.random() * 5);
  const now = Date.now();
  const wait = Math.max(0, state[channel].next - now);
  state[channel].next = now + wait + gap;
  if (wait) await sleep(wait);
}

function looksRefused(response, expectJSON) {
  if ([403, 418, 429].includes(response.status)) return true;
  const url = response.url || "";
  if (url.includes("sec.douban.com") || url.includes("/misc/sorry")) return true;
  const type = response.headers.get("content-type") || "";
  return expectJSON && response.ok && type.includes("text/html");
}

// refused 记一次拒绝。同一条通道接连有几个不同的对象被拒绝，就暂停这条通道并抛 Throttled。
async function refused(channel, object) {
  state[channel].refused.add(object);
  if (state[channel].refused.size >= CHANNELS[channel].refusals) {
    throw new Throttled(channel, await pause(channel));
  }
}

function succeeded(channel) {
  state[channel].refused.clear();
}

// installHeaderRules 让插件自己发出的豆瓣请求带上 Referer（豆瓣的手机版接口没有 Referer 会拒绝）。
// 只改插件后台发的请求（tabIds -1），你在网页上浏览豆瓣时的请求不受影响。会话规则，每次后台启动时重新装一遍。
export async function installHeaderRules() {
  const rule = (id, domain, headers) => ({
    id, priority: 1,
    action: { type: "modifyHeaders", requestHeaders: headers.map(([header, value]) => ({ header, operation: "set", value })) },
    condition: { requestDomains: [domain], tabIds: [-1] },
  });
  const rules = [
    rule(9101, "m.douban.com", [["referer", "https://m.douban.com/movie/"]]),
    rule(9102, "movie.douban.com", [["referer", "https://movie.douban.com/"], ["origin", "https://movie.douban.com"]]),
    rule(9103, "www.douban.com", [["referer", "https://www.douban.com/"]]),
  ];
  await chrome.declarativeNetRequest.updateSessionRules({ removeRuleIds: rules.map((r) => r.id), addRules: rules });
}

// doubanAccount 返回这个浏览器登录着的豆瓣账号：数字 ID 和 ck（豆瓣防跨站请求用的令牌）。没登录时返回 null。
export async function doubanAccount() {
  const login = await chrome.cookies.get({ url: "https://www.douban.com/", name: "dbcl2" });
  const match = login && /(\d+):/.exec(login.value);
  if (!match) return null;
  const ck = await chrome.cookies.get({ url: "https://www.douban.com/", name: "ck" });
  return { uid: match[1], ck: ck ? ck.value.replace(/"/g, "") : "" };
}

// fetchInterests 读你自己的一页标记（用登录状态，不公开的也能读）。status：done 看过、mark 想看、doing 在看。
// 返回豆瓣接口的原始回答，原样交给影牛服务器解析。
export async function fetchInterests(account, status, start, count = 50) {
  await pace("own");
  const url = `${MOBILE}/rexxar/api/v2/user/${encodeURIComponent(account.uid)}/interests?type=movie&status=${status}` +
    `&start=${start}&count=${count}&ck=${encodeURIComponent(account.ck)}&for_mobile=1`;
  const response = await fetch(url, { credentials: "include", cache: "no-store" });
  if (response.url.includes("accounts.douban.com") || response.status === 401) throw new NotLoggedIn();
  if (looksRefused(response, true)) {
    await refused("own", `${status}:${start}`);
    throw new Error("豆瓣这一页没有回应，稍后再试");
  }
  if (!response.ok) throw new Error(`豆瓣返回 ${response.status}`);
  const body = await response.json();
  succeeded("own");
  return body;
}

// fetchSubject 不带 Cookie 取一部作品的详情。豆瓣按电影和剧集分了两个接口，先试 type 对应的那个。
// 返回 { kind, response }；两个接口都说没有时抛 NotFound。
export async function fetchSubject(id, type) {
  const kinds = type === "movie" ? ["movie", "tv"] : ["tv", "movie"];
  let missing = 0;
  for (const kind of kinds) {
    await pace("anon");
    const response = await fetch(`${MOBILE}/rexxar/api/v2/${kind}/${encodeURIComponent(id)}?ck=&for_mobile=1`, { credentials: "omit", cache: "no-store" });
    if (response.status === 404) {
      missing++;
      continue;
    }
    if (looksRefused(response, true)) {
      await refused("anon", id);
      throw new Error("豆瓣拒绝了这个条目");
    }
    if (!response.ok) throw new Error(`豆瓣返回 ${response.status}`);
    const body = await response.json();
    succeeded("anon");
    if (!body.id || !body.title) {
      missing++;
      continue;
    }
    return { kind, response: body };
  }
  if (missing === kinds.length) throw new NotFound("豆瓣上没有这个条目");
  throw new Error("没能取到这个条目");
}

// writeInterest 把一条标记写到你的豆瓣账号上（实验功能）：wish 想看、watched 看过（带评分）。
// 走的是豆瓣网页上"想看 / 看过"按钮用的接口；豆瓣改了接口就会失败，失败会显示在插件的弹窗里。
export async function writeInterest(account, id, status, rating) {
  await pace("write");
  if (!account.ck) throw new Error("没有读到豆瓣的 ck，请在 douban.com 重新登录一次");
  const form = new URLSearchParams({
    ck: account.ck, interest: status === "watched" ? "collect" : "wish",
    rating: status === "watched" && rating > 0 ? String(rating) : "", foldcollect: "F", tags: "", comment: "",
  });
  const response = await fetch(`${MOVIE}/j/subject/${encodeURIComponent(id)}/interest`, {
    method: "POST", credentials: "include", body: form,
    headers: { "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8", "X-Requested-With": "XMLHttpRequest" },
  });
  if (response.url.includes("accounts.douban.com") || response.status === 401) throw new NotLoggedIn();
  if (looksRefused(response, false)) {
    await refused("write", id);
    throw new Error("豆瓣拒绝了这次标记");
  }
  if (!response.ok) throw new Error(`豆瓣返回 ${response.status}`);
  let body = {};
  try {
    body = await response.json();
  } catch {
    throw new Error("豆瓣的回答看不懂，可能是接口变了");
  }
  if (body.r !== undefined && body.r !== 0 && body.r !== "0") throw new Error(body.msg || "豆瓣没有接受这次标记");
  succeeded("write");
}
