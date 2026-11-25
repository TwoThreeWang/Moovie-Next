// toCSV 把备份的标记写成 CSV。开头加 BOM，Excel 才会按 UTF-8 打开；以 = + - @ 开头的格子前面加 '，防止被当成公式。
export const BACKUP_COLUMNS = [
  ["status", "状态"], ["title", "片名"], ["year", "年份"], ["type", "类型"], ["rating", "我的评分"],
  ["comment", "短评"], ["tags", "标签"], ["marked_at", "标记时间"], ["douban_id", "豆瓣 ID"], ["url", "豆瓣链接"],
];

function cell(value) {
  let text = value === undefined || value === null ? "" : String(value);
  if (/^[=+\-@\t\r]/.test(text)) text = "'" + text;
  return /[",\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

export function toCSV(entries) {
  const lines = [BACKUP_COLUMNS.map(([, label]) => cell(label)).join(",")];
  for (const entry of entries) lines.push(BACKUP_COLUMNS.map(([key]) => cell(entry[key])).join(","));
  return "﻿" + lines.join("\r\n") + "\r\n";
}
