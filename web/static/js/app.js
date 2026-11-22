// 全站通用的交互。页面在这个脚本加载失败时依然可看、可点。
// 导航滚动态 / 懒加载内容行 / 首页轮播 / 行内翻页 / 卡片视差 / 长按菜单 / 弹窗 / 预取 / 键盘导航按需加载
import { api, csrfToken, h, icon, toast, userId, showSyncError } from './core/api.js';
import * as sources from './core/sources.js';
import { flushPositions } from './core/history.js';
import * as install from './core/install.js';
import { detectPlatform, isStandalone } from './lib/platform.js';
import { collapseEpisodes } from './core/episodes.js';

const d = document;
const fine = matchMedia('(hover: hover) and (pointer: fine)');
const reduce = matchMedia('(prefers-reduced-motion: reduce)');
const $ = (s, el = d) => el.querySelector(s);
const $$ = (s, el = d) => el.querySelectorAll(s);

// ---------- HTMX：每个请求带上 CSRF 令牌 ----------
d.addEventListener('htmx:configRequest', (ev) => { ev.detail.headers['X-CSRF-Token'] = csrfToken(); });
d.addEventListener('htmx:responseError', (ev) => {
  const text = ev.detail.xhr?.responseText ?? '';
  let message = text;
  try { message = JSON.parse(text).error ?? text; } catch { /* 不是 JSON 就原样显示 */ }
  toast(message.length < 80 && message ? message : '操作失败，请稍后再试');
});
d.addEventListener('htmx:sendError', () => toast('网络连接失败'));

// 局部加载时顶部的细进度条：请求超过 150ms 才出现。懒加载的内容行不算（用户没在等它）。
const loadingBar = $('#loading-bar');
let pending = 0, barTimer = 0;
const startBar = () => {
  clearTimeout(barTimer);
  barTimer = setTimeout(() => { loadingBar.classList.remove('done'); void loadingBar.offsetWidth; loadingBar.classList.add('run'); }, 150);
};
d.addEventListener('htmx:beforeRequest', (ev) => {
  const elt = ev.detail.elt;
  if (!loadingBar || elt?.matches?.('[data-lazy], .grid-more, .fetching')) return;
  elt.dataset.bar = '1';
  if (pending++ === 0) startBar();
});
// 整页跳转也显示：添加到桌面后没有浏览器的加载指示，点了链接要等下一页回来才有反应。
// 等这次点击的所有处理函数跑完再看有没有被拦下（HTMX、站内的返回按钮等会自己处理）。
if (loadingBar) {
  const leaving = (ev, target, sameTab) => setTimeout(() => { if (!ev.defaultPrevented && sameTab && target) startBar(); });
  d.addEventListener('click', (ev) => {
    const link = ev.target.closest?.('a[href]');
    if (!link || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey || link.hasAttribute('download')) return;
    const samePage = link.pathname === location.pathname && link.search === location.search;
    leaving(ev, link.origin === location.origin && !samePage, !link.target || link.target === '_self');
  });
  d.addEventListener('submit', (ev) => {
    const form = ev.target;
    leaving(ev, !form.matches('[hx-get], [hx-post], [hx-put], [hx-patch], [hx-delete]'), !form.target || form.target === '_self');
  });
  // 从前进后退缓存里恢复的页面，把上次离开时的进度条收起来。
  addEventListener('pageshow', (ev) => { if (ev.persisted) { clearTimeout(barTimer); loadingBar.classList.remove('run', 'done'); } });
}
d.addEventListener('htmx:afterRequest', (ev) => {
  if (!loadingBar || ev.detail.elt?.dataset?.bar !== '1') return;
  delete ev.detail.elt.dataset.bar;
  if (--pending > 0) return;
  pending = 0;
  clearTimeout(barTimer);
  if (loadingBar.classList.contains('run')) { loadingBar.classList.remove('run'); loadingBar.classList.add('done'); }
});

