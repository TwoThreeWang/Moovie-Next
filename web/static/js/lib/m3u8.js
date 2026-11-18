// 分析 HLS 播放清单，找出疑似广告的片段并计算指纹。
//
// 指纹算法沿用旧版播放器的做法：它在实际使用中没有误判过。改算法会让已经众包确认过的指纹全部作废。
// 只看播放清单的文本，不下载任何视频分片。

function resolvePath(uri, base) {
  try { return new URL(uri, base || 'http://x').pathname; } catch { return uri; }
}

function pathFamily(path) {
  try {
    const p = new URL(path, 'http://x').pathname;
    const i = p.lastIndexOf('/');
    return i > 0 ? p.substring(0, i) : '/';
  } catch { return '/'; }
}

/** 是不是主播放清单（里面列的是不同清晰度，不是分片）。 */
export function isMaster(text) { return text.includes('#EXT-X-STREAM-INF'); }

/** 从主播放清单里取出码率最高的那一路的地址。 */
export function pickVariant(text, baseURL) {
  const lines = text.split('\n');
  let best = null, bestBandwidth = -1;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line.startsWith('#EXT-X-STREAM-INF')) continue;
    const bandwidth = parseInt(/BANDWIDTH=(\d+)/.exec(line)?.[1] ?? '0', 10);
    let j = i + 1;
    while (j < lines.length && (!lines[j].trim() || lines[j].trim().startsWith('#'))) j++;
    if (j < lines.length && bandwidth > bestBandwidth) {
      try { best = new URL(lines[j].trim(), baseURL).toString(); bestBandwidth = bandwidth; } catch { /* 忽略写错的地址 */ }
    }
  }
  return best;
}

export function parseSegments(text, baseURL) {
  const lines = text.split('\n'), segs = [];
  let keyURI = '', keyMethod = 'NONE', byterange = '', extinf = null, discont = false;
  for (let i = 0; i < lines.length; i++) {
    const L = lines[i].trim();
    if (L.startsWith('#EXT-X-DISCONTINUITY')) { discont = true; continue; }
    if (L.startsWith('#EXT-X-KEY:')) {
      const mm = L.match(/METHOD=([^,]+)/), mu = L.match(/URI="([^"]+)"/);
      const nm = mm ? mm[1] : 'NONE', nu = mu ? mu[1] : '';
      if (nm !== keyMethod || nu !== keyURI) { discont = true; keyMethod = nm; keyURI = nu; }
      continue;
    }
    if (L.startsWith('#EXT-X-BYTERANGE:')) { byterange = L.substring(17); continue; }
    if (L.startsWith('#EXTINF:')) { extinf = parseFloat(L.substring(8).split(',')[0]) || 0; continue; }
    if (L.startsWith('#') || !L) continue;
    if (extinf !== null) {
      const path = resolvePath(L, baseURL);
      segs.push({ dur: extinf, uri: L, path, family: pathFamily(path), br: byterange, kURI: keyURI, kMethod: keyMethod, line: i, discont });
      extinf = null; byterange = ''; discont = false;
    }
  }
  return segs;
}

export function buildBlocks(segs) {
  if (!segs.length) return [];
  const blocks = [];
  let cur = { segs: [segs[0]], family: segs[0].family, kURI: segs[0].kURI, kMethod: segs[0].kMethod };
  for (let i = 1; i < segs.length; i++) {
    const s = segs[i];
    if (s.discont || s.kURI !== cur.kURI || s.kMethod !== cur.kMethod || s.family !== cur.family) {
      blocks.push(cur);
      cur = { segs: [s], family: s.family, kURI: s.kURI, kMethod: s.kMethod };
    } else cur.segs.push(s);
  }
  blocks.push(cur);
  let t = 0;
  blocks.forEach((bl, b) => {
    bl.start = t; bl.idx = b;
    bl.dur = bl.segs.reduce((sum, s) => sum + s.dur, 0);
    bl.end = t + bl.dur; t = bl.end;
    bl.first = b === 0; bl.last = b === blocks.length - 1;
  });
  return blocks;
}

function mainBody(blocks) {
  if (!blocks.length) return null;
  let best = blocks[0], total = 0;
  for (const bl of blocks) { total += bl.dur; if (bl.dur > best.dur) best = bl; }
  return best.dur >= total * 0.3 ? best : null;
}

function scoreBlock(bl, main, blocks) {
  if (bl === main || bl.dur < 5 || bl.dur > 120 || bl.segs.length < 2) return 0;
  const total = blocks.reduce((sum, b) => sum + b.dur, 0);
  if (bl.dur > total * 0.5) return 0;
  let sc = 0;
  if (bl.first || bl.last) sc++;
  if (bl.segs[0].discont) sc++;
  else if (bl.idx < blocks.length - 1 && blocks[bl.idx + 1].segs.length && blocks[bl.idx + 1].segs[0].discont) sc++;
  if (bl.family !== main.family) sc++;
  if (bl.kURI !== main.kURI || bl.kMethod !== main.kMethod) sc++;
  const bAvg = bl.dur / bl.segs.length, mAvg = main.dur / main.segs.length;
  if (Math.abs(bAvg - mAvg) > mAvg * 0.3) sc++;
  for (const other of blocks) {
    if (other === bl || other === main) continue;
    if (other.family === bl.family && other.segs.length === bl.segs.length && Math.abs(other.dur - bl.dur) < 1) { sc++; break; }
  }
  return sc;
}

/** 候选片段的规范化文本，指纹就是它的 SHA-256。 */
export function canonicalText(block, url) {
  return block.segs.map((s) => {
    let line = Math.round(s.dur * 1000) + '|' + resolvePath(s.uri, url);
    if (s.br) line += '|' + s.br;
    return line;
  }).join('\n');
}

async function sha256Hex(text) {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * 分析一份媒体播放清单，返回疑似广告的片段：[{ start, end, dur, hex, score }]。
 * 只处理点播清单（有 #EXT-X-ENDLIST）。找不到可靠的正片主体时返回空数组，宁可不跳也不误跳。
 */
export async function analyze(text, url) {
  if (!text.includes('#EXTINF') || !text.includes('#EXT-X-ENDLIST')) return [];
  const segs = parseSegments(text, url);
  if (segs.length < 3) return [];
  const blocks = buildBlocks(segs);
  if (blocks.length < 2) return [];
  const main = mainBody(blocks);
  if (!main) return [];
  const candidates = [];
  for (const block of blocks) {
    const score = scoreBlock(block, main, blocks);
    if (score >= 3) candidates.push({ start: block.start, end: block.end, dur: block.dur, score, hex: await sha256Hex(canonicalText(block, url)) });
  }
  return candidates;
}
