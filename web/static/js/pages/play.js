// 播放页。
//
// 视频由浏览器直接连用户自己的源播放，本站服务器不参与。这个脚本负责：
// 找到这一集在各个源里的地址 → 播放，失败时自动换下一个 → 记录进度 → 跳过已确认的广告 → 弹幕。
import { api, h, icon, toast, userId, userKey } from '../core/api.js';
import * as idb from '../core/idb.js';
import * as sources from '../core/sources.js';
import { reportPosition, savePrivate, getPrivate } from '../core/history.js';
import { bestMatch, findEpisode, sameWork } from '../lib/match.js';
import { normalizeTitle } from '../lib/title.js';
import { analyze, isMaster, pickVariant } from '../lib/m3u8.js';
import { parseList, lookup } from '../lib/fplist.js';
import { collapseEpisodes } from '../core/episodes.js';
import { checkURL } from '../lib/cms.js';
import { SpeedMeter, speedRatio, fasterLine, formatSpeed } from '../lib/speed.js';

const d = document;
const pl = d.getElementById('pl'), vp = d.getElementById('vp'), v = vp.querySelector('video');
const q = (s) => vp.querySelector(s);
const bar = q('#vp-bar'), hint = q('#vp-hint'), nextCard = q('#vp-next'), menu = q('#vp-menu'), message = q('#vp-msg'), adBox = q('#vp-ad');
const settings = q('#vp-settings');
const srcBox = d.getElementById('src'), srcStatus = d.getElementById('src-status'), epGrid = d.getElementById('ep-grid');
const fine = matchMedia('(hover: hover) and (pointer: fine)').matches;
const local = pl.dataset.local === '1';
const manual = pl.dataset.manual === '1';

// ---------- 播放器偏好（存在本机） ----------
// 播放倍速、弹幕和自动播放下一集设置。
const PREFS_KEY = 'mv:player';
const rateOptions = [...menu.querySelectorAll('[data-rate]')];
const prefs = { playbackRate: 1, dm: true, dmSize: 1, dmOpacity: 1, dmSpeed: 1, dmArea: 0.75, autoNext: true };
try { Object.assign(prefs, JSON.parse(localStorage.getItem(PREFS_KEY) || '{}')); } catch { /* 用默认值 */ }
if (!rateOptions.some((option) => Number(option.dataset.rate) === prefs.playbackRate)) prefs.playbackRate = 1;
const savePrefs = () => { try { localStorage.setItem(PREFS_KEY, JSON.stringify(prefs)); } catch { /* 存不了就算了 */ } };
function applyPlaybackRate() {
  v.defaultPlaybackRate = prefs.playbackRate; // 更换视频地址时，浏览器会恢复此值。
  v.playbackRate = prefs.playbackRate;
  d.getElementById('vp-speed').textContent = `${prefs.playbackRate}×`;
  for (const option of rateOptions) {
    option.setAttribute('aria-checked', String(Number(option.dataset.rate) === prefs.playbackRate));
  }
}
function applyPrefs() {
  vp.dataset.dm = prefs.dm ? 'on' : 'off';
  q('[data-act="dm"]')?.setAttribute('aria-pressed', String(prefs.dm));
  vp.style.setProperty('--dm-size', prefs.dmSize);
  vp.style.setProperty('--dm-opacity', prefs.dmOpacity);
  vp.style.setProperty('--dm-area', prefs.dmArea);
  for (const option of vp.querySelectorAll('[data-pref]')) {
    option.setAttribute('aria-checked', String(String(prefs[option.dataset.pref]) === option.dataset.value));
  }
}
applyPrefs();
applyPlaybackRate();

const fmt = (t) => {
  t = Math.max(0, Math.floor(t || 0));
  const hours = Math.floor(t / 3600), minutes = Math.floor((t % 3600) / 60), seconds = t % 60;
  return (hours ? hours + ':' + String(minutes).padStart(2, '0') : minutes) + ':' + String(seconds).padStart(2, '0');
};

// ---------- 这一页要播什么 ----------

const state = {
  media: null,      // 有元数据：{ id, title, type, season, episode_count, … }；没有：{ title, year, type, source, vodId, pic }
  rid: '',          // 没有元数据时的加密记录 ID
  isMovie: false,
  season: 1,
  episode: 1,
  resume: 0,        // 从哪里继续播（秒）
  entries: [],      // 各个源的查询结果
  options: [],      // 这一集可用的（源, 线路）组合
  current: -1,
  failed: new Set(),
  fallback: false,  // 没有元数据的作品：已经去其他源找过同一部了
  started: false,
  switched: false,  // 是否换过源（换源后往回倒几秒）
};

function showMessage(...children) {
  message.replaceChildren(...children.filter(Boolean));
  message.hidden = false;
  vp.classList.remove('is-loading');
}

async function setup() {
  const params = new URLSearchParams(location.search);
  if (manual) {
    setupPlayer();
    return false; // 有没有链接都由 setupPlayer 处理，不走下面的查源流程
  }
  if (!local) {
    state.media = JSON.parse(pl.dataset.media);
    state.isMovie = state.media.type === 'movie';
    state.season = state.media.season || 1;
    state.episode = state.isMovie ? 1 : Math.max(1, +pl.dataset.episode || 1);
    const positions = JSON.parse(pl.dataset.positions || '[]');
    const saved = positions.find((p) => p.season === state.season && p.episode === state.episode);
    if (saved && !saved.completed) state.resume = saved.position;
    state.preferred = params.get('src') || (await sources.getLast('m:' + state.media.id))?.source || '';
    return true;
  }
  const hash = new URLSearchParams(location.hash.slice(1));
  state.rid = hash.get('rid') ?? '';
  if (!/^[0-9a-f]{64}$/.test(state.rid)) {
    showMessage(h('p', { text: '没有找到要播放的内容。' }), h('a', { class: 'btn btn-glass', href: '/search', text: '去搜索' }));
    return false;
  }
  const record = await getPrivate(state.rid);
  const cached = await idb.get('cache', userKey('local:' + state.rid));
  const info = record ?? cached;
  if (!info) {
    showMessage(h('p', { text: '这台设备上没有这部作品的播放信息。请从搜索页重新进入。' }), h('a', { class: 'btn btn-glass', href: '/search', text: '去搜索' }));
    return false;
  }
  state.media = { title: info.title, year: info.year, type: info.type, source: info.source, vodId: info.vodId, pic: info.pic, doubanId: cached?.doubanId };
  state.isMovie = info.type === 'movie';
  state.episode = state.isMovie ? 1 : Math.max(1, +hash.get('e') || record?.ep || 1);
  if (record && (record.ep || 1) === state.episode && !record.done) state.resume = record.pos || 0;
  state.preferred = info.source;
  d.getElementById('vp-title').textContent = info.title;
  d.getElementById('pl-title').textContent = info.title;
  d.title = info.title + ' - 播放';
  return true;
}

function updateEpisodeUI() {
  d.getElementById('vp-sub').textContent = state.isMovie ? '' : `第 ${state.episode} 集`;
  for (const link of epGrid.querySelectorAll('[data-ep]')) {
    if (+link.dataset.ep === state.episode) { link.setAttribute('aria-current', 'true'); link.scrollIntoView({ block: 'nearest' }); } else link.removeAttribute('aria-current');
  }
}

/** 某一集的地址。 */
function episodeHref(episode) {
  if (local) return `/play/local#rid=${state.rid}&e=${episode}`;
  const params = new URLSearchParams({ s: state.season, e: episode });
  const key = state.options[state.current]?.key;
  if (key) params.set('src', key);
  return `/play/${state.media.id}?${params}`;
}

/** 各个源里一共有哪些集。 */
function availableEpisodes() {
  const numbers = new Set();
  for (const entry of state.entries) if (entry.status === 'ok') for (const line of entry.item.lines) for (const episode of line.episodes) numbers.add(episode.number);
  return [...numbers].sort((a, b) => a - b);
}

function renderEpisodes() {
  if (state.isMovie) return;
  const numbers = availableEpisodes();
  // 作品库里没有分集资料（或者这部作品不在作品库里）时，用源里的集数生成选集。
  if (!epGrid.children.length && numbers.length) {
    epGrid.replaceChildren(...numbers.map((n) => h('a', { href: episodeHref(n), dataset: { ep: n }, text: String(n) })));
    d.getElementById('ep-sec').hidden = false;
  }
  updateEpisodeUI();
  const next = numbers.find((n) => n > state.episode);
  state.next = next ?? (epGrid.querySelector(`[data-ep="${state.episode + 1}"]`) ? state.episode + 1 : 0);
  const previous = numbers.filter((n) => n < state.episode).pop();
  state.prev = previous ?? (epGrid.querySelector(`[data-ep="${state.episode - 1}"]`) ? state.episode - 1 : 0);
  d.getElementById('vp-next-btn').hidden = !state.next;
  d.getElementById('vp-prev-btn').hidden = !state.prev;
  collapseEpisodes(epGrid);
}

