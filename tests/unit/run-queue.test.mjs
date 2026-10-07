// Unit tests for the task page run-queue line (STA-773).
// Run with: node --test tests/unit/run-queue.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { queueLabel, queuePositionIn } = require('../../internal/server/webui/lib/runsteps.js');

test('not queued → no line', () => {
  assert.equal(queueLabel(null), '');
  assert.equal(queueLabel({ queued: false, ahead: 0 }), '');
});

test('queued line counts runs ahead', () => {
  assert.equal(queueLabel({ queued: true, ahead: 0 }), 'Queued: 0 runs ahead');
  assert.equal(queueLabel({ queued: true, ahead: 1 }), 'Queued: 1 run ahead');
  assert.equal(queueLabel({ queued: true, ahead: 2, wait: 'repo' }),
    'Queued: 2 runs ahead (repo limit: this repo is at max_runs_per_repo)');
  assert.equal(queueLabel({ queued: true, ahead: 0, wait: 'quota' }),
    'Queued: 0 runs ahead (quota: provider quota is locked)');
});

test('queued line names the cap that holds the run (STA-867)', () => {
  assert.equal(queueLabel({ queued: true, ahead: 1, wait: 'org' }),
    'Queued: 1 run ahead (org limit: this organization is at max_runs_per_org)');
  assert.equal(queueLabel({ queued: true, ahead: 3, wait: 'slots' }),
    'Queued: 3 runs ahead (global limit: max_concurrent_runs reached)');
  assert.equal(queueLabel({ queued: true, ahead: 0, wait: 'folder' }),
    'Queued: 0 runs ahead (repo limit: another run is using this non-git folder)');
});

test('position from a run.queue SSE payload', () => {
  const queue = [{ task_id: 'a', wait: 'slots' }, { task_id: 'b', wait: 'repo' }];
  assert.deepEqual(queuePositionIn(queue, 'b'), { queued: true, ahead: 1, wait: 'repo' });
  assert.deepEqual(queuePositionIn(queue, 'z'), { queued: false, ahead: 0 });
  assert.deepEqual(queuePositionIn(undefined, 'a'), { queued: false, ahead: 0 });
});
