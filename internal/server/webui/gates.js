// Security Gates page (STA-587, STA-868).
//
// Loaded before app.js (whose boot() may render /gates at once) and uses its
// helpers only inside functions, which run after both scripts loaded (el, apiFetch, authHeader,
// withBoardWebAuthn, boardActionErrorText, fmtDateTime, showToast,
// navigateTo). Pure formatting lives in lib/gates.js.
//
// Requests tab: pending/decided requests with advisor recommendations,
// Approve / Deny / Approve & remember, checkboxes for batch decisions, and
// "Review with AI" (Gemini). Rules & stats tab: allow rules with hit counts,
// advisor agreement, and the advisor / Touch ID grace settings.

const gatesState = {
  tab: 'requests',
  selected: new Set(),
  review: null,       // last Gemini review result
  reviewError: '',
  reviewing: false,
  pending: [],        // last pending list (for Approve all)
  graceSeconds: 0,
  graceTimer: null,
  advisorEnabled: false,
};

async function updateGatesBadge() {
  try {
    const data = await apiFetch('/api/security/gate-requests?status=pending');
    const count = (data.gate_requests || []).length;
    const badge = document.getElementById('gates-badge');
    if (!badge) return;
    if (count > 0) {
      badge.textContent = count;
      badge.style.display = '';
    } else {
      badge.style.display = 'none';
    }
  } catch (_) {}
}

// ── Touch ID grace ──────────────────────────────────────────────────────────

function renderGateGrace() {
  const label = document.getElementById('gates-grace');
  if (!label) return;
  const text = graceLabel(gatesState.graceSeconds);
  label.textContent = text;
  label.hidden = !text;
}

// refreshGateGrace reads the server-side grace window for this Board session
// and counts it down locally. The server stays the authority: an expired or
// revoked window just means the next action asks for Touch ID again.
async function refreshGateGrace() {
  let secs = 0;
  try {
    const r = await fetch('/api/board/passkey-grace', { headers: authHeader() });
    if (r.ok) secs = (await r.json()).remaining_seconds || 0;
  } catch (_) { secs = 0; }
  gatesState.graceSeconds = secs;
  if (gatesState.graceTimer) clearInterval(gatesState.graceTimer);
  gatesState.graceTimer = null;
  if (secs > 0) {
    gatesState.graceTimer = setInterval(() => {
      gatesState.graceSeconds = Math.max(0, gatesState.graceSeconds - 1);
      renderGateGrace();
      if (gatesState.graceSeconds === 0) { clearInterval(gatesState.graceTimer); gatesState.graceTimer = null; }
    }, 1000);
  }
  renderGateGrace();
}

// gateBoardFetch sends a Board gate action. Inside the grace window it goes
// without a new Touch ID; if the server says the window is gone it falls back
// to the passkey prompt. Returns null when the Board cancelled.
async function gateBoardFetch(url, method, body, label) {
  const send = (sessionToken, assertion) => {
    const headers = { 'Content-Type': 'application/json', ...authHeader() };
    if (sessionToken) headers['X-WebAuthn-Session'] = sessionToken;
    if (assertion) headers['X-WebAuthn-Assertion'] = assertion;
    return fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  };
  if (gatesState.graceSeconds > 0) {
    const r = await send('', '');
    if (r.status !== 403) { refreshGateGrace(); return r; }
    const err = await r.clone().json().catch(() => ({}));
    if (err.error !== 'board_passkey_assertion_required') return r;
  }
  const r = await withBoardWebAuthn(send, label);
  refreshGateGrace();
  return r;
}

async function gateActionFailed(r) {
  const err = await r.json().catch(() => ({}));
  if (r.status === 403) {
    alert(err.message || 'Board session required. Open StayPoint in a browser with your board token to approve/deny gates.');
  } else {
    alert(`Failed: ${err.error || boardActionErrorText(r, err)}`);
  }
}

// ── Rendering ───────────────────────────────────────────────────────────────

// renderGatesPage can be called again before an earlier call's fetch returns
// (the security_gate_decided SSE event and decideGate both re-render), and the
// responses can arrive out of order. Only the most recent call may touch the
// DOM, otherwise a late 'pending' list wipes a row decideGate just rendered
// under 'all' (STA-651).
let gatesRenderSeq = 0;

function gateLoadError(text) {
  const p = el('p', 'muted-text', text);
  p.style.padding = '20px';
  return p;
}

function syncGateTabs() {
  document.querySelectorAll('.gates-tab').forEach(b => b.classList.toggle('active', b.dataset.tab === gatesState.tab));
  const filter = document.getElementById('gates-status-filter');
  if (filter) filter.hidden = gatesState.tab !== 'requests';
}

async function renderGatesPage() {
  syncGateTabs();
  refreshGateGrace();
  if (gatesState.tab === 'rules') return renderGateRulesTab();
  return renderGateRequestsTab();
}

function gateAdviceLines(gr, advisorEnabled) {
  const box = el('div', 'gate-advice');
  const pending = gr.status === 'pending';
  const advice = gr.advice || {};
  const views = [
    gateAdviceView('together', advice.together, pending && advisorEnabled),
    gateAdviceView('gemini', advice.gemini, false),
  ].filter(Boolean);
  for (const v of views) box.appendChild(el('div', `gate-advice-line ${v.cls}`, v.text));
  return views.length ? box : null;
}