// ---------- 找源 ----------

function buildOptions() {
  const options = [];
  for (const entry of state.entries) {
    if (entry.status !== 'ok') continue;
    for (const line of entry.item.lines) {
      const episode = findEpisode(line, state.episode, state.isMovie);
      if (episode) {
        options.push({ key: entry.key, name: entry.name, line: line.name, url: episode.url, insecure: episode.insecure, label: episode.label, ms: entry.ms,
          vodId: entry.item.vodId, pic: entry.item.pic, desc: [entry.item.name, entry.item.remarks].filter(Boolean).join(' · '), other: !!entry.other, unsure: !!entry.other && !entry.sure });
      }
    }
  }
  // 不能直接播放的 HTTP 地址排到最后
  options.sort((a, b) => Number(a.insecure) - Number(b.insecure));
  const previous = state.options[state.current];
  state.options = options;
  state.current = previous ? options.findIndex((o) => o.key === previous.key && o.line === previous.line) : -1;
}

function renderOptions() {
  srcBox.replaceChildren(...state.options.map((option, index) => h('button', {
    type: 'button', role: 'radio', 'aria-checked': String(index === state.current), dataset: { index },
    class: state.failed.has(option.url) ? 'is-failed' : '',
  }, icon('layers'), h('span', { class: 'src-text' },
    h('span', { class: 'src-title' }, h('b', { text: option.line ? `${option.name} · ${option.line}` : option.name }),
      option.other ? h('small', { class: 'src-other', text: option.unsure ? '同名，请确认' : '其他源' }) : null),
    option.desc ? h('small', { class: 'src-desc', text: option.desc }) : null),
  h('span', { class: 'lat' + (option.ms < 800 ? ' good' : ''), text: state.failed.has(option.url) ? '播放失败' : option.insecure ? 'HTTP' : option.ms + 'ms' }))));
}

function pickStart(done) {
  if (state.started || !state.options.length) return -1;
  const usable = (o) => !state.failed.has(o.url);
  const preferred = state.options.findIndex((o) => o.key === state.preferred && !o.insecure && usable(o));
  if (preferred >= 0) return preferred;
  // 指定的源还没返回结果时再等等；没有指定，或者都查完了，就用第一个。
  const waiting = state.preferred && !done && !state.entries.some((e) => e.key === state.preferred);
  // 其他源里同名、但没法用年份确认是同一部的，不自动播放，让用户自己选
  return waiting ? -1 : state.options.findIndex((o) => !o.unsure && usable(o));
}

function onEntries(result, { done }) {
  state.entries = result.entries;
  buildOptions();
  renderOptions();
  renderEpisodes();
  const index = pickStart(done);
  if (index >= 0) playOption(index);
  if (!done) { srcStatus.textContent = '正在查询…'; return; }
  srcStatus.textContent = state.options.length ? '播放失败会自动切换到下一个' : '';
  if (state.options.length || state.started) return;
  const found = state.entries.some((e) => e.status === 'ok');
  const hidden = state.entries.some((e) => e.hidden); // 只有预告片或枪版，见 match.js 的 incompleteKind
  showMessage(h('p', { text: found ? `你的源里没有第 ${state.episode} 集。` : hidden ? '你的源里目前只有预告片或枪版（已隐藏），还没有正片。' : '你的源里没有找到这部作品。' }),
    h('button', { class: 'btn btn-glass', type: 'button', text: '重新查询', onclick: () => { message.hidden = true; findSources(true); } }),
    local ? h('a', { class: 'btn btn-glass', href: '/search?q=' + encodeURIComponent(state.media.title), text: '换个名字搜' }) : null);
}

async function findSources(force = false) {
  if (state.manual) {
    state.options = [{ key: 'manual', name: '你的链接', line: '', url: state.manual.url, insecure: state.manual.insecure, ms: 0 }];
    srcStatus.textContent = '';
    renderOptions();
    playOption(0);
    return;
  }
  if (!(await sources.getSubscription())) {
    showMessage(h('p', { text: '还没有设置播放源。本站只提供播放器，视频来源需要你自己配置。' }), h('a', { class: 'btn btn-primary', href: '/settings#sources', text: '去设置播放源' }));
    return;
  }
  vp.classList.add('is-loading');
  if (!local) {
    await sources.findForMedia(state.media, { onUpdate: onEntries, force });
    return;
  }
  // 没有元数据的作品：先查当初找到它的那个源；那里找不到了，再去其他源找同名的同一部。
  const list = await sources.getSources();
  const original = list.find((s) => s.key === state.media.source);
  const entries = [];
  if (original) {
    const entry = { key: original.key, name: original.name, status: 'none', ms: 0, item: null };
    try {
      const { items, ms } = await sources.querySource(original, state.media.title);
      entry.ms = ms;
      const item = items.find((i) => i.vodId === state.media.vodId) ?? items.find((i) => normalizeTitle(i.name) === normalizeTitle(state.media.title))
        ?? bestMatch({ ...state.media, type: state.isMovie ? 'movie' : 'tv' }, items)?.item;
      if (item) { entry.status = 'ok'; entry.item = item; }
    } catch { entry.status = 'error'; }
    entries.push(entry);
  }
  const playable = entries.some((e) => e.status === 'ok' && e.item.lines.some((line) => findEpisode(line, state.episode, state.isMovie)));
  if (playable) {
    onEntries({ entries }, { done: true });
    return;
  }
  await searchOthers(list, entries, original ? '原来的源里找不到这部了' : '找到这部作品的那个源已经不在你的订阅里了');
}

/**
 * 没有元数据的作品：在原来那个源以外的源里找同一部（规则见 sameWork）。
 * 年份能对上的自动换过去；对不上号的只列出来。换过去之后，观看记录会跟着新的源走。
 */
async function searchOthers(list, entries, reason) {
  state.fallback = true;
  const others = list.filter((source) => source.key !== state.media.source);
  srcStatus.textContent = reason + '，正在其他源里找…';
  if (!others.length) {
    onEntries({ entries }, { done: true });
    return;
  }
  const media = { title: state.media.title, year: state.media.year, type: state.isMovie ? 'movie' : 'series' };
  let finished = 0;
  await Promise.all(others.map(async (source) => {
    const entry = { key: source.key, name: source.name, status: 'none', ms: 0, item: null, other: true, sure: false };
    try {
      const { items, ms } = await sources.querySource(source, state.media.title);
      entry.ms = ms;
      const found = sameWork(media, items);
      if (found) { entry.status = 'ok'; entry.item = found.item; entry.sure = found.sure; }
    } catch { entry.status = 'error'; }
    entries.push(entry);
    // 确定的排前面，再按速度
    entries.sort((a, b) => (b.status === 'ok') - (a.status === 'ok') || (b.sure === true) - (a.sure === true) || a.ms - b.ms);
    const wasStarted = state.started;
    onEntries({ entries: [...entries] }, { done: ++finished === others.length });
    if (!wasStarted && state.started) toast(`${reason}，已换到「${state.options[state.current]?.name}」上的同名作品。不是同一部的话，在下面换一个。`);
  }));
  if (!state.started && state.options.length) {
    showMessage(h('p', { text: reason + '。其他源里有同名的作品，但没法确认是不是同一部（资源站没写年份），请在下面的「来源」里选一个。' }));
    srcStatus.textContent = '同名作品，请确认后再播放';
  }
}

// ---------- 播放 ----------

let hls = null, startTimer = 0;
// 只有苹果的 Safari 用系统自带的 HLS 播放（iPhone 上也只能这样）。
// 其他浏览器即使声称支持，实际也不可靠，统一用 hls.js。
const nativeHLS = !!v.canPlayType('application/vnd.apple.mpegurl') && /^((?!chrome|chromium|android|edg).)*safari/i.test(navigator.userAgent);
const loadScript = (src) => new Promise((resolve, reject) => {
  const script = d.createElement('script');
  script.src = src; script.onload = resolve; script.onerror = reject;
  d.head.append(script);
});
// hls.js 有 188KB：不是 Safari 的话，页面一打开就开始下载，和查源同时进行，查到源时多半已经下好了。
let hlsLoading = null;
const loadHls = () => (hlsLoading ??= window.Hls ? Promise.resolve() : loadScript('/static/js/hls.min.js').catch((error) => { hlsLoading = null; throw error; }));
if (!nativeHLS) loadHls().catch(() => {});