// ---------- 弹窗（标记看过的表单等） ----------
const modal = $('#modal');
if (modal) {
  d.addEventListener('htmx:afterSwap', (ev) => { if (ev.detail.target.id === 'modal-body' && !modal.open) modal.showModal(); });
  d.addEventListener('close-modal', () => modal.open && modal.close());
  modal.addEventListener('click', (ev) => { if (ev.target === modal || ev.target.closest('[data-close-modal]')) modal.close(); });
}

// 需要确认的表单（删除用户、删除片单）
d.addEventListener('submit', (ev) => {
  const message = ev.target.dataset?.confirm;
  if (message && !confirm(message)) ev.preventDefault();
});

// ---------- 导航：滚动后加背景 ----------
const nav = $('.nav'), dTop = $('.d-top'), filters = $('.sticky-filters');
// 右下角返回顶部：滚过一屏多才出现；播放页不需要。
const toTop = $('main.pl') ? null : h('button', { class: 'to-top', type: 'button', 'aria-label': '返回顶部', title: '返回顶部',
  onclick: () => scrollTo({ top: 0, behavior: reduce.matches ? 'auto' : 'smooth' }) }, icon('chev-l'));
if (toTop) d.body.append(toTop);
let ticking = false;
const onScroll = () => {
  if (ticking) return;
  ticking = true;
  requestAnimationFrame(() => {
    nav?.classList.toggle('is-scrolled', scrollY > 24);
    d.documentElement.classList.toggle('scrolled', scrollY > 4);
    dTop?.classList.toggle('is-scrolled', scrollY > 280);
    toTop?.classList.toggle('show', scrollY > innerHeight * 1.2);
    if (filters) filters.classList.toggle('is-stuck', filters.getBoundingClientRect().top <= parseFloat(getComputedStyle(filters).top) + 1);
    ticking = false;
  });
};
addEventListener('scroll', onScroll, { passive: true });
onScroll();

// ---------- 懒加载：离视口 800px 时触发 htmx 的 near 事件 ----------
// htmx 要到 DOMContentLoaded 才给 hx-trigger 绑上监听，在那之前发出的 near 没人接，占位行会一直停在骨架屏。
// 本脚本比那更早运行，IntersectionObserver 的第一次回调也可能更早：后面的脚本还在下载时浏览器先渲染了一帧
// （iPhone 隔段时间再打开时常见），首屏的几行就全卡住了，滚到下面的行反而正常。所以先记下来，等 htmx 初始化完再触发。
// htmx 的 DOMContentLoaded 监听在它自己的脚本执行时就注册了，比这里早，所以这里的监听运行时它已经处理完页面。
let htmxReady = d.readyState === 'complete';
const pendingNear = [];
const fireNear = (el) => (htmxReady ? window.htmx?.trigger(el, 'near') : pendingNear.push(el));
const flushNear = () => {
  htmxReady = true;
  for (const el of pendingNear.splice(0)) window.htmx?.trigger(el, 'near');
};
d.addEventListener('DOMContentLoaded', flushNear, { once: true });
addEventListener('load', flushNear, { once: true });
const lazyIO = 'IntersectionObserver' in window && new IntersectionObserver((entries) => {
  for (const entry of entries) {
    if (!entry.isIntersecting) continue;
    lazyIO.unobserve(entry.target);
    fireNear(entry.target);
  }
}, { rootMargin: '800px 0px' });

// ---------- 行内左右翻页（只在鼠标下） ----------
function arrow(dir) {
  const button = d.createElement('button');
  button.className = 'rail-nav';
  button.dataset.dir = dir;
  button.tabIndex = -1;
  button.setAttribute('aria-hidden', 'true');
  button.innerHTML = '<svg class="i"><use href="#i-chev-l"/></svg>';
  return button;
}
function initRail(rail) {
  if (rail.dataset.init) return;
  rail.dataset.init = '1';
  const row = rail.closest('.row, .d-sec');
  if (!row || !fine.matches) return;
  const prev = arrow(-1), next = arrow(1);
  row.append(prev, next);
  const sync = () => {
    prev.disabled = rail.scrollLeft < 8;
    next.disabled = rail.scrollLeft + rail.clientWidth > rail.scrollWidth - 8;
  };
  for (const button of [prev, next]) {
    button.addEventListener('click', () => rail.scrollBy({ left: rail.clientWidth * 0.85 * +button.dataset.dir, behavior: reduce.matches ? 'auto' : 'smooth' }));
  }
  rail.addEventListener('scroll', sync, { passive: true });
  sync();
}

