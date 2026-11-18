// 分享图片上的文字排版：按宽度折行，超出行数时最后一行加省略号。
// 中文按字断行，英文和数字按词断行（太长的词再按字母断）。纯函数，measure 由调用方提供。

const TOKEN = /[⺀-鿿豈-﫿＀-￯　-〿]|[^\s⺀-鿿豈-﫿＀-￯　-〿]+|\s+/g;
// 不能放在行首的标点
const NO_START = /^[，。、；：！？）》」』】,.;:!?)\]]/;

export function tokenize(text) {
  return String(text ?? '').replace(/\s+/g, ' ').trim().match(TOKEN) ?? [];
}

/**
 * 把 text 折成不超过 maxLines 行、每行不超过 maxWidth 的若干行。
 * measure(string) 返回字符串的宽度。
 */
export function wrapText(text, maxWidth, measure, maxLines = Infinity) {
  const lines = [];
  let line = '';
  const push = () => { lines.push(line.trimEnd()); line = ''; };
  const pieces = [];
  for (const token of tokenize(text)) {
    if (/^\s+$/.test(token) || measure(token) <= maxWidth) pieces.push(token);
    else pieces.push(...token); // 超长的词按字母拆开
  }
  for (const piece of pieces) {
    if (line === '' && /^\s+$/.test(piece)) continue;
    if (measure(line + piece) <= maxWidth) { line += piece; continue; }
    // 标点不放行首：挤在上一行末尾
    if (NO_START.test(piece) && line) { line += piece; push(); continue; }
    push();
    if (!/^\s+$/.test(piece)) line = piece;
  }
  if (line.trim()) push();
  if (lines.length <= maxLines) return lines;
  const kept = lines.slice(0, maxLines);
  let last = kept[maxLines - 1];
  while (last && measure(last + '…') > maxWidth) last = [...last].slice(0, -1).join('');
  kept[maxLines - 1] = last.trimEnd() + '…';
  return kept;
}
