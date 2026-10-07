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
    'Queued: 2 runs ahead (waiting for another run in this repo)');
  assert.equal(queueLabel({ queued: true, ahead: 0, wait: 'quota' }),
    'Queued: 0 runs ahead (provider quota is locked)');
});

test('position from a run.queue SSE payload', () => {
  const queue = [{ task_id: 'a', wait: 'slots' }, { task_id: 'b', wait: 'repo' }];
  assert.deepEqual(queuePositionIn(queue, 'b'), { queued: true, ahead: 1, wait: 'repo' });
  assert.deepEqual(queuePositionIn(queue, 'z'), { queued: false, ahead: 0 });
  assert.deepEqual(queuePositionIn(undefined, 'a'), { queued: false, ahead: 0 });
});