function gateRememberForm(gr, onDone) {
  const form = el('div', 'gate-remember-form');
  form.hidden = true;
  const scope = el('select', 'select-filter gate-remember-scope');
  for (const [val, label, field] of [['task', 'This task', 'task_id'], ['repo', 'This repo', 'repo'], ['org', 'This organization', 'org']]) {
    const o = el('option', '', gr[field] ? `${label}` : `${label} (n/a)`);
    o.value = val;
    o.disabled = !gr[field];
    o.title = gr[field] || '';
    scope.appendChild(o);
  }
  const firstOk = [...scope.options].find(o => !o.disabled);
  if (firstOk) scope.value = firstOk.value;
  const expiry = el('select', 'select-filter gate-remember-expiry');
  for (const [val, label] of [['0', 'No expiry'], ['60', '1 hour'], ['1440', '1 day'], ['10080', '7 days']]) {
    const o = el('option', '', label);
    o.value = val;
    expiry.appendChild(o);
  }
  const go = el('button', 'gate-btn gate-remember-confirm', 'Approve & remember');
  go.addEventListener('click', async () => {
    const { spec, error } = rememberSpec(gr, scope.value, expiry.value);
    if (error) { alert(error); return; }
    go.disabled = true;
    await decideGate(gr.id, 'approved', spec);
    go.disabled = false;
    if (onDone) onDone();
  });
  const note = el('div', 'gate-remember-note',
    (gr.scripts && gr.scripts.length)
      ? `Pins ${gr.scripts.length} script${gr.scripts.length > 1 ? 's' : ''} by sha256: an edited script is held again.`
      : 'Matches this exact command (whitespace aside).');
  form.append(scope, expiry, go, note);
  return form;
}

