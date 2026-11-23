import test from 'node:test';
import assert from 'node:assert/strict';
import { parseIssueLabel, assignIssues, groupIssues, resolveIssue, latestIssue, issueLabel, partName, decodeIssue } from '../static/js/lib/issue.js';

// 资源站里的真实集名（2026-11-23），节选。
// 《现在就出发 第四季》：正片分上下，每期两次加更。
const WUJIN = ['20260905', '20261001先导片上', '20261001先导片下', '20261001先导片·沙滩小黑脚纯享', '20261010第1期上',
  '20261010第1期上·慢速目击证人纯享', '20261010第1期下', '20261010第1期下纯享', '20261012加更第1期', '20261013第1期还有加更',
  '20261015第1期花絮特辑', '20261017第2期上', '20261018第2期下', '20261019第2期加更', '20261023第3期超前彩蛋', '20261024第3期上'];
const BAOFENG = ['20260905回顾特辑', '20261001先导片', '20261010第1期上', '20261010第1期上·捣蛋鱼医生纯享', '20261011第1期下',
  '20261012加更第1期', '20261013第1期还有加更', '20261015(第1期花絮特辑)', '20261017第2期上', '20261018第2期下', '20261019第2期加更',
  '20261024第3期上'];
const JISU = ['260905回顾特辑', '先导片上', '先导片下', '沙滩小黑脚纯享', '第1期上', '第1期上纯享', '第1期上纯享', '第1期下', '加更第1期',
  '第1期还有加更', '261030直播回放', '第2期上', '第2期下'];
const FEIFAN = ['第20261010期', '第20261010期纯享', '第20261010期捣蛋', '第20261011期', '第20261012期', '第20261013期', '第20261015期',
  '第20261017期', '第20261024期上', '第20261024期下', '第20261025期'];
const DYTT = ['20260905回顾特辑', '20261001先导片上', '20261010上', '20261010捣蛋鱼医生纯享', '20261011下', '20261012加更',
  '20261013还有加更', '20261015花絮特辑', '20261017上', '20261018下', '20261024上·笨蛋杀手纯享', '20261024上'];

const line = (labels) => ({ episodes: labels.map((label) => ({ label })) });
const issues = (l) => l.episodes.map((e) => [e.label, e.issue]).filter(([, code]) => code !== null);
const codes = (l) => issues(l).map(([, code]) => code);

test('综艺集名：期号、版本和段落', () => {
  const parse = (label) => {
    const item = parseIssueLabel(label);
    return item && [item.issue, item.variant, item.half, item.date];
  };
  assert.deepEqual(parse('20261010第1期上'), [1, 0, 2, '20261010']);
  assert.deepEqual(parse('加更第1期'), [1, 1, 1, '']);
  assert.deepEqual(parse('第1期还有加更'), [1, 2, 1, '']);
  assert.deepEqual(parse('20261001先导片下'), [0, 0, 4, '20261001']);
  assert.deepEqual(parse('第5期'), [5, 0, 1, '']);
  assert.deepEqual(parse('第十二期 下'), [12, 0, 4, '']);
  assert.deepEqual(parse('第3集'), [3, 0, 1, '']);
  assert.deepEqual(parse('20261029 第3期（上）'), [3, 0, 2, '20261029']);
  assert.deepEqual(parse('20261112第5期第5期下'), [5, 0, 4, '20261112']);
  assert.deepEqual(parse('20261106沉浸版第4期上'), [4, 3, 2, '20261106']);
  assert.deepEqual(parse('20260709(旅行日记第2期（下）)'), [2, 4, 4, '20260709']);
  assert.deepEqual(parse('游戏加更第2期'), [2, 1, 1, '']);
  assert.deepEqual(parse('特别加更第2期'), [2, 2, 1, '']);
  assert.deepEqual(parse('福利篇第1期上'), [1, 5, 2, '']);
  assert.deepEqual(parse('20261022-02'), [2, 0, 1, '20261022']);
  // 期号后面跟着嘉宾名之类：算正片整期
  assert.deepEqual(parseIssueLabel('第8期：某某做客'), { issue: 8, variant: 0, half: 1, date: '', other: true });
});

test('综艺集名：只有日期时，没写的部分留给别的源推断', () => {
  const parse = (label) => {
    const item = parseIssueLabel(label);
    return item && [item.issue, item.variant, item.half, item.date];
  };
  assert.deepEqual(parse('第20261010期'), [null, null, null, '20261010']);
  assert.deepEqual(parse('20261011下'), [null, 0, 4, '20261011']);
  assert.deepEqual(parse('2026-10-12 加更'), [null, 1, null, '20261012']);
  assert.deepEqual(parse('20261030沉浸版上'), [null, 3, 2, '20261030']);
  assert.deepEqual(parse('​20260709旅行日记上'), [null, 4, 2, '20260709']);
  assert.equal(parseIssueLabel('第20261010期捣蛋'), null);
  assert.equal(parseIssueLabel('20260702(地球团集结前)'), null);
});

