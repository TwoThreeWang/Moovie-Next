// 播放页的投屏入口该用哪一种。纯函数，方便测试。
//
// - 'airplay'：苹果设备上的 Safari 有系统 AirPlay 选择器（webkitShowPlaybackTargetPicker）。
// - 'remote'：安卓上的 Chrome 一类浏览器有 Remote Playback API（video.remote），
//   能投到 Chromecast 和内置 Google Cast 的电视。
// - null：不显示投屏按钮。
//
// 桌面浏览器不显示：Chrome / Edge 菜单里自带"投放"，整个标签页都能投，保持原样。
// Safari 也有 video.remote，但它背后就是 AirPlay，用 AirPlay 按钮就够了，不重复显示。

/** @returns {'airplay' | 'remote' | null} */
export function castMode(video, userAgent) {
  if (typeof video?.webkitShowPlaybackTargetPicker === 'function') return 'airplay';
  if (video?.remote && typeof video.remote.prompt === 'function' && /Android/i.test(String(userAgent ?? ''))) return 'remote';
  return null;
}

/** video.remote.prompt() 失败时给用户看的说明；返回空字符串表示不用提示（比如用户自己关掉了设备列表）。 */
export function castErrorText(error) {
  const name = error?.name ?? '';
  if (name === 'NotAllowedError') return '';
  if (name === 'NotSupportedError' || name === 'NotFoundError') {
    return '这个浏览器或这条线路没法从网页投屏。可以换一条线路，或者用 Chrome 投到 Chromecast、内置 Google Cast 的电视；只支持 DLNA / 乐播的电视盒子，网页投不过去。';
  }
  if (name === 'InvalidStateError' || name === 'OperationError') return '';
  return '投屏没成功，请稍后再试';
}