async function renderGateRequestsTab() {
  const container = document.getElementById('gates-container');
  if (!container) return;
  const seq = ++gatesRenderSeq;
  container.innerHTML = '<p class="muted-text" style="padding:20px;">Loading…</p>';

  const statusFilter = document.getElementById('gates-status-filter')?.value || 'pending';
  const url = statusFilter === 'all'
    ? '/api/security/gate-requests?status=all'
    : `/api/security/gate-requests?status=${statusFilter}`;

  let requests = [];
  try {
    const data = await apiFetch(url);
    requests = data.gate_requests || [];
    gatesState.advisorEnabled = !!data.advisor_enabled;
  } catch (e) {
    if (seq !== gatesRenderSeq) return;
    container.replaceChildren(gateLoadError(`Failed to load gate requests: ${e.message}`));
    return;
  }
  if (seq !== gatesRenderSeq) return;

  container.innerHTML = '';
  const pending = requests.filter(r => r.status === 'pending');
  gatesState.pending = pending.map(r => r.id);
  for (const id of [...gatesState.selected]) if (!gatesState.pending.includes(id)) gatesState.selected.delete(id);

  const trustPanel = await renderTrustedTasksPanel();
  if (seq !== gatesRenderSeq) return;
  if (trustPanel) container.appendChild(trustPanel);
  container.appendChild(renderGateToolbar(pending.length));
  const reviewPanel = renderGateReviewPanel();
  if (reviewPanel) container.appendChild(reviewPanel);

  if (requests.length === 0) {
    const empty = el('div', 'gates-empty');
    empty.appendChild(el('div', 'gates-empty-icon', '🔒'));
    empty.appendChild(el('div', '', statusFilter === 'pending'
      ? 'No pending gate requests. All clear.'
      : statusFilter === 'deferred' ? 'Deferred queue is empty.' : 'No gate requests found.'));
    container.appendChild(empty);
    return;
  }

  const reviewById = new Map(((gatesState.review && gatesState.review.items) || []).map(i => [i.id, i]));
  const wrap = el('div', 'gates-table-wrapper');
  const table = el('table', 'gates-table');
  const thead = document.createElement('thead');
  const hrow = document.createElement('tr');
  const selTh = document.createElement('th');
  if (pending.length) {
    const all = el('input', 'gate-select-all');
    all.type = 'checkbox';
    all.title = 'Select all pending';
    all.checked = pending.length > 0 && pending.every(r => gatesState.selected.has(r.id));
    all.addEventListener('change', () => {
      for (const r of pending) all.checked ? gatesState.selected.add(r.id) : gatesState.selected.delete(r.id);
      renderGateRequestsTab();
    });
    selTh.appendChild(all);
  }
  hrow.appendChild(selTh);
  for (const h of ['Command', 'Reasons', 'Run / Task', 'Requested', 'Status', 'Action']) {
    const th = document.createElement('th');
    th.textContent = h;
    hrow.appendChild(th);
  }
  thead.appendChild(hrow);
  table.appendChild(thead);

  const tbody = document.createElement('tbody');
  for (const gr of requests) {
    const tr = document.createElement('tr');
    tr.dataset.gateId = gr.id;
    const rev = reviewById.get(gr.id);
    if (rev) tr.classList.add(`gate-row-ai-${rev.recommendation}`);

    const selTd = document.createElement('td');
    if (gr.status === 'pending') {
      const cb = el('input', 'gate-select');
      cb.type = 'checkbox';
      cb.checked = gatesState.selected.has(gr.id);
      cb.addEventListener('change', () => {
        cb.checked ? gatesState.selected.add(gr.id) : gatesState.selected.delete(gr.id);
        updateGateToolbarCounts();
      });
      selTd.appendChild(cb);
    }
    tr.appendChild(selTd);

    // Command + advisor recommendations
    const cmdTd = document.createElement('td');
    cmdTd.appendChild(el('div', 'gate-cmdline', gr.cmdline || '—'));
    const adv = gateAdviceLines(gr, gatesState.advisorEnabled);
    if (adv) cmdTd.appendChild(adv);
    tr.appendChild(cmdTd);

    const reasonsTd = document.createElement('td');
    const reasons = Array.isArray(gr.reasons) ? gr.reasons : [];
    reasonsTd.appendChild(el('div', 'gate-reasons', reasons.join(', ') || '—'));
    tr.appendChild(reasonsTd);

    const runTd = document.createElement('td');
    if (gr.run_id) {
      const a = el('a', 'gate-run-link', gr.run_id.slice(0, 12));
      a.href = '#';
      a.title = gr.run_id;
      runTd.appendChild(a);
    } else {
      runTd.appendChild(el('span', 'muted-text', '—'));
    }
    const ctx = [gr.task_id && `task ${gr.task_id}`, gr.repo, gr.org].filter(Boolean).join(' · ');
    if (ctx) runTd.appendChild(el('div', 'gate-context', ctx));
    tr.appendChild(runTd);

    const timeTd = document.createElement('td');
    timeTd.textContent = gr.created_at ? fmtDateTime(gr.created_at) : '—';
    tr.appendChild(timeTd);

    const statusTd = document.createElement('td');
    const label = gateStatusLabel(gr);
    const statusCls = `gate-status-${label === 'deferred' ? 'deferred' : (gr.status || 'pending')}`;
    const decidedInfo = gr.decided_at ? ` · ${fmtDateTime(gr.decided_at)}` : '';
    statusTd.appendChild(el('span', statusCls, label + decidedInfo));
    if (gr.decided_by && gr.decided_by.startsWith('rule:')) {
      const m = /^rule:(\d+)(:tev1)?$/.exec(gr.decided_by);
      const what = m && m[2] ? `decided by tev1 under trust #${m[1]}` : `auto-approved by rule #${gr.decided_by.slice(5)}`;
      statusTd.appendChild(el('div', 'gate-context', what));
    }
    if (label === 'deferred') {
      statusTd.appendChild(el('div', 'gate-context',
        'Skipped at its deadline, not run. Deciding it records your call; it is not replayed.'));
    }
    tr.appendChild(statusTd);

    const actionTd = document.createElement('td');
    if (gr.status === 'pending') {
      const btns = el('div', 'gate-action-btns');
      const approveBtn = el('button', 'gate-btn gate-approve-btn', 'Approve');
      const rememberBtn = el('button', 'gate-btn gate-remember-btn', 'Approve & remember…');
      const denyBtn = el('button', 'gate-btn gate-deny-btn', 'Deny');
      approveBtn.addEventListener('click', () => decideGate(gr.id, 'approved'));
      denyBtn.addEventListener('click', () => decideGate(gr.id, 'denied'));
      const form = gateRememberForm(gr);
      rememberBtn.addEventListener('click', () => { form.hidden = !form.hidden; });
      btns.append(approveBtn, rememberBtn, denyBtn);
      actionTd.append(btns, form);
    } else {
      actionTd.appendChild(el('span', 'muted-text', '—'));
    }
    tr.appendChild(actionTd);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  container.appendChild(wrap);
  updateGateToolbarCounts();
}

function renderGateToolbar(pendingCount) {
  const bar = el('div', 'gates-toolbar');
  bar.id = 'gates-toolbar';
  if (!pendingCount) { bar.hidden = true; return bar; }
  const approveSel = el('button', 'gate-btn gate-batch-approve', 'Approve selected');
  const denySel = el('button', 'gate-btn gate-batch-deny', 'Deny selected');
  const approveAll = el('button', 'gate-btn gate-batch-approve-all', `Approve all (${pendingCount})`);
  const review = el('button', 'gate-btn gate-review-btn', gatesState.reviewing ? 'Reviewing…' : 'Review with AI');
  review.disabled = gatesState.reviewing;
  approveSel.addEventListener('click', () => decideGateBatch([...gatesState.selected], 'approved'));
  denySel.addEventListener('click', () => decideGateBatch([...gatesState.selected], 'denied'));
  approveAll.addEventListener('click', () => {
    if (confirm(`Approve all ${gatesState.pending.length} pending requests?`)) decideGateBatch([...gatesState.pending], 'approved');
  });
  review.addEventListener('click', reviewGatesWithAI);
  const count = el('span', 'gates-selected-count muted-text', '');
  bar.append(approveSel, denySel, approveAll, review, count);
  return bar;
}

function updateGateToolbarCounts() {
  const n = gatesState.selected.size;
  const count = document.querySelector('#gates-toolbar .gates-selected-count');
  if (count) count.textContent = n ? `${n} selected` : '';
  document.querySelectorAll('#gates-toolbar .gate-batch-approve, #gates-toolbar .gate-batch-deny').forEach(b => { b.disabled = n === 0; });
}

function renderGateReviewPanel() {
  if (!gatesState.review && !gatesState.reviewError) return null;
  const panel = el('div', 'gates-review-panel');
  if (gatesState.reviewError) {
    panel.classList.add('gates-review-error');
    panel.appendChild(el('div', '', `AI review failed: ${gatesState.reviewError}`));
  } else {
    const r = gatesState.review;
    panel.appendChild(el('div', 'gates-review-title', `Gemini review${r.model ? ` (${r.model})` : ''}: ${r.suggestion || ''}`));
    if (r.summary) panel.appendChild(el('div', 'gates-review-summary', r.summary));
    const sel = reviewSelection(r, gatesState.pending);
    const pre = el('button', 'gate-btn gate-review-apply',
      `Pre-check suggestions (${sel.approved.length} approve, ${sel.denied.length} deny)`);
    pre.title = 'Checks the requests the review suggests approving. You still confirm with Touch ID.';
    pre.addEventListener('click', () => {
      gatesState.selected = new Set(sel.approved);
      renderGateRequestsTab();
    });
    panel.appendChild(pre);
    panel.appendChild(el('div', 'muted-text', 'Advisory only: nothing is approved until you confirm.'));
  }
  const close = el('button', 'gate-btn gate-review-close', 'Dismiss');
  close.addEventListener('click', () => { gatesState.review = null; gatesState.reviewError = ''; renderGateRequestsTab(); });
  panel.appendChild(close);
  return panel;
}

async function reviewGatesWithAI() {
  gatesState.reviewing = true;
  gatesState.reviewError = '';
  renderGateRequestsTab();
  try {
    const r = await fetch('/api/security/gate-requests/review', {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...authHeader() }, body: '{}',
    });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) {
      gatesState.review = null;
      gatesState.reviewError = body.error || body.message || `${r.status} ${r.statusText}`;
    } else {
      gatesState.review = body;
    }
  } catch (e) {
    gatesState.reviewError = e.message || String(e);
  } finally {
    gatesState.reviewing = false;
    renderGateRequestsTab();
  }
}

