// Unit tests for Security Gates page helpers (STA-868).
// Run with: node --test tests/unit/gates.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const g = require('../../internal/server/webui/lib/gates.js');

test('advice line: recommendation with reason', () => {
  assert.deepEqual(g.gateAdviceView('together', { recommendation: 'approved', reason: 'read-only du/git survey' }, true),
    { cls: 'gate-advice-approve', text: 'Together: approve — read-only du/git survey' });
  assert.equal(g.gateAdviceView('gemini', { recommendation: 'denied', reason: '' }, false).text, 'Gemini: deny');
});

test('advice line: error, pending and nothing', () => {
  assert.equal(g.gateAdviceView('together', { recommendation: '', error: 'context deadline exceeded' }, true).text,
    'Together: no recommendation (context deadline exceeded)');
  assert.equal(g.gateAdviceView('together', { recommendation: '' }, true).text, 'Together: no recommendation (error)');
  assert.equal(g.gateAdviceView('together', undefined, true).text, 'Together: thinking…');
  assert.equal(g.gateAdviceView('together', undefined, false), null);
});

test('grace countdown label', () => {
  assert.equal(g.graceLabel(103), 'Touch ID valid for 1:43');
  assert.equal(g.graceLabel(59.9), 'Touch ID valid for 0:59');
  assert.equal(g.graceLabel(0), '');
  assert.equal(g.graceLabel(-5), '');
  assert.equal(g.graceLabel(undefined), '');
});

test('review selection keeps only pending ids and valid recommendations', () => {
  const review = { items: [
    { id: 'a', recommendation: 'approved' },
    { id: 'b', recommendation: 'denied' },
    { id: 'c', recommendation: 'approved' }, // no longer pending
    { id: 'd', recommendation: 'maybe' },
  ] };
  assert.deepEqual(g.reviewSelection(review, ['a', 'b', 'd']), { approved: ['a'], denied: ['b'] });
  assert.deepEqual(g.reviewSelection(null, ['a']), { approved: [], denied: [] });
});

test('agreement label', () => {
  assert.equal(g.agreementLabel({ advisor: 'together', decided: 4, agreed: 3, errors: 1 }),
    'Together agreed with you 3/4 (75%), 1 without a recommendation');
  assert.equal(g.agreementLabel({ advisor: 'gemini', decided: 0, agreed: 0 }), 'Gemini: no Board decisions to compare yet');
});

test('rule labels', () => {
  assert.equal(g.ruleScopeLabel({ scope: 'repo', scope_value: '/r' }), 'Repo: /r');
  const now = Date.parse('2026-10-06T12:00:00Z');
  assert.equal(g.ruleExpiryLabel({}, now), 'never expires');
  assert.equal(g.ruleExpiryLabel({ expires_at: '2026-10-06T12:30:00Z' }, now), 'expires in 30 min');
  assert.equal(g.ruleExpiryLabel({ expires_at: '2026-10-07T12:00:00Z' }, now), 'expires in 24 h');
  assert.equal(g.ruleExpiryLabel({ expires_at: '2026-10-06T11:00:00Z' }, now), 'expired');
});

test('batch summary names failed rows', () => {
  const s = g.batchSummary({ results: [{ id: 'aaaaaaaaaaaa', ok: true }, { id: 'bbbbbbbbbbbb', ok: false, error: 'already decided' }] }, 'approved');
  assert.equal(s.ok, false);
  assert.equal(s.text, '1 approved, 1 failed: bbbbbbbb (already decided)');
  assert.deepEqual(g.batchSummary({ results: [{ id: 'x', ok: true }] }, 'denied'), { ok: true, text: '1 denied' });
});

test('remember spec needs a value for the scope', () => {
  assert.deepEqual(g.rememberSpec({ task_id: 'T1' }, 'task', '60'),
    { spec: { scope: 'task', match_kind: 'exact', expires_in_minutes: 60 } });
  assert.match(g.rememberSpec({ task_id: 'T1' }, 'org', '').error, /no organization/);
  assert.equal(g.rememberSpec({ repo: '/r' }, 'repo', '').spec.expires_in_minutes, 0);
  assert.match(g.rememberSpec({}, 'bogus', '').error, /scope/);
});

// ── Task trust (task-6c1ed91f) ─────────────────────────────────────────────

test('trust spec: never carries an expiry or exclusions', () => {
  assert.deepEqual(g.trustSpec('4h', '', false, false), { spec: { preset: '4h' } });
  assert.deepEqual(g.trustSpec('custom', '90', false, false), { spec: { preset: 'custom', minutes: 90 } });
  const s = g.trustSpec('overnight', '', true, true).spec;
  assert.deepEqual(Object.keys(s).sort(), ['preset', 'tev1', 'tev1_ack']);
});

test('trust on a backlog task moves it to todo under the same Touch ID (task-40f0a2f0)', () => {
  assert.deepEqual(g.trustSpec('overnight', '', false, false, true), { spec: { preset: 'overnight', move_to_todo: true } });
  assert.deepEqual(g.trustSpec('overnight', '', false, false, false), { spec: { preset: 'overnight' } });
  assert.equal(g.trustButtonLabel('backlog'), 'Move to todo and trust (Touch ID)');
  for (const s of ['todo', 'in_progress', undefined]) assert.equal(g.trustButtonLabel(s), 'Trust (Touch ID)');
});

test('trust spec: rejects bad windows and unconfirmed tev1', () => {
  assert.match(g.trustSpec('forever', '', false, false).error, /Pick/);
  assert.match(g.trustSpec('custom', '5', false, false).error, /15 to 1440/);
  assert.match(g.trustSpec('custom', '99999', false, false).error, /15 to 1440/);
  assert.match(g.trustSpec('1h', '', true, false).error, /warning/);
});

test('trust banner: until, count, tev1 and waiting', () => {
  const now = new Date(2026, 9, 7, 18, 0).getTime();
  const exp = new Date(2026, 9, 7, 21, 30).toISOString();
  assert.equal(g.trustBannerText({ expires_at: exp, auto_approved_count: 3 }, now), 'Trusted until 21:30 · 3 auto-approved');
  assert.equal(g.trustBannerText({ expires_at: exp, auto_approved_count: 0, tev1: true, tev1_threshold: 0.7, held_count: 1, deferred_count: 1 }, now),
    'Trusted until 21:30 · 0 auto-approved · tev1 decides (p ≥ 0.70) · 2 waiting for you');
  assert.equal(g.trustBannerText(null, now), '');
});

test('tev1 morning summary lists approvals then denials', () => {
  const lines = g.tev1SummaryLines({ tev1_approved: [{ id: 'a', cmdline: 'go test ./...' }], tev1_denied: [{ id: 'b', cmdline: 'sqlite3 x.db' }] });
  assert.deepEqual(lines.map(l => l.text), ['tev1 approved: go test ./...', 'tev1 denied: sqlite3 x.db']);
  assert.deepEqual(g.tev1SummaryLines({}), []);
});

test('gate status label: deferred and tev1', () => {
  assert.equal(g.gateStatusLabel({ status: 'pending', deferred_at: '2026-10-07T10:00:00Z' }), 'deferred');
  assert.equal(g.gateStatusLabel({ status: 'denied', decided_by: 'rule:4:tev1' }), 'denied by tev1');
  assert.equal(g.gateStatusLabel({ status: 'approved', decided_by: 'rule:4' }), 'approved');
  assert.equal(g.ruleScopeLabel({ match_kind: 'any', scope: 'task', scope_value: 'task-1', tev1: true }), 'Trust (tev1): task task-1');
});
