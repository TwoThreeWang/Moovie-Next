/* Moovie tvui-lab · app.js
 * 只做增强：页面在 JS 加载失败时依然可看、可点、可播。
 * 功能：导航滚动态 / 懒加载内容行 / Hero 轮播 / 行内翻页 / 卡片视差 /
 *      长按与右键菜单 / 想看与看过 / 预取 / 键盘导航按需加载 / 详情页小交互
 */
(() => {
  'use strict';
  const d = document;
  const mq = (q) => matchMedia(q);
  const fine = mq('(hover: hover) and (pointer: fine)');
  const reduce = mq('(prefers-reduced-motion: reduce)');
  const $ = (s, el = d) => el.querySelector(s);
  const $$ = (s, el = d) => el.querySelectorAll(s);

  /* ---------- Toast ---------- */
  let toastTimer;
  const toast = (msg) => {
    const t = $('#toast');
    if (!t) return;
    t.textContent = msg;
    t.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove('show'), 1800);
  };
  window.mvToast = toast;

  /* ---------- 1. 顶部导航：滚动后加背景 ---------- */
  const nav = $('.nav'), dTop = $('.d-top');
  let ticking = false;
  const onScroll = () => {
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(() => {
      nav && nav.classList.toggle('is-scrolled', scrollY > 24);
      dTop && dTop.classList.toggle('is-scrolled', scrollY > 280);
      ticking = false;
    });
  };
  addEventListener('scroll', onScroll, { passive: true });
  onScroll();

  /* ---------- 2. 懒加载：离视口 800px 时触发 htmx 'near' 事件 ---------- */
  const lazyIO = 'IntersectionObserver' in window && new IntersectionObserver((entries) => {
    for (const en of entries) {
      if (!en.isIntersecting) continue;
      lazyIO.unobserve(en.target);
      window.htmx && htmx.trigger(en.target, 'near');
    }
  }, { rootMargin: '800px 0px' });

  /* ---------- 3. 行内左右翻页（只在精确指针下） ---------- */
  const arrow = (dir) => {
    const b = d.createElement('button');
    b.className = 'rail-nav';
    b.dataset.dir = dir;
    b.tabIndex = -1;
    b.setAttribute('aria-hidden', 'true');
    b.innerHTML = '<svg class="i"><use href="#i-chev-l"/></svg>';
    return b;
  };
  const initRail = (rail) => {
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
    for (const b of [prev, next]) {
      b.addEventListener('click', () => rail.scrollBy({ left: rail.clientWidth * 0.85 * +b.dataset.dir, behavior: reduce.matches ? 'auto' : 'smooth' }));
    }
    rail.addEventListener('scroll', sync, { passive: true });
    sync();
  };

  /* ---------- 4. Hero 轮播：scroll-snap 负责滑动，JS 只同步圆点和氛围色 ---------- */
  const initHero = () => {
    const track = $('#hero-track');
    if (!track || track.dataset.init) return;
    track.dataset.init = '1';
    const slides = [...track.children];
    const dots = [...$$('.hero-dot')];
    const INTERVAL = 7000;
    let idx = 0, timer = 0, paused = false;
    const auto = fine.matches && !reduce.matches;
    track.style.setProperty('--hero-interval', auto ? INTERVAL + 'ms' : '0s');
    d.documentElement.style.setProperty('--hero-interval', auto ? INTERVAL + 'ms' : '0s');

    const setActive = (i) => {
      if (i === idx && dots[i].getAttribute('aria-current')) return;
      idx = i;
      dots.forEach((dot, k) => k === i ? dot.setAttribute('aria-current', 'true') : dot.removeAttribute('aria-current'));
      d.body.style.setProperty('--ambient', slides[i].dataset.c);
    };
    const go = (i) => track.scrollTo({ left: track.clientWidth * i, behavior: reduce.matches ? 'auto' : 'smooth' });
    const schedule = () => {
      clearTimeout(timer);
      if (!auto || paused || d.hidden) return;
      timer = setTimeout(() => go((idx + 1) % slides.length), INTERVAL);
    };
    let raf = 0;
    track.addEventListener('scroll', () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        const i = Math.round(track.scrollLeft / track.clientWidth);
        if (i !== idx) { setActive(i); schedule(); }
      });
    }, { passive: true });
    dots.forEach((dot) => dot.addEventListener('click', () => go(+dot.dataset.i)));
    const hero = track.parentElement;
    hero.addEventListener('pointerenter', () => { paused = true; clearTimeout(timer); hero.classList.add('is-paused'); });
    hero.addEventListener('pointerleave', () => { paused = false; hero.classList.remove('is-paused'); restartDot(); schedule(); });
    hero.addEventListener('focusin', () => { paused = true; clearTimeout(timer); });
    d.addEventListener('visibilitychange', schedule);
    const restartDot = () => { const c = dots[idx]; c.removeAttribute('aria-current'); void c.offsetWidth; c.setAttribute('aria-current', 'true'); };
    schedule();
  };

  /* ---------- 5. 卡片视差高光（精确指针） ---------- */
  if (fine.matches && !reduce.matches) {
    let art = null, pending = null;
    d.addEventListener('pointermove', (ev) => {
      const a = ev.target.closest && ev.target.closest('.card-art');
      if (art && art !== a) { art.style.removeProperty('--rx'); art.style.removeProperty('--ry'); }
      art = a;
      if (!a) return;
      pending = ev;
      requestAnimationFrame(() => {
        if (!pending || !art) return;
        const r = art.getBoundingClientRect();
        const x = (pending.clientX - r.left) / r.width, y = (pending.clientY - r.top) / r.height;
        art.style.setProperty('--gx', (x * 100).toFixed(1) + '%');
        art.style.setProperty('--gy', (y * 100).toFixed(1) + '%');
        art.style.setProperty('--ry', ((x - 0.5) * 8).toFixed(2) + 'deg');
        art.style.setProperty('--rx', ((0.5 - y) * 8).toFixed(2) + 'deg');
        pending = null;
      });
    }, { passive: true });
  }

  /* ---------- 6. 长按（触屏）/ 右键（鼠标）打开操作面板 ---------- */
  const sheet = $('#sheet');
  let sheetCard = null, pressTimer = 0, pressXY = null, suppressClick = false;
  const openSheet = (card) => {
    if (!sheet || !card.dataset.id || sheet.open) return;
    sheetCard = card;
    const id = card.dataset.id;
    $('#sheet-img').src = card.dataset.img;
    $('#sheet-title').textContent = card.dataset.title;
    $('#sheet-meta').textContent = card.dataset.meta;
    $('#sheet-play').href = `player-${id}.html`;
    $('#sheet-detail').href = `detail-${id}.html`;
    $('[data-sheet="remove"]', sheet).hidden = !card.dataset.history;
    navigator.vibrate && navigator.vibrate(8);
    sheet.showModal();
  };
  if (sheet) {
    sheet.addEventListener('click', (ev) => {
      if (ev.target === sheet) return sheet.close(); // 点遮罩关闭
      const act = ev.target.closest('[data-sheet]');
      if (!act) return;
      const kind = act.dataset.sheet;
      sheet.close();
      if (kind === 'wish') toast('已加入想看');
      if (kind === 'seen') toast('已标记为看过');
      if (kind === 'remove' && sheetCard) {
        const c = sheetCard;
        c.animate([{ opacity: 1, transform: 'scale(1)' }, { opacity: 0, transform: 'scale(.9)' }], { duration: 220, easing: 'ease-out' }).onfinish = () => c.remove();
        toast('已从观看记录移除');
      }
    });
  }
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
  ['pointermove', 'pointerup', 'pointercancel'].forEach((t) => d.addEventListener(t, cancelPress, { passive: true }));
  d.addEventListener('click', (ev) => {
    if (suppressClick) { suppressClick = false; ev.preventDefault(); ev.stopPropagation(); }
  }, true);
  d.addEventListener('contextmenu', (ev) => {
    const card = ev.target.closest('.card[data-id]');
    if (!card) return;
    ev.preventDefault();
    openSheet(card);
  });

  /* ---------- 7. 想看 / 看过 / 点赞：乐观更新（生产环境换成 hx-post） ---------- */
  d.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-wish], [data-seen], [data-like], [data-share], [data-more]');
    if (!b) return;
    if (b.dataset.more) {
      const el = d.getElementById(b.dataset.more);
      const open = el.classList.toggle('is-open');
      b.textContent = open ? '收起' : '更多';
      return;
    }
    if (b.hasAttribute('data-share')) {
      const data = { title: d.title, url: location.href };
      if (navigator.share) navigator.share(data).catch(() => {});
      else navigator.clipboard && navigator.clipboard.writeText(location.href).then(() => toast('链接已复制'));
      return;
    }
    const on = b.getAttribute('aria-pressed') !== 'true';
    if (b.hasAttribute('data-like')) {
      const n = $('span', b);
      n.textContent = +n.textContent + (on ? 1 : -1);
      b.setAttribute('aria-pressed', on);
      return;
    }
    const key = b.hasAttribute('data-wish') ? 'wish' : 'seen';
    // 同一部作品的所有同类按钮一起变（Hero / 吸底栏 / 侧栏）
    $$(`[data-${key}="${b.dataset[key]}"]`).forEach((x) => {
      x.setAttribute('aria-pressed', on);
      const use = $('use', x);
      if (use && key === 'wish') use.setAttribute('href', on ? '#i-check' : '#i-plus');
    });
    toast(key === 'wish' ? (on ? '已加入想看' : '已移出想看') : (on ? '已标记为看过' : '已取消看过'));
  });

  /* ---------- 8. 预取：没有 Speculation Rules 的浏览器（Safari / Firefox） ---------- */
  const hasSpec = HTMLScriptElement.supports && HTMLScriptElement.supports('speculationrules');
  if (!hasSpec) {
    const done = new Set();
    const prefetch = (a) => {
      if (!a || a.hasAttribute('data-no-prerender') || a.origin !== location.origin) return;
      const url = a.href.split('#')[0];
      if (done.has(url) || url === location.href.split('#')[0]) return;
      done.add(url);
      fetch(url, { credentials: 'same-origin', priority: 'low' }).catch(() => {});
    };
    let hoverTimer;
    d.addEventListener('touchstart', (ev) => prefetch(ev.target.closest && ev.target.closest('a[href]')), { passive: true });
    d.addEventListener('mouseover', (ev) => {
      const a = ev.target.closest && ev.target.closest('a[href]');
      clearTimeout(hoverTimer);
      if (a) hoverTimer = setTimeout(() => prefetch(a), 65);
    }, { passive: true });
  }

  /* ---------- 9. 键盘导航：第一次按方向键时才加载 focus.js ---------- */
  let focusLoaded = false;
  d.addEventListener('keydown', (ev) => {
    if (ev.key === '/' && !/INPUT|TEXTAREA/.test(d.activeElement.tagName) && !d.querySelector('#vp')) { ev.preventDefault(); location.href = 'search.html'; }
  });
  d.addEventListener('keydown', (ev) => {
    if (focusLoaded || !/^Arrow/.test(ev.key) || /INPUT|TEXTAREA|VIDEO/.test(d.activeElement.tagName) || d.querySelector('#vp')) return;
    focusLoaded = true;
    const s = d.createElement('script');
    s.src = 'js/focus.js';
    s.onload = () => window.mvFocus && window.mvFocus(ev.key);
    d.head.append(s);
  });

  /* ---------- 10. 详情页：Hero 按钮滚出视口后显示吸底栏 ---------- */
  const acts = $('#d-actions'), bar = $('#d-bar');
  if (acts && bar && 'IntersectionObserver' in window) {
    new IntersectionObserver(([en]) => bar.classList.toggle('show', !en.isIntersecting && en.boundingClientRect.top < 0)).observe(acts);
  }
  // 返回：有站内历史就后退（命中 bfcache，秒回），否则走链接
  d.addEventListener('click', (ev) => {
    const a = ev.target.closest('[data-back]');
    if (a && history.length > 1 && d.referrer.startsWith(location.origin)) { ev.preventDefault(); history.back(); }
  });
  // 季切换：同步分段控件选中态
  d.addEventListener('htmx:beforeRequest', (ev) => {
    const btn = ev.target.closest && ev.target.closest('.seg button');
    if (!btn) return;
    $$('button', btn.parentElement).forEach((b) => b.setAttribute('aria-selected', b === btn));
  });

  /* ---------- 初始化：首屏 + 每次 htmx 换入新内容 ---------- */
  const scan = (root) => {
    if (lazyIO) $$('[data-lazy]', root).forEach((el) => lazyIO.observe(el));
    else $$('[data-lazy]', root).forEach((el) => window.htmx && htmx.trigger(el, 'near'));
    $$('.rail', root).forEach(initRail);
    if (root.matches && root.matches('.rail')) initRail(root);
  };
  const boot = () => {
    initHero();
    scan(d);
    if (window.htmx) htmx.onLoad((el) => el !== d.body && scan(el));
  };
  if (d.readyState === 'loading') d.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