test('综艺集名：纯享、彩蛋、抢鲜、回顾、花絮、直播、直拍、宣传不进选集', () => {
  for (const label of ['20261010第1期上·慢速目击证人纯享', '20261120(第7期超前彩蛋)', '20260905回顾特辑', '20260919回顾特辑第1期上',
    '20261015第1期花絮特辑', '261030直播回放', '第20261010期纯享', '20261106抢鲜版第3期', '20260623超前抢鲜看', '20260627总宣',
    '20260706竖屏直拍', '20260805(半熟恋人5特别联动)', '20260830(迷妹专访)', 'MV纯享']) {
    assert.equal(parseIssueLabel(label), null, label);
  }
  assert.equal(parseIssueLabel(''), null);
  assert.equal(parseIssueLabel('正片'), null);
});

test('现在就出发：各个源按期对齐，只有日期的源靠别的源推断', () => {
  const lines = [line(WUJIN), line(BAOFENG), line(JISU), line(FEIFAN), line(DYTT)];
  assignIssues(lines);
  const [wujin, baofeng, jisu, feifan, dytt] = lines;
  assert.deepEqual(issues(wujin), [['20261001先导片上', 2], ['20261001先导片下', 4], ['20261010第1期上', 102], ['20261010第1期下', 104],
    ['20261012加更第1期', 111], ['20261013第1期还有加更', 121], ['20261017第2期上', 202], ['20261018第2期下', 204], ['20261019第2期加更', 211],
    ['20261024第3期上', 302]]);
  assert.deepEqual(codes(baofeng), [1, 102, 104, 111, 121, 202, 204, 211, 302]);
  // 同一条线路里重复的"第1期上纯享"本来就被排除了；正片不重复
  assert.deepEqual(codes(jisu), [2, 4, 102, 104, 111, 121, 202, 204]);
  // 非凡只有日期：10 号是第 1 期上，11 号是下，12、13 号是两次加更
  assert.deepEqual(issues(feifan), [['第20261010期', 102], ['第20261011期', 104], ['第20261012期', 111], ['第20261013期', 121],
    ['第20261017期', 202], ['第20261024期上', 302], ['第20261024期下', 304]]);
  // 电影天堂：日期 + 上下
  assert.deepEqual(issues(dytt), [['20261001先导片上', 2], ['20261010上', 102], ['20261011下', 104], ['20261012加更', 111],
    ['20261013还有加更', 121], ['20261017上', 202], ['20261018下', 204], ['20261024上', 302]]);
});

test('后会友期：正片、沉浸版、加更版各自编号，一天两期', () => {
  const wujin = line(['20261022第1期', '20261022第2期', '20261022沉浸版第1期', '20261023加更版第1期', '20261023沉浸版第2期',
    '20261029 第3期（上）', '20261030加更版', '20261105上', '20261105下', '20261106抢鲜版第3期', '20261106沉浸版第4期上',
    '20261112第5期上', '20261112第5期第5期下']);
  const baofeng = line(['20261022第1期', '20261022第2期', '20261029第3期上', '20261029第3期下', '20261030加更版第2期',
    '20261105第4期上', '20261105第4期下', '20261112第5期上', '20261112第5期下', '20261120抢鲜版']);
  const feifan = line(['第20261022期上', '第20261022期下', '第20261023期加更', '第20261023期抢鲜', '第20261029期', '第20261030期加更', '第20261105期上']);
  const dytt = line(['20261022-01', '20261022-02', '20261022沉浸版', '20261023加更版', '20261029上', '20261030沉浸版上', '20261105下']);
  assignIssues([wujin, baofeng, feifan, dytt]);
  assert.deepEqual(issues(wujin), [['20261022第1期', 101], ['20261022第2期', 201], ['20261022沉浸版第1期', 131], ['20261023加更版第1期', 111],
    ['20261023沉浸版第2期', 231], ['20261029 第3期（上）', 302], ['20261030加更版', 211], ['20261105上', 402], ['20261105下', 404],
    ['20261106沉浸版第4期上', 432], ['20261112第5期上', 502], ['20261112第5期第5期下', 504]]);
  assert.deepEqual(codes(baofeng), [101, 201, 302, 304, 211, 402, 404, 502, 504]);
  // 非凡把 22 号播的两期写成"上""下"：别的源那天是第 1 期、第 2 期两个整期
  assert.deepEqual(codes(feifan), [101, 201, 111, 302, 211, 402]);
  // "20261030沉浸版上"：别的源那天没有沉浸版，对不上就不认
  assert.deepEqual(codes(dytt), [101, 201, 131, 111, 302, 404]);
});

