// 详情页：页面一显示就去用户自己的源里找这部作品。
// 作品资料来自本站，秒开；源的查询由浏览器直连，结果缓存在本机，到播放页时直接用。
import { h, icon } from '../core/api.js';
import * as sources from '../core/sources.js';
import { renderIssueGrid } from '../core/episodes.js';
import { assignIssues, latestIssue } from '../lib/issue.js';

const main = document.querySelector('main.detail');
const media = JSON.parse(main.dataset.media);
const list = document.getElementById('src-list'), status = document.getElementById('src-status');
const isMovie = media.type === 'movie';
// 综艺：源里的集名按期对齐（lib/issue.js），选集按期重排
const isShow = media.type === 'show';

const issueCodes = (item) => item.lines.flatMap((line) => line.episodes.map((episode) => episode.issue)).filter((code) => code != null);

function describe(item) {
  const episodes = Math.max(...item.lines.map((line) => line.episodes.length));
  const parts = [item.name];
  const latest = isShow ? latestIssue(issueCodes(item)) : 0;
  // 综艺的集数没有意义（上下、加更、纯享都算一集），写最新一期
  if (latest) parts.push(`更新至第 ${latest} 期`);
  else if (!isMovie) parts.push(`${episodes} 集`);
  if (item.lines.length > 1) parts.push(`${item.lines.length} 条线路`);
  if (item.remarks) parts.push(item.remarks);
  return parts.join(' · ');
}

// 源里只有预告片或枪版时（见 match.js 的 incompleteKind），不能播，但说明一下为什么
const HIDDEN_TEXT = { trailer: '只有预告片，已隐藏', cam: '只有枪版，已隐藏' };

function row(entry) {
  if (entry.status === 'ok') {
    const insecure = entry.item.lines.every((line) => line.episodes.every((episode) => episode.insecure));
    const play = document.querySelector('[data-play]');
    const href = new URL(play.href);
    href.searchParams.set('src', entry.key);
    return h('a', { class: 'src-row is-ok', href: href.pathname + href.search, 'data-no-prerender': true },
      icon('layers'), h('span', { class: 'src-name', text: entry.name }),
      h('span', { class: 'src-desc', text: insecure ? '只有 HTTP 地址，可能播不了' : describe(entry.item) }),
      h('span', { class: 'lat' + (entry.ms < 800 ? ' good' : ''), text: entry.ms + 'ms' }));
  }
  return h('div', { class: 'src-row is-off' }, icon('layers'), h('span', { class: 'src-name', text: entry.name }),
    h('span', { class: 'src-desc', text: entry.status === 'error' ? entry.message : HIDDEN_TEXT[entry.hidden] ?? '没有找到这部作品' }));
}

// 源很多时只显示前 10 个（能播的排在前面），其余点"展开全部"再看。展开后重新查询也保持展开。
const LIMIT = 10;
let expanded = false;

// 综艺的选集：用源里实际有的期和段落重排。服务器先按资料列出"第 N 期"，这里换成"第 N 期：上 / 下 / 加更"。
let issueGridKey = '';
function renderIssues(entries) {
  const grid = document.getElementById('ep-grid');
  if (!grid) return;
  const codes = [...new Set(entries.flatMap((entry) => issueCodes(entry.item)))];
  const current = +grid.dataset.current || 0;
  const key = codes.sort((a, b) => a - b).join(',');
  if (!codes.length || key === issueGridKey) return;
  issueGridKey = key;
  const seen = new Set(JSON.parse(grid.dataset.seen || '[]'));
  const season = media.season || 1;
  renderIssueGrid(grid, codes, { href: (code) => `/play/${media.id}?s=${season}&e=${code}`, current, seen });
  document.getElementById('episodes').hidden = false;
  const latest = latestIssue(codes), label = document.getElementById('ep-status');
  const known = +(/(\d+)/.exec(label?.textContent ?? '')?.[1] ?? 0);
  if (label && latest > known) label.textContent = `更新至第 ${latest} 期`;
}

function render(result, { done }) {
  const found = result.entries.filter((entry) => entry.status === 'ok').length;
  if (isShow) {
    const playable = result.entries.filter((entry) => entry.status === 'ok');
    assignIssues(playable.flatMap((entry) => entry.item.lines));
    renderIssues(playable);
  }
  const rows = result.entries.map(row);
  if (!expanded && rows.length > LIMIT) {
    rows.slice(LIMIT).forEach((el) => { el.hidden = true; });
    rows.push(h('button', { class: 'episodes-toggle src-toggle', type: 'button', text: `展开全部 ${rows.length} 个源`, onclick: (ev) => {
      expanded = true;
      for (const el of list.children) el.hidden = false;
      ev.currentTarget.remove();
    } }));
  }
  list.replaceChildren(...rows);
  if (!done) { status.textContent = `正在查询… 已找到 ${found} 个`; return; }
  if (found) status.textContent = `${found} 个源可以播放`;
  else if (result.entries.some((entry) => entry.hidden)) {
    status.textContent = '';
    list.prepend(h('p', { class: 'hint', text: '你的源里目前只有预告片或枪版（已隐藏），还没有正片。' }));
  } else {
    status.textContent = '';
    list.prepend(h('p', { class: 'hint' }, '你的源里没有找到这部作品。可以', h('a', { href: '/search?q=' + encodeURIComponent(media.title), text: '在搜索页直接搜源' }), '，或者换一批源。'));
  }
}

async function start(force = false) {
  const subscription = await sources.getSubscription();
  if (!subscription) {
    status.textContent = '';
    list.replaceChildren(h('div', { class: 'src-empty' },
      h('p', { text: '还没有设置播放源。本站只提供作品资料和播放器，视频来源需要你自己配置。' }),
      h('a', { class: 'btn btn-glass', href: '/settings#sources', text: '去设置播放源' })));
    return;
  }
  status.textContent = '正在查询…';
  list.replaceChildren(h('p', { class: 'hint', text: '正在查询你的播放源…' }));
  try {
    await sources.findForMedia(media, { onUpdate: render, force });
  } catch {
    status.textContent = '查询失败';
  }
}

// 手动刷新
document.querySelector('#sources .row-head')?.insertBefore(
  h('button', { class: 'row-more', type: 'button', text: '重新查询', onclick: () => start(true) }),
  document.querySelector('#sources .row-more'));

start();
