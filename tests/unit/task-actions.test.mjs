// Unit tests for the task page's Board close controls (STA-861).
// Run with: node --test tests/unit/task-actions.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const {
  taskIsTerminal, taskRunActive, taskCloseActions, markDoneConfirmText,
  cancelConfirmText, markDoneBody, kindChangeRefusal, WORK_KINDS,
} = require('../../internal/server/webui/lib/taskactions.js');

const task = (execution_stage, extra = {}) => ({ id: 'task-1', status: 'active', execution_stage, ...extra });

test('Mark done and Cancel show for every non-terminal stage', () => {
  for (const stage of ['backlog', 'todo', 'in_progress', 'paused', 'blocked', 'in_review', 'stopped', 'capped', 'error']) {
    const a = taskCloseActions(task(stage));
    assert.equal(a.markDone, true, `${stage}: markDone`);
    assert.equal(a.cancel, true, `${stage}: cancel`);
  }
});

test('terminal tasks get no close controls', () => {
  for (const t of [task('done', { status: 'done' }), task('cancelled', { status: 'soft_deleted' }), task('rejected'), task('todo', { status: 'done' })]) {
    assert.equal(taskIsTerminal(t), true);
    assert.deepEqual(taskCloseActions(t), { markDone: false, cancel: false, runActive: false });
  }
  assert.equal(taskCloseActions({ status: 'active', execution_stage: 'todo' }).markDone, false, 'no id');
});

test('a run is active only while in_progress or paused', () => {
  assert.equal(taskRunActive(task('in_progress')), true);
  assert.equal(taskRunActive(task('paused')), true);
  for (const s of ['todo', 'stopped', 'in_review', 'backlog']) assert.equal(taskRunActive(task(s)), false, s);
  assert.equal(taskCloseActions(task('in_progress')).runActive, true);
});

test('Mark done confirms only when a run will be stopped or a review card is open', () => {
  assert.equal(markDoneConfirmText(task('todo')), '');
  assert.match(markDoneConfirmText(task('in_progress')), /run is in progress.*stopped first/s);
  assert.match(markDoneConfirmText(task('in_review'), { hasActiveCard: true }), /ship review card/);
  assert.match(cancelConfirmText(task('paused')), /stopped first/);
  assert.match(cancelConfirmText(task('todo')), /^Cancel this task\?/);
});

test('Mark done body always asks for the Board path and trims the note', () => {
  assert.deepEqual(markDoneBody(''), { board: true });
  assert.deepEqual(markDoneBody(null), { board: true });
  assert.deepEqual(markDoneBody('  done by hand  '), { board: true, note: 'done by hand' });
});

test('kind changes: known kinds only, never mid-run, no gemini code kind in a work repo', () => {
  assert.deepEqual(WORK_KINDS, ['coding', 'review', 'architecture', 'planning', 'qa', 'docs']);
  assert.equal(kindChangeRefusal(task('todo'), 'docs', false), '');
  assert.match(kindChangeRefusal(task('todo'), 'poetry', false), /Unknown kind/);
  assert.match(kindChangeRefusal(task('in_progress'), 'docs', false), /Stop the run/);
  const gem = task('todo', { provider: 'gemini' });
  assert.match(kindChangeRefusal(gem, 'coding', true), /Gemini never writes code/);
  assert.match(kindChangeRefusal(gem, 'qa', true), /Gemini never writes code/);
  assert.equal(kindChangeRefusal(gem, 'planning', true), '');
  assert.equal(kindChangeRefusal(gem, 'coding', false), '', 'personal repo: server gates per run');
});
