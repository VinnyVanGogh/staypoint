/* Pure, DOM-free helpers for the task page's Board close controls (STA-861):
 * Mark done / Cancel task on any non-terminal task, and the work-kind select.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api), like runsteps.js.
 */
(function (root) {
  'use strict';

  const TERMINAL_STAGES = new Set(['done', 'cancelled', 'rejected']);
  const TERMINAL_STATUSES = new Set(['done', 'soft_deleted']);
  const RUN_ACTIVE_STAGES = new Set(['in_progress', 'paused']);
  // Code kinds route to Claude only; mirrors router.KindMayWriteCode.
  const CODE_KINDS = new Set(['coding', 'qa']);
  const WORK_KINDS = ['coding', 'review', 'architecture', 'planning', 'qa', 'docs'];

  function taskStage(task) {
    return (task && (task.execution_stage || task.status)) || '';
  }

  // A task is terminal once done, cancelled or rejected: nothing to close.
  function taskIsTerminal(task) {
    if (!task) return true;
    return TERMINAL_STAGES.has(task.execution_stage) || TERMINAL_STATUSES.has(task.status);
  }

  // A run holds the task while it is in_progress or paused.
  function taskRunActive(task) {
    return !!task && !taskIsTerminal(task) && RUN_ACTIVE_STAGES.has(taskStage(task));
  }

  // Which Board close controls the task page shows. Every non-terminal stage
  // (backlog, todo, in_progress, blocked, in_review, stopped, capped, error)
  // gets Mark done and Cancel task.
  function taskCloseActions(task) {
    const open = !!(task && task.id) && !taskIsTerminal(task);
    return { markDone: open, cancel: open, runActive: open && taskRunActive(task) };
  }

  // The confirm shown before Mark done, or '' when none is needed.
  function markDoneConfirmText(task, opts) {
    const hasActiveCard = !!(opts && opts.hasActiveCard);
    const parts = [];
    if (taskRunActive(task)) parts.push('A run is in progress. It will be stopped first.');
    if (hasActiveCard) parts.push('This task has an open ship review card; marking it done closes the task without merging.');
    if (!parts.length) return '';
    return parts.join('\n') + '\n\nMark this task done?';
  }

  function cancelConfirmText(task) {
    const lead = taskRunActive(task) ? 'A run is in progress. It will be stopped first.\n\n' : '';
    return lead + 'Cancel this task? It is closed and will not run again unless reopened.';
  }

  // Request body for the Board's Mark done (POST /api/tasks/{id}/done).
  function markDoneBody(note) {
    const body = { board: true };
    const n = String(note || '').trim();
    if (n) body.note = n;
    return body;
  }

  // Why the Board cannot switch task to kind, or '' when it can. Mirrors the
  // server: no change while a run holds the task, and Gemini never takes a
  // code kind in a work repo (#231).
  function kindChangeRefusal(task, kind, isWork) {
    if (!WORK_KINDS.includes(kind)) return `Unknown kind: ${kind}`;
    if (taskRunActive(task)) return 'Stop the run before changing the kind of work.';
    if (task && task.provider === 'gemini' && CODE_KINDS.has(kind) && isWork === true) {
      return 'Gemini never writes code in a work repo: change the provider first.';
    }
    return '';
  }

  // Stages POST /api/tasks/{id}/stage accepts; mirrors
  // governance.BoardSettableStages (checked by stage_mirror_test.go).
  const BOARD_STAGES = ['backlog', 'todo', 'in_progress', 'in_review', 'done', 'cancelled'];
  // Stages that are never run; mirrors governance.nonRunnableStages. An
  // agent task parked in one keeps it when blocked (context.blockedStageSQL).
  const PARKED_STAGES = new Set(['backlog', 'done', 'cancelled', 'rejected', 'stopped', 'error']);
  const STAGE_LABELS = {
    backlog: 'Backlog', todo: 'Todo', in_progress: 'In progress (Run Now)',
    in_review: 'In review', blocked: 'Blocked…', done: 'Done…',
  };

  // The task page's "Move to…" choices (task-40f0a2f0): {stage, label, via}.
  // via picks the request: 'stage' posts /stage, 'run' is Run Now, 'done' is
  // Mark done (its confirm and gate), 'block' asks for a reason and posts
  // /block. The server still decides; this only leaves out moves it would
  // refuse or ignore. cancelled is the Cancel task button's.
  function stageMenuOptions(task, opts) {
    if (!task || !task.id) return [];
    const canRun = !!(opts && opts.canRun);
    const current = taskStage(task);
    const terminal = taskIsTerminal(task);
    const out = [];
    const add = (stage, via) => { if (stage !== current) out.push({ stage, label: STAGE_LABELS[stage], via }); };
    add('backlog', 'stage');
    add('todo', 'stage');
    if (terminal) return out; // a closed task can only be reopened
    if (canRun) add('in_progress', 'run');
    add('in_review', 'stage');
    if (!(task.origin === 'agent' && PARKED_STAGES.has(current))) add('blocked', 'block');
    add('done', 'done');
    return out;
  }

  // The confirm shown before moving task to stage, or '' when none is needed.
  // Done and Run Now have their own.
  function stageChangeConfirmText(task, stage) {
    if (stage === 'done' || stage === 'in_progress' || !taskRunActive(task)) return '';
    const label = (STAGE_LABELS[stage] || stage).replace(/…$/, '');
    return `A run is in progress. It will be stopped first.\n\nMove this task to ${label}?`;
  }

  const api = { WORK_KINDS, BOARD_STAGES, stageMenuOptions, stageChangeConfirmText, taskIsTerminal, taskRunActive, taskCloseActions, markDoneConfirmText, cancelConfirmText, markDoneBody, kindChangeRefusal };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
