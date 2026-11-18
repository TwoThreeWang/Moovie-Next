// 搜索页。上半部分搜的是本站的作品库（由服务器返回）；
// 下半部分"我的源"由浏览器直接查询用户自己的源，本站服务器不参与。
import { api, h } from '../core/api.js';
import * as idb from '../core/idb.js';
import { userKey } from '../core/api.js';
import * as sources from '../core/sources.js';
import { privateId } from '../core/history.js';
import { normalizeTitle } from '../lib/title.js';

const input = document.getElementById('q'), empty = document.getElementById('search-empty');
const section = document.getElementById('source-search'), grid = document.getElementById('source-results'), status = document.getElementById('source-status');
const modal = document.getElementById('modal'), modalBody = document.getElementById('modal-body');

// ---------- 最近搜索（只存在本机） ----------
const RECENT = 'mv:recent';
const readRecent = () => { try { return JSON.parse(localStorage.getItem(RECENT) || '[]'); } catch { return []; } };
const saveRecent = (items) => { try { localStorage.setItem(RECENT, JSON.stringify(items.slice(0, 10))); } catch { /* 存不了就算了 */ } };
function renderRecent() {
  const items = readRecent(), wrap = document.getElementById('recent-wrap');
  wrap.hidden = !items.length;
  document.getElementById('recent').replaceChildren(...items.map((text) => h('a', {
    class: 'chip-l', href: '/search?q=' + encodeURIComponent(text), text,
    onclick: (ev) => { ev.preventDefault(); input.value = text; run(); },
  })));
}
document.getElementById('recent-clear').addEventListener('click', () => { saveRecent([]); renderRecent(); });

// ---------- 我的源 ----------

function sourceCard(item, sourceName, link, badge) {
  const sub = [item.year || '', item.typeName, sourceName].filter(Boolean).join(' · ');
  return h('a', { class: 'card', href: link.href, 'data-no-prerender': true, onclick: link.onclick },
    h('div', { class: 'card-art card-art-local', dataset: { initial: [...(item.name || '?')][0] } },
      item.pic ? h('img', { src: item.pic, loading: 'lazy', decoding: 'async', alt: '', referrerpolicy: 'no-referrer', onerror: (ev) => ev.target.remove() }) : null,
      badge ? h('span', { class: 'badge', text: badge }) : null),
    h('span', { class: 'card-title', text: item.name }), h('span', { class: 'card-meta', text: sub }));
}

// 作品库里没有的作品：把播放需要的信息存在本机，播放页按记录 ID 取。
async function localLink(item, source) {
  const info = { title: item.name, year: item.year, type: item.type || (item.lines[0].episodes.length > 1 ? 'series' : 'movie') };
  const rid = await privateId(info);
  await idb.set('cache', userKey('local:' + rid), { ...info, pic: item.pic, source: source.key, vodId: item.vodId, at: Date.now() });
  return { href: '/play/local#rid=' + rid };
}

function choose(item, options, local) {
  modalBody.replaceChildren(h('div', { class: 'form' },
    h('h2', { text: `“${item.name}”是哪一部？` }),
    h('div', { class: 'choices' }, options.map((option) => h('a', { class: 'choice', href: '/m/' + option.id },
      h('b', { text: option.title }), h('span', { class: 'dim', text: [option.year || '', option.type === 'movie' ? '电影' : '剧集'].filter(Boolean).join(' · ') })))),
    h('div', { class: 'form-actions' }, h('a', { class: 'btn btn-glass', href: local.href, text: '都不是，直接播放' }),
      h('button', { class: 'btn btn-glass', type: 'button', 'data-close-modal': true, text: '取消' }))));
  modal.showModal();
}

let controller = null, generation = 0;
async function searchSources(keyword) {
  if (!section) return; // 没登录时没有"我的源"
  controller?.abort();
  const current = ++generation;
  grid.replaceChildren();
  const subscription = await sources.getSubscription();
  if (!keyword) { section.hidden = true; return; }
  section.hidden = false;
  if (!subscription) {
    status.textContent = '';
    grid.replaceChildren(h('p', { class: 'hint' }, '还没有设置播放源。', h('a', { href: '/settings#sources', text: '去设置' })));
    return;
  }
  controller = new AbortController();
  status.textContent = '正在查询…';
  const seen = new Set();
  let total = 0, answered = 0;
  const count = (await sources.getSources()).length;
  if (!count) { status.textContent = '订阅里没有可用的源'; return; }
  await sources.searchAll(keyword, async ({ source, items }) => {
    if (current !== generation) return;
    answered++;
    const fresh = items.filter((item) => {
      // 同一部作品在多个源里都有时只显示一次
      const key = normalizeTitle(item.name) + '|' + (item.year || '');
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    }).slice(0, 12);
    if (fresh.length) {
      // 问一下本站：这些片名在作品库里吗？只发送片名、年份、类型和豆瓣 ID。
      let results = [];
      try {
        const response = await api('/api/media/resolve', { method: 'POST', json: { items: fresh.map((item) => ({ douban_id: item.doubanId, title: item.name, year: item.year, type: item.type === 'movie' ? 'movie' : item.type ? 'tv' : '' })) } });
        if (response.ok) results = (await response.json()).results;
      } catch { /* 匹配不了就全部按"未收录"处理 */ }
      if (current !== generation) return;
      for (const [index, item] of fresh.entries()) {
        const result = results[index] ?? { status: 'unknown' };
        if (result.status === 'matched') {
          grid.append(sourceCard(item, source.name, { href: '/m/' + result.media.id }));
        } else {
          const local = await localLink(item, source);
          if (result.status === 'ambiguous') {
            grid.append(sourceCard(item, source.name, { href: local.href, onclick: (ev) => { ev.preventDefault(); choose(item, result.options, local); } }, '需要确认'));
          } else {
            grid.append(sourceCard(item, source.name, local, '未收录'));
          }
        }
        total++;
      }
    }
    status.textContent = answered < count ? `正在查询… ${answered}/${count}` : (total ? `${total} 个结果` : '没有结果');
  }, { signal: controller.signal });
}

// 不边打字边查：按回车（或点最近搜索）才查。作品库由 htmx 的自定义事件 run 触发。
let logged = '';
function run() {
  const keyword = input.value.trim();
  empty.hidden = !!keyword;
  const url = new URL(location.href);
  if (keyword) url.searchParams.set('q', keyword); else url.searchParams.delete('q');
  history.replaceState(history.state, '', url);
  if (!keyword) document.getElementById('results').replaceChildren();
  else window.htmx?.trigger(input, 'run');
  searchSources(keyword);
}
document.querySelector('.search-box').addEventListener('submit', (ev) => { ev.preventDefault(); input.blur(); run(); });
// 清空输入框时回到初始状态
input.addEventListener('input', () => { if (!input.value.trim()) run(); });
// 作品库有结果才算一次搜索（记进最近搜索，也计入热搜榜），同一个词连按回车只记一次。
document.getElementById('results').addEventListener('htmx:afterSwap', () => {
  const keyword = input.value.trim();
  if (!keyword || keyword === logged || !document.querySelector('#results .card')) return;
  logged = keyword;
  saveRecent([keyword, ...readRecent().filter((x) => x !== keyword)]);
  renderRecent();
  api('/api/search-log', { method: 'POST', json: { q: keyword } }).catch(() => {});
});

renderRecent();
// 地址里带着关键词进来的（热搜、最近搜索的链接）：等 htmx 初始化完直接查。
if (input.value.trim()) document.addEventListener('DOMContentLoaded', run);