const HLS_CONFIG = {
  // 往前多缓冲：慢的源下载速度只比视频码率快一点，能多攒一点缓冲，后面就少卡一次
  maxBufferLength: 60, maxMaxBufferLength: 180, maxBufferSize: 90 * 1000 * 1000, backBufferLength: 30,
  capLevelToPlayerSize: true, startFragPrefetch: true, enableWorker: true,
  // 慢的源一个分片可能要下十几秒：超时太短会反复中断重下，反而永远下不完
  manifestLoadingTimeOut: 12000, manifestLoadingMaxRetry: 2,
  fragLoadingTimeOut: 40000, fragLoadingMaxRetry: 4, fragLoadingRetryDelay: 800,
};

/**
 * 观察 hls.js 加载的播放清单（不修改）。广告分析要用 hls.js 实际在播的那一份、
 * 以及跳转之后的最终地址，算出来的指纹才对得上；也省掉了再单独下载一次清单。
 */
function playlistObserver(onPlaylist) {
  const Base = window.Hls.DefaultConfig.loader;
  return class PlaylistObserver extends Base {
    load(context, config, callbacks) {
      const onSuccess = callbacks.onSuccess;
      super.load(context, config, {
        ...callbacks,
        onSuccess: (response, stats, ctx, details) => {
          if (typeof response.data === 'string' && response.data.includes('#EXTINF')) {
            try { onPlaylist(response.data, response.url || ctx.url, ctx.level ?? 0); } catch { /* 分析失败不影响播放 */ }
          }
          onSuccess(response, stats, ctx, details);
        },
      });
    }
  };
}

/** 用 hls.js 播放，出错时先自己恢复，恢复不了再换源。 */
function attachHls(url) {
  const token = ads.token;
  hls = new window.Hls({ ...HLS_CONFIG, pLoader: playlistObserver((text, finalURL, level) => analyzePlaylist(text, finalURL, level, token)) });
  let networkRetries = 0, mediaRecoveries = 0;
  hls.on(window.Hls.Events.ERROR, (_, data) => {
    if (!data.fatal) return;
    // 播放清单本身加载失败（链接过期、没权限，或者对方不允许跨域，浏览器只报"网络错误"）：
    // hls.js 自己已经重试过了，再拉也没用，直接换下一个。分片的网络错误才值得重试。
    const { MANIFEST_LOAD_ERROR, MANIFEST_LOAD_TIMEOUT, MANIFEST_PARSING_ERROR } = window.Hls.ErrorDetails;
    if ([MANIFEST_LOAD_ERROR, MANIFEST_LOAD_TIMEOUT, MANIFEST_PARSING_ERROR].includes(data.details)) return failover('线路出错');
    // 网络错误：重新拉流，最多 3 次（加载成功一个分片就清零）
    if (data.type === window.Hls.ErrorTypes.NETWORK_ERROR && networkRetries < 3) {
      networkRetries++;
      setTimeout(() => hls?.startLoad(), 500 * networkRetries);
      return;
    }
    // 解码错误：先让 hls.js 自己恢复；第二次连音频编码一起换
    if (data.type === window.Hls.ErrorTypes.MEDIA_ERROR && mediaRecoveries < 2) {
      mediaRecoveries++;
      if (mediaRecoveries === 2) hls.swapAudioCodec();
      hls.recoverMediaError();
      return;
    }
    failover('线路出错');
  });
  hls.on(window.Hls.Events.FRAG_LOADED, (_, data) => {
    networkRetries = 0;
    const stats = data.frag?.stats ?? data.stats;
    if (stats?.loaded && stats.loading) line.meter.add(stats.loaded, stats.loading.end - stats.loading.start, data.frag.duration);
    applyBufferTarget();
    checkLineSpeed();
  });
  // 清单拿到了说明线路是通的，给第一个分片再留 15 秒，慢的源不至于还没开播就被换掉
  hls.on(window.Hls.Events.MANIFEST_PARSED, () => { clearTimeout(startTimer); startTimer = setTimeout(() => failover('超时'), 15000); });
  hls.on(window.Hls.Events.LEVEL_SWITCHED, (_, data) => {
    ads.level = data.level;
    if (ads.levels.has(data.level)) ads.candidates = ads.levels.get(data.level);
  });
  hls.loadSource(url);
  hls.attachMedia(v);
}

