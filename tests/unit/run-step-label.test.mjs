// Unit tests for the stats strip "Step" label (STA-775 item 3).
// Run with: node --test tests/unit/run-step-label.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { runStepLabel } = require('../../internal/server/webui/lib/runsteps.js');

test('no steps → idle', () => {
  assert.equal(runStepLabel([]), 'idle');
  assert.equal(runStepLabel(undefined), 'idle');
});

test('running run → last step title', () => {
  assert.equal(runStepLabel([
    { kind: 'wake', title: 'Woke up' },
    { kind: 'checkpoint', title: 'Checkpoint' },
  ]), 'Checkpoint');
});

test('silent run that failed → Failed: <reason>, not idle', () => {
  const steps = [
    { kind: 'wake', title: 'Woke up', status: 'done' },
    { kind: 'route', title: 'Ran on Gemini', status: 'done' },
    { kind: 'checkpoint', title: 'Checkpoint', status: 'done' },
    { kind: 'message', title: 'Run ended with no output', status: 'error',
      body: 'Run ended with no output (exit 0, stderr: empty response).' },
    { kind: 'state', title: 'Finished: error', status: 'done' },
  ];
  assert.equal(runStepLabel(steps), 'Failed: Run ended with no output');
});

test('error run without a reason row keeps the state title', () => {
  assert.equal(runStepLabel([{ kind: 'state', title: 'Finished: error' }]), 'Finished: error');
});

test('successful finish is unchanged', () => {
  assert.equal(runStepLabel([
    { kind: 'message', title: 'Run ended with no output', status: 'error' },
    { kind: 'state', title: 'Finished: stopped' },
  ]), 'Finished: stopped');
});
