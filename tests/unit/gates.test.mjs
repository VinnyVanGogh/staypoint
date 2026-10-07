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