async function playOption(index, { keepTime = false } = {}) {
  const option = state.options[index];
  if (!option) return;
  if (keepTime && v.currentTime > 0) { state.resume = Math.max(0, v.currentTime - 5); state.switched = true; }
  state.started = true;
  state.current = index;
  renderOptions();
  message.hidden = true;
  vp.classList.add('is-loading');
  resetAds();
  resetLine();
  hls?.destroy();
  hls = null;
  clearTimeout(startTimer);
  // HTTP 地址浏览器不让播：有些 CDN 两种协议都支持，换成 HTTPS 试一次。
  const url = option.insecure ? option.url.replace(/^http:/, 'https:') : option.url;
  const autoplay = !v.paused || state.switched || state.autoplay !== false;
  startTimer = setTimeout(() => failover('超时'), 15000);
  v.preload = 'metadata';
  if (/\.mp4(\?|#|$)/i.test(url) || nativeHLS) {
    v.src = url;
  } else {
    try {
      await loadHls();
    } catch { return showMessage(h('p', { text: '播放组件加载失败，请刷新重试。' })); }
    if (window.Hls?.isSupported()) {
      attachHls(url);
    } else if (v.canPlayType('application/vnd.apple.mpegurl')) {
      v.src = url;
    } else {
      return showMessage(h('p', { text: '当前浏览器不支持播放这种视频。' }));
    }
  }
  if (autoplay) v.play().catch(() => {});
  if (!hls) prepareAds(url); // 用 hls.js 时，广告分析由 playlistObserver 做
  updateDownload(url);
}

// 第三方下载：把当前这一集的地址交给第三方的 M3U8 下载工具。在新标签页打开，不带来源页面地址。
function updateDownload(url) {
  const link = d.getElementById('dl-link');
  if (!link) return;
  link.href = 'https://www.zngg.net/tool/detail/M3U8V2Pro?url=' + encodeURIComponent(url);
  link.hidden = false;
}

v.addEventListener('error', () => { if (state.started && v.error) failover('线路出错'); });

function failover(reason) {
  clearTimeout(startTimer);
  const option = state.options[state.current];
  if (!option || state.failed.has(option.url)) return;
  state.failed.add(option.url);
  const next = state.options.findIndex((o) => !state.failed.has(o.url) && !o.unsure);
  if (next < 0 && local && !state.fallback) {
    // 原来那个源的地址都播不了（比如视频被删了）：去其他源找同一部
    renderOptions();
    hls?.destroy(); hls = null;
    state.started = false;
    sources.getSources().then((list) => searchOthers(list, state.entries.filter((e) => !e.other), `「${option.name}」上的视频播放不了`));
    return;
  }
  if (next < 0 && manual) {
    hls?.destroy(); hls = null;
    showMessage(h('p', { text: option.insecure
      ? '这是 HTTP 链接，浏览器不允许在 HTTPS 页面里播放。换成 HTTPS 地址再试试。'
      : '这个链接播放不了：可能已经过期，或者对方的服务器不允许在其他网站播放。' }),
    h('button', { class: 'btn btn-glass', type: 'button', text: '重试', onclick: () => openLink(option.url) }));
    return;
  }
  if (next < 0) {
    renderOptions();
    hls?.destroy(); hls = null;
    const http = state.options.every((o) => o.insecure);
    showMessage(h('p', { text: http ? '这一集只有 HTTP 地址，浏览器不允许在 HTTPS 页面里播放。' : '这一集在你的源里都没能播放。' }),
      h('button', { class: 'btn btn-glass', type: 'button', text: '重新查询', onclick: () => { state.failed.clear(); state.started = false; message.hidden = true; findSources(true); } }));
    return;
  }
  showHint(`${option.name}${reason}，正在切换…`, 2000);
  state.autoplay = true;
  playOption(next, { keepTime: true });
}

v.addEventListener('loadedmetadata', () => {
  clearTimeout(startTimer);
  d.getElementById('vp-dur').textContent = fmt(v.duration);
  if (state.resume > 5 && state.resume < v.duration - 15) {
    v.currentTime = state.resume;
    showHint(state.switched ? '已切换来源，进度可能有几秒偏差' : `从 ${fmt(state.resume)} 继续播放`, 2400);
  }
  if (manual) { rememberLink(state.manual.url, { dur: Math.round(v.duration || 0) }); return; }
  if (local) discoverLocal();
  const option = state.options[state.current];
  if (option) sources.setLast(local ? 'p:' + state.rid : 'm:' + state.media.id, { source: option.key, line: option.line });
  loadDanmaku();
});

const play = () => {
  if (!state.started) { state.autoplay = true; const index = pickStart(true); if (index >= 0) playOption(index); return; }
  v.play().catch(() => {});
};
const toggle = () => (v.paused ? play() : v.pause());

srcBox.addEventListener('click', (ev) => {
  const button = ev.target.closest('[data-index]');
  if (!button || +button.dataset.index === state.current) return;
  state.failed.delete(state.options[+button.dataset.index].url);
  state.autoplay = !v.paused || !state.started;
  playOption(+button.dataset.index, { keepTime: true });
});

function goEpisode(episode) {
  saveProgress({ keepalive: true });
  if (!local) { location.href = episodeHref(episode); return; }
  location.hash = `rid=${state.rid}&e=${episode}`;
}
// 没有元数据的作品：换集只改 # 后面的部分，页面不重新加载。
addEventListener('hashchange', () => { if (local) location.reload(); });
epGrid.addEventListener('click', (ev) => {
  const link = ev.target.closest('[data-ep]');
  if (!link || !local) return;
  ev.preventDefault();
  goEpisode(+link.dataset.ep);
});

// ---------- 进度 ----------

let lastSaved = 0;
function saveProgress({ keepalive = false, ended = false } = {}) {
  const duration = v.duration || 0, position = v.currentTime || 0;
  if (!state.started || !duration || (position < 5 && !ended)) return;
  const completed = ended || position / duration > 0.93;
  lastSaved = Date.now();
  if (manual) {
    rememberLink(state.manual.url, { pos: Math.round(position), dur: Math.round(duration), done: completed }, v.paused || ended);
    return;
  }
  if (!local) {
    reportPosition({ media_id: state.media.id, season: state.season, episode: state.episode, position, duration, completed }, { keepalive });
    return;
  }
  const option = state.options[state.current];
  savePrivate(state.rid, {
    title: state.media.title, year: state.media.year || 0, type: state.media.type, pic: option?.pic || state.media.pic || '',
    source: option?.key ?? state.media.source, vodId: option?.vodId ?? state.media.vodId, line: option?.line ?? '',
    ep: state.episode, pos: Math.round(position), dur: Math.round(duration), done: completed, at: Date.now(),
  }, { keepalive }).catch(() => {});
}
v.addEventListener('pause', () => saveProgress());
// 往前缓冲多少，按情况调整（只对 hls.js 有效；Safari 的系统播放器自己决定）：
// - 下载速率达到当前播放倍速的 2 倍：以 60 秒为目标；
// - 下载余量较小时：边播边多攒到 3 分钟，扛住更长的低谷；
// - 暂停：用户多半去干别的了，趁这时攒到 5 分钟。
// 线路比视频码率还慢时，缓冲多少都会被耗光，这时靠的是提示换线路。
const BUFFER = {
  normal: { maxBufferLength: 60, maxMaxBufferLength: 180, maxBufferSize: 90 * 1000 * 1000 },
  marginal: { maxBufferLength: 180, maxMaxBufferLength: 300, maxBufferSize: 120 * 1000 * 1000 },
  paused: { maxBufferLength: 300, maxMaxBufferLength: 600, maxBufferSize: 150 * 1000 * 1000 },
};
function applyBufferTarget() {
  if (!hls) return;
  const limited = line.meter.known && line.meter.value < 2 * prefs.playbackRate;
  const mode = v.paused && !v.ended ? 'paused' : limited ? 'marginal' : 'normal';
  Object.assign(hls.config, BUFFER[mode]); // hls.js 每一拍都按配置检查缓冲目标，改了就会接着下载
}
v.addEventListener('pause', applyBufferTarget);
v.addEventListener('play', applyBufferTarget);
// 不注册 unload，这样浏览器的页面缓存（后退秒开）还能用。
addEventListener('pagehide', () => saveProgress({ keepalive: true }));
d.addEventListener('visibilitychange', () => { if (d.hidden) saveProgress({ keepalive: true }); });

// ---------- 状态同步 ----------

v.addEventListener('play', () => { vp.dataset.state = 'playing'; wake(); });
v.addEventListener('pause', () => { vp.dataset.state = 'paused'; wake(); });
// 卡住超过 1.2 秒：先看是不是卡在刚跳过的广告结尾，是的话跳到已经缓冲好的位置；否则让 hls.js 重新拉流。
let stallTimer = 0;
v.addEventListener('waiting', () => {
  vp.classList.add('is-loading');
  clearTimeout(stallTimer);
  const nearAd = skippedAdAt(v.currentTime);
  stallTimer = setTimeout(() => {
    if (v.paused || v.readyState >= 3) return;
    if (!jumpPastAd(skippedAdAt(v.currentTime))) hls?.startLoad();
    v.play().catch(() => {});
  }, nearAd ? 400 : 1200);
  if (state.started) line.stalledAt ||= Date.now();
  showBuffering();
});
for (const type of ['playing', 'canplay', 'seeked']) {
  v.addEventListener(type, () => { vp.classList.remove('is-loading'); clearTimeout(stallTimer); line.stalledAt = 0; spinText.textContent = ''; });
}

// ---------- 慢线路 ----------
// 资源站的线路速度差别很大。边播边量当前线路的"倍速"（每秒能下载几秒视频）；
// 相对于用户选择的播放倍速，余量不足时试探其他线路，有明显更快的就提示切换。

const spinText = vp.querySelector('.vp-spin-text');
const line = { meter: new SpeedMeter(), probing: false, probes: [], offered: -1, stalledAt: 0 };

function resetLine() {
  line.meter = new SpeedMeter();
  line.probing = false; line.probes = []; line.offered = -1; line.stalledAt = 0; line.probedAt = 0;
  if (spinText) spinText.textContent = '';
}

/** 缓冲时在转圈下面显示下载速度；卡了 8 秒以上时提示换线路（试探过有更快的才提示）。 */
function showBuffering() {
  if (!spinText || !vp.classList.contains('is-loading')) return;
  const speed = formatSpeed(line.meter.bytesPerSecond);
  spinText.textContent = speed ? `正在缓冲 · ${speed}${line.meter.isSlow(prefs.playbackRate) ? '（这条线路偏慢）' : ''}` : '正在缓冲…';
  if (line.stalledAt && Date.now() - line.stalledAt > 8000) {
    const faster = fasterLine(line.meter.value, line.probes, prefs.playbackRate);
    if (faster) offerLine(faster, '卡住好一会儿了');
    else if (!line.probing && state.options.length > 1) checkLineSpeed(true);
  }
  setTimeout(showBuffering, 1000);
}

/** 提示换到更快的线路：一个按钮，点了才换。同一段时间里只提示一次。 */
function offerLine(faster, why) {
  if (line.offered === faster.index || !state.started) return;
  line.offered = faster.index;
  const option = state.options[faster.index];
  const times = faster.ratio / Math.max(line.meter.value, 0.1);
  adBox.replaceChildren(h('span', { text: `${why}，「${option.name} · ${option.line}」${times >= 1.5 ? `快 ${times.toFixed(1)} 倍` : '更快'}` }),
    h('button', { type: 'button', text: '切换线路', onclick: () => { hideAdBox(); playOption(faster.index, { keepTime: true }); } }),
    h('button', { type: 'button', 'aria-label': '关闭', text: '×', onclick: hideAdBox }));
  adBox.hidden = false;
  ads.prompt = 'line';
}

/** 当前线路偏慢（或者卡住了）时试探其他线路，有明显更快的就提示。 */
function checkLineSpeed(stalled = false) {
  if ((!line.meter.isSlow(prefs.playbackRate) && !stalled) || line.probing || Date.now() - (line.probedAt || 0) < 60000) return; // 试探要下载数据，一分钟最多一次
  line.probing = true;
  line.probedAt = Date.now();
  probeOthers().then(() => {
    line.probing = false;
    const faster = fasterLine(line.meter.value, line.probes, prefs.playbackRate);
    if (faster) offerLine(faster, stalled ? '卡住好一会儿了' : '当前线路偏慢');
  });
}

/** 试探其他线路（最多 3 条）：下载清单和第一个分片的开头（最多 1.5 MB），算出倍速。 */
async function probeOthers() {
  const current = state.options[state.current];
  const others = state.options.map((option, index) => ({ option, index }))
    .filter(({ option, index }) => index !== state.current && !option.insecure && !option.unsure && !state.failed.has(option.url) && option.url !== current?.url)
    .slice(0, 3);
  line.probes = (await Promise.all(others.map(async ({ option, index }) => ({ index, ratio: await probeSpeed(option.url) })))).filter((p) => p.ratio > 0);
}

async function probeSpeed(url) {
  if (!/\.m3u8(\?|#|$)/i.test(url)) return 0;
  let playlist = await fetchPlaylist(url, 5000);
  if (playlist.text && isMaster(playlist.text)) {
    const variant = pickVariant(playlist.text, playlist.url);
    playlist = variant ? await fetchPlaylist(variant, 5000) : { text: '', url: '' };
  }
  const match = /#EXTINF:([\d.]+)[^\n]*\n(?:#[^\n]*\n)*([^#\s][^\n]*)/.exec(playlist.text);
  if (!match) return 0;
  const duration = parseFloat(match[1]);
  let segment;
  try { segment = new URL(match[2].trim(), playlist.url).href; } catch { return 0; }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 8000);
  const started = performance.now();
  let bytes = 0, total = 0, complete = false;
  try {
    const response = await fetch(segment, { credentials: 'omit', referrerPolicy: 'no-referrer', signal: controller.signal });
    if (!response.ok || !response.body) return 0;
    total = Number(response.headers.get('Content-Length')) || 0;
    const reader = response.body.getReader();
    while (bytes < 1.5e6) {
      const { done, value } = await reader.read();
      if (done) { complete = true; break; }
      bytes += value.length;
    }
    reader.cancel().catch(() => {});
  } catch { /* 超时：按已经下到的算 */ } finally { clearTimeout(timer); }
  const ms = performance.now() - started;
  // 分片总大小未知时，只有读完整段才能把下载时间和视频时长相比。
  if (!complete && total <= 0) return 0;
  const seconds = total > bytes ? duration * (bytes / total) : duration;
  return bytes > 50e3 ? speedRatio(bytes, ms, seconds) : 0;
}

/** 快看完时，先把下一集的播放清单取一次：DNS、TLS 连接和 CDN 缓存都热了，换集更快。 */
let warmed = '';
function warmNextEpisode() {
  const option = state.options[state.current];
  if (!state.next || !option || !state.entries) return;
  const entry = state.entries.find((e) => e.key === option.key && e.status === 'ok');
  const lineInfo = entry?.item.lines.find((l) => l.name === option.line);
  const next = lineInfo && findEpisode(lineInfo, state.next, state.isMovie);
  if (!next || next.url === warmed || next.insecure) return;
  warmed = next.url;
  fetchPlaylist(next.url, 8000).catch(() => {});
}
let dragging = false;
const setPos = (p) => bar.style.setProperty('--pos', (Math.min(1, Math.max(0, p)) * 100).toFixed(2) + '%');
v.addEventListener('timeupdate', () => {
  const t = v.currentTime, duration = v.duration || 0;
  if (!dragging) setPos(duration ? t / duration : 0);
  d.getElementById('vp-cur').textContent = fmt(t);
  const left = duration - t;
  const showNext = !!state.next && prefs.autoNext && !state.nextDismissed && duration > 60 && left < 20;
  nextCard.classList.toggle('show', showNext);
  if (showNext) nextCard.querySelector('.ring').style.setProperty('--deg', (((20 - left) / 20) * 360).toFixed(0) + 'deg');
  if (!v.paused && Date.now() - lastSaved > 30000) saveProgress();
  checkAds(t);
  tickDanmaku(t);
  if (left > 0 && left < 90) warmNextEpisode();
});
v.addEventListener('progress', () => {
  if (!v.duration || !v.buffered.length) return;
  bar.style.setProperty('--buf', ((v.buffered.end(v.buffered.length - 1) / v.duration) * 100).toFixed(1) + '%');
});
v.addEventListener('ended', () => {
  saveProgress({ ended: true, keepalive: true });
  if (state.next && prefs.autoNext && !state.nextDismissed) goEpisode(state.next);
});

// 第一次开始播放时提醒：资源站常在视频里插博彩、贷款广告。
v.addEventListener('playing', () => {
  const warn = d.getElementById('vp-warn');
  if (!warn || warn.dataset.shown) return;
  warn.dataset.shown = '1';
  warn.hidden = false;
  setTimeout(() => { warn.hidden = true; }, 6000);
}, { once: true });

// 支持系统 AirPlay 选择器时就显示入口，由系统在打开选择器后列出可用设备。
q('[data-act="airplay"]').hidden = typeof v.webkitShowPlaybackTargetPicker !== 'function';
v.addEventListener('seeking', () => { danmakuIndex = -1; });

// ---------- 广告跳过 ----------
// 判断完全在本地完成：分析播放清单的文本算出指纹，和服务器下发的列表比对。
// 发给服务器的只有用户主动投的票（一个指纹加"是 / 不是"）。

const ads = {
  enabled: d.body.dataset.adskip === 'true', list: null, candidates: [], undone: new Set(), asked: new Set(), yes: new Set(), recovered: new Set(), prompt: null,
  token: 0,          // 换线路时加一，丢掉上一条线路迟到的分析结果
  levels: new Map(), // 清晰度序号 → 这一路的候选片段（各清晰度的分片地址不同，指纹也不同）
  level: -1,         // 正在播放的清晰度
};

function resetAds() {
  ads.token++;
  ads.candidates = [];
  ads.levels.clear();
  ads.level = -1;
  hideAdBox();
}

/** 分析一份媒体播放清单。url 必须是跳转之后的最终地址（分片的相对路径以它为准）。 */
async function analyzePlaylist(text, url, level, token) {
  const found = await analyze(text, url);
  const list = found.length ? await fingerprintList() : null;
  if (token !== ads.token) return;
  const candidates = found.map((c) => ({ ...c, status: lookup(list, c.hex) }));
  ads.levels.set(level, candidates);
  if (ads.level === -1 || ads.level === level) { ads.level = level; ads.candidates = candidates; }
}
function hideAdBox() { adBox.hidden = true; adBox.replaceChildren(); ads.prompt = null; }

async function fingerprintList() {
  if (ads.list) return ads.list;
  const cached = await idb.get('kv', 'fplist');
  if (cached) ads.list = parseList(cached.bytes);
  if (!cached || Date.now() - cached.at > 6 * 3600 * 1000) {
    try {
      const response = await api('/api/ad-fingerprints/list', { headers: cached?.etag ? { 'If-None-Match': cached.etag } : {} });
      if (response.status === 200) {
        const bytes = new Uint8Array(await response.arrayBuffer());
        const parsed = parseList(bytes);
        if (parsed) { ads.list = parsed; await idb.set('kv', 'fplist', { bytes, etag: response.headers.get('ETag'), at: Date.now() }); }
      } else if (response.status === 304 && cached) {
        await idb.set('kv', 'fplist', { ...cached, at: Date.now() });
      }
    } catch { /* 拿不到列表就不跳过任何片段 */ }
  }
  return ads.list;
}

/** 下载播放清单，返回 { text, url }。url 是跳转之后的最终地址。 */
async function fetchPlaylist(url, timeout = 6000) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeout);
  try {
    const response = await fetch(url, { credentials: 'omit', referrerPolicy: 'no-referrer', signal: controller.signal });
    return response.ok ? { text: await response.text(), url: response.url || url } : { text: '', url };
  } catch { return { text: '', url }; } finally { clearTimeout(timer); }
}

