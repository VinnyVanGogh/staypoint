// Unit tests for the task page turn timer and stall stop label.
// Run with: node --test tests/unit/turn-timer.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { turnElapsedLabel, runStepLabel } = require('../../internal/server/webui/lib/runsteps.js');

test('no turn in flight shows nothing', () => {
  assert.equal(turnElapsedLabel(null, Date.now()), '');
  assert.equal(turnElapsedLabel({ turn: 0 }, Date.now()), '');
});

test('turn timer counts past the old 30 minute cap', () => {
  const start = Date.parse('2026-10-06T20:00:00Z');
  const turn = { turn: 0, started_at: '2026-10-06T20:00:00Z' };
  assert.equal(turnElapsedLabel(turn, start + 65 * 1000), '#1 · 1m 05s');
  assert.equal(turnElapsedLabel(turn, start + 47 * 60 * 1000), '#1 · 47m 00s');
  assert.equal(turnElapsedLabel({ ...turn, turn: 2 }, start + 2 * 3600 * 1000 + 5 * 60 * 1000), '#3 · 2h 05m');
});

test('stall stop row is the step label as is', () => {
  const steps = [
    { kind: 'wake', title: 'Woke up' },
    { kind: 'message', status: 'error', title: 'Stopped: no activity for 20m' },
    { kind: 'state', title: 'Finished: error' },
  ];
  assert.equal(runStepLabel(steps), 'Stopped: no activity for 20m');
});