// ---------- 首页轮播：scroll-snap 负责滑动，这里只同步圆点和自动翻页 ----------
function initHero() {
  const track = $('#hero-track');
  if (!track || track.dataset.init) return;
  track.dataset.init = '1';
  const slides = [...track.children], dots = [...$$('.hero-dot')];
  if (slides.length < 2) return;
  const INTERVAL = 7000;
  let index = 0, timer = 0, paused = false;
  const auto = fine.matches && !reduce.matches;
  d.documentElement.style.setProperty('--hero-interval', auto ? INTERVAL + 'ms' : '0s');
  const setActive = (i) => {
    index = i;
    dots.forEach((dot, k) => (k === i ? dot.setAttribute('aria-current', 'true') : dot.removeAttribute('aria-current')));
  };
  const go = (i) => track.scrollTo({ left: track.clientWidth * i, behavior: reduce.matches ? 'auto' : 'smooth' });
  const schedule = () => {
    clearTimeout(timer);
    if (!auto || paused || d.hidden) return;
    timer = setTimeout(() => go((index + 1) % slides.length), INTERVAL);
  };
  let raf = 0;
  track.addEventListener('scroll', () => {
    cancelAnimationFrame(raf);
    raf = requestAnimationFrame(() => {
      const i = Math.round(track.scrollLeft / track.clientWidth);
      if (i !== index) { setActive(i); schedule(); }
    });
  }, { passive: true });
  dots.forEach((dot) => dot.addEventListener('click', () => go(+dot.dataset.i)));
  const hero = track.parentElement;
  hero.addEventListener('pointerenter', () => { paused = true; clearTimeout(timer); hero.classList.add('is-paused'); });
  hero.addEventListener('pointerleave', () => { paused = false; hero.classList.remove('is-paused'); schedule(); });
  hero.addEventListener('focusin', () => { paused = true; clearTimeout(timer); });
  d.addEventListener('visibilitychange', schedule);
  schedule();
}

// ---------- 卡片视差高光（鼠标） ----------
if (fine.matches && !reduce.matches) {
  let art = null, pending = null;
  d.addEventListener('pointermove', (ev) => {
    const target = ev.target.closest?.('.card-art');
    if (art && art !== target) { art.style.removeProperty('--rx'); art.style.removeProperty('--ry'); }
    art = target;
    if (!target) return;
    pending = ev;
    requestAnimationFrame(() => {
      if (!pending || !art) return;
      const rect = art.getBoundingClientRect();
      const x = (pending.clientX - rect.left) / rect.width, y = (pending.clientY - rect.top) / rect.height;
      art.style.setProperty('--gx', (x * 100).toFixed(1) + '%');
      art.style.setProperty('--gy', (y * 100).toFixed(1) + '%');
      art.style.setProperty('--ry', ((x - 0.5) * 8).toFixed(2) + 'deg');
      art.style.setProperty('--rx', ((0.5 - y) * 8).toFixed(2) + 'deg');
      pending = null;
    });
  }, { passive: true });
}