// ── Actions ─────────────────────────────────────────────────────────────────

async function decideGate(id, decision, remember) {
  try {
    const body = remember ? { decision, remember } : { decision };
    const r = await gateBoardFetch(`/api/security/gate-requests/${id}/decide`, 'POST', body,
      `${decision === 'approved' ? 'approving' : 'denying'} the gate request`);
    if (r === null) return; // enrollment required — user was prompted
    if (!r.ok) { await gateActionFailed(r); return; }
    if (remember) {
      const out = await r.json().catch(() => ({}));
      if (out.rule) showToast(`Approved and remembered as rule #${out.rule.id} (${ruleScopeLabel(out.rule)})`, 'success');
    }
    gatesState.selected.delete(id);
    // Switch to 'all' so the decided row stays visible with its new status.
    const filterEl = document.getElementById('gates-status-filter');
    if (filterEl) filterEl.value = 'all';
    await renderGatesPage();
    await updateGatesBadge();
  } catch (e) {
    alert(`Request failed: ${e.message}`);
  }
}

async function decideGateBatch(ids, decision) {
  if (!ids.length) return;
  try {
    const r = await gateBoardFetch('/api/security/gate-requests/decide-batch', 'POST', { ids, decision },
      `${decision === 'approved' ? 'approving' : 'denying'} ${ids.length} gate requests`);
    if (r === null) return;
    if (!r.ok) { await gateActionFailed(r); return; }
    const out = await r.json();
    const summary = batchSummary(out, decision);
    showToast(summary.text, summary.ok ? 'success' : 'error', summary.ok ? 6000 : 15000);
    for (const res of out.results || []) if (res.ok) gatesState.selected.delete(res.id);
    await renderGatesPage();
    await updateGatesBadge();
  } catch (e) {
    alert(`Request failed: ${e.message}`);
  }
}

// ── Rules & stats tab ───────────────────────────────────────────────────────

