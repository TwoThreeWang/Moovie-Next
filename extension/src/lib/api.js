// 调用影牛网站的 /api/ext/ 接口。凭证在"连接插件"页面拿到，放在 Authorization 头里；不带网站的 Cookie。

import { SITE_URL } from "../config.js";
import { getState, setState } from "./settings.js";

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export async function getToken() {
  return getState("token", "");
}

export async function setToken(token) {
  await setState("token", token || "");
}

export function siteURL(path = "") {
  return SITE_URL + path;
}

// resolve 把影牛接口给的站内路径（/m/12）拼成这个插件连着的网站上的完整链接。
export function resolve(path) {
  return path ? new URL(path, SITE_URL).href : "";
}

// api 发一个请求。没有凭证、或者网站说凭证失效时抛 ApiError（status 401）。
export async function api(path, { method = "GET", body, token } = {}) {
  token = token ?? (await getToken());
  if (!token && path !== "/api/ext/version") throw new ApiError(401, "还没有连接影牛账号");
  const headers = { Accept: "application/json" };
  if (token) headers.Authorization = "Bearer " + token;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let response;
  try {
    response = await fetch(SITE_URL + path, {
      method, headers, credentials: "omit", cache: "no-store",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "连不上影牛，请检查网络");
  }
  let data = {};
  try {
    data = await response.json();
  } catch {
    // 不是 JSON（比如网关报错页），按状态码处理
  }
  if (!response.ok) {
    if (response.status === 401) await setState("connectionLost", true);
    throw new ApiError(response.status, data.error || `影牛返回 ${response.status}`);
  }
  return data;
}