// ---------- 长按（触屏）/ 右键（鼠标）打开操作面板 ----------
const sheet = $('#sheet');
let sheetCard = null, pressTimer = 0, pressXY = null, suppressClick = false;
function openSheet(card) {
  if (!sheet || !card.dataset.id || sheet.open) return;
  sheetCard = card;
  const image = $('#sheet-img');
  if (card.dataset.img) { image.src = card.dataset.img; image.hidden = false; } else image.hidden = true;
  $('#sheet-title').textContent = card.dataset.title;
  $('#sheet-meta').textContent = card.dataset.meta ?? '';
  // 本地（加密历史）的卡片没有作品页，只能直接播放。
  const local = card.dataset.local === '1';
  $('#sheet-play').href = local ? card.href : `/play/${card.dataset.id}`;
  $('#sheet-detail').hidden = local;
  $('#sheet-detail').href = `/m/${card.dataset.id}`;
  $('[data-sheet="wish"]', sheet).hidden = local;
  $('[data-sheet="remove"]', sheet).hidden = !card.dataset.history;
  navigator.vibrate?.(8);
  sheet.showModal();
}
sheet?.addEventListener('click', async (ev) => {
  if (ev.target === sheet) return sheet.close(); // 点遮罩关闭
  const action = ev.target.closest('[data-sheet]');
  if (!action || !sheetCard) return;
  const card = sheetCard, id = card.dataset.id;
  sheet.close();
  if (action.dataset.sheet === 'wish') {
    const response = await api(`/api/library/${id}/wish`, { method: 'POST' }).catch(() => null);
    if (!response?.ok) return toast('操作失败');
    const { status } = await response.json();
    toast(status === 'wish' ? '已加入想看' : status === 'watched' ? '已经标记过看过' : '已移出想看');
  }
  if (action.dataset.sheet === 'remove') {
    if (card.dataset.local === '1') {
      d.dispatchEvent(new CustomEvent('mv:remove-private', { detail: { rid: card.dataset.rid } }));
    } else {
      const response = await api(`/api/positions/${id}`, { method: 'DELETE' }).catch(() => null);
      if (!response?.ok) return toast('操作失败');
    }
    card.animate([{ opacity: 1, transform: 'scale(1)' }, { opacity: 0, transform: 'scale(.9)' }], { duration: 220, easing: 'ease-out' }).onfinish = () => card.remove();
    toast('已从观看记录移除');
  }
});
d.addEventListener('pointerdown', (ev) => {
  if (ev.pointerType !== 'touch') return;
  const card = ev.target.closest('.card[data-id]');
  if (!card) return;
  pressXY = [ev.clientX, ev.clientY];
  pressTimer = setTimeout(() => { suppressClick = true; openSheet(card); }, 450);
}, { passive: true });
const cancelPress = (ev) => {
  if (!pressTimer) return;
  if (ev.type === 'pointermove' && pressXY && Math.hypot(ev.clientX - pressXY[0], ev.clientY - pressXY[1]) < 10) return;
  clearTimeout(pressTimer);
  pressTimer = 0;
};
for (const type of ['pointermove', 'pointerup', 'pointercancel']) d.addEventListener(type, cancelPress, { passive: true });
d.addEventListener('click', (ev) => {
  if (suppressClick) { suppressClick = false; ev.preventDefault(); ev.stopPropagation(); }
}, true);
d.addEventListener('contextmenu', (ev) => {
  const card = ev.target.closest('.card[data-id]');
  if (!card) return;
  ev.preventDefault();
  openSheet(card);
});

