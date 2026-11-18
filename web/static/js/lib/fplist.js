// 解析服务器下发的广告指纹列表，并在本地查询。
// 文件格式见 internal/adskip：
//   "MVFP" | 版本(1) | 前缀字节数(1) | 保留(2) | 已确认数量(4, 大端) | 已否认数量(4, 大端) | 已确认前缀… | 已否认前缀…
// 两组前缀都按字节序排好，所以用二分查找。

/** 解析列表。格式不对时返回 null。 */
export function parseList(buffer) {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  if (bytes.length < 16 || String.fromCharCode(...bytes.subarray(0, 4)) !== 'MVFP' || bytes[4] !== 1) return null;
  const prefix = bytes[5];
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const confirmedCount = view.getUint32(8), rejectedCount = view.getUint32(12);
  if (prefix < 8 || prefix > 32 || bytes.length !== 16 + (confirmedCount + rejectedCount) * prefix) return null;
  const split = 16 + confirmedCount * prefix;
  return { prefix, confirmed: bytes.subarray(16, split), rejected: bytes.subarray(split), confirmedCount, rejectedCount };
}

function hexToBytes(hex, length) {
  const out = new Uint8Array(length);
  for (let i = 0; i < length; i++) out[i] = parseInt(hex.substr(i * 2, 2), 16);
  return out;
}

function contains(table, count, prefix, needle) {
  let low = 0, high = count - 1;
  while (low <= high) {
    const mid = (low + high) >> 1;
    let cmp = 0;
    for (let i = 0; i < prefix && cmp === 0; i++) cmp = table[mid * prefix + i] - needle[i];
    if (cmp === 0) return true;
    if (cmp < 0) low = mid + 1; else high = mid - 1;
  }
  return false;
}

/** 查询一个指纹（64 位十六进制）的状态：confirmed、rejected 或 unknown。 */
export function lookup(list, hex) {
  if (!list || !/^[0-9a-f]{64}$/.test(hex)) return 'unknown';
  const needle = hexToBytes(hex, list.prefix);
  if (contains(list.confirmed, list.confirmedCount, list.prefix, needle)) return 'confirmed';
  if (contains(list.rejected, list.rejectedCount, list.prefix, needle)) return 'rejected';
  return 'unknown';
}
