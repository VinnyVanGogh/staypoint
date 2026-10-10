// Unit tests for the task page "Draining for deploy" line (task-db71fba9).
// Run with: node --test tests/unit/drain-line.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { drainLabel, queueLabel } = require('../../internal/server/webui/lib/runsteps.js');

test('not draining → no line', () => {
  assert.equal(drainLabel(null), '');
  assert.equal(drainLabel({ draining: false, live: 2, queued: 1 }), '');
});

test('draining line counts live and queued runs', () => {
  assert.equal(drainLabel({ draining: true, mode: 'finish', live: 2, queued: 1 }),
    'Draining for deploy: 2 runs left, 1 queued');
  assert.equal(drainLabel({ draining: true, mode: 'finish', live: 1, queued: 0 }),
    'Draining for deploy: 1 run left, 0 queued');
  assert.equal(drainLabel({ draining: true, mode: 'boundary', live: 1, queued: 3 }),
    'Draining for deploy: 1 run left, 3 queued (suspending at the next turn)');
  assert.equal(drainLabel({ draining: true, mode: 'now', live: 0, queued: 0 }),
    'Draining for deploy: 0 runs left, 0 queued (suspending now)');
});

test('queued line names drain and resume waits', () => {
  assert.equal(queueLabel({ queued: true, ahead: 0, wait: 'drain' }),
    'Queued: 0 runs ahead (StayPoint is draining for a deploy: starts after the restart)');
  assert.equal(queueLabel({ queued: true, ahead: 1, wait: 'resume' }),
    'Queued: 1 run ahead (paused for a deploy: resumes from where it stopped)');
});
