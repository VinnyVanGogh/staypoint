/* Pure, DOM-free task URL helpers shared by app.js and Node unit tests
 * (task-eb38c245). A task's canonical page is /STA-123/<slug>: organization
 * key + number, then an optional slug that is decoration only; lookup uses the
 * number alone. The project is never in the URL.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 */
(function (root) {
  'use strict';

  // A task reference: organization key + number (STA-123).
  const TASK_REF_RE = /^[A-Za-z]{2,5}-[1-9][0-9]{0,8}$/;
  // /STA-123 or /STA-123/<slug>.
  const TASK_REF_PATH_RE = /^\/([A-Za-z]{2,5}-[1-9][0-9]{0,8})(?:\/([^/]*))?\/?$/;

  function isTaskRef(s) {
    return TASK_REF_RE.test(String(s || ''));
  }

  // Any path that shows a task page: the canonical form or the old
  // /tasks/<org>/<project>/<id>, /tasks/<id> and /issues/... forms.
  function isTaskPagePath(p) {
    p = String(p || '');
    return p.startsWith('/tasks/') || p.startsWith('/issues/') || TASK_REF_PATH_RE.test(p);
  }

  // The upper-cased reference a canonical path names, or '' for any other path.
  function taskRefFromPath(p) {
    const m = TASK_REF_PATH_RE.exec(String(p || ''));
    return m ? m[1].toUpperCase() : '';
  }

  // The canonical path of a numbered daemon task (id task-…, identifier
  // STA-123), or '' when the task has no reference (a fleet issue, or a task
  // written before numbering) and the caller falls back to the old form.
  function taskRefPath(task) {
    if (!task || !String(task.id || '').startsWith('task-') || !isTaskRef(task.identifier)) return '';
    return task.slug ? `/${task.identifier}/${encodeURIComponent(task.slug)}` : `/${task.identifier}`;
  }

  const api = { isTaskRef, isTaskPagePath, taskRefFromPath, taskRefPath };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