async function renderGateRulesTab() {
  const container = document.getElementById('gates-container');
  if (!container) return;
  const seq = ++gatesRenderSeq;
  container.innerHTML = '<p class="muted-text" style="padding:20px;">Loading…</p>';
  let rules, stats, settings;
  try {
    [rules, stats, settings] = await Promise.all([
      apiFetch('/api/security/gate-rules'),
      apiFetch('/api/security/gate-stats'),
      apiFetch('/api/settings/security-gate'),
    ]);
  } catch (e) {
    if (seq !== gatesRenderSeq) return;
    container.replaceChildren(gateLoadError(`Failed to load rules: ${e.message}`));
    return;
  }
  if (seq !== gatesRenderSeq) return;
  container.innerHTML = '';

  const statsBox = el('div', 'gates-stats');
  statsBox.appendChild(el('div', 'gates-stats-title', 'Advisor agreement'));
  const advs = stats.advisors || [];
  if (!advs.length) statsBox.appendChild(el('div', 'muted-text', 'No advisor recommendations yet.'));
  for (const s of advs) statsBox.appendChild(el('div', 'gates-stat-line', agreementLabel(s)));
  statsBox.appendChild(el('div', 'gates-stat-line', `${stats.active_rules || 0} active rules · ${stats.rule_hits || 0} auto-approvals`));
  container.appendChild(statsBox);
  container.appendChild(renderGateSettings(settings));

  const list = rules.rules || [];
  if (!list.length) {
    container.appendChild(el('div', 'gates-empty', 'No allow rules. Use "Approve & remember" on a request to add one.'));
    return;
  }
  const wrap = el('div', 'gates-table-wrapper');
  const table = el('table', 'gates-table gates-rules-table');
  const thead = document.createElement('thead');
  const hrow = document.createElement('tr');
  for (const h of ['#', 'Command', 'Scope', 'Pinned scripts', 'Expiry', 'Hits', 'Created', '']) {
    const th = document.createElement('th');
    th.textContent = h;
    hrow.appendChild(th);
  }
  thead.appendChild(hrow);
  table.appendChild(thead);
  const tbody = document.createElement('tbody');
  const now = Date.now();
  for (const rule of list) {
    const tr = document.createElement('tr');
    tr.dataset.ruleId = rule.id;
    tr.appendChild(el('td', '', String(rule.id)));
    const cmd = document.createElement('td');
    cmd.appendChild(el('div', 'gate-cmdline', rule.pattern + (rule.match_kind === 'prefix' ? ' *' : '')));
    if ((rule.reasons || []).length) cmd.appendChild(el('div', 'gate-reasons', rule.reasons.join(', ')));
    tr.appendChild(cmd);
    tr.appendChild(el('td', '', ruleScopeLabel(rule)));
    const scripts = document.createElement('td');
    for (const s of rule.scripts || []) {
      const d = el('div', 'gate-context', `${s.path} · ${String(s.sha256).slice(0, 12)}`);
      d.title = s.sha256;
      scripts.appendChild(d);
    }
    if (!(rule.scripts || []).length) scripts.textContent = '—';
    tr.appendChild(scripts);
    tr.appendChild(el('td', '', ruleExpiryLabel(rule, now)));
    tr.appendChild(el('td', '', `${rule.hit_count}${rule.last_hit_at ? ` · last ${fmtDateTime(rule.last_hit_at)}` : ''}`));
    tr.appendChild(el('td', '', fmtDateTime(rule.created_at)));
    const act = document.createElement('td');
    const del = el('button', 'gate-btn gate-deny-btn gate-rule-delete', 'Delete');
    del.addEventListener('click', () => deleteGateRule(rule.id));
    act.appendChild(del);
    tr.appendChild(act);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  container.appendChild(wrap);
}

function renderGateSettings(settings) {
  const box = el('div', 'gates-settings');
  box.appendChild(el('div', 'gates-stats-title', 'Settings'));
  const graceRow = el('label', 'gates-setting-row', 'Touch ID covers further gate actions for ');
  const grace = el('select', 'select-filter gates-grace-minutes');
  for (const m of [0, 1, 2, 3, 4, 5]) {
    const o = el('option', '', m === 0 ? 'off (ask every time)' : `${m} min`);
    o.value = String(m);
    grace.appendChild(o);
  }
  grace.value = String(settings.passkey_grace_minutes ?? 2);
  grace.addEventListener('change', () => saveGateSetting({ passkey_grace_minutes: Number(grace.value) }));
  graceRow.appendChild(grace);
  box.appendChild(graceRow);

  const advRow = el('label', 'gates-setting-row');
  const adv = el('input', 'gates-advisor-toggle');
  adv.type = 'checkbox';
  adv.checked = settings.advisor_enabled !== false;
  adv.addEventListener('change', () => saveGateSetting({ advisor_enabled: adv.checked }));
  advRow.append(adv, document.createTextNode(' Together advisory on every gate request'
    + (settings.advisor_configured ? '' : ' (not configured: no TOGETHER_API_KEY)')));
  box.appendChild(advRow);

  // Task trust (task-6c1ed91f).
  const deferRow = el('label', 'gates-setting-row', 'Under trust, a delete outside the worktree waits ');
  const deferSel = el('select', 'select-filter gates-trust-defer');
  for (const m of [1, 5, 10, 15, 30, 60, 120]) {
    const o = el('option', '', `${m} min`);
    o.value = String(m);
    deferSel.appendChild(o);
  }
  deferSel.value = String(settings.trust_defer_minutes ?? 10);
  deferSel.addEventListener('change', () => saveGateSetting({ trust_defer_minutes: Number(deferSel.value) }));
  deferRow.append(deferSel, document.createTextNode(' for you, then is skipped (Deferred queue)'));
  box.appendChild(deferRow);

  const thRow = el('label', 'gates-setting-row', 'tev1 overnight mode approves only at p ≥ ');
  const th = el('select', 'select-filter gates-tev1-threshold');
  for (const v of [0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99]) {
    const o = el('option', '', v.toFixed(2));
    o.value = String(v);
    th.appendChild(o);
  }
  th.value = String(settings.tev1_threshold ?? 0.7);
  th.addEventListener('change', () => saveGateSetting({ tev1_threshold: Number(th.value) }));
  thRow.append(th, document.createTextNode(' (applies to trusts created after the change)'));
  box.appendChild(thRow);
  return box;
}

async function saveGateSetting(patch) {
  try {
    // Settings change the gate itself: always a fresh Touch ID, never grace.
    const r = await withBoardWebAuthn((sessionToken, assertion) => fetch('/api/settings/security-gate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeader(), 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
      body: JSON.stringify(patch),
    }), 'changing the gate settings');
    if (r === null) return;
    if (!r.ok) { await gateActionFailed(r); }
  } catch (e) {
    showToast('Gate setting not saved: ' + (e.message || e), 'error');
  }
  refreshGateGrace();
  renderGateRulesTab();
}

async function deleteGateRule(id) {
  if (!confirm(`Delete allow rule #${id}? Matching commands will be held for you again.`)) return;
  try {
    const r = await gateBoardFetch(`/api/security/gate-rules/${id}`, 'DELETE', undefined, `deleting rule #${id}`);
    if (r === null) return;
    if (!r.ok) { await gateActionFailed(r); return; }
    renderGateRulesTab();
  } catch (e) {
    alert(`Request failed: ${e.message}`);
  }
}

// ── Wiring ──────────────────────────────────────────────────────────────────