// ---------- 小交互：展开简介、分享、返回 ----------
d.addEventListener('click', (ev) => {
  const more = ev.target.closest('[data-more]');
  if (more) {
    const open = d.getElementById(more.dataset.more).classList.toggle('is-open');
    more.textContent = open ? '收起' : '更多';
    return;
  }
  // 管理员删除别人的内容：先问原因，原因会写进给作者的通知。
  const remove = ev.target.closest('[data-remove]');
  if (remove) {
    const reason = prompt('删除原因（会通知作者）', '违反社区规范');
    if (reason === null) return;
    const url = remove.dataset.remove + '?reason=' + encodeURIComponent(reason.trim().slice(0, 100));
    window.htmx?.ajax(remove.dataset.method, url, { target: remove.dataset.target, swap: remove.dataset.swap });
    return;
  }
  // 剧照：点开看大图
  const zoom = ev.target.closest('[data-zoom]');
  if (zoom) {
    const all = [...(zoom.closest('[data-gallery]') ?? zoom.parentElement).querySelectorAll('[data-zoom]')];
    const list = all.map((el) => ({ src: el.dataset.zoom, alt: el.querySelector('img')?.alt ?? '' }));
    import('./core/lightbox.js').then((m) => m.openLightbox(list, all.indexOf(zoom))).catch(() => window.open(zoom.dataset.zoom, '_blank', 'noopener'));
    return;
  }
  const share = ev.target.closest('[data-share]');
  if (share) {
    // 有卡片内容的：打开分享面板（生成图片 + 复制链接），模块用到时才加载
    if (share.dataset.card) {
      import('./core/sharecard.js').then((m) => m.openShare(JSON.parse(share.dataset.card))).catch(() => toast('打开分享失败'));
      return;
    }
    const data = { title: d.title, url: new URL(share.dataset.url || location.pathname, location.href).href };
    if (navigator.share) navigator.share(data).catch(() => {});
    else if (navigator.clipboard) navigator.clipboard.writeText(data.url).then(() => toast('链接已复制'), () => prompt('复制链接', data.url));
    else prompt('复制链接', data.url);
    return;
  }
  // 返回：有站内历史就后退（通常命中浏览器的页面缓存，立刻显示），否则走链接。
  const back = ev.target.closest('[data-back]');
  if (back && history.length > 1 && d.referrer.startsWith(location.origin)) { ev.preventDefault(); history.back(); }
});
// 简介不长时不显示"更多"
for (const button of $$('[data-more]')) {
  const target = d.getElementById(button.dataset.more);
  if (target && target.scrollHeight <= target.clientHeight + 2) button.hidden = true;
}

// ---------- 预取：没有 Speculation Rules 的浏览器（Safari / Firefox） ----------
// HTML 不能被浏览器缓存，自己 fetch 一遍打开时还是要重新请求，所以交给 Service Worker 取回放在内存里，
// 打开时由它直接交出（规则见 sw.js 的"预取交接"）。还没有 Service Worker 接管的页面不预取。
// 播放页离开时会保存进度，预取的页面可能比进度旧，所以播放页上不预取。
if (userId() && !HTMLScriptElement.supports?.('speculationrules') && !location.pathname.startsWith('/play')) {
  const done = new Set();
  const prefetch = (link) => {
    const worker = navigator.serviceWorker?.controller;
    if (!worker || !link || link.hasAttribute('data-no-prerender') || link.hasAttribute('download') || link.target === '_blank'
      || link.origin !== location.origin || /^\/(play|auth|admin|notifications)(\/|$)/.test(link.pathname)) return;
    const url = link.href.split('#')[0];
    if (done.has(url) || url === location.href.split('#')[0]) return;
    done.add(url);
    setTimeout(() => done.delete(url), 10000); // 和 Service Worker 里的有效期一致，过期后可以再取
    worker.postMessage({ type: 'prefetch', url });
  };
  let hoverTimer;
  d.addEventListener('touchstart', (ev) => prefetch(ev.target.closest?.('a[href]')), { passive: true });
  d.addEventListener('mouseover', (ev) => {
    const link = ev.target.closest?.('a[href]');
    clearTimeout(hoverTimer);
    if (link) hoverTimer = setTimeout(() => prefetch(link), 65);
  }, { passive: true });
}

