/* Pure, DOM-free helpers for how every task list shows a task's state
 * (task-3387cad2): a Stage (where the task is in its life) and, separately,
 * Running (a run is in flight right now). The old single STATUS pill mixed
 * "active" (not cancelled/archived), "running" (stage in_progress) and "done",
 * and "ACTIVE" read as running. Lists no longer say "active" at all.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api), like taskboard.js.
 */
(function (root) {
  'use strict';

  const STAGES = ['backlog', 'todo', 'in_progress', 'in_review', 'blocked', 'done', 'cancelled'];

  const STAGE_LABELS = {
    backlog: 'Backlog',
    todo: 'Todo',
    in_progress: 'In progress',
    in_review: 'In review',
    blocked: 'Blocked',
    done: 'Done',
    cancelled: 'Cancelled',
  };

  const RUNNING_CHOICES = ['all', 'yes', 'no'];

  // taskStageKey maps any task (local: execution_stage + status; fleet and
  // Paperclip items: their status words) to one of STAGES.
  function taskStageKey(task) {
    if (!task) return 'todo';
    const status = String(task.status || '').toLowerCase();
    const stage = String(task.execution_stage || '').toLowerCase();
    if (task.is_blocked || stage === 'blocked' || status === 'blocked') return 'blocked';
    if (stage === 'done' || status === 'done' || status === 'completed' || status === 'closed') return 'done';
    if (stage === 'cancelled' || stage === 'rejected' || status === 'soft_deleted' || status === 'cancelled' || status === 'stopped') return 'cancelled';
    switch (stage || status) {
      case 'backlog':
        return 'backlog';
      case 'in_progress':
      case 'paused':
      case 'running':
        return 'in_progress';
      case 'in_review':
        return 'in_review';
      default:
        // todo, active, capped, stopped run, error: waiting for a run.
        return 'todo';
    }
  }

  function stageLabel(key) {
    return STAGE_LABELS[key] || STAGE_LABELS.todo;
  }

  // liveRunIndex turns GET /api/runs/live's runs into a Map task_id -> run.
  function liveRunIndex(runs) {
    const m = new Map();
    for (const r of Array.isArray(runs) ? runs : []) {
      if (r && r.task_id) m.set(r.task_id, r);
    }
    return m;
  }

  // taskIsRunning: a run is in flight. With the live index (the one source
  // the lists share) only it decides; without one, the task's own flag.
  function taskIsRunning(task, live) {
    if (!task) return false;
    if (live instanceof Map) return live.has(task.id);
    return task.running === true;
  }

  // runningByOrg counts live runs per organization, for the sidebar.
  function runningByOrg(live) {
    const counts = {};
    if (!(live instanceof Map)) return counts;
    for (const r of live.values()) {
      const org = String(r.organization || '').trim() || 'StayPoint';
      counts[org] = (counts[org] || 0) + 1;
    }
    return counts;
  }

  // elapsedLabel: "45s", "12m", "1h 05m" since startedAt (ISO), or '' when
  // unknown.
  function elapsedLabel(startedAt, nowMs) {
    const t = Date.parse(startedAt || '');
    if (!Number.isFinite(t)) return '';
    const s = Math.max(0, Math.floor(((nowMs === undefined ? Date.now() : nowMs) - t) / 1000));
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    return `${h}h ${String(m % 60).padStart(2, '0')}m`;
  }

  // normalizeListFilter: {running: 'all'|'yes'|'no', stages: [stage...]}.
  // Unknown values drop out; no stages means every stage.
  function normalizeListFilter(raw) {
    const r = raw && typeof raw === 'object' ? raw : {};
    const running = RUNNING_CHOICES.includes(r.running) ? r.running : 'all';
    const stages = [];
    for (const s of Array.isArray(r.stages) ? r.stages : []) {
      if (STAGES.includes(s) && !stages.includes(s)) stages.push(s);
    }
    return { running, stages };
  }

  function listFilterActive(f) {
    const n = normalizeListFilter(f);
    return n.running !== 'all' || n.stages.length > 0;
  }

  // matchesListFilter: does task pass the Running and Stage filters?
  function matchesListFilter(task, f, live) {
    const n = normalizeListFilter(f);
    if (n.running !== 'all' && taskIsRunning(task, live) !== (n.running === 'yes')) return false;
    if (n.stages.length && !n.stages.includes(taskStageKey(task))) return false;
    return true;
  }

  function listFilterKey(page) {
    return `staypoint_list_filter_${page}`;
  }

  // loadListFilter / saveListFilter persist one page's filter in storage
  // (localStorage in the browser). Storage errors fall back to the default.
  function loadListFilter(storage, page) {
    try {
      const raw = storage && storage.getItem(listFilterKey(page));
      return normalizeListFilter(raw ? JSON.parse(raw) : null);
    } catch {
      return normalizeListFilter(null);
    }
  }

  function saveListFilter(storage, page, f) {
    const n = normalizeListFilter(f);
    try {
      if (storage) storage.setItem(listFilterKey(page), JSON.stringify(n));
    } catch { /* storage full or blocked: keep the in-memory filter */ }
    return n;
  }

  // stageFilterSummary is the Stage dropdown's button text.
  function stageFilterSummary(stages) {
    const n = normalizeListFilter({ stages }).stages;
    if (!n.length || n.length === STAGES.length) return 'All stages';
    if (n.length <= 2) return n.map(stageLabel).join(', ');
    return `${n.length} stages`;
  }

  const api = {
    STAGES, STAGE_LABELS, RUNNING_CHOICES, taskStageKey, stageLabel, liveRunIndex, taskIsRunning,
    runningByOrg, elapsedLabel, normalizeListFilter, listFilterActive, matchesListFilter,
    listFilterKey, loadListFilter, saveListFilter, stageFilterSummary,
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
