// Unit tests for the lists' Stage + Running display and filters (task-3387cad2).
// Run with: node --test tests/unit/task-state.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const {
  STAGES, taskStageKey, stageLabel, liveRunIndex, taskIsRunning, runningByOrg, elapsedLabel,
  normalizeListFilter, listFilterActive, matchesListFilter, loadListFilter, saveListFilter,
  stageFilterSummary,
} = require('../../internal/server/webui/lib/taskstate.js');

test('stage never reads "active": every task maps to one of the seven stages', () => {
  const cases = [
    [{ status: 'active', execution_stage: 'todo' }, 'todo'],
    [{ status: 'active', execution_stage: 'backlog' }, 'backlog'],
    [{ status: 'active', execution_stage: 'in_progress' }, 'in_progress'],
    [{ status: 'active', execution_stage: 'paused' }, 'in_progress'],
    [{ status: 'active', execution_stage: 'in_review' }, 'in_review'],
    [{ status: 'active', execution_stage: 'todo', is_blocked: true }, 'blocked'],
    [{ status: 'done', execution_stage: 'done' }, 'done'],
    [{ status: 'active', execution_stage: 'cancelled' }, 'cancelled'],
    [{ status: 'active', execution_stage: 'rejected' }, 'cancelled'],
    [{ status: 'active', execution_stage: 'capped' }, 'todo'],
    [{ status: 'active', execution_stage: 'stopped' }, 'todo'],
    // Fleet / Paperclip words with no stage.
    [{ status: 'active' }, 'todo'],
    [{ status: 'running' }, 'in_progress'],
    [{ status: 'stopped' }, 'cancelled'],
    [{ status: 'errored' }, 'todo'],
    [null, 'todo'],
  ];
  for (const [task, want] of cases) {
    const got = taskStageKey(task);
    assert.equal(got, want, JSON.stringify(task));
    assert.ok(STAGES.includes(got));
    assert.notEqual(stageLabel(got).toLowerCase(), 'active');
  }
});

test('running comes from the live run index, not from stage in_progress', () => {
  const live = liveRunIndex([{ task_id: 'a', organization: 'General', started_at: '2026-10-09T11:00:00Z' }]);
  assert.equal(taskIsRunning({ id: 'a', execution_stage: 'todo' }, live), true);
  // In progress with no run in flight (stopped or crashed run) is not running.
  assert.equal(taskIsRunning({ id: 'b', execution_stage: 'in_progress', running: true }, live), false);
  // Without the index, the task's own server-computed flag.
  assert.equal(taskIsRunning({ id: 'b', running: true }), true);
  assert.equal(taskIsRunning({ id: 'b', execution_stage: 'in_progress' }), false);
});

test('runningByOrg counts live runs per org; no org is StayPoint', () => {
  const live = liveRunIndex([
    { task_id: 'a', organization: 'General' },
    { task_id: 'b', organization: 'RuneLite' },
    { task_id: 'c', organization: 'RuneLite' },
    { task_id: 'd', organization: '' },
  ]);
  assert.deepEqual(runningByOrg(live), { General: 1, RuneLite: 2, StayPoint: 1 });
  assert.deepEqual(runningByOrg(null), {});
});

test('elapsedLabel', () => {
  const t0 = Date.parse('2026-10-09T10:00:00Z');
  assert.equal(elapsedLabel('2026-10-09T10:00:00Z', t0 + 45_000), '45s');
  assert.equal(elapsedLabel('2026-10-09T10:00:00Z', t0 + 12 * 60_000), '12m');
  assert.equal(elapsedLabel('2026-10-09T10:00:00Z', t0 + 65 * 60_000), '1h 05m');
  assert.equal(elapsedLabel('', t0), '');
  assert.equal(elapsedLabel('garbage', t0), '');
  assert.equal(elapsedLabel('2026-10-09T10:00:00Z', t0 - 5000), '0s');
});

test('filters: Running yes/no and Stage multi-select', () => {
  const live = liveRunIndex([{ task_id: 'run' }]);
  const tasks = [
    { id: 'run', execution_stage: 'in_progress' },
    { id: 'todo', execution_stage: 'todo' },
    { id: 'stale', execution_stage: 'in_progress' },
    { id: 'rev', execution_stage: 'in_review' },
  ];
  const ids = (f) => tasks.filter(t => matchesListFilter(t, f, live)).map(t => t.id);
  assert.deepEqual(ids({}), ['run', 'todo', 'stale', 'rev']);
  assert.deepEqual(ids({ running: 'yes' }), ['run']);
  assert.deepEqual(ids({ running: 'no' }), ['todo', 'stale', 'rev']);
  assert.deepEqual(ids({ stages: ['in_progress'] }), ['run', 'stale']);
  assert.deepEqual(ids({ stages: ['in_progress', 'in_review'], running: 'no' }), ['stale', 'rev']);
  assert.equal(listFilterActive({}), false);
  assert.equal(listFilterActive({ stages: ['todo'] }), true);
});

test('normalizeListFilter drops junk, keeps order, dedupes', () => {
  assert.deepEqual(normalizeListFilter({ running: 'maybe', stages: ['todo', 'active', 'todo', 'done', 7] }),
    { running: 'all', stages: ['todo', 'done'] });
  assert.deepEqual(normalizeListFilter('x'), { running: 'all', stages: [] });
});

test('filters persist per page and survive bad storage', () => {
  const mem = new Map();
  const storage = { getItem: k => (mem.has(k) ? mem.get(k) : null), setItem: (k, v) => mem.set(k, v) };
  saveListFilter(storage, 'recent-tasks', { running: 'yes', stages: ['todo'] });
  assert.deepEqual(loadListFilter(storage, 'recent-tasks'), { running: 'yes', stages: ['todo'] });
  assert.deepEqual(loadListFilter(storage, 'task-status'), { running: 'all', stages: [] });
  mem.set('staypoint_list_filter_kanban', '{not json');
  assert.deepEqual(loadListFilter(storage, 'kanban'), { running: 'all', stages: [] });
  const throwing = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('full'); } };
  assert.deepEqual(loadListFilter(throwing, 'x'), { running: 'all', stages: [] });
  assert.deepEqual(saveListFilter(throwing, 'x', { running: 'no' }), { running: 'no', stages: [] });
});

test('stageFilterSummary', () => {
  assert.equal(stageFilterSummary([]), 'All stages');
  assert.equal(stageFilterSummary(STAGES), 'All stages');
  assert.equal(stageFilterSummary(['todo']), 'Todo');
  assert.equal(stageFilterSummary(['todo', 'in_review']), 'Todo, In review');
  assert.equal(stageFilterSummary(['todo', 'in_review', 'done']), '3 stages');
});