document.getElementById('gates-refresh-btn')?.addEventListener('click', () => renderGatesPage());
document.getElementById('gates-status-filter')?.addEventListener('change', () => renderGatesPage());
document.querySelectorAll('.gates-tab').forEach(b => b.addEventListener('click', () => {
  gatesState.tab = b.dataset.tab;
  renderGatesPage();
}));

// Load badge on startup, once app.js (apiFetch) has loaded.
document.addEventListener('DOMContentLoaded', () => updateGatesBadge());

// ── Pending gates on task page ──────────────────────────────────────────────

async function renderPendingGatesForTask(container, runId) {
  if (!runId) return;
  let pending = [];
  try {
    const data = await apiFetch('/api/security/gate-requests?status=pending');
    pending = (data.gate_requests || []).filter(g => g.run_id === runId);
  } catch (_) { return; }
  if (pending.length === 0) return;

  const sec = el('div', 'task-page-section');
  const box = el('div', 'task-pending-gates');
  box.appendChild(el('div', 'task-pending-gates-title', `⚠ ${pending.length} pending gate request${pending.length !== 1 ? 's' : ''}`));
  for (const g of pending) {
    const row = el('div', 'task-pending-gate-row');
    row.appendChild(el('span', 'task-pending-gate-cmd', g.cmdline || '—'));
    const btns = el('div', 'gate-action-btns');
    const approveBtn = el('button', 'gate-btn gate-approve-btn', 'Approve');
    const denyBtn = el('button', 'gate-btn gate-deny-btn', 'Deny');
    approveBtn.addEventListener('click', async () => { await decideGate(g.id, 'approved'); box.remove(); });
    denyBtn.addEventListener('click', async () => { await decideGate(g.id, 'denied'); box.remove(); });
    btns.appendChild(approveBtn);
    btns.appendChild(denyBtn);
    row.appendChild(btns);
    box.appendChild(row);
  }
  const viewAllLink = document.createElement('a');
  viewAllLink.href = '/gates';
  viewAllLink.className = 'gate-run-link';
  viewAllLink.textContent = 'View all gates →';
  viewAllLink.addEventListener('click', e => { e.preventDefault(); navigateTo('gates', null, true); });
  box.appendChild(viewAllLink);
  sec.appendChild(box);
  container.insertBefore(sec, container.firstChild);
}

// ── Task trust: "Trust this task until…" (task-6c1ed91f) ────────────────────

