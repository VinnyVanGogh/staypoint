// Unit tests for the ship review card's Target CI row (task-2114d4aa).
// Run with: node --test tests/unit/target-ci.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const t = require('../../internal/server/webui/lib/targetci.js');

const red = {
  branch: 'main', state: 'red', head_sha: '0ef0b68abcdef',
  failing: [{ workflow: 'CI/CD Pipeline', url: 'https://github.com/o/r/actions/runs/1', failed_jobs: ['Playwright UI Specs'] }],
};

test('red target blocks and names the failing job', () => {
  const v = t.targetCIView(red);
  assert.equal(v.blocking, true);
  assert.equal(v.state, 'red');
  assert.match(v.text, /main is red at 0ef0b68: Playwright UI Specs/);
  assert.deepEqual(v.links, [{ label: 'CI/CD Pipeline', url: 'https://github.com/o/r/actions/runs/1' }]);
});

test('red without job names falls back to the workflow', () => {
  assert.deepEqual(t.targetCIFailing({ failing: [{ workflow: 'CI' }, { workflow: 'Lint', failed_jobs: [] }] }), ['CI', 'Lint']);
});

test('red stays blocking while a newer run is in progress', () => {
  const v = t.targetCIView({ ...red, running: ['CI/CD Pipeline'] });
  assert.equal(v.blocking, true);
  assert.match(v.text, /newer run is in progress/);
});

test('unknown, pending and green never block', () => {
  for (const ci of [null, undefined, {}, { state: 'unknown', note: 'gh missing' }, { state: 'pending', branch: 'main' }, { state: 'green', branch: 'main' }]) {
    assert.equal(t.targetCIView(ci).blocking, false, JSON.stringify(ci));
  }
  assert.match(t.targetCIView({ state: 'unknown', branch: 'dev', note: 'gh missing' }).text, /dev CI unknown: gh missing/);
});

test('an unrecognised state reads as unknown, not green', () => {
  const v = t.targetCIView({ state: 'weird', branch: 'main' });
  assert.equal(v.state, 'unknown');
  assert.doesNotMatch(v.text, /green/);
});
