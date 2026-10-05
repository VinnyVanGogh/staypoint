// Unit tests for the task page header fixes from the STA-638 step 2 review:
// the subtitle model comes only from a route title naming the model, and a
// finished run is never flagged STUCK.
// Run with: node --test tests/unit/task-page-header.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { isStuck, routeModel } = require('../../internal/server/webui/lib/runsteps.js');

const NOW = 1_700_000_000_000;
const ago = ms => new Date(NOW - ms).toISOString();
const SIX_MIN = 6 * 60 * 1000;

test('routeModel reads the model from "Routed to" and "Ran on" titles', () => {
  assert.equal(routeModel('Routed to claude · sonnet'), 'claude · sonnet');
  assert.equal(routeModel('Ran on claude · sonnet'), 'claude · sonnet');
});

test('routeModel reads the selected model from a fallback title, not the reason', () => {
  assert.equal(routeModel('Fell back to Claude: Gemini quota locked'), 'Claude');
});

test('routeModel returns empty for route titles that name no model', () => {
  assert.equal(routeModel('All providers locked'), '');
  assert.equal(routeModel('Running in Claude Cloud'), '');
  assert.equal(routeModel(''), '');
  assert.equal(routeModel(undefined), '');
});

test('in_review task whose run finished with a state step 6 min ago -> not stuck', () => {
  const steps = [
    { run_id: 'r1', kind: 'wake',  created_at: ago(SIX_MIN + 60_000) },
    { run_id: 'r1', kind: 'state', title: 'Finished: in_review', created_at: ago(SIX_MIN) },
  ];
  assert.equal(isStuck(steps, NOW, 'in_review'), false);
});

test('in_progress task whose latest run ended with a state step -> not stuck', () => {
  const steps = [
    { run_id: 'r1', kind: 'wake',  created_at: ago(SIX_MIN + 60_000) },
    { run_id: 'r1', kind: 'state', title: 'Finished: in_progress', created_at: ago(SIX_MIN) },
  ];
  assert.equal(isStuck(steps, NOW, 'in_progress'), false);
});

test('inactive statuses are never stuck, even mid-run', () => {
  const steps = [{ run_id: 'r1', kind: 'edit', created_at: ago(SIX_MIN) }];
  for (const status of ['in_review', 'done', 'cancelled', 'blocked']) {
    assert.equal(isStuck(steps, NOW, status), false, status);
  }
});

test('active run with no state step and a 6 min old last step -> still stuck', () => {
  const steps = [
    { run_id: 'r1', kind: 'wake', created_at: ago(SIX_MIN + 60_000) },
    { run_id: 'r1', kind: 'edit', created_at: ago(SIX_MIN) },
  ];
  assert.equal(isStuck(steps, NOW, 'in_progress'), true);
});
