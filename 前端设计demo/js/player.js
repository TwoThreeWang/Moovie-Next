/* Moovie tvui-lab · player.js
 * 轻量播放控制层：替代 artplayer。
 * - HLS：Safari / iOS 走原生；其余浏览器才按需加载 hls.js
 * - 触屏：单击显隐控件、双击左右 ±10s、横滑拖进度、全屏时右侧竖滑调亮度
 * - 鼠标键盘：悬停预览时间、空格/K 播放、←/→ 跳转、F 全屏、M 静音、N 下一集、D 弹幕、T 剧场
 * - 跳过片头、下一集倒计时、进度记忆、弹幕层、倍速、画中画、换源
 */
(() => {
  'use strict';
  const d = document;
  const vp = d.getElementById('vp');
  if (!vp) return;
  const v = vp.querySelector('video');
  const cfg = JSON.parse(vp.dataset.cfg);
  const q = (s) => vp.querySelector(s);
  const bar = q('#vp-bar'), hint = q('#vp-hint'), skipBtn = q('#vp-skip'), nextCard = q('#vp-next'), menu = q('#vp-menu');
  const fine = matchMedia('(hover: hover) and (pointer: fine)').matches;
  const params = new URLSearchParams(location.search);
  const ep = Math.max(1, +params.get('ep') || 1);
  const hasNext = cfg.series && ep < cfg.total;
  const KEY = `mv:pos:${cfg.id}:${ep}`;

  const fmt = (t) => {
    t = Math.max(0, Math.floor(t || 0));
    const h = Math.floor(t / 3600), m = Math.floor((t % 3600) / 60), s = t % 60;
    return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(s).padStart(2, '0');
  };
  const store = {
    get: (k) => { try { return localStorage.getItem(k); } catch { return null; } },
    set: (k, val) => { try { localStorage.setItem(k, val); } catch {} },
  };

  /* ---------- 集数 / 标题 ---------- */
  if (cfg.series) {
    d.getElementById('vp-sub').textContent = `第 ${ep} 集`;
    const cur = d.querySelector(`#ep-grid [data-ep="${ep}"]`);
    if (cur) { cur.setAttribute('aria-current', 'true'); cur.scrollIntoView({ block: 'nearest' }); }
  }
  const nextHref = hasNext ? `player-${cfg.id}.html?ep=${ep + 1}` : '';
  if (!hasNext) q('[data-act="next"]')?.remove();

  /* ---------- 加载视频：原生 HLS 优先 ---------- */
  let hlsLoaded = false;
  const loadScript = (src) => new Promise((ok, fail) => {
    const s = d.createElement('script');
    s.src = src; s.onload = ok; s.onerror = fail;
    d.head.append(s);
  });
  const attach = () => {
    if (hlsLoaded) return Promise.resolve();
    hlsLoaded = true;
    vp.classList.add('is-loading');
    if (v.canPlayType('application/vnd.apple.mpegurl')) {
      v.src = cfg.src;
      return Promise.resolve();
    }
    return loadScript('js/hls.min.js').then(() => {
      if (!window.Hls || !Hls.isSupported()) { hint.textContent = '当前浏览器不支持播放'; return; }
      const h = new Hls({ maxBufferLength: 30, startLevel: -1, capLevelToPlayerSize: true });
      h.loadSource(cfg.src);
      h.attachMedia(v);
      h.on(Hls.Events.ERROR, (_, data) => { if (data.fatal) showHint('线路异常，正在切换…', 2000); });
    });
  };
  // 页面空闲时预热（用户来这页就是为了看），首帧更快
  (window.requestIdleCallback || setTimeout)(() => attach(), { timeout: 1200 });

  const saved = +store.get(KEY) || 0;
  v.addEventListener('loadedmetadata', () => {
    d.getElementById('vp-dur').textContent = fmt(v.duration);
    if (saved > 5 && saved < v.duration - 15) {
      v.currentTime = saved;
      showHint(`从 ${fmt(saved)} 继续播放`, 2200);
    }
  }, { once: true });

  const play = () => attach().then(() => v.play()).catch(() => {});
  const toggle = () => (v.paused ? play() : v.pause());

  /* ---------- 状态同步 ---------- */
  v.addEventListener('play', () => { vp.dataset.state = 'playing'; wake(); });
  v.addEventListener('pause', () => { vp.dataset.state = 'paused'; wake(); });
  v.addEventListener('waiting', () => vp.classList.add('is-loading'));
  v.addEventListener('playing', () => vp.classList.remove('is-loading'));
  v.addEventListener('canplay', () => vp.classList.remove('is-loading'));
  let lastSave = 0, dragging = false;
  v.addEventListener('timeupdate', () => {
    const t = v.currentTime, dur = v.duration || 0;
    if (!dragging) setPos(dur ? t / dur : 0);
    d.getElementById('vp-cur').textContent = fmt(t);
    // 跳过片头：片头 cfg.intro 秒内出现
    skipBtn.classList.toggle('show', t > 1 && t < cfg.intro);
    // 下一集倒计时：最后 20 秒
    const left = dur - t;
    const showNext = hasNext && dur > 60 && left < 20;
    nextCard.classList.toggle('show', showNext);
    if (showNext) nextCard.querySelector('.ring').style.setProperty('--deg', ((20 - left) / 20 * 360).toFixed(0) + 'deg');
    if (t - lastSave > 5 || t < lastSave) { lastSave = t; store.set(KEY, t.toFixed(0)); }
  });
  v.addEventListener('progress', () => {
    if (!v.duration || !v.buffered.length) return;
    bar.style.setProperty('--buf', (v.buffered.end(v.buffered.length - 1) / v.duration * 100).toFixed(1) + '%');
  });
  v.addEventListener('ended', () => { store.set(KEY, '0'); if (hasNext) location.href = nextHref; });
  const setPos = (p) => bar.style.setProperty('--pos', (Math.min(1, Math.max(0, p)) * 100).toFixed(2) + '%');

  /* ---------- 控件自动隐藏 ---------- */
  let idleTimer = 0;
  const wake = () => {
    vp.classList.remove('is-idle');
    clearTimeout(idleTimer);
    if (!v.paused && !menu.classList.contains('show')) idleTimer = setTimeout(() => vp.classList.add('is-idle'), 3000);
  };
  const sleep = () => { if (!v.paused) { clearTimeout(idleTimer); vp.classList.add('is-idle'); } };
  if (fine) {
    vp.addEventListener('pointermove', wake, { passive: true });
    vp.addEventListener('pointerleave', () => !v.paused && (idleTimer = setTimeout(sleep, 800)));
  }

  /* ---------- 中央提示 ---------- */
  let hintTimer = 0;
  function showHint(text, ms = 900) {
    hint.textContent = text;
    hint.classList.add('show');
    clearTimeout(hintTimer);
    hintTimer = setTimeout(() => hint.classList.remove('show'), ms);
  }
  const seekBy = (s) => {
    if (!v.duration) return;
    v.currentTime = Math.min(v.duration - 0.5, Math.max(0, v.currentTime + s));
    showHint(`${s > 0 ? '+' : '−'}${Math.abs(s)} 秒 · ${fmt(v.currentTime)}`);
  };
  const ripple = (side) => {
    const r = q(`.vp-ripple[data-side="${side}"]`);
    r.classList.remove('show'); void r.offsetWidth; r.classList.add('show');
  };

  /* ---------- 进度条：拖动 + 悬停预览 ---------- */
  const ratioAt = (x) => { const r = bar.getBoundingClientRect(); return Math.min(1, Math.max(0, (x - r.left) / r.width)); };
  bar.addEventListener('pointerdown', (ev) => {
    if (!v.duration) return;
    dragging = true;
    bar.setPointerCapture(ev.pointerId);
    bar.classList.add('is-drag');
    const p = ratioAt(ev.clientX);
    setPos(p); bar.style.setProperty('--hov', p * 100 + '%');
    bar.querySelector('.tip').textContent = fmt(p * v.duration);
  });
  bar.addEventListener('pointermove', (ev) => {
    if (!v.duration) return;
    const p = ratioAt(ev.clientX);
    bar.style.setProperty('--hov', p * 100 + '%');
    bar.querySelector('.tip').textContent = fmt(p * v.duration);
    if (dragging) setPos(p);
  });
  const endDrag = (ev) => {
    if (!dragging) return;
    dragging = false;
    bar.classList.remove('is-drag');
    v.currentTime = ratioAt(ev.clientX) * v.duration;
    wake();
  };
  bar.addEventListener('pointerup', endDrag);
  bar.addEventListener('pointercancel', () => { dragging = false; bar.classList.remove('is-drag'); });
  bar.addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowLeft' || ev.key === 'ArrowRight') { ev.preventDefault(); ev.stopPropagation(); seekBy(ev.key === 'ArrowLeft' ? -5 : 5); }
  });

  /* ---------- 画面上的手势 ---------- */
  const isControl = (el) => el.closest('button, a, .vp-bar, .vp-menu, input');
  let g = null, tapTimer = 0, lastTap = 0, lastTapSide = 0;
  const sideOf = (x) => {
    const r = vp.getBoundingClientRect();
    const p = (x - r.left) / r.width;
    return p < 0.38 ? -1 : p > 0.62 ? 1 : 0;
  };
  const immersive = () => d.fullscreenElement === vp || matchMedia('(orientation: landscape) and (max-height: 520px)').matches;

  vp.addEventListener('pointerdown', (ev) => {
    if (isControl(ev.target)) return;
    g = { id: ev.pointerId, x: ev.clientX, y: ev.clientY, t0: v.currentTime, dim: +getComputedStyle(vp).getPropertyValue('--dim') || 0, mode: '', type: ev.pointerType };
  });
  vp.addEventListener('pointermove', (ev) => {
    if (!g || ev.pointerId !== g.id || g.type !== 'touch') return;
    const dx = ev.clientX - g.x, dy = ev.clientY - g.y;
    const r = vp.getBoundingClientRect();
    if (!g.mode && Math.hypot(dx, dy) > 12) {
      if (Math.abs(dx) > Math.abs(dy)) g.mode = 'seek';
      else if (immersive() && g.x > r.left + r.width / 2) g.mode = 'bright';
      else g.mode = 'none';
      if (g.mode !== 'none') vp.setPointerCapture(ev.pointerId);
    }
    if (g.mode === 'seek' && v.duration) {
      const span = Math.min(v.duration, 240);
      g.target = Math.min(v.duration, Math.max(0, g.t0 + dx / r.width * span));
      const delta = g.target - g.t0;
      setPos(g.target / v.duration);
      showHint(`${fmt(g.target)} / ${fmt(v.duration)}  (${delta >= 0 ? '+' : '−'}${fmt(Math.abs(delta))})`, 1200);
      dragging = true;
    } else if (g.mode === 'bright') {
      const dim = Math.min(0.75, Math.max(0, g.dim + dy / r.height));
      vp.style.setProperty('--dim', dim.toFixed(2));
      showHint(`亮度 ${Math.round((1 - dim / 0.75) * 100)}%`, 1000);
    }
  }, { passive: true });
  vp.addEventListener('pointercancel', () => { g = null; dragging = false; });
  vp.addEventListener('pointerup', (ev) => {
    if (!g || ev.pointerId !== g.id) return;
    const st = g; g = null;
    if (st.mode === 'seek') { dragging = false; if (st.target != null) v.currentTime = st.target; wake(); return; }
    if (st.mode) return;
    if (isControl(ev.target)) return;
    if (st.type !== 'touch') { // 鼠标：单击播放/暂停，双击全屏
      const now = Date.now();
      if (now - lastTap < 280) { clearTimeout(tapTimer); fullscreen(); lastTap = 0; return; }
      lastTap = now;
      tapTimer = setTimeout(toggle, 220);
      return;
    }
    // 触屏：双击两侧快退快进；单击显隐控件
    const side = sideOf(ev.clientX), now = Date.now();
    if (side && now - lastTap < 300 && side === lastTapSide) {
      clearTimeout(tapTimer);
      seekBy(side * 10); ripple(side);
      lastTap = now; // 允许连续多次双击累计
      return;
    }
    lastTap = now; lastTapSide = side;
    clearTimeout(tapTimer);
    tapTimer = setTimeout(() => (vp.classList.contains('is-idle') || v.paused ? wake() : sleep()), side ? 300 : 0);
  });

  /* ---------- 按钮 ---------- */
  const fullscreen = () => {
    if (d.fullscreenElement) return d.exitFullscreen();
    if (vp.requestFullscreen) {
      vp.requestFullscreen({ navigationUI: 'hide' }).then(() => screen.orientation?.lock?.('landscape').catch(() => {})).catch(() => {});
    } else if (v.webkitEnterFullscreen) { // iPhone：只能让 <video> 进入系统全屏
      v.webkitEnterFullscreen();
    }
  };
  const actions = {
    toggle,
    back: () => { seekBy(-10); ripple(-1); },
    fwd: () => { seekBy(10); ripple(1); },
    next: () => { if (nextHref) location.href = nextHref; },
    mute: () => { v.muted = !v.muted; showHint(v.muted ? '已静音' : '取消静音'); },
    dm: (b) => {
      const on = vp.dataset.dm !== 'on';
      vp.dataset.dm = on ? 'on' : 'off';
      b.setAttribute('aria-pressed', on);
      showHint(on ? '弹幕已开启' : '弹幕已关闭');
    },
    speed: () => { menu.classList.toggle('show'); wake(); },
    pip: () => {
      if (d.pictureInPictureElement) return d.exitPictureInPicture();
      if (v.requestPictureInPicture) v.requestPictureInPicture().catch(() => {});
      else if (v.webkitSetPresentationMode) v.webkitSetPresentationMode('picture-in-picture');
    },
    fs: fullscreen,
    theater: () => d.getElementById('pl').classList.toggle('is-theater'),
    skip: () => { v.currentTime = cfg.intro; skipBtn.classList.remove('show'); showHint('已跳过片头'); },
  };
  vp.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-act]');
    if (b) { ev.stopPropagation(); actions[b.dataset.act]?.(b); wake(); return; }
    const r = ev.target.closest('[data-rate]');
    if (r) {
      v.playbackRate = +r.dataset.rate;
      menu.querySelectorAll('[data-rate]').forEach((x) => x.setAttribute('aria-checked', x === r));
      d.getElementById('vp-speed').textContent = (+r.dataset.rate).toFixed(r.dataset.rate.length > 3 ? 2 : 1) + '×';
      menu.classList.remove('show');
      showHint(`${r.dataset.rate}× 倍速`);
    } else if (!ev.target.closest('.vp-menu')) menu.classList.remove('show');
  });

  /* ---------- 键盘 ---------- */
  d.addEventListener('keydown', (ev) => {
    if (/INPUT|TEXTAREA/.test(d.activeElement.tagName) || ev.metaKey || ev.ctrlKey || ev.altKey) return;
    const k = ev.key.toLowerCase();
    const map = {
      ' ': toggle, k: toggle, f: fullscreen, m: actions.mute, n: actions.next, t: actions.theater,
      arrowleft: () => seekBy(-10), arrowright: () => seekBy(10), j: () => seekBy(-10), l: () => seekBy(10),
      d: () => actions.dm(q('[data-act="dm"]')),
      arrowup: () => { v.volume = Math.min(1, v.volume + 0.1); showHint(`音量 ${Math.round(v.volume * 100)}%`); },
      arrowdown: () => { v.volume = Math.max(0, v.volume - 0.1); showHint(`音量 ${Math.round(v.volume * 100)}%`); },
    };
    if (!map[k]) return;
    if (k === ' ' && d.activeElement.closest('button, a')) return;
    ev.preventDefault();
    map[k]();
    wake();
  });

  /* ---------- 弹幕层（只用 transform 动画） ---------- */
  const dmLayer = q('.vp-dm');
  const DM = ['前方高能', '这个镜头绝了', '配乐好评', '又是雾', '老陈好帅', '第二季终于来了', '码头这场戏我看了三遍', '有人和我一样在吃饭吗',
              '这个转场', '演技炸裂', '摄影是电影级的', '弹幕护体', '哈哈哈哈哈', '细节：船票日期', '我就知道是她', '泪目'];
  const lanes = 6;
  const laneFree = new Array(lanes).fill(0);
  const spawn = (text, mine = false) => {
    if (vp.dataset.dm !== 'on' || d.hidden) return;
    const now = performance.now();
    let lane = laneFree.findIndex((t) => t < now);
    if (lane < 0) { if (!mine) return; lane = 0; }
    const s = d.createElement('span');
    s.textContent = text;
    s.style.top = `${(lane / lanes) * 100}%`;
    const dur = 7 + Math.random() * 3;
    s.style.setProperty('--d', dur + 's');
    if (mine) s.style.cssText += ';outline:1.5px solid rgba(255,255,255,.8);outline-offset:2px;border-radius:4px;padding:0 4px';
    dmLayer.append(s);
    laneFree[lane] = now + dur * 1000 * 0.35;
    s.addEventListener('animationend', () => s.remove(), { once: true });
  };
  setInterval(() => { if (!v.paused && Math.random() < 0.7) spawn(DM[(Math.random() * DM.length) | 0]); }, 900);
  d.getElementById('dm-form')?.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const inp = ev.target.elements.t;
    const t = inp.value.trim();
    if (!t) return;
    spawn(t, true);
    inp.value = '';
    window.mvToast && mvToast('弹幕已发送');
  });

  /* ---------- 换源 ---------- */
  d.getElementById('src')?.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-src]');
    if (!b || b.getAttribute('aria-checked') === 'true') return;
    b.parentElement.querySelectorAll('[data-src]').forEach((x) => x.setAttribute('aria-checked', x === b));
    const t = v.currentTime;
    vp.classList.add('is-loading');
    setTimeout(() => { vp.classList.remove('is-loading'); v.currentTime = t; }, 500); // 原型：模拟同集换源并保持进度
    window.mvToast && mvToast(`已切换到 ${b.dataset.src.split(' · ')[0]}`);
  });

  /* ---------- 离开前保存进度；不注册 unload，保证 bfcache ---------- */
  addEventListener('pagehide', () => store.set(KEY, v.currentTime.toFixed(0)));
  wake();
})();
