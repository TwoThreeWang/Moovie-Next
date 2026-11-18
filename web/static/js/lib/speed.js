// 线路速度：下载一段视频花的时间，和这段视频本身的时长比。
// 下载倍速表示每秒能下载几秒视频。接近播放倍速时，网速波动容易耗光缓冲。

/** 下载 bytes 字节花了 ms 毫秒，这些字节是 mediaSeconds 秒的视频：返回倍速。算不出来时返回 0。 */
export function speedRatio(bytes, ms, mediaSeconds) {
  if (!(bytes > 0) || !(ms > 0) || !(mediaSeconds > 0)) return 0;
  const bytesPerSecond = bytes / (ms / 1000);
  const bitrate = bytes / mediaSeconds; // 这段视频每秒需要的字节数
  return bytesPerSecond / bitrate;
}

/** 平滑的倍速估计：新样本占 30%。前几个分片受连接建立影响偏慢，所以至少 3 个样本才下结论。 */
export class SpeedMeter {
  constructor() { this.value = 0; this.samples = 0; this.bytesPerSecond = 0; }
  add(bytes, ms, mediaSeconds) {
    const ratio = speedRatio(bytes, ms, mediaSeconds);
    if (!ratio) return;
    this.value = this.samples ? this.value * 0.7 + ratio * 0.3 : ratio;
    this.bytesPerSecond = this.samples ? this.bytesPerSecond * 0.7 + (bytes / (ms / 1000)) * 0.3 : bytes / (ms / 1000);
    this.samples++;
  }
  get known() { return this.samples >= 3; }
  isSlow(playbackRate = 1) { return this.known && this.value < playbackRate * 1.3; }
}

/**
 * 从其他线路里挑更快的：下载速率至少是播放倍速的 1.5 倍，并且比当前快一半以上。
 * probes: [{ index, ratio }]
 */
export function fasterLine(current, probes, playbackRate = 1) {
  let best = null;
  for (const probe of probes) {
    if (!(probe.ratio >= playbackRate * 1.5) || probe.ratio < current * 1.5) continue;
    if (!best || probe.ratio > best.ratio) best = probe;
  }
  return best;
}

/** 下载速度的文字：1.2 MB/s、640 KB/s。 */
export function formatSpeed(bytesPerSecond) {
  if (!(bytesPerSecond > 0)) return '';
  return bytesPerSecond >= 1e6 ? (bytesPerSecond / 1e6).toFixed(1) + ' MB/s' : Math.round(bytesPerSecond / 1e3) + ' KB/s';
}
