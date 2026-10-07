/* Pure, DOM-free helpers for task documents (artifacts), shared by app.js and
 * Node unit tests.
 *
 * Task documents come from GET /api/documents and GET /api/tasks/{id}/documents
 * as summaries: one per (task, doc_key) with its latest version.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 */
(function (root) {
  'use strict';

  // artifactTaskLabel names a summary's task: identifier, else a short id.
  function artifactTaskLabel(doc) {
    if (!doc) return '';
    const ref = doc.task_identifier || (doc.task_id ? `#${String(doc.task_id).slice(0, 8)}` : '');
    return doc.task_name ? `${ref} ${doc.task_name}`.trim() : ref;
  }

  // filterArtifacts keeps the summaries matching every set filter. 'all' or ''
  // matches anything; org compares case-insensitively.
  function filterArtifacts(docs, filter) {
    const f = filter || {};
    const any = v => !v || v === 'all';
    const org = any(f.org) ? null : String(f.org).toLowerCase();
    return (docs || []).filter(d =>
      (org === null || String(d.organization || '').toLowerCase() === org) &&
      (any(f.task) || d.task_id === f.task) &&
      (any(f.kind) || d.doc_key === f.kind));
  }

  // artifactFilterOptions lists the distinct orgs, tasks and kinds in docs,
  // sorted, for the filter dropdowns. Docs without an org are left out of orgs.
  function artifactFilterOptions(docs) {
    const orgs = new Map();
    const tasks = new Map();
    const kinds = new Set();
    for (const d of docs || []) {
      const org = d.organization || '';
      if (org && !orgs.has(org.toLowerCase())) orgs.set(org.toLowerCase(), org);
      if (d.task_id && !tasks.has(d.task_id)) tasks.set(d.task_id, artifactTaskLabel(d));
      if (d.doc_key) kinds.add(d.doc_key);
    }
    const byText = (a, b) => a.localeCompare(b);
    return {
      orgs: [...orgs.values()].sort(byText),
      tasks: [...tasks.entries()].map(([id, label]) => ({ id, label }))
        .sort((a, b) => byText(a.label, b.label)),
      kinds: [...kinds].sort(byText),
    };
  }

  // artifactFileName is the download name for one version of a document:
  // <task>-<doc_key>-v<version>.md with anything unsafe in a filename dashed.
  function artifactFileName(doc, version) {
    const d = doc || {};
    const ref = d.task_identifier || String(d.task_id || 'task').slice(0, 8);
    const v = version || d.version || d.latest_version || 1;
    const safe = s => String(s).replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'doc';
    return `${safe(ref)}-${safe(d.doc_key || 'doc')}-v${v}.md`;
  }

  // formatDocSize renders a byte count as B / KB / MB.
  function formatDocSize(bytes) {
    const n = Number(bytes) || 0;
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  }

  const api = { artifactTaskLabel, filterArtifacts, artifactFilterOptions, artifactFileName, formatDocSize };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
