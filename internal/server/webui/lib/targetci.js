/* Pure, DOM-free helpers for the ship review card's "Target CI" row
 * (task-2114d4aa), shared by app.js and Node unit tests.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 */
(function (root) {
  'use strict';

  // targetCIFailing names what is red on the target: the failed jobs, or the
  // workflow when its jobs could not be read.
  function targetCIFailing(ci) {
    const out = [];
    for (const f of (ci && ci.failing) || []) {
      if (f.failed_jobs && f.failed_jobs.length) out.push(...f.failed_jobs);
      else out.push(f.workflow || 'CI');
    }
    return out;
  }

  // targetCIView describes the API's target_ci for display.
  // Returns { state, blocking, status, text, links: [{ label, url }] }.
  // Only red blocks: unknown and pending say so and leave Approve alone (the
  // server re-reads at Approve either way).
  function targetCIView(ci) {
    const branch = (ci && ci.branch) || 'target';
    const state = (ci && ci.state) || 'unknown';
    const head = ci && ci.head_sha ? ` at ${ci.head_sha.slice(0, 7)}` : '';
    const running = (ci && ci.running) || [];
    if (state === 'red') {
      const failing = targetCIFailing(ci);
      const links = ((ci && ci.failing) || []).filter((f) => f.url)
        .map((f) => ({ label: f.workflow || 'CI', url: f.url }));
      const still = running.length ? ` A newer run is in progress (${running.join(', ')}).` : '';
      return {
        state, blocking: true, status: 'red', links,
        text: `✗ ${branch} is red${head}: ${failing.join(', ')}. Approve is blocked until ${branch} is green.${still}`,
      };
    }
    if (state === 'green') {
      const still = running.length ? ` (newer run in progress: ${running.join(', ')})` : '';
      return { state, blocking: false, status: 'green', links: [], text: `✓ ${branch} is green${head}${still}` };
    }
    if (state === 'pending') {
      return { state, blocking: false, status: 'running', links: [], text: `⏳ ${branch}'s first CI run is still in progress${head}` };
    }
    const note = ci && ci.note ? `: ${ci.note}` : '';
    return { state: 'unknown', blocking: false, status: 'unknown', links: [], text: `${branch} CI unknown${note}` };
  }

  const api = { targetCIFailing, targetCIView };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  Object.assign(root, api);
})(typeof globalThis !== 'undefined' ? globalThis : this);