// ---------- 离开页面时取消没下完的图片 ----------
// 点了站内链接之后，浏览器还会接着下载这一页没下完的图片，直到新页面回来才停。网络慢的时候，
// 新页面的 HTML 要和这些图片抢带宽，点了好久才跳转。所以离开前把没下完的图片地址先收起来
// （移到 data-parked-* 里），浏览器就会放弃这些请求。下完的图片不动。
// 如果最后没有离开（下载文件、204、导航被取消），或者用户从后退缓存回到这一页，再放回去重新加载。
const PARK_ATTRS = ['srcset', 'src'];
let parkTimer = 0;
function parkImages() {
  for (const img of d.images) {
    if (img.complete) continue;
    const picture = img.parentElement?.tagName === 'PICTURE' ? img.parentElement : null;
    for (const el of [...(picture ? $$('source', picture) : []), img]) {
      for (const attr of PARK_ATTRS) {
        if (!el.hasAttribute(attr)) continue;
        el.setAttribute('data-parked-' + attr, el.getAttribute(attr));
        el.removeAttribute(attr);
      }
    }
  }
  clearTimeout(parkTimer);
  parkTimer = setTimeout(unparkImages, 10000);
}
function unparkImages() {
  clearTimeout(parkTimer);
  for (const el of $$('[data-parked-src], [data-parked-srcset]')) {
    for (const attr of PARK_ATTRS) {
      const value = el.getAttribute('data-parked-' + attr);
      if (value === null) continue;
      el.removeAttribute('data-parked-' + attr);
      el.setAttribute(attr, value);
    }
  }
}
// 挂在 window 上：比 document 上的点击处理晚执行，能看到它们有没有 preventDefault
addEventListener('click', (ev) => {
  if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
  const link = ev.target.closest?.('a[href]');
  if (!link || link.hasAttribute('download') || (link.target && link.target !== '_self') || link.origin !== location.origin) return;
  // 只是跳到本页的某个位置，不算离开
  if (link.pathname === location.pathname && link.search === location.search && link.hash) return;
  parkImages();
});
addEventListener('pageshow', (ev) => { if (ev.persisted) unparkImages(); });

// ---------- 键盘 ----------
let focusLoaded = false;
d.addEventListener('keydown', (ev) => {
  const typing = /INPUT|TEXTAREA|SELECT/.test(d.activeElement?.tagName ?? '');
  if (typing || $('#vp') || !userId()) return;
  if (ev.key === '/') { ev.preventDefault(); location.href = '/search'; return; }
  // 第一次按方向键时才加载空间导航的脚本
  if (!focusLoaded && /^Arrow/.test(ev.key)) {
    focusLoaded = true;
    const script = d.createElement('script');
    script.src = '/static/js/focus.js';
    script.onload = () => window.mvFocus?.(ev.key);
    d.head.append(script);
  }
});

// ---------- 详情页：播放按钮滚出视口后显示吸底栏 ----------
const actions = $('#d-actions'), bar = $('#d-bar');
if (actions && bar && 'IntersectionObserver' in window) {
  new IntersectionObserver(([entry]) => bar.classList.toggle('show', !entry.isIntersecting && entry.boundingClientRect.top < 0)).observe(actions);
}

// ---------- 初始化：首屏，以及每次 htmx 换入新内容 ----------
// 海报加载中显示闪光占位，加载完（或失败）就停。动画只在卡片进入屏幕时才跑（见 site.css），
// 滚出屏幕就停：网络慢的时候几十张卡片同时在加载，全部一起动会让滚动和点击变卡。
const shimmerIO = 'IntersectionObserver' in window && new IntersectionObserver((entries) => {
  for (const entry of entries) entry.target.classList.toggle('in-view', entry.isIntersecting);
});
function shimmer(root) {
  for (const image of $$('.card-art > img', root)) {
    if (image.complete) continue; // 已经加载完；失败的已经被换成了默认图（见 modules.go 的 imageGuard）
    const art = image.parentElement;
    art.classList.add('is-loading');
    if (shimmerIO) shimmerIO.observe(art);
    else art.classList.add('in-view');
    const done = () => {
      art.classList.remove('is-loading', 'in-view');
      if (shimmerIO) shimmerIO.unobserve(art);
    };
    image.addEventListener('load', done, { once: true });
    image.addEventListener('error', done, { once: true });
  }
}

function scan(root) {
  shimmer(root);
  for (const grid of $$('.d-sec .ep-grid', root)) collapseEpisodes(grid);
  for (const el of $$('[data-lazy]', root)) lazyIO ? lazyIO.observe(el) : fireNear(el);
  if (root.matches?.('[data-lazy]')) lazyIO ? lazyIO.observe(root) : fireNear(root);
  $$('.rail', root).forEach(initRail);
  if (root.matches?.('.rail')) initRail(root);
}
initHero();
scan(d);
window.htmx?.onLoad((el) => el !== d.body && scan(el));