/** 系统播放器（Safari）播放时没法看到它加载的清单，只能自己再下载一份来分析。 */
async function prepareAds(url) {
  if (!/\.m3u8(\?|#|$)/i.test(url)) return;
  const token = ads.token;
  let playlist = await fetchPlaylist(url);
  if (playlist.text && isMaster(playlist.text)) {
    const variant = pickVariant(playlist.text, playlist.url);
    playlist = variant ? await fetchPlaylist(variant) : { text: '', url: '' };
  }
  if (playlist.text && token === ads.token) await analyzePlaylist(playlist.text, playlist.url, 0, token);
}

/** 广告结尾之后、已经缓冲好的第一个位置。跳到正好的结尾有时会卡住（那里的分片还没下载）。 */
function afterAd(candidate) {
  const boundary = candidate.end, fallback = boundary + 0.2;
  try {
    for (let i = 0; i < v.buffered.length; i++) {
      const start = v.buffered.start(i), end = v.buffered.end(i);
      if (end <= boundary) continue;
      if (start > boundary + 0.5) break;
      const target = Math.max(boundary + 0.05, start + 0.05);
      if (target < end) return Math.min(target, boundary + 0.5);
    }
  } catch { /* buffered 读不了就用默认值 */ }
  return Math.min(fallback, (v.duration || fallback) - 0.5);
}

/** 当前时间是不是在一段已经确认、正在跳过的广告附近。 */
function skippedAdAt(t) {
  return ads.candidates.find((c) => (c.status === 'confirmed' || ads.yes.has(c.hex)) && ads.enabled && !ads.undone.has(c.hex)
    && t >= c.start - 0.5 && t <= c.end + 0.45) ?? null;
}

/** 卡在广告结尾时跳到已缓冲的位置。每段广告只跳一次，避免来回跳。 */
function jumpPastAd(candidate) {
  if (!candidate || ads.recovered.has(candidate.hex + candidate.start)) return false;
  const target = afterAd(candidate);
  if (target <= v.currentTime + 0.01) return false;
  ads.recovered.add(candidate.hex + candidate.start);
  v.currentTime = target;
  hls?.startLoad();
  return true;
}

function vote(hex, kind) {
  if (!userId()) return; // 游客不投票：防止匿名刷票。跳过广告照样可以用
  api('/api/ad-fingerprints/vote', { method: 'POST', json: { fingerprint: hex, vote: kind } }).catch(() => {});
}

/**
 * 撤销自动跳过之后：问一句是不是跳错了。点"这段不是广告"就投一张否认票；
 * 投的人多了，这个指纹会变成已否认，大家都不会再跳过。只是想看看的话点"只是看看"，不投票。
 */
function offerNotAd(candidate) {
  if (!userId()) return hideAdBox();
  adBox.replaceChildren(h('span', { text: '跳错了吗？' }),
    h('button', { type: 'button', text: '这段不是广告', onclick: () => { vote(candidate.hex, 'reject'); hideAdBox(); toast('谢谢，已记下'); } }),
    h('button', { type: 'button', text: '只是看看', onclick: hideAdBox }));
  adBox.hidden = false;
  ads.prompt = 'undo';
  setTimeout(() => { if (ads.prompt === 'undo') hideAdBox(); }, 8000);
}

function checkAds(t) {
  if (!ads.candidates.length) return;
  const candidate = ads.candidates.find((c) => t >= c.start && t < c.end - 0.3);
  if (!candidate) { if (ads.prompt && ads.prompt !== 'line' && ads.prompt !== 'undo') hideAdBox(); return; }
  const confirmed = candidate.status === 'confirmed' || ads.yes.has(candidate.hex);
  if (confirmed && ads.enabled && !ads.undone.has(candidate.hex)) {
    v.currentTime = afterAd(candidate);
    adBox.replaceChildren(h('span', { text: `已跳过 ${Math.round(candidate.dur)} 秒广告` }),
      h('button', { type: 'button', text: '撤销', onclick: () => { ads.undone.add(candidate.hex); v.currentTime = candidate.start; offerNotAd(candidate); } }));
    adBox.hidden = false;
    ads.prompt = null;
    setTimeout(() => { if (!ads.prompt) hideAdBox(); }, 5000);
    return;
  }
  // 结构上很像广告，但还没有足够多的人确认：问一下用户。同一段只问一次。
  if (candidate.status !== 'unknown' || confirmed || ads.asked.has(candidate.hex) || ads.undone.has(candidate.hex)) return;
  ads.asked.add(candidate.hex);
  ads.prompt = candidate.hex;
  adBox.replaceChildren(h('span', { text: '这一段是广告吗？' }),
    h('button', { type: 'button', text: '是，跳过', onclick: () => { vote(candidate.hex, 'confirm'); ads.yes.add(candidate.hex); v.currentTime = afterAd(candidate); hideAdBox(); } }),
    h('button', { type: 'button', text: '不是', onclick: () => { vote(candidate.hex, 'reject'); hideAdBox(); } }),
    h('button', { type: 'button', 'aria-label': '关闭', text: '×', onclick: hideAdBox }));
  adBox.hidden = false;
}

// 作品库里还没有、播放源给了豆瓣 ID 的：真正开始播放后让本站收录，后台去豆瓣抓资料。
// 之后搜索能直接匹配到作品页，本地观看记录也会在每天的检查里转成普通进度。每个页面只报一次。
let discovered = false;
function discoverLocal() {
  if (discovered || !/^\d{6,9}$/.test(state.media.doubanId ?? '')) return;
  discovered = true;
  api('/api/media/discover', { method: 'POST', json: { douban_id: state.media.doubanId } }).catch(() => {});
}

// ---------- 弹幕 ----------

const dmLayer = q('.vp-dm');
let danmaku = [], danmakuIndex = -1, danmakuLoaded = '';
const LANES = 6, laneFree = new Array(LANES).fill(0);

function danmakuQuery() {
  if (!local) return { media_id: state.media.id, season: state.season, episode: state.episode };
  return { title: state.media.title, season: 1, episode: state.episode };
}

// 第三方弹幕库第一次查一集比较慢，服务器没等到时返回 pending，这里隔几秒再取（那时一般已经进了服务器缓存）。
async function loadDanmaku(attempt = 0) {
  const key = new URLSearchParams(danmakuQuery()).toString();
  if (!attempt && danmakuLoaded === key) return;
  danmakuLoaded = key;
  try {
    const response = await api('/api/danmaku?' + key);
    if (!response.ok || danmakuLoaded !== key) return;
    const body = await response.json();
    if (danmakuLoaded !== key) return; // 等结果的时候已经换了集
    danmaku = body.items ?? [];
    danmakuIndex = -1;
    const more = body.pending && attempt < 4;
    d.getElementById('dm-count').textContent = more ? `${danmaku.length ? danmaku.length + ' 条，' : ''}弹幕库加载中…`
      : danmaku.length ? `${danmaku.length} 条` : '还没有弹幕';
    if (more) setTimeout(() => { if (danmakuLoaded === key) loadDanmaku(attempt + 1); }, 5000);
  } catch { /* 弹幕不影响播放 */ }
}

function spawn(item, mine = false) {
  if (vp.dataset.dm !== 'on' || d.hidden) return;
  const now = performance.now();
  let lane = laneFree.findIndex((t) => t < now);
  if (lane < 0) { if (!mine) return; lane = 0; }
  const span = h('span', { text: item.text, class: mine ? 'is-mine' : '' });
  span.style.top = `${(lane / LANES) * 100}%`;
  if (/^#[0-9A-F]{6}$/.test(item.color ?? '') && item.color !== '#FFFFFF') span.style.color = item.color;
  // 速度跟着弹幕设置和播放倍速走：2 倍速看的时候弹幕也快一倍
  const duration = (7 + Math.random() * 3) / prefs.dmSpeed / (v.playbackRate || 1);
  span.style.setProperty('--d', duration + 's');
  dmLayer.append(span);
  laneFree[lane] = now + duration * 1000 * 0.35;
  span.addEventListener('animationend', () => span.remove(), { once: true });
}

function tickDanmaku(t) {
  if (!danmaku.length || v.paused) return;
  if (danmakuIndex < 0) { danmakuIndex = danmaku.findIndex((item) => item.time >= t); if (danmakuIndex < 0) danmakuIndex = danmaku.length; return; }
  let shown = 0;
  while (danmakuIndex < danmaku.length && danmaku[danmakuIndex].time <= t) {
    if (shown++ < 4) spawn(danmaku[danmakuIndex]);
    danmakuIndex++;
  }
}

d.getElementById('dm-form')?.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const input = ev.target.elements.t, text = input.value.trim();
  if (!text) return;
  if (!state.started) return toast('开始播放后才能发弹幕');
  const item = { ...danmakuQuery(), time: v.currentTime, text, mode: 0, color: '#FFFFFF' };
  input.value = '';
  spawn(item, true);
  const response = await api('/api/danmaku', { method: 'POST', json: item }).catch(() => null);
  if (!response?.ok) {
    let reason = '发送失败';
    try { reason = (await response.json()).error ?? reason; } catch { /* 用默认提示 */ }
    toast(reason);
    return;
  }
  // 放进本页的弹幕列表（按时间排好），往回拉进度时还能看到自己发的。
  const at = danmaku.findIndex((x) => x.time > item.time);
  const index = at < 0 ? danmaku.length : at;
  danmaku.splice(index, 0, item);
  if (danmakuIndex >= 0 && index < danmakuIndex) danmakuIndex++; // 插在已经播过的位置：游标跟着后移，不重复弹出
  d.getElementById('dm-count').textContent = `${danmaku.length} 条`;
});

// ---------- 控件自动隐藏 ----------

let idleTimer = 0;
function wake() {
  vp.classList.remove('is-idle');
  clearTimeout(idleTimer);
  if (!v.paused && !menu.classList.contains('show') && !settings.classList.contains('show')) idleTimer = setTimeout(() => vp.classList.add('is-idle'), 3000);
}
const sleep = () => { if (!v.paused) { clearTimeout(idleTimer); vp.classList.add('is-idle'); } };
if (fine) {
  vp.addEventListener('pointermove', wake, { passive: true });
  vp.addEventListener('pointerleave', () => { if (!v.paused) idleTimer = setTimeout(sleep, 800); });
}

let hintTimer = 0;
function showHint(text, ms = 900) {
  hint.textContent = text;
  hint.classList.add('show');
  clearTimeout(hintTimer);
  hintTimer = setTimeout(() => hint.classList.remove('show'), ms);
}
function seekBy(seconds) {
  if (!v.duration) return;
  v.currentTime = Math.min(v.duration - 0.5, Math.max(0, v.currentTime + seconds));
  showHint(`${seconds > 0 ? '+' : '−'}${Math.abs(seconds)} 秒 · ${fmt(v.currentTime)}`);
}
function ripple(side) {
  const el = q(`.vp-ripple[data-side="${side}"]`);
  el.classList.remove('show'); void el.offsetWidth; el.classList.add('show');
}

// ---------- 进度条：拖动和悬停预览 ----------

const ratioAt = (x) => { const rect = bar.getBoundingClientRect(); return Math.min(1, Math.max(0, (x - rect.left) / rect.width)); };
function preview(x) {
  const p = ratioAt(x);
  bar.style.setProperty('--hov', p * 100 + '%');
  bar.querySelector('.tip').textContent = fmt(p * v.duration);
  return p;
}
bar.addEventListener('pointerdown', (ev) => {
  if (!v.duration) return;
  dragging = true;
  bar.setPointerCapture(ev.pointerId);
  bar.classList.add('is-drag');
  setPos(preview(ev.clientX));
});
bar.addEventListener('pointermove', (ev) => {
  if (!v.duration) return;
  const p = preview(ev.clientX);
  if (dragging) setPos(p);
});
bar.addEventListener('pointerup', (ev) => {
  if (!dragging) return;
  dragging = false;
  bar.classList.remove('is-drag');
  v.currentTime = ratioAt(ev.clientX) * v.duration;
  wake();
});
bar.addEventListener('pointercancel', () => { dragging = false; bar.classList.remove('is-drag'); });
bar.addEventListener('keydown', (ev) => {
  if (ev.key === 'ArrowLeft' || ev.key === 'ArrowRight') { ev.preventDefault(); ev.stopPropagation(); seekBy(ev.key === 'ArrowLeft' ? -5 : 5); }
});

// ---------- 画面上的手势 ----------
// 触屏：单击显隐控件，双击两侧快退快进，横滑拖进度，全屏时右侧竖滑调亮度。
// 鼠标：单击播放或暂停，双击全屏。

const isControl = (el) => el.closest('button, a, .vp-bar, .vp-menu, .vp-msg, .vp-ad, input');
let gesture = null, tapTimer = 0, lastTap = 0, lastTapSide = 0;
const sideOf = (x) => {
  const rect = vp.getBoundingClientRect(), p = (x - rect.left) / rect.width;
  return p < 0.38 ? -1 : p > 0.62 ? 1 : 0;
};
const immersive = () => d.fullscreenElement === vp || matchMedia('(orientation: landscape) and (max-height: 520px)').matches;

const locked = () => vp.dataset.locked === '1';

// 长按画面：3 倍速快进，松手恢复。
function endFast(g) {
  clearTimeout(g?.pressTimer);
  if (g?.mode !== 'fast') return false;
  v.playbackRate = prefs.playbackRate;
  hint.classList.remove('show');
  return true;
}

vp.addEventListener('pointerdown', (ev) => {
  if (isControl(ev.target)) return;
  if (locked()) { vp.classList.add('show-lock'); clearTimeout(vp._lockTimer); vp._lockTimer = setTimeout(() => vp.classList.remove('show-lock'), 2500); return; }
  gesture = { id: ev.pointerId, x: ev.clientX, y: ev.clientY, t0: v.currentTime, dim: +getComputedStyle(vp).getPropertyValue('--dim') || 0, mode: '', type: ev.pointerType };
  const current = gesture;
  if (!v.paused && state.started) {
    current.pressTimer = setTimeout(() => {
      if (gesture !== current || current.mode) return;
      current.mode = 'fast';
      v.playbackRate = 3;
      hint.textContent = '3× 快进中';
      hint.classList.add('show');
      navigator.vibrate?.(10);
    }, 500);
  }
});
vp.addEventListener('pointermove', (ev) => {
  if (!gesture || ev.pointerId !== gesture.id) return;
  if (!gesture.mode && Math.hypot(ev.clientX - gesture.x, ev.clientY - gesture.y) > 12) clearTimeout(gesture.pressTimer);
  if (gesture.type !== 'touch' || gesture.mode === 'fast') return;
  const dx = ev.clientX - gesture.x, dy = ev.clientY - gesture.y, rect = vp.getBoundingClientRect();
  if (!gesture.mode && Math.hypot(dx, dy) > 12) {
    if (Math.abs(dx) > Math.abs(dy)) gesture.mode = 'seek';
    else if (immersive() && gesture.x > rect.left + rect.width / 2) gesture.mode = 'bright';
    else gesture.mode = 'none';
    if (gesture.mode !== 'none') vp.setPointerCapture(ev.pointerId);
  }
  if (gesture.mode === 'seek' && v.duration) {
    const span = Math.min(v.duration, 240);
    gesture.target = Math.min(v.duration, Math.max(0, gesture.t0 + (dx / rect.width) * span));
    const delta = gesture.target - gesture.t0;
    setPos(gesture.target / v.duration);
    showHint(`${fmt(gesture.target)} / ${fmt(v.duration)}  (${delta >= 0 ? '+' : '−'}${fmt(Math.abs(delta))})`, 1200);
    dragging = true;
  } else if (gesture.mode === 'bright') {
    const dim = Math.min(0.75, Math.max(0, gesture.dim + dy / rect.height));
    vp.style.setProperty('--dim', dim.toFixed(2));
    showHint(`亮度 ${Math.round((1 - dim / 0.75) * 100)}%`, 1000);
  }
}, { passive: true });
vp.addEventListener('pointercancel', () => { endFast(gesture); gesture = null; dragging = false; });
vp.addEventListener('contextmenu', (ev) => { if (gesture?.mode === 'fast') ev.preventDefault(); });
vp.addEventListener('pointerup', (ev) => {
  if (!gesture || ev.pointerId !== gesture.id) return;
  const ended = gesture;
  gesture = null;
  if (endFast(ended)) return;
  if (ended.mode === 'seek') { dragging = false; if (ended.target != null) v.currentTime = ended.target; wake(); return; }
  if (ended.mode || isControl(ev.target)) return;
  const now = Date.now();
  if (ended.type !== 'touch') {
    if (now - lastTap < 280) { clearTimeout(tapTimer); fullscreen(); lastTap = 0; return; }
    lastTap = now;
    tapTimer = setTimeout(toggle, 220);
    return;
  }
  const side = sideOf(ev.clientX);
  if (side && now - lastTap < 300 && side === lastTapSide) {
    clearTimeout(tapTimer);
    seekBy(side * 10); ripple(side);
    lastTap = now; // 连续双击可以累计
    return;
  }
  lastTap = now; lastTapSide = side;
  clearTimeout(tapTimer);
  tapTimer = setTimeout(() => {
    if (!state.started) return play();
    if (vp.classList.contains('is-idle') || v.paused) wake(); else sleep();
  }, side ? 300 : 0);
});

// ---------- 按钮和键盘 ----------

function fullscreen() {
  if (d.fullscreenElement) return d.exitFullscreen();
  if (vp.requestFullscreen) {
    vp.requestFullscreen({ navigationUI: 'hide' }).then(() => screen.orientation?.lock?.('landscape').catch(() => {})).catch(() => {});
  } else if (v.webkitEnterFullscreen) v.webkitEnterFullscreen(); // iPhone 只能让 <video> 进入系统全屏
}
const actions = {
  toggle,
  back: () => { seekBy(-10); ripple(-1); },
  fwd: () => { seekBy(10); ripple(1); },
  next: () => { if (state.next) goEpisode(state.next); },
  mute: () => { v.muted = !v.muted; showHint(v.muted ? '已静音' : '取消静音'); },
  dm: () => {
    prefs.dm = !prefs.dm;
    savePrefs();
    applyPrefs();
    showHint(prefs.dm ? '弹幕已开启' : '弹幕已关闭');
  },
  speed: () => { settings.classList.remove('show'); menu.classList.toggle('show'); wake(); },
  settings: () => { menu.classList.remove('show'); settings.classList.toggle('show'); wake(); },
  prev: () => { if (state.prev) goEpisode(state.prev); },
  'next-cancel': () => { state.nextDismissed = true; nextCard.classList.remove('show'); },
  lock: () => {
    const locked = vp.dataset.locked !== '1';
    vp.dataset.locked = locked ? '1' : '0';
    q('[data-act="lock"]')?.setAttribute('aria-pressed', String(locked));
    showHint(locked ? '已锁定，点锁图标解锁' : '已解锁');
  },
  airplay: () => v.webkitShowPlaybackTargetPicker?.(),
  pip: () => {
    if (d.pictureInPictureElement) return d.exitPictureInPicture();
    if (v.requestPictureInPicture) v.requestPictureInPicture().catch(() => {});
    else v.webkitSetPresentationMode?.('picture-in-picture');
  },
  fs: fullscreen,
  theater: () => pl.classList.toggle('is-theater'),
};
vp.addEventListener('click', (ev) => {
  const button = ev.target.closest('[data-act]');
  if (button) { ev.stopPropagation(); actions[button.dataset.act]?.(button); wake(); return; }
  const pref = ev.target.closest('[data-pref]');
  if (pref) {
    const value = pref.dataset.value;
    prefs[pref.dataset.pref] = value === 'true' ? true : value === 'false' ? false : Number(value);
    savePrefs();
    applyPrefs();
    return;
  }
  const rate = ev.target.closest('[data-rate]');
  if (rate) {
    prefs.playbackRate = Number(rate.dataset.rate);
    savePrefs();
    applyPlaybackRate();
    applyBufferTarget();
    menu.classList.remove('show');
    showHint(`${rate.dataset.rate}× 倍速`);
  } else if (!ev.target.closest('.vp-menu')) { menu.classList.remove('show'); settings.classList.remove('show'); }
});
d.addEventListener('keydown', (ev) => {
  if (/INPUT|TEXTAREA|SELECT/.test(d.activeElement.tagName) || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  const key = ev.key.toLowerCase();
  const map = {
    ' ': toggle, k: toggle, f: fullscreen, m: actions.mute, n: actions.next, t: actions.theater,
    arrowleft: () => seekBy(-10), arrowright: () => seekBy(10), j: () => seekBy(-10), l: () => seekBy(10),
    d: () => actions.dm(q('[data-act="dm"]')),
    arrowup: () => { v.volume = Math.min(1, v.volume + 0.1); showHint(`音量 ${Math.round(v.volume * 100)}%`); },
    arrowdown: () => { v.volume = Math.max(0, v.volume - 0.1); showHint(`音量 ${Math.round(v.volume * 100)}%`); },
  };
  if (!map[key]) return;
  if (key === ' ' && d.activeElement.closest('button, a')) return;
  ev.preventDefault();
  map[key]();
  wake();
});

// ---------- 在线播放器（/m3u8） ----------
// 游客也能用。链接放在网址的 # 后面，只在浏览器里用；最近播放和进度只存在本机。

const RECENT_KEY = 'mv:recent-links', RECENT_MAX = 10;
const linkInput = d.getElementById('manual-url'), linkError = d.getElementById('manual-error');

function recentList() {
  try { return JSON.parse(localStorage.getItem(RECENT_KEY) || '[]').filter((item) => item && typeof item.url === 'string'); } catch { return []; }
}
function saveRecent(list) {
  try { localStorage.setItem(RECENT_KEY, JSON.stringify(list.slice(0, RECENT_MAX))); } catch { /* 存不了就算了 */ }
}
/** 记下一个链接（放到最前面），可以顺带更新进度。render 为 false 时只存不重画列表（播放中定时保存用）。 */
function rememberLink(url, patch = {}, render = true) {
  const list = recentList();
  const old = list.find((item) => item.url === url) ?? { url, pos: 0, dur: 0 };
  saveRecent([{ ...old, ...patch, at: Date.now() }, ...list.filter((item) => item.url !== url)]);
  if (render) renderRecent();
}

/** 显示用的短地址：域名 + 文件名。 */
function shortURL(raw) {
  try {
    const url = new URL(raw);
    const name = url.pathname.split('/').filter(Boolean).pop() ?? '';
    return url.hostname + (name ? ' · ' + decodeURIComponent(name) : '');
  } catch { return raw; }
}

function renderRecent() {
  const box = d.getElementById('recent'), section = d.getElementById('recent-sec');
  if (!box) return;
  const list = recentList();
  section.hidden = !list.length;
  box.replaceChildren(...list.map((item) => {
    const progress = item.done ? '看完了' : item.pos > 5 && item.dur ? `看到 ${fmt(item.pos)} / ${fmt(item.dur)}` : item.dur ? `时长 ${fmt(item.dur)}` : '';
    const current = state.manual?.url === item.url;
    return h('li', { class: current ? 'is-current' : '' },
      h('button', { type: 'button', class: 'recent-open', title: item.url, onclick: () => openLink(item.url) },
        h('span', { class: 'recent-url', text: shortURL(item.url) }), progress && h('small', { text: progress })),
      h('button', { type: 'button', class: 'recent-x', 'aria-label': '删除这条记录', onclick: () => { saveRecent(recentList().filter((x) => x.url !== item.url)); renderRecent(); } }, icon('x')));
  }));
}

function linkProblem(checked) {
  if (checked.ok) return '';
  if (checked.reason === 'http') return '这是 HTTP 链接。浏览器不允许在 HTTPS 页面里播放 HTTP 视频，会先换成 HTTPS 试一次。';
  return '链接格式不对：应该是以 https:// 开头的完整地址。';
}

/** 播放一个链接：更新地址栏的 #、输入框、最近播放，然后开始播。 */
function openLink(raw) {
  const checked = checkURL(raw);
  linkError.hidden = true;
  if (!checked.ok && checked.reason !== 'http') {
    linkError.textContent = linkProblem(checked);
    linkError.hidden = false;
    linkInput.focus();
    return;
  }
  if (checked.reason === 'http') { linkError.textContent = linkProblem(checked); linkError.hidden = false; }
  history.replaceState(null, '', '#' + new URLSearchParams({ u: checked.url }));
  linkInput.value = checked.url;
  const saved = recentList().find((item) => item.url === checked.url);
  saveProgress(); // 换链接之前，先把正在播的那个的进度存下来
  Object.assign(state, { manual: { url: checked.url, insecure: !checked.ok }, resume: saved && !saved.done ? saved.pos || 0 : 0,
    started: false, switched: false, autoplay: true, current: -1 });
  state.failed.clear();
  d.getElementById('vp-title').textContent = shortURL(checked.url);
  d.getElementById('manual-share').hidden = false;
  message.hidden = true;
  delete vp.dataset.idle;
  renderRecent(); // 能播起来才记进最近播放（见 loadedmetadata）
  findSources();
  if (!fine) d.getElementById('vp').scrollIntoView({ block: 'start', behavior: 'smooth' });
}

function setupPlayer() {
  state.media = { title: '在线播放', type: 'movie' };
  state.isMovie = true;
  updateEpisodeUI();
  wake();
  renderRecent();
  d.getElementById('manual-form').addEventListener('submit', (ev) => { ev.preventDefault(); openLink(linkInput.value); });
  // 粘贴一个完整的链接就直接开始播，不用再点按钮。
  linkInput.addEventListener('paste', () => setTimeout(() => { if (checkURL(linkInput.value).url) openLink(linkInput.value); }, 0));
  linkInput.addEventListener('input', () => { linkError.hidden = true; });
  d.getElementById('recent-clear').addEventListener('click', () => { saveRecent([]); renderRecent(); });
  d.getElementById('manual-share').addEventListener('click', async () => {
    const link = location.origin + location.pathname + '#' + new URLSearchParams({ u: state.manual?.url ?? '' });
    try { await navigator.clipboard.writeText(link); toast('已复制，对方打开就能播'); } catch { prompt('复制这个链接', link); }
  });
  // 把链接拖到播放器上也能播。
  vp.addEventListener('dragover', (ev) => ev.preventDefault());
  vp.addEventListener('drop', (ev) => {
    const text = ev.dataTransfer?.getData('text/uri-list') || ev.dataTransfer?.getData('text/plain') || '';
    if (text.trim()) { ev.preventDefault(); openLink(text.trim().split(/\s+/)[0]); }
  });
  addEventListener('hashchange', () => { const u = new URLSearchParams(location.hash.slice(1)).get('u'); if (u && u !== state.manual?.url) openLink(u); });
  const fromHash = new URLSearchParams(location.hash.slice(1)).get('u');
  if (fromHash) { openLink(fromHash); return; }
  vp.dataset.idle = '1';
  showMessage(h('p', { text: '粘贴视频链接，开始播放' }),
    h('button', { class: 'btn btn-glass', type: 'button', text: '输入链接', onclick: () => linkInput.focus() }));
}

// ---------- 开始 ----------

(async () => {
  if (!(await setup())) return;
  updateEpisodeUI();
  wake();
  await findSources();
})();
