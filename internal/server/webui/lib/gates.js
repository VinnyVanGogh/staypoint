/* Pure, DOM-free helpers for the Security Gates page (STA-868), shared by
 * gates.js and Node unit tests.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 */
(function (root) {
  'use strict';

  const ADVISOR_LABELS = { together: 'Together', gemini: 'Gemini' };
  const REC_WORDS = { approved: 'approve', denied: 'deny' };

  // gateAdviceView describes one advisor's recommendation for a gate row.
  // advice is the API's {recommendation, reason, error} or undefined.
  // pendingOk: the advisor is enabled and the request is still pending, so a
  // missing row means "still thinking" rather than "none".
  // Returns null when there is nothing to show.
  function gateAdviceView(advisor, advice, pendingOk) {
    const label = ADVISOR_LABELS[advisor] || advisor;
    if (!advice) {
      return pendingOk ? { cls: 'gate-advice-wait', text: `${label}: thinking…` } : null;
    }
    const word = REC_WORDS[advice.recommendation];
    if (!word) {
      const why = advice.error ? ` (${advice.error})` : ' (error)';
      return { cls: 'gate-advice-none', text: `${label}: no recommendation${why}` };
    }
    const reason = advice.reason ? ` — ${advice.reason}` : '';
    return { cls: `gate-advice-${word}`, text: `${label}: ${word}${reason}` };
  }

  // graceLabel formats the passkey grace countdown, '' when none is open.
  function graceLabel(seconds) {
    const s = Math.floor(Number(seconds) || 0);
    if (s <= 0) return '';
    const m = Math.floor(s / 60);
    const r = String(s % 60).padStart(2, '0');
    return `Touch ID valid for ${m}:${r}`;
  }

  // reviewSelection turns an AI review into the ids to pre-check per
  // decision. Only ids still pending are kept.
  function reviewSelection(review, pendingIds) {
    const pending = new Set(pendingIds || []);
    const out = { approved: [], denied: [] };
    for (const it of (review && review.items) || []) {
      if (!pending.has(it.id)) continue;
      if (it.recommendation === 'approved' || it.recommendation === 'denied') {
        out[it.recommendation].push(it.id);
      }
    }
    return out;
  }

  // agreementLabel summarises one advisor's agreement with the Board.
  function agreementLabel(stat) {
    if (!stat) return '';
    const label = ADVISOR_LABELS[stat.advisor] || stat.advisor;
    if (!stat.decided) return `${label}: no Board decisions to compare yet`;
    const pct = Math.round((stat.agreed / stat.decided) * 100);
    const errs = stat.errors ? `, ${stat.errors} without a recommendation` : '';
    return `${label} agreed with you ${stat.agreed}/${stat.decided} (${pct}%)${errs}`;
  }

  // ruleScopeLabel names a rule's scope for display.
  function ruleScopeLabel(rule) {
    if (!rule) return '';
    const names = { task: 'Task', repo: 'Repo', org: 'Organization' };
    return `${names[rule.scope] || rule.scope}: ${rule.scope_value}`;
  }

  // ruleExpiryLabel describes when a rule stops applying, relative to now (ms).
  function ruleExpiryLabel(rule, now) {
    if (!rule || !rule.expires_at) return 'never expires';
    const left = new Date(rule.expires_at).getTime() - now;
    if (!(left > 0)) return 'expired';
    const mins = Math.round(left / 60000);
    if (mins < 60) return `expires in ${mins} min`;
    const hours = Math.round(mins / 60);
    if (hours < 48) return `expires in ${hours} h`;
    return `expires in ${Math.round(hours / 24)} d`;
  }

  // batchSummary reports a decide-batch response, naming failed rows.
  function batchSummary(resp, decision) {
    const verb = decision === 'approved' ? 'approved' : 'denied';
    const results = (resp && resp.results) || [];
    const failed = results.filter(r => !r.ok);
    let text = `${results.length - failed.length} ${verb}`;
    if (failed.length) {
      text += `, ${failed.length} failed: ` + failed.map(r => `${String(r.id).slice(0, 8)} (${r.error})`).join('; ');
    }
    return { ok: failed.length === 0, text };
  }

  // rememberSpec builds the "remember" body for Approve & remember, or an
  // error message. expiry is minutes ('' or 0 = never).
  function rememberSpec(request, scope, expiry) {
    const need = { task: 'task_id', repo: 'repo', org: 'org' }[scope];
    if (!need) return { error: 'Pick a scope' };
    if (!request || !request[need]) {
      return { error: `This request has no ${scope === 'org' ? 'organization' : scope}; pick another scope` };
    }
    const mins = Number(expiry) || 0;
    if (mins < 0) return { error: 'Expiry must not be negative' };
    return { spec: { scope, match_kind: 'exact', expires_in_minutes: mins } };
  }

  const api = {
    gateAdviceView, graceLabel, reviewSelection, agreementLabel,
    ruleScopeLabel, ruleExpiryLabel, batchSummary, rememberSpec,
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  Object.assign(root, api);
})(typeof globalThis !== 'undefined' ? globalThis : this);