// 广告图片加载不出来时整条隐藏，不留一个空框（见 partials/ads.html）。脚本加载前就失败了的也要查一遍。
const hideBrokenAd = (img) => { const ad = img.closest('.ad'); if (ad) ad.hidden = true; };
d.addEventListener('error', (ev) => { if (ev.target.tagName === 'IMG') hideBrokenAd(ev.target); }, true);
for (const img of $$('.ad img')) if (img.complete && !img.naturalWidth) hideBrokenAd(img);

// ---------- 登录后的后台事务 ----------
if (userId()) {
  sources.refreshSubscription().catch(() => {});
  flushPositions();
}

// ---------- 添加到桌面 ----------

// Service Worker：缓存静态文件、断网时显示提示页。页面加载完、空闲时再注册，不和首屏抢资源。
if ('serviceWorker' in navigator) {
  addEventListener('load', () => {
    (window.requestIdleCallback ?? setTimeout)(() => navigator.serviceWorker.register('/sw.js').catch(() => {}), { timeout: 3000 });
  });
}

// 首页提示条：只在手机浏览器里、第二次来的时候出现；关掉后 30 天内不再出现。
function maybeShowInstallTip() {
  if (location.pathname !== '/' || !userId() || isStandalone()) return;
  if (!matchMedia('(pointer: coarse)').matches || innerWidth > 900) return;
  let state = {};
  try { state = JSON.parse(localStorage.getItem('mv:installTip') || '{}'); } catch { /* 用默认值 */ }
  if (localStorage.getItem('mv:installed') || Date.now() - (state.dismissed || 0) < 30 * 86400e3) return;
  state.visits = (state.visits || 0) + 1;
  try { localStorage.setItem('mv:installTip', JSON.stringify(state)); } catch { /* 存不了就每次都算第一次 */ }
  if (state.visits < 2) return;
  const rows = $('.rows');
  if (!rows) return;
  const info = detectPlatform(navigator.userAgent, { maxTouchPoints: navigator.maxTouchPoints, platform: navigator.platform });
  const tip = d.createElement('div');
  tip.className = 'install-tip';
  tip.innerHTML = '<img src="/static/img/apple-touch-icon.png" alt=""><div><b>添加到桌面</b><span></span></div><a class="btn btn-primary" href="/install"></a><button class="close" type="button" aria-label="关闭">×</button>';
  tip.querySelector('span').textContent = info.inApp ? `在${info.appName}里不能添加，先换到浏览器打开` : '像 App 一样全屏打开，启动更快';
  const action = tip.querySelector('.btn');
  action.textContent = '怎么添加';
  const sync = (available) => { action.textContent = available ? '添加' : '怎么添加'; };
  sync(install.canPrompt());
  install.onPromptChange(sync);
  action.addEventListener('click', async (ev) => {
    if (!install.canPrompt()) return; // 没有一键安装：跳到教程
    ev.preventDefault();
    if (await install.prompt()) tip.remove();
  });
  tip.querySelector('.close').addEventListener('click', () => {
    tip.remove();
    try { localStorage.setItem('mv:installTip', JSON.stringify({ ...state, dismissed: Date.now() })); } catch { /* 忽略 */ }
  });
  rows.prepend(tip);
}
try { maybeShowInstallTip(); } catch { /* 提示条出错不影响页面 */ }

// ---------- 账号同步 ----------
// 别的设备改了订阅地址（或者这台设备第一次用这个账号）：在后台同步过来。每 5 分钟最多查一次。
if (userId()) {
  import('./core/keyring.js').then((m) => m.syncKeyring()).catch((error) => showSyncError(error.message || '请稍后重试。'));
}

// ---------- 后台：全选、按钮确认 ----------
d.addEventListener('change', (ev) => {
  const all = ev.target.closest('[data-check-all]');
  if (!all) return;
  for (const box of all.closest('table').querySelectorAll('tbody input[type="checkbox"]')) box.checked = all.checked;
});
d.addEventListener('click', (ev) => {
  const button = ev.target.closest('[data-confirm-click]');
  if (button && !confirm(button.dataset.confirmClick)) ev.preventDefault();
});
