// Unit tests for the task board columns, legacy toggle and company grouping.
// Run with: node --test tests/unit/taskboard.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { BOARD_COLUMNS, boardColumnFor, buildBoard, countLegacy, NO_COMPANY } = require('../../internal/server/webui/lib/taskboard.js');

test('board has a Backlog column first and In Review', () => {
  assert.deepEqual(BOARD_COLUMNS, ['backlog', 'todo', 'in_progress', 'in_review', 'blocked', 'done']);
});

test('local tasks map by execution_stage', () => {
  const col = (execution_stage, status = 'active', extra = {}) => boardColumnFor({ status, execution_stage, ...extra });
  assert.equal(col('backlog'), 'backlog');
  assert.equal(col('todo'), 'todo');
  assert.equal(col('stopped'), 'todo');
  assert.equal(col('capped'), 'todo');
  assert.equal(col('in_progress'), 'in_progress');
  assert.equal(col('paused'), 'in_progress');
  assert.equal(col('in_review'), 'in_review');
  assert.equal(col('blocked'), 'blocked');
  assert.equal(col('todo', 'active', { is_blocked: true }), 'blocked');
  assert.equal(col('done', 'done'), 'done');
  assert.equal(col('cancelled', 'soft_deleted'), null);
  assert.equal(col('rejected'), null);
});

test('Paperclip fleet tasks map by normalized status', () => {
  assert.equal(boardColumnFor({ status: 'todo' }), 'todo');
  assert.equal(boardColumnFor({ status: 'in_progress' }), 'in_progress');
  assert.equal(boardColumnFor({ status: 'done' }), 'done');
});

const tasks = [
  { id: 'a', execution_stage: 'backlog', status: 'active', organization: 'StayPoint', origin: 'paperclip_import', updated_at: '2026-10-01' },
  { id: 'b', execution_stage: 'todo', status: 'active', organization: 'Managed Solution', origin: 'native', updated_at: '2026-10-02' },
  { id: 'c', execution_stage: 'todo', status: 'active', organization: 'StayPoint', origin: 'legacy', updated_at: '2026-10-03' },
  { id: 'd', execution_stage: 'todo', status: 'active', organization: '', origin: 'native', updated_at: '2026-10-04' },
  { id: 'e', execution_stage: 'todo', status: 'active', organization: 'StayPoint', origin: 'native', updated_at: '2026-10-05' },
];

test('legacy tasks are hidden unless the toggle is on', () => {
  const [hidden] = buildBoard(tasks, {});
  assert.deepEqual(hidden.columns.todo.map(t => t.id), ['e', 'd', 'b']);
  assert.deepEqual(hidden.columns.backlog.map(t => t.id), ['a']);
  const [shown] = buildBoard(tasks, { showLegacy: true });
  assert.deepEqual(shown.columns.todo.map(t => t.id), ['e', 'd', 'c', 'b']);
  assert.equal(countLegacy(tasks), 1);
});

test('group by company sorts lanes by name with no-company last', () => {
  const groups = buildBoard(tasks, { groupByCompany: true });
  assert.deepEqual(groups.map(g => g.company), ['Managed Solution', 'StayPoint', NO_COMPANY]);
  const sta = groups.find(g => g.company === 'StayPoint');
  assert.equal(sta.total, 2);
  assert.deepEqual(sta.columns.backlog.map(t => t.id), ['a']);
  assert.deepEqual(sta.columns.todo.map(t => t.id), ['e']);
});

test('an empty board still renders one ungrouped set of columns', () => {
  const groups = buildBoard([], {});
  assert.equal(groups.length, 1);
  assert.equal(groups[0].company, null);
  assert.equal(groups[0].total, 0);
});
