// Board action plans (task-e1b24d66).
//
// A Board session proposes a batch of Board actions (staypoint board plan
// propose / the staypoint_board_plan_propose MCP tool). This page lists them
// and shows one plan per /board/plans/<id>: every row with its task, the
// exact send-back text, the head it will merge and the reason, each with a
// checkbox (on by default). "Sign & run" asks for ONE Touch ID whose
// challenge the daemon binds to exactly the ticked rows, then shows each
// row's result.
//
// Loaded before app.js and uses its helpers only inside functions (el,
// apiFetch, authHeader, boardWebAuthnGetAssertion, enrollBoardPasskey,
// boardActionError, fmtDateTime, navigateTo, showToast).

const boardPlansState = {
  planId: null,   // set from the URL by pathToRoute
  selected: null, // Set of row indexes ticked on the open plan
  running: false,
};

const BOARD_PLAN_LABELS = {
  approve_merge: 'Approve & Merge',
  send_back: 'Send Back',
  mark_done: 'Mark done',
  run_now: 'Run Now',
  cancel: 'Cancel task',
  gate_approve: 'Approve gate',
  gate_deny: 'Deny gate',
  unblock: 'Unblock',
};

async function updateBoardPlansBadge() {
  try {
    const data = await apiFetch('/api/board/plans?status=pending');
    const count = (data.plans || []).length;
    const badge = document.getElementById('board-plans-badge');
    if (!badge) return;
    badge.textContent = count;
    badge.style.display = count > 0 ? '' : 'none';
  } catch (_) {}
}

function openBoardPlan(id) {
  boardPlansState.planId = id;
  boardPlansState.selected = null;
  navigateTo('board-plans', null, true);
}

