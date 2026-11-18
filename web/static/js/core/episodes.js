// 选集很多（几百集的动漫）时折叠：只显示前两集、最后两集、当前集前后两集，中间用省略号，
// 下面一个"展开全部"按钮。重复调用是安全的（播放页查到新的源后会重新调用）。

const LIMIT = 30;

/** 算出折叠时要显示哪些位置。纯函数，方便测试。 */
export function visibleIndexes(total, current) {
  const keep = new Set();
  for (let i = 0; i < total; i++) {
    if (i < 2 || i >= total - 2 || (current >= 0 && Math.abs(i - current) <= 2)) keep.add(i);
  }
  return keep;
}

export function collapseEpisodes(grid) {
  if (!grid) return;
  const items = [...grid.children].filter((el) => !el.classList.contains('episode-gap'));
  for (const gap of grid.querySelectorAll('.episode-gap')) gap.remove();
  let toggle = grid.nextElementSibling?.classList.contains('episodes-toggle') ? grid.nextElementSibling : null;
  if (items.length <= LIMIT) {
    items.forEach((item) => { item.hidden = false; });
    toggle?.remove();
    return;
  }
  const current = items.findIndex((item) => item.getAttribute('aria-current') === 'true');
  const expanded = grid.dataset.expanded === '1';
  const keep = visibleIndexes(items.length, current);
  let last = -1;
  items.forEach((item, index) => {
    const visible = expanded || keep.has(index);
    item.hidden = !visible;
    if (!expanded && visible && last >= 0 && index - last > 1) {
      const gap = document.createElement('span');
      gap.className = 'episode-gap';
      gap.textContent = '…';
      gap.setAttribute('aria-hidden', 'true');
      grid.insertBefore(gap, item);
    }
    if (visible) last = index;
  });
  if (!toggle) {
    toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.className = 'episodes-toggle';
    toggle.addEventListener('click', () => {
      grid.dataset.expanded = grid.dataset.expanded === '1' ? '0' : '1';
      collapseEpisodes(grid);
    });
    grid.after(toggle);
  }
  toggle.textContent = expanded ? '收起选集' : `展开全部 ${items.length} 集`;
  toggle.setAttribute('aria-expanded', String(expanded));
}
