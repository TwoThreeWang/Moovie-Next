import test from 'node:test';
import assert from 'node:assert/strict';
import { tokenize, wrapText } from '../static/js/lib/cardtext.js';

// 假的测量：每个字符宽 1
const measure = (s) => [...s].length;

test('分词：中文按字，英文按词', () => {
  assert.deepEqual(tokenize('你好 Hello world！'), ['你', '好', ' ', 'Hello', ' ', 'world', '！']);
});

test('折行：中文按宽度断开', () => {
  assert.deepEqual(wrapText('一二三四五六七', 3, measure), ['一二三', '四五六', '七']);
});

test('折行：英文不在词中间断开', () => {
  assert.deepEqual(wrapText('hello big world', 9, measure), ['hello big', 'world']);
});

test('折行：超长的词按字母断', () => {
  assert.deepEqual(wrapText('abcdefgh', 3, measure), ['abc', 'def', 'gh']);
});

test('折行：标点不放在行首', () => {
  assert.deepEqual(wrapText('一二三，四五', 3, measure), ['一二三，', '四五']);
});

test('折行：超出行数时最后一行加省略号', () => {
  assert.deepEqual(wrapText('一二三四五六七八九', 3, measure, 2), ['一二三', '四五…']);
  assert.deepEqual(wrapText('', 3, measure), []);
});
