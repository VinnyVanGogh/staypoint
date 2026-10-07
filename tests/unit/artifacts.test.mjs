// Unit tests for the task document (artifact) helpers in lib/artifacts.js:
// filtering across tasks by org/task/kind, filter options, download names and
// sizes, plus the Markdown rendering the Artifacts tab relies on.
// Run with: node --test tests/unit/artifacts.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const {
  artifactTaskLabel, filterArtifacts, artifactFilterOptions, artifactFileName, formatDocSize,
} = require('../../internal/server/webui/lib/artifacts.js');
const { renderMarkdown } = require('../../internal/server/webui/lib/markdown.js');

const DOCS = [
  { task_id: 'aaaaaaaa-1111', task_name: 'Plan auth', task_identifier: 'STA-9', organization: 'StayPoint', doc_key: 'plan', latest_version: 3 },
  { task_id: 'aaaaaaaa-1111', task_name: 'Plan auth', task_identifier: 'STA-9', organization: 'StayPoint', doc_key: 'bundle', latest_version: 1 },
  { task_id: 'bbbbbbbb-2222', task_name: 'Map infra', organization: 'staypoint', doc_key: 'architecture', latest_version: 2 },
  { task_id: 'cccccccc-3333', task_name: 'Orgless', organization: '', doc_key: 'plan', latest_version: 1 },
];

test('filterArtifacts with no or "all" filters keeps everything', () => {
  assert.equal(filterArtifacts(DOCS).length, 4);
  assert.equal(filterArtifacts(DOCS, { org: 'all', task: 'all', kind: 'all' }).length, 4);
  assert.deepEqual(filterArtifacts(null, {}), []);
});

test('filterArtifacts matches org case-insensitively and combines filters', () => {
  assert.equal(filterArtifacts(DOCS, { org: 'STAYPOINT' }).length, 3);
  assert.deepEqual(filterArtifacts(DOCS, { org: 'StayPoint', kind: 'plan' }).map(d => d.task_id), ['aaaaaaaa-1111']);
  assert.deepEqual(filterArtifacts(DOCS, { task: 'bbbbbbbb-2222' }).map(d => d.doc_key), ['architecture']);
  assert.deepEqual(filterArtifacts(DOCS, { org: 'nope' }), []);
});

test('artifactFilterOptions lists distinct orgs, tasks and kinds, sorted', () => {
  const o = artifactFilterOptions(DOCS);
  assert.deepEqual(o.orgs, ['StayPoint']);
  assert.deepEqual(o.kinds, ['architecture', 'bundle', 'plan']);
  assert.deepEqual(o.tasks.map(t => t.id), ['bbbbbbbb-2222', 'cccccccc-3333', 'aaaaaaaa-1111']);
  assert.equal(o.tasks[2].label, 'STA-9 Plan auth');
});

test('artifactTaskLabel falls back to a short task id', () => {
  assert.equal(artifactTaskLabel(DOCS[2]), '#bbbbbbbb Map infra');
  assert.equal(artifactTaskLabel({ task_id: 'dddddddd-4444' }), '#dddddddd');
  assert.equal(artifactTaskLabel(null), '');
});

test('artifactFileName is filesystem-safe and versioned', () => {
  assert.equal(artifactFileName(DOCS[0]), 'STA-9-plan-v3.md');
  assert.equal(artifactFileName(DOCS[0], 1), 'STA-9-plan-v1.md');
  assert.equal(artifactFileName({ task_id: 'bbbbbbbb-2222', doc_key: '../../etc/passwd', version: 2 }), 'bbbbbbbb-..-..-etc-passwd-v2.md');
  assert.ok(!artifactFileName({ task_id: 'x', doc_key: 'a/b\\c:d' }).match(/[\/\\:]/));
});

test('formatDocSize', () => {
  assert.equal(formatDocSize(0), '0 B');
  assert.equal(formatDocSize(1023), '1023 B');
  assert.equal(formatDocSize(2048), '2.0 KB');
  assert.equal(formatDocSize(3 * 1024 * 1024), '3.0 MB');
  assert.equal(formatDocSize(undefined), '0 B');
});

test('document Markdown renders as HTML with injected tags escaped', () => {
  const html = renderMarkdown('# Plan\n\n- step one\n- step two\n\n<script>alert(1)</script>');
  assert.match(html, /<h1>Plan<\/h1>/);
  assert.match(html, /<ul><li>step one<\/li>\n<li>step two<\/li><\/ul>/);
  assert.ok(!html.includes('<script>'));
  assert.match(html, /&lt;script&gt;/);
});
