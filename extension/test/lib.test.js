// 插件里不依赖浏览器的逻辑：node --test extension/test/
import { test } from "node:test";
import assert from "node:assert/strict";

// 假的 chrome.storage：douban.js、settings.js 只在函数里用到它
const store = {};
globalThis.chrome = {
  storage: {
    local: {
      async get(key) {
        return typeof key === "string" ? { [key]: store[key] } : Object.fromEntries(key.map((k) => [k, store[k]]));
      },
      async set(values) {
        Object.assign(store, values);
      },
    },
  },
};

const { compareVersions } = await import("../src/lib/settings.js");
const { toCSV } = await import("../src/lib/csv.js");
const douban = await import("../src/lib/douban.js");

test("版本号按数字比较", () => {
  assert.ok(compareVersions("1.10.0", "1.9.3") > 0);
  assert.ok(compareVersions("1.0", "1.0.1") < 0);
  assert.equal(compareVersions("2.0.0", "2.0"), 0);
});

test("CSV 带 BOM、转义引号和换行、防公式", () => {
  const csv = toCSV([{ status: "看过", title: '片名,"带引号"', comment: "第一行\n第二行", rating: 5, tags: "=HYPERLINK()" }]);
  assert.ok(csv.startsWith("﻿状态,片名"));
  assert.ok(csv.includes('"片名,""带引号"""'));
  assert.ok(csv.includes('"第一行\n第二行"'));
  assert.ok(csv.includes("'=HYPERLINK()"));
});

function reply(status, body, { url = "https://m.douban.com/x", type = "application/json" } = {}) {
  return { status, ok: status >= 200 && status < 300, url, headers: { get: () => type }, json: async () => body };
}

test("详情先试电影接口，没有再试剧集接口；不带 Cookie", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push([url, options.credentials]);
    return url.includes("/movie/") ? reply(404, {}) : reply(200, { id: "36000001", title: "雾港" });
  };
  const pending = douban.fetchSubject("36000001", "movie");
  for (let i = 0; i < 10; i++) {
    await Promise.resolve();
    t.mock.timers.tick(20000);
    await new Promise((resolve) => setImmediate(resolve));
  }
  const result = await pending;
  assert.equal(result.kind, "tv");
  assert.equal(calls.length, 2);
  assert.ok(calls.every(([, credentials]) => credentials === "omit"));
});

test("接连几个条目被拒绝就暂停这条通道", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  globalThis.fetch = async () => reply(403, {});
  const errors = [];
  for (const id of ["36000011", "36000012", "36000013"]) {
    const pending = douban.fetchSubject(id, "movie").catch((error) => error);
    for (let i = 0; i < 10; i++) {
      await Promise.resolve();
      t.mock.timers.tick(20000);
      await new Promise((resolve) => setImmediate(resolve));
    }
    errors.push(await pending);
  }
  assert.ok(!(errors[0] instanceof douban.Throttled), "单个条目被拒绝不算限流");
  assert.ok(errors[2] instanceof douban.Throttled, "第三个不同的条目被拒绝就算限流");
  assert.ok((await douban.pausedUntil("anon")) > Date.now());
  await assert.rejects(douban.fetchSubject("36000014", "movie"), douban.Throttled, "暂停期间直接失败，不再请求豆瓣");
});