// trustSendRevoke ends a task's trust at once. Revoking only removes power,
// so it needs the Board session but no Touch ID.
async function trustRevoke(taskId) {
  try {
    const r = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/trust/revoke`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...authHeader() },
    });
    if (!r.ok) { await gateActionFailed(r); return false; }
    showToast('Trust revoked: Red requests wait for you again.', 'success');
    return true;
  } catch (e) {
    alert(`Revoke failed: ${e.message}`);
    return false;
  }
}

// trustCreate asks for a fresh Touch ID (never the grace window) and creates
// the trust. The body names a preset only; the server sets the expiry.
async function trustCreate(taskId, spec) {
  const r = await withBoardWebAuthn((sessionToken, assertion) => fetch(`/api/tasks/${encodeURIComponent(taskId)}/trust`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeader(), 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
    body: JSON.stringify(spec),
  }), 'trusting this task');
  if (r === null) return false;
  if (!r.ok) { await gateActionFailed(r); return false; }
  return true;
}

function trustTev1Banner(who) {
  return el('div', 'trust-tev1-banner',
    `⚠ tev1 decides ${who}'s Red requests. It is an unproven 4B model: expect false denials that park the task, and possible false approvals.`);
}

function trustRequestList(title, list, now) {
  const box = el('div', 'trust-request-list');
  box.appendChild(el('div', 'trust-list-title', `${title} (${list.length})`));
  for (const gr of list) {
    const row = el('div', 'trust-request-row');
    row.appendChild(el('span', 'trust-request-time', gr.decided_at ? clockLabel(gr.decided_at, now) : ''));
    row.appendChild(el('span', 'gate-cmdline', gr.cmdline || '—'));
    box.appendChild(row);
  }
  return box;
}

function trustSummaryBox(trust, now) {
  const lines = tev1SummaryLines(trust);
  if (!lines.length) return null;
  const box = el('div', 'trust-tev1-summary');
  box.appendChild(el('div', 'trust-list-title', `tev1 summary: ${(trust.tev1_approved || []).length} approved, ${(trust.tev1_denied || []).length} denied`));
  for (const l of lines) box.appendChild(el('div', `gate-advice-line ${l.cls}`, l.text));
  return box;
}

function trustCreateForm(taskId, info, onDone) {
  const form = el('div', 'trust-create');
  form.appendChild(el('div', 'task-page-section-title', 'Trust this task until…'));
  const row = el('div', 'trust-create-row');
  const preset = el('select', 'select-filter trust-preset');
  for (const [v, label] of TRUST_PRESETS) {
    const o = el('option', '', label);
    o.value = v;
    preset.appendChild(o);
  }
  preset.value = 'overnight';
  const mins = el('input', 'trust-custom-minutes');
  mins.type = 'number';
  mins.min = String(info.min_minutes || 15);
  mins.max = String(info.max_minutes || 1440);
  mins.placeholder = 'minutes';
  mins.hidden = true;
  preset.addEventListener('change', () => { mins.hidden = preset.value !== 'custom'; });
  const tev1Label = el('label', 'trust-tev1-toggle');
  const tev1 = el('input', '');
  tev1.type = 'checkbox';
  tev1Label.append(tev1, document.createTextNode(' tev1 decides overnight (off by default)'));
  if (!info.tev1_configured) { tev1.disabled = true; tev1Label.title = 'No local tev1 advisor configured'; }
  const go = el('button', 'gate-btn gate-approve-btn trust-create-btn', 'Trust (Touch ID)');
  go.addEventListener('click', async () => {
    let ack = false;
    if (tev1.checked) {
      ack = confirm(`Enable tev1 overnight mode?\n\n${info.tev1_warning || ''}\n\nOn a deny, low confidence or error the request is not run and the whole task is parked until you review it.`);
      if (!ack) return;
    }
    const { spec, error } = trustSpec(preset.value, mins.value, tev1.checked, ack);
    if (error) { alert(error); return; }
    go.disabled = true;
    const ok = await trustCreate(taskId, spec);
    go.disabled = false;
    if (ok && onDone) onDone();
  });
  row.append(preset, mins, tev1Label, go);
  form.appendChild(row);
  form.appendChild(el('div', 'muted-text trust-create-note',
    `While trusted, this task's Red requests run without asking you, and each is logged here. Always excluded: `
    + `merges and pushes to protected branches wait for you; deletes outside the task worktree wait ${info.defer_minutes || 10} min, `
    + `then are skipped and queued for you. Ends early when the task leaves in progress.`));
  return form;
}

// renderTaskTrust draws the trust section at the top of a task page: the
// banner and auto-approved list when trusted, the create form otherwise, and
// the latest trust's tev1 summary.
async function renderTaskTrust(container, taskId) {
  if (!taskId) return;
  let info;
  try {
    info = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/trust`);
  } catch (_) { return; }
  const sec = el('div', 'task-page-section task-trust-section');
  sec.dataset.taskId = taskId;
  const now = Date.now();
  const rerender = () => { sec.remove(); renderTaskTrust(container, taskId); };
  const active = info.active;
  if (active) {
    if (active.tev1) sec.appendChild(trustTev1Banner('this task'));
    const banner = el('div', 'trust-banner');
    banner.appendChild(el('span', 'trust-banner-text', trustBannerText(active, now)));
    const revoke = el('button', 'gate-btn gate-deny-btn trust-revoke-btn', 'Revoke');
    revoke.addEventListener('click', async () => { if (await trustRevoke(taskId)) rerender(); });
    banner.appendChild(revoke);
    sec.appendChild(banner);
    const approved = [...(active.auto_approved || []), ...(active.tev1_approved || [])]
      .sort((a, b) => String(b.decided_at || '').localeCompare(String(a.decided_at || '')));
    if (approved.length) sec.appendChild(trustRequestList('Auto-approved, newest first', approved, now));
  } else {
    sec.appendChild(trustCreateForm(taskId, info, rerender));
    const last = info.latest;
    if (last) {
      sec.appendChild(el('div', 'muted-text trust-ended',
        `Last trust ended ${last.ended_reason ? `(${last.ended_reason})` : `at ${clockLabel(last.expires_at, now)}`}: `
        + `${last.auto_approved_count || 0} auto-approved.`));
    }
  }
  const summary = info.latest && trustSummaryBox(info.latest, now);
  if (summary) sec.appendChild(summary);
  const held = info.held || [];
  if (held.length && (active || (info.latest && info.latest.active))) {
    sec.appendChild(trustRequestList('Waiting for you (merges, pushes, deferred deletes)', held, now));
  }
  container.insertBefore(sec, container.firstChild);
}

// renderTrustedTasksPanel is the Gates page's view of trusted tasks: an
// amber banner while any tev1 mode is on, and per trust its auto-approvals
// and the tev1 morning summary. Returns null when nothing is or was trusted.
async function renderTrustedTasksPanel() {
  let data;
  try { data = await apiFetch('/api/security/trusts'); } catch (_) { return null; }
  const trusts = data.trusts || [];
  if (!trusts.length) return null;
  const now = Date.now();
  const panel = el('div', 'gates-trust-panel');
  if (trusts.some(t => t.active && t.tev1)) panel.appendChild(trustTev1Banner('a trusted task'));
  panel.appendChild(el('div', 'gates-stats-title', 'Trusted tasks (live and last 24 h)'));
  for (const t of trusts) {
    const row = el('div', `gates-trust-row${t.active ? '' : ' gates-trust-ended'}`);
    const name = el('a', 'gate-run-link', t.task_name || t.scope_value);
    name.href = '#';
    name.addEventListener('click', e => { e.preventDefault(); openTaskPage(t.scope_value); });
    row.appendChild(name);
    row.appendChild(el('span', 'trust-banner-text', t.active
      ? trustBannerText(t, now)
      : `ended${t.ended_reason ? ` (${t.ended_reason})` : ''} · ${t.auto_approved_count || 0} auto-approved`));
    if (t.active) {
      const revoke = el('button', 'gate-btn gate-deny-btn trust-revoke-btn', 'Revoke');
      revoke.addEventListener('click', async () => { if (await trustRevoke(t.scope_value)) renderGatesPage(); });
      row.appendChild(revoke);
    }
    panel.appendChild(row);
    const summary = trustSummaryBox(t, now);
    if (summary) panel.appendChild(summary);
  }
  return panel;
}

// ── Org trust: "Trust this organization until…" (task-33692ffb) ─────────────

// orgTrustCreate asks for a fresh Touch ID and trusts every running task of
// org. The body names a preset only; the server sets the expiry (max 16 h).
async function orgTrustCreate(org, spec) {
  const r = await withBoardWebAuthn((sessionToken, assertion) => fetch('/api/settings/org-trust', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeader(), 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
    body: JSON.stringify({ org, ...spec }),
  }), `trusting ${org}`);
  if (r === null) return false;
  if (!r.ok) { await gateActionFailed(r); return false; }
  return true;
}

// orgTrustRevoke ends an org trust at once (Board session, no Touch ID).
async function orgTrustRevoke(org) {
  try {
    const r = await fetch('/api/settings/org-trust/revoke', {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ org }),
    });
    if (!r.ok) { await gateActionFailed(r); return false; }
    showToast(`Trust revoked: ${org}'s Red requests wait for you again.`, 'success');
    return true;
  } catch (e) {
    alert(`Revoke failed: ${e.message}`);
    return false;
  }
}

// renderOrgTrustSection fills sec (a Settings section) with the active org
// trusts and a "Trust this organization until…" form for orgNames.
async function renderOrgTrustSection(sec, orgNames) {
  let info;
  try { info = await apiFetch('/api/settings/org-trust'); } catch (_) { return; }
  const body = sec.querySelector('.org-trust-body') || sec.appendChild(el('div', 'org-trust-body'));
  body.innerHTML = '';
  const rerender = () => renderOrgTrustSection(sec, orgNames);
  const now = Date.now();
  const trusts = info.trusts || [];
  const trusted = new Set(trusts.map(t => t.scope_value));
  if (trusts.some(t => t.tev1)) body.appendChild(trustTev1Banner('a trusted organization'));
  for (const t of trusts) {
    const banner = el('div', 'trust-banner org-trust-banner');
    banner.dataset.org = t.scope_value;
    banner.appendChild(el('span', 'trust-banner-text', `${t.scope_value}: ${trustBannerText(t, now)}`));
    const revoke = el('button', 'gate-btn gate-deny-btn trust-revoke-btn', 'Revoke');
    revoke.addEventListener('click', async () => { if (await orgTrustRevoke(t.scope_value)) rerender(); });
    banner.appendChild(revoke);
    body.appendChild(banner);
  }

  const form = el('div', 'trust-create org-trust-create');
  form.appendChild(el('div', 'task-page-section-title', 'Trust this organization until…'));
  const row = el('div', 'trust-create-row');
  // A free-text field with the known organizations as suggestions: the
  // server checks the name against the tasks table.
  const org = el('input', 'select-filter org-trust-org');
  org.placeholder = 'organization';
  const list = el('datalist', '');
  list.id = 'org-trust-orgs';
  org.setAttribute('list', list.id);
  const seen = new Set();
  for (const name of orgNames) {
    const key = String(name).trim().toLowerCase();
    if (!key || seen.has(key) || trusted.has(key)) continue;
    seen.add(key);
    const o = el('option', '');
    o.value = name;
    list.appendChild(o);
  }
  const preset = el('select', 'select-filter trust-preset');
  for (const [v, label] of TRUST_PRESETS) {
    const o = el('option', '', label);
    o.value = v;
    preset.appendChild(o);
  }
  preset.value = 'overnight';
  const maxMins = info.max_minutes || 960;
  const mins = el('input', 'trust-custom-minutes');
  mins.type = 'number';
  mins.min = String(info.min_minutes || 15);
  mins.max = String(maxMins);
  mins.placeholder = 'minutes';
  mins.hidden = true;
  preset.addEventListener('change', () => { mins.hidden = preset.value !== 'custom'; });
  const tev1Label = el('label', 'trust-tev1-toggle');
  const tev1 = el('input', '');
  tev1.type = 'checkbox';
  tev1Label.append(tev1, document.createTextNode(' tev1 decides overnight (off by default)'));
  if (!info.tev1_configured) { tev1.disabled = true; tev1Label.title = 'No local tev1 advisor configured'; }
  const go = el('button', 'gate-btn gate-approve-btn trust-create-btn', 'Trust (Touch ID)');
  go.addEventListener('click', async () => {
    if (!org.value.trim()) { alert('Pick an organization'); return; }
    let ack = false;
    if (tev1.checked) {
      ack = confirm(`Enable tev1 overnight mode for ${org.value}?\n\n${info.tev1_warning || ''}\n\nOn a deny, low confidence or error the request is not run and that task is parked until you review it.`);
      if (!ack) return;
    }
    const { spec, error } = trustSpec(preset.value, mins.value, tev1.checked, ack);
    if (error) { alert(error); return; }
    if (spec.minutes && spec.minutes > maxMins) { alert(`An organization trust is at most ${maxMins} minutes`); return; }
    if (!confirm(`Trust every running task in ${org.value}? Their Red requests run without asking you until the trust ends.`)) return;
    go.disabled = true;
    const ok = await orgTrustCreate(org.value, spec);
    go.disabled = false;
    if (ok) rerender();
  });
  row.append(org, list, preset, mins, tev1Label, go);
  form.appendChild(row);
  form.appendChild(el('div', 'muted-text trust-create-note',
    `While trusted, Red requests of every running task in the organization run without asking you, and each is logged. `
    + `Always excluded: merges, and pushes to protected branches; prod writes and deploys; external API writes; `
    + `destructive deletes of real data; sending PII. Deletes outside a task worktree wait, then are skipped and queued for you. `
    + `At most ${Math.round(maxMins / 60)} h. Opening pull requests is allowed.`));
  body.appendChild(form);
}
