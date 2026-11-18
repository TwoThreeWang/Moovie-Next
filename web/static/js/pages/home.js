// 首页和"我的"页：把本地的加密历史（没有元数据的作品）插进"继续观看"。
// 服务器只能输出它看得懂的记录；加密的那部分由浏览器解密后补上。
import { h, toast } from '../core/api.js';
import { listPrivate, syncPrivate, promotePrivate, savePrivate } from '../core/history.js';

function remaining(position, duration) {
  if (!duration) return '';
  const left = Math.floor((duration - position) / 60);
  if (left <= 0) return '即将看完';
  return left >= 60 ? `剩 ${Math.floor(left / 60)} 小时 ${left % 60} 分` : `剩 ${left} 分钟`;
}

function privateCard(record) {
  const parts = [];
  if (record.type !== 'movie' && record.ep) parts.push(`第 ${record.ep} 集`);
  const left = remaining(record.pos, record.dur);
  if (left) parts.push(left);
  const percent = record.dur ? Math.min(100, (record.pos / record.dur) * 100) : 0;
  return h('a', {
    class: 'card card-wide', href: '/play/local#rid=' + record.rid, 'data-no-prerender': true,
    dataset: { id: record.rid, rid: record.rid, local: '1', history: '1', title: record.title, meta: parts.join(' · '), at: String(record.at) },
  }, h('div', { class: 'card-art card-art-local', dataset: { initial: [...(record.title || '?')][0] } },
    // 海报是资源站的地址：源挂了图片就加载不出来，这时去掉图片，露出片名首字
    record.pic?.startsWith('https://') ? h('img', { src: record.pic, loading: 'lazy', decoding: 'async', alt: '', referrerpolicy: 'no-referrer', onerror: (ev) => ev.target.remove() }) : null,
    h('div', { class: 'card-over' }, h('span', { class: 'card-title', text: record.title }), h('span', { class: 'card-meta', text: parts.join(' · ') })),
    h('div', { class: 'progress' }, h('i', { style: `--p:${percent.toFixed(0)}%` }))));
}

function insertByTime(container, card) {
  const at = Number(card.dataset.at);
  for (const existing of container.children) {
    if (existing.dataset.rid === card.dataset.rid) { existing.replaceWith(card); return; }
  }
  const after = [...container.children].find((el) => Number(el.dataset.at || 0) < at);
  container.insertBefore(card, after ?? null);
}

async function render() {
  const records = await listPrivate();
  const row = document.querySelector('[data-continue]'), list = document.querySelector('[data-continue-list]');
  if (list || (!row && document.querySelector('[data-empty]') && location.pathname === '/me/history')) {
    let grid = list;
    if (!grid && records.length) {
      // 服务器那边没有记录，但本地有加密历史：把空状态换成列表。
      grid = h('div', { class: 'grid grid-wide', dataset: { continueList: '' } });
      document.querySelector('[data-empty]').replaceWith(grid);
    }
    if (grid) for (const record of records) insertByTime(grid, privateCard(record));
    return;
  }
  if (!row || !records.length) return;
  let rail = row.querySelector('.rail');
  if (!rail) {
    rail = h('div', { class: 'rail rail-wide' });
    row.append(h('section', { class: 'row' },
      h('div', { class: 'row-head' }, h('h2', { class: 'row-title', text: location.pathname === '/me' ? '观看历史' : '继续观看' }),
        h('a', { class: 'row-more', href: '/me/history', text: '全部' })), rail));
  }
  for (const record of records.slice(0, 12)) insertByTime(rail, privateCard(record));
}

document.addEventListener('mv:remove-private', (ev) => {
  savePrivate(ev.detail.rid, null).catch(() => toast('删除没有同步成功，稍后会重试'));
});

render().catch(() => {});
// 先显示本地已有的，再和服务器同步；有变化时重新显示。
syncPrivate().then(async (changed) => {
  const promoted = await promotePrivate();
  if (changed || promoted) await render();
  if (promoted) toast(`${promoted} 部作品已被作品库收录，观看记录已转为普通记录`);
}).catch(() => {});