test('地球超新鲜：加更、特别加更、旅行日记、福利篇不和正片混在一起', () => {
  const mgtv = line(['20260627(第1期上)', '20260628(第1期下)', '20260630(游戏加更第1期)', '20260701(特别加更第1期)', '20260702(地球团集结前)',
    '20260702(旅行日记第1期)', '20260709(旅行日记第2期（上）)', '20260709(旅行日记第2期（下）)', '20260905(福利篇第1期上)', '20260906福利篇第1期下',
    '20260919回顾特辑第1期上']);
  const dytt = line(['20260627上', '20260627总宣', '20260628下', '20260630', '20260701特别加更', '20260702旅行日记', '20260706直拍1',
    '20260709旅行日记上', '20260709旅行日记下', '20260905福利篇', '20260906福利篇', '20260919回顾特辑']);
  const jisu = line(['第1期上', '第1期下', '游戏加更第1期', '回顾特辑上', '特别加更第1期', '旅行日记第1期', '福利篇第1期上', '福利篇第1期下']);
  assignIssues([mgtv, dytt, jisu]);
  assert.deepEqual(codes(mgtv), [102, 104, 111, 121, 141, 242, 244, 152, 154]);
  assert.deepEqual(codes(dytt), [102, 104, 111, 121, 141, 242, 244, 152, 154]);
  assert.deepEqual(codes(jisu), [102, 104, 111, 121, 141, 152, 154]);
});

test('各站日期差一天时，版本和段落写明了的看前后一天', () => {
  const wujin = line(['20261010第1期上', '20261010第1期下', '20261031第4期上']);
  const dytt = line(['20261010上', '20261011下', '20261101上', '20261011']);
  assignIssues([wujin, dytt]);
  // "20261101上"：前一天是第 4 期上，对得上；"20261011"什么都没写，不猜
  assert.deepEqual(dytt.episodes.map((e) => e.issue), [102, 104, 402, null]);
});

test('所有源都只有日期时按日期先后编期号', () => {
  const a = line(['20261101', '20261102', '20261103']), b = line(['第20261102期', '第20261104期']);
  assignIssues([a, b]);
  assert.deepEqual(a.episodes.map((e) => e.issue), [101, 201, 301]);
  assert.deepEqual(b.episodes.map((e) => e.issue), [201, 401]);
});

test('同一条线路里重复的集号只保留第一个', () => {
  const a = line(['第1期', '第1期 嘉宾特别版', '第2期']);
  assignIssues([a]);
  assert.deepEqual(a.episodes.map((e) => e.issue), [101, null, 201]);
});

test('集号的文字和分组', () => {
  assert.equal(issueLabel(302), '第 3 期 上');
  assert.equal(issueLabel(211), '第 2 期 加更');
  assert.equal(issueLabel(121), '第 1 期 加更 2');
  assert.equal(issueLabel(244), '第 2 期 日记 下');
  assert.equal(issueLabel(501), '第 5 期');
  assert.equal(issueLabel(4), '先导片 下');
  assert.equal(partName(501), '正片');
  assert.equal(partName(302), '上');
  assert.equal(partName(131), '沉浸版');
  assert.deepEqual(decodeIssue(1244), { issue: 12, variant: 4, half: 4 });
  const { list, flat } = groupIssues([104, 102, 2, 111, 102, 202]);
  assert.deepEqual(list, [{ issue: 0, parts: [2] }, { issue: 1, parts: [102, 104, 111] }, { issue: 2, parts: [202] }]);
  assert.equal(flat, false);
  assert.equal(groupIssues([101, 201, 301]).flat, true);
});

test('"第 N 期，哪段都行"换成实际的段落', () => {
  assert.equal(resolveIssue(100, [102, 104, 111]), 102);
  assert.equal(resolveIssue(200, [111, 211, 221]), 211); // 这一期只有加更
  assert.equal(resolveIssue(300, [102]), 300); // 源里没有这一期：保持原样
  assert.equal(resolveIssue(104, [102]), 104);
  assert.equal(latestIssue([2, 102, 711, 302]), 3); // 加更不算
  assert.equal(latestIssue([]), 0);
});
