// Unit tests for quota gauge display helpers (STA-853).
// Run with: node --test tests/unit/quota-gauge.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { quotaWindowView, quotaStatusPill, quotaWindowMeasured } = require('../../internal/server/webui/lib/quotagauge.js');

test('unmeasured gauge renders No data, not 0% used', () => {
  const q = { measured: false, five_hour_used_pct: 0, five_hour_remaining_pct: 100, projection_status: 'unknown' };
  const v = quotaWindowView(q, 'five_hour');
  assert.equal(v.measured, false);
  assert.equal(v.text, 'No data');
  assert.equal(v.used, null);
  assert.deepEqual(quotaStatusPill(q), { cls: 'pill-stopped', text: 'No data' });
});

test('one unmeasured window only blanks that window', () => {
  const q = { measured: true, five_hour_measured: true, weekly_measured: false, five_hour_used_pct: 42, five_hour_remaining_pct: 58 };
  assert.equal(quotaWindowView(q, 'five_hour').text, '42.0% used · 58.0% left');
  assert.equal(quotaWindowView(q, 'weekly').text, 'No data');
});

test('locked work seat shows real 100% and Locked Out pill', () => {
  const q = { measured: true, five_hour_measured: true, five_hour_used_pct: 100, five_hour_remaining_pct: 0, is_locked: true, projection_status: 'locked_out' };
  assert.equal(quotaWindowView(q, 'five_hour').text, '100.0% used · 0.0% left');
  assert.equal(quotaStatusPill(q).text, '✖ Locked Out');
});

test('payload without measured flags keeps legacy rendering', () => {
  const q = { five_hour_used_pct: 10 };
  assert.equal(quotaWindowMeasured(q, 'five_hour'), true);
  assert.equal(quotaWindowView(q, 'five_hour').text, '10.0% used · 90.0% left');
  assert.equal(quotaStatusPill({ projection_status: 'on_track' }).text, '✔ On Track');
});
