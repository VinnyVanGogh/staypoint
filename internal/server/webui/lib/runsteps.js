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

  // Task page line for a run waiting in the daemon's run queue (STA-773).
  // pos is GET /api/tasks/{id} .queue: { queued, ahead, wait }. Returns ''
  // when the task is not queued.
  // Each reason names the cap that holds the run (STA-867).
  const QUEUE_WAIT_TEXT = {
    repo: 'repo limit: this repo is at max_runs_per_repo',
    folder: 'repo limit: another run is using this non-git folder',
    org: 'org limit: this organization is at max_runs_per_org',
    slots: 'global limit: max_concurrent_runs reached',
    quota: 'quota: provider quota is locked',
  };
  function queueLabel(pos) {
    if (!pos || !pos.queued) return '';
    const n = Math.max(0, Number(pos.ahead) || 0);
    const base = `Queued: ${n} run${n === 1 ? '' : 's'} ahead`;
    const why = QUEUE_WAIT_TEXT[pos.wait];
    return why ? `${base} (${why})` : base;
  }

  // Queue position of taskId in a "run.queue" SSE payload's queue array.
  function queuePositionIn(queue, taskId) {
    const list = Array.isArray(queue) ? queue : [];
    const i = list.findIndex(q => q && q.task_id === taskId);
    return i < 0 ? { queued: false, ahead: 0 } : { queued: true, ahead: i, wait: list[i].wait };
  }

  // Stats strip "Step" label for a run's steps (STA-775). A run that ended
  // with an error message row (e.g. "Run ended with no output") reads
  // "Failed: <that title>" rather than a bare state row; no steps is 'idle'.
  function runStepLabel(runSteps) {
    const steps = Array.isArray(runSteps) ? runSteps.filter(Boolean) : [];
    if (!steps.length) return 'idle';
    const last = steps[steps.length - 1];
    if (last.kind === 'state' && /error$/i.test(String(last.title || ''))) {
      const why = [...steps].reverse().find(s => s.kind === 'message' && s.status === 'error' && s.title);
      // A turn the stall watch stopped already reads as the reason.
      if (why) return /^Stopped:/.test(why.title) ? why.title : `Failed: ${why.title}`;
    }
    return last.title || 'idle';
  }

  // Stats strip "Turn" value for the turn in flight: GET /api/tasks/{id}
  // .turn or a run.turn SSE payload ({turn, started_at}). '' when idle.
  function turnElapsedLabel(turn, nowMs) {
    if (!turn || !turn.started_at) return '';
    const start = new Date(turn.started_at).getTime();
    if (isNaN(start)) return '';
    const sec = Math.max(0, Math.floor((nowMs - start) / 1000));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    const pad = (n) => String(n).padStart(2, '0');
    const elapsed = h > 0 ? `${h}h ${pad(m)}m` : `${m}m ${pad(s)}s`;
    return `#${(turn.turn || 0) + 1} · ${elapsed}`;
  }

  const api = { turnElapsedLabel, groupStepsByRun, isRealRunGroup, latestRunSteps, currentRunSteps, runElapsedMs, isStuck, routeModel, queueLabel, queuePositionIn, runStepLabel };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