async function renderBoardPlansPage() {
  const root = document.getElementById('board-plans-container');
  if (!root) return;
  if (boardPlansState.planId) return renderBoardPlan(root, boardPlansState.planId);
  root.innerHTML = '';
  let plans = [];
  try {
    plans = (await apiFetch('/api/board/plans')).plans || [];
  } catch (e) {
    root.appendChild(el('div', 'board-plan-error', 'Could not load plans: ' + (e.message || e)));
    return;
  }
  if (!plans.length) {
    root.appendChild(el('div', 'empty-state',
      'No Board action plans. A Board session proposes one with `staypoint board plan propose --file plan.json`.'));
    return;
  }
  const table = el('table', 'gates-table');
  table.innerHTML = '<thead><tr><th>Plan</th><th>Status</th><th>Actions</th><th>Proposed by</th><th>Created</th></tr></thead>';
  const tbody = el('tbody');
  for (const p of plans) {
    const tr = el('tr');
    const idCell = el('td');
    const link = el('a', 'gate-task-link', p.id);
    link.href = `/board/plans/${encodeURIComponent(p.id)}`;
    link.addEventListener('click', (ev) => { ev.preventDefault(); openBoardPlan(p.id); });
    idCell.appendChild(link);
    tr.appendChild(idCell);
    tr.appendChild(el('td', `board-plan-status board-plan-status-${p.status}`, p.status));
    tr.appendChild(el('td', '', String((p.actions || []).length)));
    tr.appendChild(el('td', 'board-plan-proposer', p.proposer || ''));
    tr.appendChild(el('td', '', fmtDateTime(p.created_at)));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  const wrap = el('div', 'gates-table-wrapper');
  wrap.appendChild(table);
  root.appendChild(wrap);
}

function boardPlanRowTarget(row) {
  const a = row.action;
  const cell = el('td', 'board-plan-target');
  if (a.task_id) {
    const link = el('a', 'gate-task-link', row.task ? row.task.name : a.task_id);
    link.href = `/tasks/${encodeURIComponent(a.task_id)}`;
    link.target = '_blank';
    link.rel = 'noopener';
    cell.appendChild(link);
    cell.appendChild(el('div', 'gate-reasons', a.task_id + (row.task ? ` · ${row.task.stage}` : '')));
  }
  if (a.gate_id) {
    cell.appendChild(el('div', 'gate-reasons', 'gate ' + a.gate_id + (row.gate ? ` · ${row.gate.status}` : '')));
    if (row.gate) cell.appendChild(el('div', 'gate-cmdline', row.gate.cmdline));
  }
  return cell;
}

function boardPlanRowDetail(row) {
  const a = row.action;
  const cell = el('td', 'board-plan-detail');
  if (a.action === 'approve_merge') {
    const head = (a.expected_head_sha || '').slice(0, 12);
    let text = `Merges head ${head}`;
    if (row.card) {
      text += ` of ${row.card.branch}` + (row.card.target_branch ? ` into ${row.card.target_branch}` : '');
      if (row.card.via_pr_merge) text += ` via PR #${row.card.pr_number}`;
      text += ` · card ${row.card.status}`;
    }
    cell.appendChild(el('div', 'board-plan-head', text));
    if (a.task_id) {
      const diff = el('a', 'gate-run-link', 'View diff');
      diff.href = `/tasks/${encodeURIComponent(a.task_id)}`;
      diff.target = '_blank';
      diff.rel = 'noopener';
      cell.appendChild(diff);
    }
  }
  if (a.text) {
    const pre = el('pre', 'board-plan-text');
    pre.textContent = a.text;
    cell.appendChild(pre);
  }
  if (a.reason) cell.appendChild(el('div', 'gate-reasons', 'Why: ' + a.reason));
  if (row.problem) cell.appendChild(el('div', 'board-plan-problem', '⚠ ' + row.problem));
  return cell;
}

async function renderBoardPlan(root, id) {
  let data;
  try {
    data = await apiFetch(`/api/board/plans/${encodeURIComponent(id)}`);
  } catch (e) {
    root.innerHTML = '';
    root.appendChild(el('div', 'board-plan-error', `Could not load plan ${id}: ${e.message || e}`));
    return;
  }
  const plan = data.plan;
  const rows = data.rows || [];
  const pending = plan.status === 'pending';
  if (!boardPlansState.selected) boardPlansState.selected = new Set(rows.map(r => r.index));
  const results = new Map((plan.results || []).map(r => [r.index, r]));

  root.innerHTML = '';
  const back = el('a', 'gate-run-link', '← All plans');
  back.href = '/board/plans';
  back.addEventListener('click', (ev) => { ev.preventDefault(); openBoardPlan(null); });
  root.appendChild(back);

  const head = el('div', 'board-plan-header');
  head.appendChild(el('h2', 'board-plan-title', `Plan ${plan.id}`));
  head.appendChild(el('span', `board-plan-status board-plan-status-${plan.status}`, plan.status));
  root.appendChild(head);
  const meta = el('div', 'gate-reasons',
    `Proposed by ${plan.proposer} (${plan.proposer_kind}) · ${fmtDateTime(plan.created_at)} · content ${String(plan.content_hash).slice(0, 12)}`
    + (plan.signer_credential ? ` · signed by passkey ${String(plan.signer_credential).slice(0, 12)}` : ''));
  root.appendChild(meta);

  const table = el('table', 'gates-table board-plan-table');
  table.innerHTML = '<thead><tr><th></th><th>#</th><th>Action</th><th>Target</th><th>What it does</th><th>Result</th></tr></thead>';
  const tbody = el('tbody');
  const signBtn = el('button', 'gate-btn gate-approve-btn board-plan-sign-btn');
  const updateSignLabel = () => {
    const n = boardPlansState.selected.size;
    signBtn.textContent = `Sign & run ${n} action${n === 1 ? '' : 's'} (Touch ID)`;
    signBtn.disabled = n === 0 || boardPlansState.running;
  };
  for (const row of rows) {
    const tr = el('tr', 'board-plan-row');
    tr.dataset.index = row.index;
    const cb = el('td');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.className = 'board-plan-check';
    box.checked = pending ? boardPlansState.selected.has(row.index) : (plan.selected || []).includes(row.index);
    box.disabled = !pending;
    box.addEventListener('change', () => {
      if (box.checked) boardPlansState.selected.add(row.index);
      else boardPlansState.selected.delete(row.index);
      updateSignLabel();
    });
    cb.appendChild(box);
    tr.appendChild(cb);
    tr.appendChild(el('td', '', String(row.index + 1)));
    tr.appendChild(el('td', 'board-plan-action', BOARD_PLAN_LABELS[row.action.action] || row.action.action));
    tr.appendChild(boardPlanRowTarget(row));
    tr.appendChild(boardPlanRowDetail(row));
    const res = results.get(row.index);
    const resCell = el('td', 'board-plan-result');
    if (res) {
      resCell.appendChild(el('span', `gate-status-${res.status === 'ok' ? 'approved' : 'denied'}`, res.status));
      if (res.error) resCell.appendChild(el('div', 'gate-reasons', res.error));
    } else if (!pending) {
      resCell.appendChild(el('span', 'gate-reasons', 'not run'));
    }
    tr.appendChild(resCell);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  const wrap = el('div', 'gates-table-wrapper');
  wrap.appendChild(table);
  root.appendChild(wrap);

  if (!pending) return;
  const bar = el('div', 'board-plan-actions');
  const errBox = el('div', 'board-plan-error');
  errBox.style.display = 'none';
  updateSignLabel();
  signBtn.addEventListener('click', () => signBoardPlan(plan.id, signBtn, errBox));
  const discardBtn = el('button', 'gate-btn gate-deny-btn', 'Discard plan');
  discardBtn.addEventListener('click', async () => {
    if (!confirm('Discard this plan? Nothing in it will run.')) return;
    const r = await fetch(`/api/board/plans/${encodeURIComponent(plan.id)}/discard`, { method: 'POST', headers: authHeader() });
    if (!r.ok) { errBox.textContent = (await boardActionError(r)).message; errBox.style.display = ''; return; }
    updateBoardPlansBadge();
    renderBoardPlansPage();
  });
  bar.appendChild(signBtn);
  bar.appendChild(discardBtn);
  bar.appendChild(el('span', 'gate-reasons',
    'One Touch ID signs exactly the ticked rows. Unticked rows do not run and are not covered by the signature.'));
  root.appendChild(bar);
  root.appendChild(errBox);
}

async function signBoardPlan(planId, btn, errBox) {
  const selected = [...boardPlansState.selected].sort((a, b) => a - b);
  if (!selected.length) return;
  boardPlansState.running = true;
  btn.disabled = true;
  btn.textContent = 'Waiting for Touch ID…';
  errBox.style.display = 'none';
  const body = JSON.stringify({ selected });
  const challengeUrl = `/api/board/plans/${encodeURIComponent(planId)}/challenge`;
  try {
    let sig;
    try {
      sig = await boardWebAuthnGetAssertion(challengeUrl, body);
    } catch (e) {
      if (!e.needsEnrollment) throw e;
      if (!confirm('Signing a plan needs a registered passkey. Enroll one now?') || !(await enrollBoardPasskey())) return;
      sig = await boardWebAuthnGetAssertion(challengeUrl, body);
    }
    btn.textContent = 'Running…';
    const r = await fetch(`/api/board/plans/${encodeURIComponent(planId)}/execute`, {
      method: 'POST',
      headers: { ...authHeader(), 'Content-Type': 'application/json',
        'X-WebAuthn-Session': sig.sessionToken, 'X-WebAuthn-Assertion': sig.assertion },
      body,
    });
    if (!r.ok) throw await boardActionError(r);
    const out = await r.json();
    const failed = (out.results || []).filter(x => x.status !== 'ok').length;
    showToast(failed ? `Plan ran: ${failed} of ${out.results.length} actions failed` : `Plan ran: all ${out.results.length} actions ok`,
      failed ? 'error' : 'success');
  } catch (e) {
    errBox.textContent = 'Not run: ' + (e.message || e);
    errBox.style.display = '';
  } finally {
    boardPlansState.running = false;
    updateBoardPlansBadge();
    if (document.getElementById('view-board-plans')?.classList.contains('active')) renderBoardPlansPage();
  }
}

// The sidebar opens the plan list, not the last plan viewed. This script
// runs before app.js registers its generic sidebar handler, so this one
// fires first.
document.querySelector('.sidebar-item[data-view="board-plans"]')
  ?.addEventListener('click', () => { boardPlansState.planId = null; boardPlansState.selected = null; });
document.addEventListener('DOMContentLoaded', () => updateBoardPlansBadge());
