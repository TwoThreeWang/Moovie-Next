// 选集很多（几百集的动漫）时折叠：只显示前两集、最后两集、当前集前后两集，中间用省略号，
// 下面一个"展开全部"按钮。重复调用是安全的（播放页查到新的源后会重新调用）。
// 综艺按期分行显示（见 renderIssueGrid），一行一期，超过 12 期就折叠。
import { groupIssues, partName } from '../lib/issue.js';

const LIMIT = 30, ISSUE_LIMIT = 12;

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
  const rows = grid.classList.contains('ep-issues');
  if (items.length <= (rows ? ISSUE_LIMIT : LIMIT)) {
    items.forEach((item) => { item.hidden = false; });
    toggle?.remove();
    return;
  }
  const current = items.findIndex((item) => item.getAttribute('aria-current') === 'true' || item.querySelector('[aria-current="true"]'));
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
  toggle.textContent = expanded ? '收起选集' : `展开全部 ${items.length} ${rows ? '期' : '集'}`;
  toggle.setAttribute('aria-expanded', String(expanded));
}

/**
 * 综艺的选集：按期分行，每行是这一期的各段（上、下、加更……）。每一期都只有一段整期时，像剧集一样排成一排数字。
 * codes 是源里有的集号（见 lib/issue.js），href(code) 生成链接，seen 是看完了的集号。
 */
export function renderIssueGrid(grid, codes, { href, current, seen = new Set() }) {
  const link = (code, text) => {
    const a = document.createElement('a');
    a.href = href(code);
    a.dataset.ep = String(code);
    a.dataset.noPrerender = '';
    a.textContent = text;
    if (seen.has(code)) a.dataset.seen = '';
    if (code === current) a.setAttribute('aria-current', 'true');
    return a;
  };
  const { list, flat } = groupIssues(codes);
  grid.classList.toggle('ep-issues', !flat);
  if (flat) {
    grid.replaceChildren(...list.map(({ issue, parts }) => link(parts[0], issue === 0 ? '先导' : String(issue))));
  } else {
    grid.replaceChildren(...list.map(({ issue, parts }) => {
      const row = document.createElement('div');
      row.className = 'ep-issue';
      const name = document.createElement('span');
      name.className = 'ep-issue-no';
      name.textContent = issue === 0 ? '先导片' : `第 ${issue} 期`;
      const cells = document.createElement('div');
      cells.className = 'ep-parts';
      cells.append(...parts.map((code) => link(code, partName(code))));
      row.append(name, cells);
      return row;
    }));
  }
  collapseEpisodes(grid);
}
