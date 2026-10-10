// Unit tests for the task URL helpers (task-eb38c245): /STA-123/<slug> is the
// canonical task page; the project never appears in the URL.
// Run with: node --test tests/unit/taskroutes.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { isTaskRef, isTaskPagePath, taskRefFromPath, taskRefPath } = require('../../internal/server/webui/lib/taskroutes.js');

test('non-references are rejected', () => {
  for (const s of ['', 'task-662143ce', 'STA', 'STA-0', 'STA-012', 'S-1', 'TOOLONG-1', 'STA-1/x', undefined, null]) {
    assert.equal(isTaskRef(s), false, String(s));
  }
  assert.equal(isTaskRef('sta-12'), true);
});

test('taskRefFromPath reads the reference and ignores the slug', () => {
  assert.equal(taskRefFromPath('/STA-123'), 'STA-123');
  assert.equal(taskRefFromPath('/sta-123/playwright-ui-specs'), 'STA-123');
  assert.equal(taskRefFromPath('/STA-123/any-wrong-slug'), 'STA-123');
  assert.equal(taskRefFromPath('/STA-123/a/b'), '');
  assert.equal(taskRefFromPath('/tasks/STA/CI%20%26%20Testing/task-662143ce'), '');
  assert.equal(taskRefFromPath('/kanban'), '');
  assert.equal(taskRefFromPath('/task-status'), '');
});

test('isTaskPagePath covers canonical and old forms only', () => {
  for (const p of ['/STA-1', '/MAN-45/slug', '/tasks/task-1', '/tasks/STA/x/STA-1', '/issues/STA/x/STA-1']) {
    assert.equal(isTaskPagePath(p), true, p);
  }
  for (const p of ['/', '/kanban', '/org/StayPoint', '/STA-1/a/b']) {
    assert.equal(isTaskPagePath(p), false, p);
  }
});

test('taskRefPath: numbered daemon task -> /REF/slug, no project', () => {
  const t = { id: 'task-662143ce', identifier: 'STA-123', slug: 'playwright-ui-specs', project: 'CI & Testing' };
  assert.equal(taskRefPath(t), '/STA-123/playwright-ui-specs');
  assert.equal(taskRefPath({ ...t, slug: '' }), '/STA-123');
});

test('taskRefPath: unnumbered or fleet tasks fall back (empty)', () => {
  assert.equal(taskRefPath(null), '');
  assert.equal(taskRefPath({ id: 'task-1', identifier: 'RHI-task-e' }), '');
  assert.equal(taskRefPath({ id: 'task-1', identifier: '' }), '');
  assert.equal(taskRefPath({ id: '6b2c…uuid', identifier: 'STA-775' }), '');
});
