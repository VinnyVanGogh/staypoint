/* Pure, DOM-free run-step helpers shared by app.js and Node unit tests.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).  Do NOT convert app.js to a
 * module yet — inline onclick handlers rely on globals.
 */
(function (root) {
  'use strict';

  // Group all steps by run_id, returning arrays in order of first appearance.
  function groupStepsByRun(allSteps) {
    const runs  = new Map();
    const order = [];
    for (const s of allSteps) {
      const rid = s.run_id || '';
      if (!runs.has(rid)) { runs.set(rid, []); order.push(rid); }
      runs.get(rid).push(s);
    }
    return order.map(rid => runs.get(rid));
  }

  // A run is "real" when it has at least one step beyond the overhead trio
  // (wake / route / state). Refused runs (lock held) only ever write those three.
  function isRealRunGroup(steps) {
    return steps.some(s => s.kind !== 'wake' && s.kind !== 'route' && s.kind !== 'state');
  }

  // Return the steps of the most recently started run (the last run_id group),
  // regardless of whether it qualifies as a "real" run. Used for the stuck-age
  // check so a fresh run whose only steps are wake/route does not inherit the
  // previous run's stale last-step timestamp.
  function latestRunSteps(allSteps) {
    if (!allSteps || !allSteps.length) return [];
    const groups = groupStepsByRun(allSteps);
    return groups[groups.length - 1] || [];
  }

  // Return the steps of the current or most-recent real run.
  // Falls back to the last run group when no real run exists.
  function currentRunSteps(allSteps) {
    if (!allSteps || !allSteps.length) return [];
    const groups = groupStepsByRun(allSteps);
    for (let i = groups.length - 1; i >= 0; i--) {
      if (isRealRunGroup(groups[i])) return groups[i];
    }
    return groups[groups.length - 1] || [];
  }

  // Elapsed for a run's own steps: from the first step to the last state step
  // (if the run ended) or to nowMs (if still running).
  function runElapsedMs(runSteps, nowMs) {
    if (!runSteps.length) return null;
    const startMs   = new Date(runSteps[0].created_at).getTime();
    const stateStep = [...runSteps].reverse().find(s => s.kind === 'state');
    if (stateStep) return new Date(stateStep.created_at).getTime() - startMs;
    return nowMs - startMs;
  }

  // Extracted isStuck predicate used by both refreshTaskStatsBar and renderTaskPage.
  // Caller passes (steps, Date.now(), task.status).
  // Statuses with no run in flight: a quiet timeline there is expected.
  const NOT_RUNNING = new Set(['in_review', 'done', 'cancelled', 'blocked']);

  function isStuck(allSteps, nowMs, taskStatus) {
    if (NOT_RUNNING.has(taskStatus)) return false;
    const latest     = latestRunSteps(allSteps);
    // A terminal state step means the run finished, however long ago.
    if (latest.some(s => s.kind === 'state')) return false;
    const lastStepAt = latest.length ? new Date(latest[latest.length - 1].created_at).getTime() : null;
    return lastStepAt !== null && (nowMs - lastStepAt) > 5 * 60 * 1000;
  }

  // Model named by a route step title, for the task page header. Only titles
  // that name the model count: "Routed to X", "Ran on X", or "Fell back to X:
  // <reason>". Anything else ("All providers locked", ...) is not a model.
  function routeModel(title) {
    const t = String(title || '').trim();
    let m = t.match(/^(?:Routed to|Ran on)\s+(.+)$/i);
    if (m) return m[1].trim();
    m = t.match(/^Fell back to\s+([^:]+):/i);
    return m ? m[1].trim() : '';
  }

  const api = { groupStepsByRun, isRealRunGroup, latestRunSteps, currentRunSteps, runElapsedMs, isStuck, routeModel };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
