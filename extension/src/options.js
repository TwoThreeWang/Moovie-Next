// 设置页：功能开关、备份豆瓣标记、断开连接、检查更新。

import { toCSV } from "./lib/csv.js";

const $ = (id) => document.getElementById(id);
const send = (type, payload = {}) => chrome.runtime.sendMessage({ ...payload, type });
const form = $("settings");

async function render() {
  const s = await send("status");
  for (const input of form.querySelectorAll("input[type=checkbox]")) input.checked = Boolean(s.settings[input.name]);
  form.elements.helpOthers.disabled = !s.settings.contribute;
  $("connection").textContent = s.connected && s.me ? `已连接影牛账号 ${s.me.username}（${s.site}）` : "还没有连接影牛账号。";
  $("disconnect").disabled = !s.connected;
  $("version-line").textContent = `当前版本 ${s.version}` + (s.update ? `，有新版本 ${s.update.version}` : "");
  $("guide").href = s.site + "/extension";
}

form.addEventListener("change", async (event) => {
  const input = event.target;
  await send("settings", { patch: { [input.name]: input.checked } });
  $("saved").hidden = false;
  setTimeout(() => ($("saved").hidden = true), 1500);
  render();
});

function download(name, type, body) {
  const url = URL.createObjectURL(new Blob([body], { type }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

async function backup(format) {
  for (const id of ["backup-json", "backup-csv"]) $(id).disabled = true;
  $("backup-status").textContent = "正在读取豆瓣标记，标记多的话要一两分钟…";
  const onProgress = (changes) => {
    const progress = changes.backupProgress && changes.backupProgress.newValue;
    if (progress) $("backup-status").textContent = `正在读取「${progress.status}」… 已读到 ${progress.count} 条`;
  };
  chrome.storage.onChanged.addListener(onProgress);
  try {
    const result = await send("backup");
    if (!result.ok) {
      $("backup-status").textContent = `没有备份成功：${result.error}`;
      return;
    }
    const stamp = new Date().toISOString().slice(0, 10);
    const name = `douban-${result.account}-${stamp}`;
    if (format === "json") download(name + ".json", "application/json", JSON.stringify(result, null, 2));
    else download(name + ".csv", "text/csv;charset=utf-8", toCSV(result.entries));
    $("backup-status").textContent = `已导出 ${result.entries.length} 条标记。`;
  } finally {
    chrome.storage.onChanged.removeListener(onProgress);
    for (const id of ["backup-json", "backup-csv"]) $(id).disabled = false;
  }
}

$("backup-json").addEventListener("click", () => backup("json"));
$("backup-csv").addEventListener("click", () => backup("csv"));

$("disconnect").addEventListener("click", async () => {
  if (!confirm("断开以后插件不再同步豆瓣标记，也不会显示影牛卡片。要断开吗？")) return;
  await send("disconnect");
  render();
});

$("check-update").addEventListener("click", async () => {
  $("check-update").disabled = true;
  const result = await send("checkUpdate");
  $("check-update").disabled = false;
  if (!result.ok) $("version-line").textContent = `检查更新失败：${result.error}`;
  else $("version-line").textContent = result.newer ? `当前版本 ${result.current}，有新版本 ${result.version}，请到使用说明页下载。` : `当前版本 ${result.current}，已经是最新的。`;
});

render();
