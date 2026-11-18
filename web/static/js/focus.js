/* Moovie tvui-lab · focus.js（按需加载，约 1.5KB）
 * 方向键空间导航：按几何位置找下一个可聚焦元素，Apple TV 式的焦点移动。
 * 只在用户第一次按方向键时由 app.js 注入，触屏用户永远不会下载它。
 */
(() => {
  'use strict';
  const d = document;
  const SEL = 'a.card, .ep, a.nav-link, .btn, .hero-dot, .chip-l, .seg button, .hot a, .menu a, .row-more';
  const DIR = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] };

  const visible = (el) => {
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden';
  };
  const center = (r) => [r.left + r.width / 2, r.top + r.height / 2];

  const pick = (from, [dx, dy]) => {
    const fr = from.getBoundingClientRect();
    const [fx, fy] = center(fr);
    const sameRail = from.closest('.rail');
    let best = null, bestScore = Infinity;
    for (const el of d.querySelectorAll(SEL)) {
      if (el === from || !visible(el)) continue;
      const r = el.getBoundingClientRect();
      const [x, y] = center(r);
      const main = (x - fx) * dx + (y - fy) * dy;          // 沿方向的距离
      if (main <= 4) continue;
      const cross = Math.abs((x - fx) * dy) + Math.abs((y - fy) * dx); // 偏离方向的距离
      // 左右移动时强烈偏好同一行；上下移动时偏好水平位置接近的
      let score = main + cross * (dx ? 4 : 1.5);
      if (dx && sameRail && el.closest('.rail') === sameRail) score -= 1e4;
      if (score < bestScore) { bestScore = score; best = el; }
    }
    return best;
  };

  const move = (key) => {
    const cur = d.activeElement && d.activeElement !== d.body ? d.activeElement.closest(SEL) : null;
    let next;
    if (!cur) {
      next = [...d.querySelectorAll(SEL)].find((el) => visible(el) && el.getBoundingClientRect().top >= 0);
    } else {
      next = pick(cur, DIR[key]);
    }
    if (!next) return;
    next.focus({ preventScroll: true, focusVisible: true });
    next.scrollIntoView({ block: 'nearest', inline: 'center', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
    // 焦点变化同步氛围色
    const c = next.style.getPropertyValue('--c') || (next.closest('[style*="--c"]') || {}).style?.getPropertyValue('--c');
    if (c) d.body.style.setProperty('--ambient', c.trim());
    // 横向行里纵向移动后，若到了 Hero，则同步轮播位置
    const slide = next.closest('.slide');
    if (slide) slide.parentElement.scrollTo({ left: slide.offsetLeft, behavior: 'smooth' });
  };

  d.addEventListener('keydown', (ev) => {
    if (!DIR[ev.key] || ev.altKey || ev.metaKey || ev.ctrlKey) return;
    if (/INPUT|TEXTAREA|SELECT/.test(d.activeElement.tagName)) return;
    if (d.querySelector('dialog[open]')) return;
    ev.preventDefault();
    move(ev.key);
  });
  window.mvFocus = move;
})();
