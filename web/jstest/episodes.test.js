import test from 'node:test';
import assert from 'node:assert/strict';
import { visibleIndexes } from '../static/js/core/episodes.js';

const sorted = (set) => [...set].sort((a, b) => a - b);

test('折叠选集：保留开头两集、结尾两集和当前集前后两集', () => {
  assert.deepEqual(sorted(visibleIndexes(100, 50)), [0, 1, 48, 49, 50, 51, 52, 98, 99]);
});

test('折叠选集：没有当前集时只保留首尾', () => {
  assert.deepEqual(sorted(visibleIndexes(100, -1)), [0, 1, 98, 99]);
});

test('折叠选集：当前集靠近开头时不越界、不重复', () => {
  assert.deepEqual(sorted(visibleIndexes(40, 1)), [0, 1, 2, 3, 38, 39]);
  assert.deepEqual(sorted(visibleIndexes(40, 39)), [0, 1, 37, 38, 39]);
});
