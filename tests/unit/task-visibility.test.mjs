// Unit tests for the Board rule on legacy tasks and archived imports: hidden
// on every page by default, a "Show archive & legacy" toggle only on Recent
// Tasks, Task Status and the Kanban board, and an All Tasks page that shows
// everything by default.
// Run with: node --test tests/unit/task-visibility.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const require = createRequire(import.meta.url);
const tb = require('../../internal/server/webui/lib/taskboard.js');
const here = dirname(fileURLToPath(import.meta.url));
const webui = join(here, '../../internal/server/webui');
const html = readFileSync(join(webui, 'index.html'), 'utf8');
const app = readFileSync(join(webui, 'app.js'), 'utf8');

const tasks = [
  { id: 'n', origin: 'native', execution_stage: 'todo', organization: 'StayPoint', title: 'native todo', updated_at: '2026-10-05' },
  { id: 'i', origin: 'paperclip_import', execution_stage: 'in_progress', organization: 'Managed Solution', title: 'open import', updated_at: '2026-10-04' },
  { id: 'a', origin: 'paperclip_import', execution_stage: 'done', organization: 'StayPoint', title: 'archived import', updated_at: '2026-10-03' },
  { id: 'c', origin: 'paperclip_import', execution_stage: 'cancelled', organization: 'StayPoint', title: 'cancelled import', updated_at: '2026-10-02' },
  { id: 'l', origin: 'legacy', execution_stage: 'todo', organization: 'StayPoint', title: 'legacy todo', updated_at: '2026-10-01' },
];
const ids = list => list.map(t => t.id).sort();

// sectionOf returns the id of the <section id="view-..."> enclosing offset.
function sectionOf(offset) {
  const before = html.slice(0, offset);
  const open = before.lastIndexOf('<section id="');
  const close = before.lastIndexOf('</section>');
  if (open === -1 || close > open) return null;
  return before.slice(open + '<section id="'.length).split('"')[0];
}

test('the toggle exists only on Recent Tasks, Task Status and Kanban', () => {
  const found = [];
  for (const m of html.matchAll(/Show archive &amp; legacy/g)) found.push(sectionOf(m.index));
  assert.deepEqual(found.sort(), ['view-kanban', 'view-recent-tasks', 'view-task-status']);
  const boxes = [...html.matchAll(/id="([a-z-]+-show-legacy)"/g)].map(m => [m[1], sectionOf(m.index)]);
  assert.deepEqual(boxes.sort(), [
    ['kanban-show-legacy', 'view-kanban'],
    ['recent-tasks-show-legacy', 'view-recent-tasks'],
    ['ts-show-legacy', 'view-task-status'],
  ]);
  assert.deepEqual([...tb.HIDDEN_TOGGLE_PAGES].sort(), ['kanban', 'recent-tasks', 'task-status']);
});

test('app.js wires a toggle for exactly the three toggle pages', () => {
  const block = app.slice(app.indexOf('const SHOW_HIDDEN_TOGGLES = {'), app.indexOf('};', app.indexOf('const SHOW_HIDDEN_TOGGLES = {')));
  const keys = [...block.matchAll(/^\s*'?([a-z-]+)'?:\s*\{/gm)].map(m => m[1]).sort();
  assert.deepEqual(keys, [...tb.HIDDEN_TOGGLE_PAGES].sort());
});

test('All Tasks page is in the sidebar and has no toggle', () => {
  assert.match(html, /data-view="all-tasks"[^>]*>[\s\S]*?All Tasks/);
  const start = html.indexOf('<section id="view-all-tasks"');
  assert.ok(start > 0, 'view-all-tasks section exists');
  const section = html.slice(start, html.indexOf('</section>', start));
  assert.doesNotMatch(section, /show-legacy|Show archive/);
  for (const id of ['all-tasks-search', 'all-tasks-org-filter', 'all-tasks-origin-filter', 'all-tasks-stage-filter', 'all-tasks-visibility-filter']) {
    assert.ok(section.includes(`id="${id}"`), `${id} filter present`);
  }
  // Visibility filter defaults to everything.
  assert.match(section, /id="all-tasks-visibility-filter"[^>]*>\s*<option value="all">/);
});

test('includeHiddenFor: All Tasks always, toggle pages only when on, others never', () => {
  assert.equal(tb.includeHiddenFor('all-tasks'), true);
  assert.equal(tb.includeHiddenFor('all-tasks', false), true);
  for (const page of ['recent-tasks', 'task-status', 'kanban']) {
    assert.equal(tb.includeHiddenFor(page, false), false, page);
    assert.equal(tb.includeHiddenFor(page, true), true, page);
  }
  for (const page of ['overview', 'projects', 'agents', 'cost', 'boss', 'org-detail', 'gates']) {
    assert.equal(tb.includeHiddenFor(page, true), false, page);
  }
});

test('visibleTasks hides legacy and archived imports by default without mutating', () => {
  const copy = tasks.slice();
  assert.deepEqual(ids(tb.visibleTasks(tasks, false)), ['i', 'n']);
  assert.deepEqual(ids(tb.visibleTasks(tasks, true)), ['a', 'c', 'i', 'l', 'n']);
  assert.deepEqual(tasks, copy);
  assert.deepEqual(tb.visibleTasks(undefined, false), []);
});

test('All Tasks default filter shows every task, newest first', () => {
  const all = tb.filterAllTasks(tasks, { q: '', org: 'all', origin: 'all', stage: 'all', visibility: 'all' });
  assert.deepEqual(all.map(t => t.id), ['n', 'i', 'a', 'c', 'l']);
  assert.deepEqual(tb.filterAllTasks(tasks).map(t => t.id), ['n', 'i', 'a', 'c', 'l']);
});

test('All Tasks filters: org, origin, stage, visibility and search', () => {
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { org: 'Managed Solution' })), ['i']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { origin: 'legacy' })), ['l']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { stage: 'todo' })), ['l', 'n']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { visibility: 'current' })), ['i', 'n']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { visibility: 'hidden' })), ['a', 'c', 'l']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { visibility: 'archive' })), ['a', 'c']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { visibility: 'legacy' })), ['l']);
  assert.deepEqual(ids(tb.filterAllTasks(tasks, { q: 'IMPORT' })), ['a', 'c', 'i']);
});

test('taskVisibility labels badges', () => {
  assert.equal(tb.taskVisibility(tasks[0]), 'current');
  assert.equal(tb.taskVisibility(tasks[2]), 'archive');
  assert.equal(tb.taskVisibility(tasks[4]), 'legacy');
});

test('list surfaces in app.js read tasks through the visibility helpers', () => {
  // Raw Object.values(state.tasks) is allowed only in the helpers, the hidden
  // count, the direct-link resolvers (findTask, isFleetTaskId: a pasted
  // STA-123 link to an archived task still opens it) and the Kanban board
  // (which filters through buildBoard's showLegacy).
  const allowed = ['function taskValues', 'function updateShowHiddenCounts', 'function findTask', 'function isFleetTaskId', 'function renderKanban'];
  const fnStarts = [...app.matchAll(/^(?:async )?function ([A-Za-z_]+)\(/gm)].map(m => ({ name: m[1], at: m.index }));
  const owner = at => (fnStarts.filter(f => f.at <= at).pop() || { name: '<top>' }).name;
  const offenders = [];
  for (const m of app.matchAll(/Object\.values\(state\.tasks/g)) {
    const fn = owner(m.index);
    if (!allowed.some(a => a === `function ${fn}`)) offenders.push(fn);
  }
  assert.deepEqual(offenders, []);
});
