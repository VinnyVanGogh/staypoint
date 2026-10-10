// Unit tests for quota gauge display helpers (STA-853).
// Run with: node --test tests/unit/quota-gauge.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const {
  quotaWindowView, quotaStatusPill, quotaWindowMeasured,
  formatCountdown, quotaProjectionText, quotaFiveHourResetText,
} = require('../../internal/server/webui/lib/quotagauge.js');

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

// task-036279f2: the Settings card showed "4m" in its header and "6m" in its
// Reset Time row for the same reset. Every section renders from the same pool
// fixture and the same tick, so all countdowns must read the same.
test('locked pool: header, banner and reset row show one countdown', () => {
  const now = Date.parse('2026-10-10T02:04:00Z');
  const reset = '2026-10-10T02:10:00Z';
  const q = {
    measured: true, five_hour_measured: true, five_hour_used_pct: 100, five_hour_remaining_pct: 0,
    is_locked: true, projection_status: 'locked_out',
    // Server text frozen at gather time, minutes before this render.
    projection_message: 'Locked out: resets in 9m',
    five_hour_resets_at: reset, lockout_until: reset,
  };
  const row = quotaFiveHourResetText(q, now);
  const banner = formatCountdown(q.lockout_until, now);
  assert.equal(row, 'in 6m');
  assert.equal(banner, row);
  assert.equal(quotaProjectionText(q, now), `Locked out: resets ${row}`);
  // Overview gauge card and Settings card use the same helpers.
  assert.equal(quotaProjectionText(q, now, 'Sustainable pacing'), quotaProjectionText(q, now, 'Standard allocation'));
});

test('unlocked or expired pool keeps the server projection text', () => {
  const now = Date.parse('2026-10-10T02:04:00Z');
  const onTrack = { projection_status: 'on_track', projection_message: 'On Track: healthy 5-hour headroom' };
  assert.equal(quotaProjectionText(onTrack, now), 'On Track: healthy 5-hour headroom');
  const expired = { is_locked: true, projection_message: 'Locked out: quota limit reached', lockout_until: '2026-10-10T02:00:00Z' };
  assert.equal(quotaProjectionText(expired, now), 'Locked out: quota limit reached');
  assert.equal(quotaProjectionText({}, now, 'Standard allocation'), 'Standard allocation');
});

test('formatCountdown is deterministic for a given now', () => {
  const now = Date.parse('2026-10-10T00:00:00Z');
  assert.equal(formatCountdown('2026-10-10T01:30:00Z', now), 'in 1h 30m');
  assert.equal(formatCountdown('2026-10-12T03:00:00Z', now), 'in 2d 3h');
  assert.equal(formatCountdown('2026-10-09T23:00:00Z', now), 'resets soon');
  assert.equal(formatCountdown(null, now), '');
  assert.equal(quotaFiveHourResetText({ measured: false, five_hour_resets_at: '2026-10-10T01:00:00Z' }, now), '');
});
