/* Pure, DOM-free task board helpers shared by app.js and Node unit tests.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api), like runsteps.js.
 *
 * Board columns follow the execution stage set (docs/task-stages.md):
 * backlog, todo, in_progress, in_review, blocked, done. cancelled and
 * soft-deleted tasks are not on the board.
 */
(function (root) {
  'use strict';

  const BOARD_COLUMNS = ['backlog', 'todo', 'in_progress', 'in_review', 'blocked', 'done'];

  const BOARD_COLUMN_TITLES = {
    backlog: 'Backlog',
    todo: 'Todo',
    in_progress: 'In Progress',
    in_review: 'In Review',
    blocked: 'Blocked',
    done: 'Done',
  };

  const NO_COMPANY = 'No company';

  // boardColumnFor maps a task (local: execution_stage + status; Paperclip
  // fleet: normalized status) to a board column, or null when it is closed
  // without being done (cancelled, soft-deleted, rejected).
  function boardColumnFor(task) {
    if (!task) return null;
    const status = String(task.status || '').toLowerCase();
    const stage = String(task.execution_stage || '').toLowerCase();
    if (status === 'soft_deleted' || stage === 'cancelled' || stage === 'rejected' || status === 'cancelled') return null;
    if (status === 'done' || status === 'completed' || stage === 'done') return 'done';
    if (task.is_blocked || stage === 'blocked' || status === 'blocked') return 'blocked';
    switch (stage || status) {
      case 'backlog':
        return 'backlog';
      case 'in_progress':
      case 'running':
      case 'paused':
        return 'in_progress';
      case 'in_review':
        return 'in_review';
      default:
        // todo, active, capped, stopped: waiting for a run.
        return 'todo';
    }
  }

  function isLegacy(task) {
    return !!task && task.origin === 'legacy';
  }

  // isArchived: a finished Paperclip import (done or cancelled), including
  // the "Paperclip archive — <Company>" parents.
  function isArchived(task) {
    if (!task || task.origin !== 'paperclip_import') return false;
    const stage = String(task.execution_stage || '').toLowerCase();
    return stage === 'done' || stage === 'cancelled';
  }

  // isHiddenByDefault: behind the one "Show archive & legacy" toggle.
  function isHiddenByDefault(task) {
    return isLegacy(task) || isArchived(task);
  }

  // Legacy tasks and archived imports are hidden on every page and count by
  // default. Only these three pages carry a "Show archive & legacy" toggle;
  // the All Tasks page shows everything by default and has its own filters.
  const HIDDEN_TOGGLE_PAGES = ['recent-tasks', 'task-status', 'kanban'];
  const ALL_TASKS_PAGE = 'all-tasks';

  function hasHiddenToggle(page) {
    return HIDDEN_TOGGLE_PAGES.includes(page);
  }

  // includeHiddenFor: whether a page lists legacy/archived tasks. All Tasks
  // always does; the toggle pages only while their toggle is on; every other
  // page never does.
  function includeHiddenFor(page, toggleOn) {
    if (page === ALL_TASKS_PAGE) return true;
    if (hasHiddenToggle(page)) return !!toggleOn;
    return false;
  }

  // visibleTasks drops legacy tasks and archived imports unless includeHidden.
  // It never mutates its input.
  function visibleTasks(tasks, includeHidden) {
    const list = Array.isArray(tasks) ? tasks : [];
    if (includeHidden) return list.slice();
    return list.filter(t => t && !isHiddenByDefault(t));
  }

  // taskVisibility labels a task for badges and the All Tasks visibility
  // filter: 'legacy', 'archive' or 'current'.
  function taskVisibility(task) {
    if (isLegacy(task)) return 'legacy';
    if (isArchived(task)) return 'archive';
    return 'current';
  }

  function taskStageOf(task) {
    return String((task && (task.execution_stage || task.status)) || '').toLowerCase();
  }

  // filterAllTasks backs the All Tasks page: every task, narrowed by org,
  // origin, stage, visibility ('all' | 'current' | 'hidden' | 'legacy' |
  // 'archive') and a free-text query over title, identifier, org and project.
  // 'all' (or empty) means no filter. Sorted newest update first.
  function filterAllTasks(tasks, f) {
    const o = f || {};
    const q = String(o.q || '').trim().toLowerCase();
    const want = (v) => v && v !== 'all';
    const out = (Array.isArray(tasks) ? tasks : []).filter(t => {
      if (!t) return false;
      if (want(o.org) && companyOf(t) !== o.org) return false;
      if (want(o.origin) && String(t.origin || '') !== o.origin) return false;
      if (want(o.stage) && taskStageOf(t) !== o.stage) return false;
      if (want(o.visibility)) {
        const v = taskVisibility(t);
        if (o.visibility === 'hidden' ? v === 'current' : v !== o.visibility) return false;
      }
      if (q) {
        const hay = [t.title, t.name, t.identifier, t.source_ref, t.id, t.organization, t.project]
          .map(x => String(x || '').toLowerCase()).join(' ');
        if (!hay.includes(q)) return false;
      }
      return true;
    });
    return out.sort(byUpdatedDesc);
  }

  function companyOf(task) {
    const org = String((task && (task.organization || task.org)) || '').trim();
    return org || NO_COMPANY;
  }

  function byUpdatedDesc(a, b) {
    return new Date(b.updated_at || 0) - new Date(a.updated_at || 0);
  }

  function emptyColumns() {
    const cols = {};
    for (const c of BOARD_COLUMNS) cols[c] = [];
    return cols;
  }

  // buildBoard returns [{ company, columns: {col: [task]}, total }]. Without
  // groupByCompany there is one group with company null. Legacy tasks are
  // left out unless showLegacy. Groups sort by company name, NO_COMPANY last;
  // each column sorts newest update first.
  function buildBoard(tasks, opts) {
    const o = opts || {};
    const groups = new Map();
    for (const t of tasks || []) {
      if (!o.showLegacy && isHiddenByDefault(t)) continue;
      const col = boardColumnFor(t);
      if (!col) continue;
      const key = o.groupByCompany ? companyOf(t) : null;
      if (!groups.has(key)) groups.set(key, { company: key, columns: emptyColumns(), total: 0 });
      const g = groups.get(key);
      g.columns[col].push(t);
      g.total++;
    }
    if (!o.groupByCompany && !groups.has(null)) {
      groups.set(null, { company: null, columns: emptyColumns(), total: 0 });
    }
    const out = [...groups.values()];
    for (const g of out) {
      for (const c of BOARD_COLUMNS) g.columns[c].sort(byUpdatedDesc);
    }
    out.sort((a, b) => {
      if (a.company === b.company) return 0;
      if (a.company === NO_COMPANY) return 1;
      if (b.company === NO_COMPANY) return -1;
      return String(a.company).localeCompare(String(b.company));
    });
    return out;
  }

  // countLegacy is the number of board tasks the archive & legacy toggle
  // would reveal.
  function countLegacy(tasks) {
    let n = 0;
    for (const t of tasks || []) if (isHiddenByDefault(t) && boardColumnFor(t)) n++;
    return n;
  }

  const api = {
    BOARD_COLUMNS, BOARD_COLUMN_TITLES, NO_COMPANY, HIDDEN_TOGGLE_PAGES, ALL_TASKS_PAGE,
    boardColumnFor, isLegacy, isArchived, isHiddenByDefault, hasHiddenToggle, includeHiddenFor,
    visibleTasks, taskVisibility, taskStageOf, filterAllTasks, companyOf, buildBoard, countLegacy,
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
