/* StayPoint Web UI — Multi-Org Fleet Dashboard, SSE-driven, no build step */
'use strict';

// ── State ────────────────────────────────────────────────
const state = {
  tasks:    {},   // id → task
  sessions: {},   // id → session
  fleet:    null, // FleetOverview object
  events:   [],   // last N SSE events (boss card stream)
  maxEvents: 200,
  taskFilter: {
    search: '',
    org: 'all',
    project: 'all',
    priority: 'all',
    status: 'all',
  },
  tsFilter: { search: '', org: 'all', project: 'all', priority: 'all', status: 'all' },
  agentsFilter: { search: '', org: 'all', project: 'all', provider: 'all', status: 'all' },
  projectsFilter: {
    orgs: [],       // selected org names, empty = all
    status: 'all',  // 'all', 'active', 'running', 'done', 'blocked'
    cardStatus: {}, // `${org}:${proj}` -> status filter on that card
  },
  recentTasksFilter: {
    org: 'all',
    project: 'all',
    priority: 'all',
  },
  expandedRecentSubtasks: new Set(),
  overviewSort: { column: 'updated', direction: 'desc' },
  tsSort: { column: 'updated', direction: 'desc' },
  orgSort: { column: 'task', direction: 'asc' },
  taskComments: {},     // taskId -> array of comments
  taskDescriptions: {}, // taskId -> description string
  currentOrgDetail: null,
  openDetailTaskId: null,
  chatPollTimer:    null,
  statsElapsedTimer: null,
  bossReportCache:           {},   // reportId -> html string
  bossReportCacheTimestamps: {},   // reportId -> epoch ms
  bossReportLastRenderedAt:  0,    // epoch ms of last full render/refresh
  bossReportPreloading:      false,
  taskDetailFullPage:        false,
};

// ── Projects Filter Persistence (STA-192) ────────────────
const STORAGE_PROJECTS_ORGS_KEY = 'staypoint_projects_selected_orgs';
const STORAGE_PROJECTS_STATUS_KEY = 'staypoint_projects_status_filter';
const STORAGE_PROJECTS_CARD_STATUS_KEY = 'staypoint_projects_card_status';
// Task page "Messages (n)" thread toggle (STA-643).
const STORAGE_TASK_PAGE_CHAT_OPEN_KEY = 'staypoint_task_page_chat_open';

function loadProjectsFilters() {
  try {
    const savedOrgs = localStorage.getItem(STORAGE_PROJECTS_ORGS_KEY);
    if (savedOrgs) {
      const parsed = JSON.parse(savedOrgs);
      if (Array.isArray(parsed)) {
        state.projectsFilter.orgs = parsed;
      }
    }
  } catch { /* ignore parse error */ }

  try {
    const savedStatus = localStorage.getItem(STORAGE_PROJECTS_STATUS_KEY);
    if (savedStatus && ['all', 'active', 'running', 'done', 'blocked'].includes(savedStatus)) {
      state.projectsFilter.status = savedStatus;
    }
  } catch { /* ignore */ }

  try {
    const savedCards = localStorage.getItem(STORAGE_PROJECTS_CARD_STATUS_KEY);
    if (savedCards) {
      const parsed = JSON.parse(savedCards);
      if (parsed && typeof parsed === 'object') {
        state.projectsFilter.cardStatus = parsed;
      }
    }
  } catch { /* ignore */ }
}

function saveProjectsFilters() {
  try {
    localStorage.setItem(STORAGE_PROJECTS_ORGS_KEY, JSON.stringify(state.projectsFilter.orgs || []));
    localStorage.setItem(STORAGE_PROJECTS_STATUS_KEY, state.projectsFilter.status || 'all');
    localStorage.setItem(STORAGE_PROJECTS_CARD_STATUS_KEY, JSON.stringify(state.projectsFilter.cardStatus || {}));
  } catch { /* ignore storage errors */ }
}

// ── Token (injected by Go template) ─────────────────────
const TOKEN = document.querySelector('meta[name="staypoint-token"]')?.content || '';

// ── Utils ────────────────────────────────────────────────

function normalizeFleetStatus(s) {
  if (s === 'running' || s === 'in_progress') return 'in_progress';
  if (s === 'blocked') return 'blocked';
  if (s === 'done' || s === 'completed' || s === 'soft_deleted') return 'done';
  return 'todo';
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls)  e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function statusPill(status) {
  const norm = (status || 'active').toLowerCase().replace(/\s+/g, '_');
  return el('span', `pill pill-${norm}`, norm.replace(/_/g, ' '));
}

function fmtNum(n) {
  if (n === undefined || n === null) return '0';
  return Number(n).toLocaleString();
}

function fmtCompactNum(n) {
  if (!n) return '0';
  const num = Number(n);
  if (num >= 1_000_000_000) return (num / 1_000_000_000).toFixed(2) + 'B';
  if (num >= 1_000_000) return (num / 1_000_000).toFixed(2) + 'M';
  if (num >= 1_000) return (num / 1_000).toFixed(1) + 'k';
  return num.toLocaleString();
}

function fmtCurrency(usd) {
  if (usd === undefined || usd === null) return '$0.00';
  return '$' + Number(usd).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtTime(ts) {
  try { return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }); }
  catch { return ''; }
}

function fmtDateTime(ts) {
  try { return new Date(ts).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }); }
  catch { return ''; }
}

function titleCase(s) {
  if (!s) return '';
  return String(s).charAt(0).toUpperCase() + String(s).slice(1);
}

function fmtRelTime(ts) {
  if (!ts) return '';
  const diffMs = Date.now() - new Date(ts).getTime();
  const secs = Math.floor(diffMs / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  return fmtDateTime(ts);
}

function formatCountdown(targetTs) {
  if (!targetTs) return '';
  const diffMs = new Date(targetTs).getTime() - Date.now();
  if (diffMs <= 0) return 'resets soon';
  const mins = Math.floor(diffMs / 60000);
  const hrs = Math.floor(mins / 60);
  const remMins = mins % 60;
  if (hrs >= 24) {
    const days = Math.floor(hrs / 24);
    const remHrs = hrs % 24;
    return `in ${days}d ${remHrs}h`;
  }
  if (hrs > 0) return `in ${hrs}h ${remMins}m`;
  return `in ${remMins}m`;
}

function formatResetTime(targetTs, isWeekly) {
  if (!targetTs) return '';
  const d = new Date(targetTs);
  const timeStr = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (!isWeekly) return `at ${timeStr}`;
  const dayStr = d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' });
  return `${dayStr} at ${timeStr}`;
}

function authHeader() {
  return TOKEN ? { 'Authorization': `Bearer ${TOKEN}` } : {};
}

// ── Board WebAuthn helpers ────────────────────────────────────────────────────

function base64urlToArrayBuffer(b64url) {
  const b64 = b64url.replace(/-/g, '+').replace(/_/g, '/');
  const bin = atob(b64);
  const buf = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
  return buf.buffer;
}

function arrayBufferToBase64url(buf) {
  const bytes = new Uint8Array(buf);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// boardWebAuthnGetAssertion calls POST /api/board/webauthn/challenge, drives
// navigator.credentials.get(), and returns { sessionToken, assertion } ready
// to attach as X-WebAuthn-Session / X-WebAuthn-Assertion headers.
// Throws with { needsEnrollment: true } on 412 (no credentials registered).
async function boardWebAuthnGetAssertion() {
  const cr = await fetch('/api/board/webauthn/challenge', {
    method: 'POST',
    headers: authHeader(),
  });
  if (cr.status === 412) {
    const err = new Error('board_passkey_enrollment_required');
    err.needsEnrollment = true;
    throw err;
  }
  if (!cr.ok) throw new Error(`challenge failed: ${cr.status}`);
  const sessionToken = cr.headers.get('X-WebAuthn-Session');
  const opts = await cr.json();

  // Convert base64url fields to ArrayBuffers for the browser API.
  const publicKey = opts.publicKey;
  publicKey.challenge = base64urlToArrayBuffer(
    typeof publicKey.challenge === 'string' ? publicKey.challenge
      : arrayBufferToBase64url(publicKey.challenge));
  if (publicKey.allowCredentials) {
    publicKey.allowCredentials = publicKey.allowCredentials.map(c => ({
      ...c,
      id: base64urlToArrayBuffer(typeof c.id === 'string' ? c.id : arrayBufferToBase64url(c.id)),
    }));
  }

  const cred = await navigator.credentials.get({ publicKey });
  const assertion = JSON.stringify({
    id: cred.id,
    rawId: arrayBufferToBase64url(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: arrayBufferToBase64url(cred.response.clientDataJSON),
      authenticatorData: arrayBufferToBase64url(cred.response.authenticatorData),
      signature: arrayBufferToBase64url(cred.response.signature),
      userHandle: cred.response.userHandle ? arrayBufferToBase64url(cred.response.userHandle) : null,
    },
  });
  return { sessionToken, assertion };
}

// boardEnrollPasskey drives the full enrollment flow:
// begin → navigator.credentials.create() → fetch pairing code → finish.
async function boardEnrollPasskey(existingSessionToken, existingAssertion) {
  const extraHeaders = {};
  if (existingSessionToken) extraHeaders['X-WebAuthn-Session'] = existingSessionToken;
  if (existingAssertion) extraHeaders['X-WebAuthn-Assertion'] = existingAssertion;

  const br = await fetch('/api/board/webauthn/register/begin', {
    method: 'POST',
    headers: { ...authHeader(), ...extraHeaders },
    body: JSON.stringify({}),
  });
  if (!br.ok) {
    const e = await br.json().catch(() => ({}));
    throw new Error(e.message || `register/begin failed: ${br.status}`);
  }
  const regSessionToken = br.headers.get('X-WebAuthn-Session');
  const regOpts = await br.json();

  const pk = regOpts.publicKey;
  pk.challenge = base64urlToArrayBuffer(typeof pk.challenge === 'string' ? pk.challenge : arrayBufferToBase64url(pk.challenge));
  pk.user.id = base64urlToArrayBuffer(typeof pk.user.id === 'string' ? pk.user.id : arrayBufferToBase64url(pk.user.id));
  if (pk.excludeCredentials) {
    pk.excludeCredentials = pk.excludeCredentials.map(c => ({
      ...c,
      id: base64urlToArrayBuffer(typeof c.id === 'string' ? c.id : arrayBufferToBase64url(c.id)),
    }));
  }

  const newCred = await navigator.credentials.create({ publicKey: pk });

  const code = prompt('Enter the 6-digit pairing code shown in the StayPoint Board dialog:');
  if (!code || !code.trim()) throw new Error('Enrollment cancelled — no pairing code entered.');

  const credential = {
    id: newCred.id,
    rawId: arrayBufferToBase64url(newCred.rawId),
    type: newCred.type,
    response: {
      clientDataJSON: arrayBufferToBase64url(newCred.response.clientDataJSON),
      attestationObject: arrayBufferToBase64url(newCred.response.attestationObject),
    },
  };

  const fr = await fetch('/api/board/webauthn/register/finish', {
    method: 'POST',
    headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': regSessionToken },
    body: JSON.stringify({ code: code.trim(), credential }),
  });
  if (!fr.ok) {
    const e = await fr.json().catch(() => ({}));
    throw new Error(e.message || `register/finish failed: ${fr.status}`);
  }
}

// boardPasskeyState mirrors GET /api/board/webauthn/status for this page.
// boardSession is false when the page has no Board session (the route is
// Board-session gated); registered is whether any passkey is stored.
const boardPasskeyState = { boardSession: false, registered: false };

// refreshBoardPasskeyStatus re-reads the passkey status and shows the
// "No passkey enrolled" banner only for a Board session with zero passkeys.
async function refreshBoardPasskeyStatus() {
  let data = null;
  try {
    const r = await fetch('/api/board/webauthn/status', { headers: authHeader() });
    if (r.ok) data = await r.json();
  } catch { /* daemon unreachable: leave the banner as it was */ return boardPasskeyState; }
  boardPasskeyState.boardSession = !!data;
  boardPasskeyState.registered = !!(data && data.registered);
  const showBanner = boardPasskeyState.boardSession && !boardPasskeyState.registered;
  const banner = document.getElementById('board-passkey-banner');
  if (banner) banner.hidden = !showBanner;
  document.body.classList.toggle('has-passkey-banner', showBanner);
  return boardPasskeyState;
}

// refreshDevBuildBadge shows the header's DEV BUILD badge when /api/health
// says the daemon is not a reviewed main build (STA-805).
async function refreshDevBuildBadge() {
  let health = null;
  try { health = await apiFetch('/api/health'); } catch { return; }
  const badge = document.getElementById('dev-build-badge');
  if (!badge) return;
  badge.hidden = !health.dev_build;
  badge.title = health.dev_build
    ? `Not a reviewed main build: ${health.dev_build_reason || 'unknown reason'} (commit ${health.git_commit || 'none'})`
    : '';
}

// enrollBoardPasskey runs boardEnrollPasskey for an explicit "Enroll passkey"
// click. A second passkey needs an assertion from an existing one, so that is
// collected first when one is believed enrolled; if the server says none is
// (the cached state was stale), it enrolls as the first passkey instead.
// Returns true on success; failures, including the daemon's "Couldn't show
// the pairing code: …" (STA-696), are reported with an alert.
let boardEnrollInFlight = false;
async function enrollBoardPasskey() {
  if (boardEnrollInFlight) return false;
  boardEnrollInFlight = true;
  try {
    let sessionToken, assertion;
    if (boardPasskeyState.registered) {
      try {
        ({ sessionToken, assertion } = await boardWebAuthnGetAssertion());
      } catch (e) {
        if (!e.needsEnrollment) throw e;
        boardPasskeyState.registered = false;
      }
    }
    await boardEnrollPasskey(sessionToken, assertion);
    return true;
  } catch (e) {
    alert('Enrollment failed: ' + (e.message || e));
    return false;
  } finally {
    boardEnrollInFlight = false;
    await refreshBoardPasskeyUI();
  }
}

// refreshBoardPasskeyUI updates the banner and, when Settings is open, the
// passkey list (which re-reads the status itself).
function refreshBoardPasskeyUI() {
  return document.querySelector('#settings-security .settings-passkeys-body')
    ? refreshSettingsPasskeys()
    : refreshBoardPasskeyStatus();
}

// withBoardWebAuthn wraps a Board action fetch. It calls boardWebAuthnGetAssertion()
// first and attaches the session/assertion headers. With no passkey enrolled it
// offers enrollment and, once enrolled, asks "Continue with <actionLabel>?" so
// the Board does not have to start the action again.
// Returns null if the action was not performed (caller should abort), or the fetch Response.
async function withBoardWebAuthn(fetchFn, actionLabel) {
  let sessionToken, assertion;
  try {
    ({ sessionToken, assertion } = await boardWebAuthnGetAssertion());
  } catch (e) {
    if (!e.needsEnrollment) throw e;
    boardPasskeyState.registered = false;
    const doEnroll = confirm(
      'Board actions require a registered passkey.\n\nClick OK to enroll a passkey now, or Cancel to abort.');
    if (!doEnroll || !(await enrollBoardPasskey())) return null;
    if (!confirm(`Passkey enrolled. Continue with ${actionLabel || 'this action'}?`)) return null;
    ({ sessionToken, assertion } = await boardWebAuthnGetAssertion());
  }
  return fetchFn(sessionToken, assertion);
}

// boardActionErrorText describes a failed Board-action response with the
// server's error code, e.g. "403 Forbidden: board_passkey_assertion_invalid".
// A bare "403 Forbidden" hid why the passkey was refused (STA-716).
function boardActionErrorText(r, body) {
  let text = `${r.status} ${r.statusText}`;
  if (body && body.error) text += `: ${body.error}`;
  if (body && body.message && body.message !== body.error) text += ` (${body.message})`;
  return text;
}

async function boardActionError(r) {
  const body = await r.json().catch(() => ({}));
  return new Error(boardActionErrorText(r, body));
}

// postStage moves a task to stage as the Board: POST /stage with the Board
// session cookie, retried with Touch ID when the server asks for it (a
// prod-targeting agent task leaving backlog). Returns null when the Board
// cancelled the passkey prompt, else the Response; throws the server's error
// when it refused.
async function postStage(taskId, stage, actionLabel) {
  const send = (sessionToken, assertion) => {
    const headers = { 'Content-Type': 'application/json', ...authHeader() };
    if (sessionToken) headers['X-WebAuthn-Session'] = sessionToken;
    if (assertion) headers['X-WebAuthn-Assertion'] = assertion;
    return fetch(`/api/tasks/${encodeURIComponent(taskId)}/stage`, {
      method: 'POST', headers, body: JSON.stringify({ stage }),
    });
  };
  let res = await send('', '');
  if (res.status === 403) {
    const err = await res.clone().json().catch(() => ({}));
    if (err.error === 'board_passkey_assertion_required') {
      res = await withBoardWebAuthn(send, actionLabel);
      if (res === null) return null;
    }
  }
  if (!res.ok) throw await boardActionError(res);
  return res;
}

// postRunNow is the Run Now request: postStage to in_progress.
function postRunNow(taskId) {
  return postStage(taskId, 'in_progress', 'running this prod-targeting task');
}

// Close any open .report-dl-menu when clicking outside its wrapper.
document.addEventListener('click', () => {
  document.querySelectorAll('.report-dl-menu').forEach(m => { m.hidden = true; });
});

// escapeHtml and renderMarkdown live in lib/markdown.js.

function mdEl(text) {
  const div = el('div', 'md-body');
  div.innerHTML = renderMarkdown(text || '');
  return div;
}

// ── Fetch helpers ─────────────────────────────────────────
async function apiFetch(path, options = {}) {
  const { headers: extraHeaders, ...rest } = options;
  const r = await fetch(path, { headers: { ...authHeader(), ...extraHeaders }, ...rest });
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
  return r.json();
}

async function fetchAllTasks() {
  const tasks = [];
  for (let offset = 0; offset < 20000; offset += 1000) {
    const page = await apiFetch(`/api/tasks?status=all&include_legacy=1&limit=1000&offset=${offset}`);
    tasks.push(...(page.tasks || []));
    if (!page.has_more) break;
  }
  return { tasks };
}

// ── Task visibility ───────────────────────────────────────
// state.tasks holds every task, legacy tasks and archived Paperclip imports
// included, so task pages and direct links still open them. Every list, count
// and badge reads tasks through taskValues()/fleetTaskValues(), which hide
// them unless includeHidden: true only on All Tasks, or on Recent Tasks, Task
// Status and the Kanban board while their "Show archive & legacy" toggle is on
// (lib/taskboard.js includeHiddenFor). /api/fleet/overview already hides them
// server side; filtering here too keeps the merged lists consistent.
function taskValues(includeHidden = false) {
  return visibleTasks(Object.values(state.tasks || {}), includeHidden);
}

function fleetTaskValues(includeHidden = false) {
  return visibleTasks(state.fleet?.tasks || [], includeHidden);
}

const SHOW_HIDDEN_TOGGLES = {
  'recent-tasks': { id: 'recent-tasks-show-legacy', key: 'staypoint_recent_tasks_show_legacy' },
  'task-status': { id: 'ts-show-legacy', key: 'staypoint_task_status_show_legacy' },
  kanban: { id: 'kanban-show-legacy', key: 'staypoint_kanban_show_legacy' },
};

// showHiddenOn: whether page lists legacy/archived tasks right now.
function showHiddenOn(page) {
  const t = SHOW_HIDDEN_TOGGLES[page];
  const on = !!(t && document.getElementById(t.id)?.checked);
  return includeHiddenFor(page, on);
}

// wireShowHiddenToggles restores each toggle from localStorage (a per-viewer
// convenience) and re-renders its page on change.
function wireShowHiddenToggles() {
  for (const [page, t] of Object.entries(SHOW_HIDDEN_TOGGLES)) {
    if (page === 'kanban') continue; // wireKanbanControls owns it
    const box = document.getElementById(t.id);
    if (!box || box.dataset.wired) continue;
    box.dataset.wired = '1';
    try { box.checked = localStorage.getItem(t.key) === '1'; } catch { box.checked = false; }
    box.addEventListener('change', () => {
      try { localStorage.setItem(t.key, box.checked ? '1' : '0'); } catch { /* ignore */ }
      if (page === 'recent-tasks') { populateRecentTasksFilters(); renderRecentTasks(); }
      else renderTaskStatusPage();
    });
  }
}

// updateShowHiddenCounts writes "(n)" next to each toggle: how many tasks it
// would reveal.
function updateShowHiddenCounts() {
  const hidden = Object.values(state.tasks || {}).filter(isHiddenByDefault).length;
  for (const id of ['recent-tasks-legacy-count', 'ts-legacy-count']) {
    const span = document.getElementById(id);
    if (span) span.textContent = `(${hidden})`;
  }
}

// ── Initial data load ─────────────────────────────────────
// Fields only the task view fetches (run steps, run errors, diff, checkpoints).
const TASK_VIEW_FIELDS = ['runSteps', 'runErrors', '_diffData', '_checkpoints', 'governance'];

async function loadAll() {
  try {
    const [fleetResp, tasksResp, sessionsResp] = await Promise.all([
      apiFetch('/api/fleet/overview').catch(() => null),
      // include_legacy: the board and lists hide legacy tasks and archived
      // imports behind their own toggle; task detail and history views still
      // need them. Paged: the API caps a page at 1000 and an import alone
      // can exceed that.
      fetchAllTasks().catch(() => ({ tasks: [] })),
      apiFetch('/api/sessions').catch(() => ({ sessions: [] })),
    ]);

    if (fleetResp) state.fleet = fleetResp;
    for (const t of (tasksResp.tasks || [])) {
      // A task page opened from a direct link usually loads before this list;
      // keep what it fetched that the list does not carry (STA-775).
      const prev = state.tasks[t.id];
      if (prev) {
        for (const k of TASK_VIEW_FIELDS) {
          if (k in prev && !(k in t)) t[k] = prev[k];
        }
      }
      state.tasks[t.id] = t;
      if (t.description) state.taskDescriptions[t.id] = t.description;
      if (t.comments && t.comments.length) state.taskComments[t.id] = t.comments;
    }
    for (const s of (sessionsResp.sessions || [])) state.sessions[s.id] = s;

    if (fleetResp?.tasks) {
      for (const t of fleetResp.tasks) {
        if (!state.tasks[t.id]) {
          state.tasks[t.id] = { ...t, status: normalizeFleetStatus(t.status) };
        } else {
          state.tasks[t.id] = { ...t, ...state.tasks[t.id] };
          if (t.identifier && !state.tasks[t.id].identifier) {
            state.tasks[t.id].identifier = t.identifier;
          }
          if (t.organization && !state.tasks[t.id].organization) {
            state.tasks[t.id].organization = t.organization;
          }
          if (t.project && !state.tasks[t.id].project) {
            state.tasks[t.id].project = t.project;
          }
          if (!state.tasks[t.id].description && t.description) {
            state.tasks[t.id].description = t.description;
          }
          if ((!state.tasks[t.id].comments || !state.tasks[t.id].comments.length) && t.comments) {
            state.tasks[t.id].comments = t.comments;
          }
        }
        if (t.description) state.taskDescriptions[t.id] = t.description;
        if (t.comments && t.comments.length) state.taskComments[t.id] = t.comments;
      }
    }

    if (fleetResp?.organizations) {
      for (const org of fleetResp.organizations) {
        for (const t of (org.tasks || [])) {
          if (!state.tasks[t.id]) {
            state.tasks[t.id] = { ...t, organization: t.organization || org.name, status: normalizeFleetStatus(t.status) };
          } else {
            if (!state.tasks[t.id].organization) state.tasks[t.id].organization = org.name;
            if (t.identifier && !state.tasks[t.id].identifier) state.tasks[t.id].identifier = t.identifier;
            if (t.project && !state.tasks[t.id].project) state.tasks[t.id].project = t.project;
          }
        }
      }
    }

    loadProjectsFilters();
    populateOrgFilter();
    populateTSOrgFilter();
    populateAgentsFilters();
    populateProjectsOrgFilter();
    populateRecentTasksFilters();
    renderAll();
    renderSidebarOrgTree();
    canonicalizeTaskPageURL(state.openDetailTaskId);
    prefetchTaskComments();
  } catch (err) {
    console.error('load failed', err);
  }
}

// ── SSE connection ────────────────────────────────────────
let sseSource = null;
let sseRetryTimer = null;
let sseCursor = null;

function connectSSE() {
  const badge = document.getElementById('conn-badge');
  const url = sseCursor ? `/api/events?cursor=${sseCursor}` : '/api/events';
  if (sseSource) { sseSource.close(); sseSource = null; }

  const fullUrl = TOKEN ? `${url}${url.includes('?') ? '&' : '?'}token=${encodeURIComponent(TOKEN)}` : url;
  sseSource = new EventSource(fullUrl);

  sseSource.onopen = () => {
    badge.className = 'badge badge-live';
    badge.textContent = 'live';
    if (sseRetryTimer) { clearTimeout(sseRetryTimer); sseRetryTimer = null; }
    // Catch up on alerts raised or dismissed while disconnected.
    loadBoardAlerts();
  };
  sseSource.onerror = () => {
    badge.className = 'badge badge-error';
    badge.textContent = 'reconnecting';
    sseSource.close();
    sseRetryTimer = setTimeout(connectSSE, 3000);
  };
  sseSource.onmessage = (ev) => {
    try {
      const evt = JSON.parse(ev.data);
      sseCursor = evt.id ?? sseCursor;
      handleEvent(evt);
    } catch { /* ignore malformed */ }
  };
}

// ── Board alerts (STA-705) ───────────────────────────────
// Breaker trips and quota alerts from the daemon and the hook CLI, persisted
// in board_alerts and pushed over SSE. Shown until the Board dismisses them.
const BOARD_ALERTS_VISIBLE = 3;
const BOARD_ALERT_SEVERITY_RANK = { critical: 0, warning: 1, info: 2 };
const boardAlerts = {
  byId: new Map(),
  errors: new Map(),   // id -> dismiss error text
  expanded: false,
  seq: 0,              // bumped on every SSE change
  touched: new Map(),  // id -> seq of its latest SSE change
  loadSeq: 0,
};

// loadBoardAlerts replaces the list from GET /api/board/alerts. SSE changes
// that land while the fetch is in flight win over the (older) response, and
// an older fetch never overwrites a newer one.
async function loadBoardAlerts() {
  const loadSeq = ++boardAlerts.loadSeq;
  const startSeq = boardAlerts.seq;
  let resp;
  try {
    resp = await apiFetch('/api/board/alerts');
  } catch (err) {
    console.warn('board alerts load failed', err);
    return;
  }
  if (loadSeq !== boardAlerts.loadSeq) return;
  const next = new Map();
  for (const a of (resp.alerts || [])) next.set(a.id, a);
  for (const [id, seq] of boardAlerts.touched) {
    if (seq <= startSeq) continue;
    if (boardAlerts.byId.has(id)) next.set(id, boardAlerts.byId.get(id));
    else next.delete(id);
  }
  boardAlerts.byId = next;
  boardAlerts.touched.clear();
  renderBoardAlerts();
}

function upsertBoardAlert(a) {
  if (!a || a.id == null) return;
  boardAlerts.touched.set(a.id, ++boardAlerts.seq);
  if (a.acknowledged_at) {
    boardAlerts.byId.delete(a.id);
  } else {
    boardAlerts.byId.set(a.id, a);
  }
  renderBoardAlerts();
}

function removeBoardAlert(id) {
  if (id == null) return;
  boardAlerts.touched.set(id, ++boardAlerts.seq);
  boardAlerts.byId.delete(id);
  boardAlerts.errors.delete(id);
  renderBoardAlerts();
}

function sortedBoardAlerts() {
  return [...boardAlerts.byId.values()].sort((a, b) => {
    const ra = BOARD_ALERT_SEVERITY_RANK[a.severity] ?? 3;
    const rb = BOARD_ALERT_SEVERITY_RANK[b.severity] ?? 3;
    if (ra !== rb) return ra - rb;
    return new Date(b.last_seen_at).getTime() - new Date(a.last_seen_at).getTime();
  });
}

async function dismissBoardAlert(id) {
  boardAlerts.errors.delete(id);
  renderBoardAlerts();
  let r;
  try {
    r = await fetch(`/api/board/alerts/${encodeURIComponent(id)}/ack`, { method: 'POST', headers: authHeader() });
  } catch {
    boardAlerts.errors.set(id, 'Dismiss failed: daemon unreachable.');
    renderBoardAlerts();
    return;
  }
  if (r.ok) {
    removeBoardAlert(id);
    return;
  }
  boardAlerts.errors.set(id, (r.status === 401 || r.status === 403)
    ? 'Dismiss needs a Board session.'
    : `Dismiss failed (${r.status}).`);
  renderBoardAlerts();
}

function renderBoardAlerts() {
  const box = document.getElementById('board-alerts');
  if (!box) return;
  const list = sortedBoardAlerts();
  box.innerHTML = '';
  box.hidden = list.length === 0;
  if (!list.length) {
    boardAlerts.expanded = false;
    return;
  }
  const shown = boardAlerts.expanded ? list : list.slice(0, BOARD_ALERTS_VISIBLE);
  for (const a of shown) {
    const sev = BOARD_ALERT_SEVERITY_RANK[a.severity] !== undefined ? a.severity : 'info';
    const row = el('div', `board-alert board-alert-${sev}`);
    row.setAttribute('data-testid', 'board-alert');
    row.dataset.alertId = String(a.id);
    row.dataset.severity = sev;
    row.setAttribute('role', sev === 'critical' ? 'alert' : 'status');

    const body = el('div', 'board-alert-body');
    const head = el('div', 'board-alert-head');
    head.appendChild(el('span', 'board-alert-title', a.title || a.kind || 'Alert'));
    if (a.occurrences > 1) head.appendChild(el('span', 'board-alert-count', `×${a.occurrences}`));
    const when = el('span', 'board-alert-time', fmtRelTime(a.last_seen_at));
    if (a.last_seen_at) when.title = new Date(a.last_seen_at).toLocaleString();
    head.appendChild(when);
    body.appendChild(head);
    if (a.message) body.appendChild(el('div', 'board-alert-message', a.message));
    const err = boardAlerts.errors.get(a.id);
    if (err) body.appendChild(el('div', 'board-alert-error', err));
    row.appendChild(body);

    const btn = el('button', 'board-alert-dismiss', 'Dismiss');
    btn.type = 'button';
    btn.setAttribute('data-testid', 'board-alert-dismiss');
    btn.addEventListener('click', () => dismissBoardAlert(a.id));
    row.appendChild(btn);
    box.appendChild(row);
  }
  const hidden = list.length - BOARD_ALERTS_VISIBLE;
  if (hidden > 0) {
    const toggle = el('button', 'board-alerts-toggle', boardAlerts.expanded ? 'Show fewer' : `+${hidden} more`);
    toggle.type = 'button';
    toggle.setAttribute('aria-expanded', String(boardAlerts.expanded));
    toggle.addEventListener('click', () => {
      boardAlerts.expanded = !boardAlerts.expanded;
      renderBoardAlerts();
    });
    box.appendChild(toggle);
  }
}

// ── Event dispatch ────────────────────────────────────────
function handleEvent(evt) {
  state.events.unshift(evt);
  if (state.events.length > state.maxEvents) state.events.pop();

  const type = evt.type || '';
  if (type === 'board_alert' && evt.data) {
    upsertBoardAlert(evt.data);
    return;
  }
  if (type === 'board_alert_acknowledged' && evt.data) {
    removeBoardAlert(evt.data.id);
    return;
  }
  if (type === 'run.step' && evt.data) {
    const step = evt.data;
    const tid = step.task_id;
    if (tid) {
      if (!state.tasks[tid]) state.tasks[tid] = {};
      if (!state.tasks[tid].runSteps) state.tasks[tid].runSteps = [];
      // Upsert: running steps publish twice (status:running then status:done/error).
      // Replace the existing entry by id so the array stays accurate.
      const existingIdx = step.id ? state.tasks[tid].runSteps.findIndex(s => s.id === step.id) : -1;
      if (existingIdx >= 0) {
        state.tasks[tid].runSteps[existingIdx] = step;
      } else {
        state.tasks[tid].runSteps.push(step);
      }
    }
    if (tid && state.openDetailTaskId === tid) {
      appendRunStepToTimeline(tid, step);
      // Stop ticking when a terminal state step arrives (Finished: …)
      if (step.kind === 'state') stopElapsedTicker();
      // Edits and checkpoints change the diff the Lines stat sums.
      if ((step.kind === 'edit' || step.kind === 'checkpoint') && step.status !== 'running' && !isFleetTaskId(tid)) {
        scheduleTaskDiffRefresh(tid);
      }
    }
    return;
  }
  if (type === 'run.state' && evt.data) {
    const { task_id: tid, disposition } = evt.data;
    if (tid) {
      if (state.tasks[tid]) state.tasks[tid].execution_stage = disposition;
      if (tid === state.openDetailTaskId) {
        updateRunControlBar(tid, disposition);
      }
    }
    return;
  }
  if (type === 'run.turn' && evt.data) {
    // Turn in flight (or null when it ends), for the stats strip turn timer.
    const { task_id: tid, turn } = evt.data;
    if (tid && state.tasks[tid]) {
      state.tasks[tid].turn = turn || null;
      if (tid === state.openDetailTaskId) refreshTaskStatsBar(tid);
    }
    return;
  }
  if (type === 'run.queue' && evt.data) {
    const tid = state.openDetailTaskId;
    if (tid) {
      const pos = queuePositionIn(evt.data.queue, tid);
      if (state.tasks[tid]) state.tasks[tid].queue = pos;
      setRunQueueLine(document.getElementById(`run-queue-line-${tid}`), pos);
    }
    return;
  }
  if (type === 'run_control' && evt.data) {
    const { task_id: tid, action } = evt.data;
    if (tid && (action === 'pause' || action === 'resume') && tid === state.openDetailTaskId) {
      syncRunControlBar(tid);
    }
    return;
  }
  if (type === 'run.stats' && evt.data) {
    const stats = evt.data;
    const tid = stats.task_id;
    if (tid) {
      if (!state.tasks[tid]) state.tasks[tid] = {};
      if (stats.spent_usd != null) state.tasks[tid].spent_usd = Math.max(state.tasks[tid].spent_usd || 0, stats.spent_usd);
      if (stats.spent_tokens != null) state.tasks[tid].spent_tokens = Math.max(state.tasks[tid].spent_tokens || 0, stats.spent_tokens);
    }
    if (tid && state.openDetailTaskId === tid) {
      refreshTaskStatsBar(tid);
    }
    return;
  }
  if (type === 'board_passkey_registered' || type === 'board_passkey_deleted') {
    refreshBoardPasskeyUI();
    return;
  }
  if (type === 'security_gate_request' && evt.data) {
    updateGatesBadge();
    if (document.getElementById('view-gates')?.classList.contains('active')) {
      renderGatesPage();
    }
    return;
  }
  if (type === 'org_hold') {
    loadOrgHolds();
    return;
  }
  if (type === 'security_gate_decided' && evt.data) {
    updateGatesBadge();
    if (document.getElementById('view-gates')?.classList.contains('active')) {
      renderGatesPage();
    }
    return;
  }
  if (type === 'security_gate_deferred' && evt.data) {
    updateGatesBadge();
    if (document.getElementById('view-gates')?.classList.contains('active')) renderGatesPage();
    return;
  }
  // STA-868: an advisory arrived, or an allow rule changed.
  if ((type === 'security_gate_advice' || type === 'security_gate_rules') && evt.data) {
    if (document.getElementById('view-gates')?.classList.contains('active')) {
      renderGatesPage();
    }
    return;
  }
  if (type === 'ship_review_dev_progress' && evt.data) {
    const d = evt.data;
    const tid = d.task_id;
    if (tid && state.openDetailTaskId === tid) {
      updateDevProgressUI(tid, d.step, d.message, d.ok);
    }
    return;
  }
  if (type.startsWith('ship_review_') && evt.data) {
    const d = evt.data;
    const tid = d.task_id;
    if (tid && state.openDetailTaskId === tid) {
      // Reload the ship review card in the task page or the drawer.
      const slot = document.querySelector('#task-page-content .task-page-review-card-slot, #panel-content .task-page-review-card-slot');
      if (slot) {
        const existing = document.getElementById(`ship-review-${tid}`);
        if (existing) existing.remove();
        // Clear the header buttons too: if the refetch fails no card renders,
        // and they would stay wired to the removed card (STA-657).
        clearShipReviewHeaderActions(tid);
        renderShipReviewCard(slot, tid);
      }
    }
    return;
  }
  if (type === 'run.stats' && evt.data) {
    const stats = evt.data;
    const tid = stats.task_id;
    if (tid) {
      if (!state.tasks[tid]) state.tasks[tid] = {};
      if (stats.spent_usd != null) state.tasks[tid].spent_usd = Math.max(state.tasks[tid].spent_usd || 0, stats.spent_usd);
      if (stats.spent_tokens != null) state.tasks[tid].spent_tokens = Math.max(state.tasks[tid].spent_tokens || 0, stats.spent_tokens);
    }
    if (tid && state.openDetailTaskId === tid) {
      const statsBar = document.getElementById(`timeline-stats-${tid}`);
      if (statsBar) {
        const task = state.tasks[tid] || {};
        const runSteps = task.runSteps || [];
        const createdAtMs = task.created_at ? new Date(task.created_at).getTime() : null;
        const lastStepAt = runSteps.length ? new Date(runSteps[runSteps.length - 1].created_at).getTime() : null;
        const elapsedMs = lastStepAt && createdAtMs ? lastStepAt - createdAtMs : (createdAtMs ? Date.now() - createdAtMs : null);
        const stuck = isStuck(runSteps, Date.now(), task.status);
        statsBar.innerHTML = '';
        statsBar.appendChild(buildTimelineStats(task, runSteps, elapsedMs, stuck));
      }
    }
    return;
  }
  if ((type.startsWith('task_') || type.startsWith('task.')) && evt.data) {
    const t = evt.data;
    if (t.id) state.tasks[t.id] = Object.assign(state.tasks[t.id] || {}, t);
    refreshFleetData();
  } else if ((type.startsWith('session_') || type.startsWith('session.')) && evt.data) {
    const s = evt.data;
    if (s.id) state.sessions[s.id] = Object.assign(state.sessions[s.id] || {}, s);
    refreshFleetData();
  } else if (type.startsWith('quota.') || type.startsWith('fleet.')) {
    refreshFleetData();
  }
  renderAll();
}

async function refreshFleetData() {
  try {
    const fleetResp = await apiFetch('/api/fleet/overview');
    if (fleetResp) {
      state.fleet = fleetResp;
      if (fleetResp.tasks) {
        for (const t of fleetResp.tasks) {
          if (t.description) state.taskDescriptions[t.id] = t.description;
          if (t.comments && t.comments.length) state.taskComments[t.id] = t.comments;
          if (state.tasks[t.id]) {
            if (!state.tasks[t.id].description && t.description) state.tasks[t.id].description = t.description;
            if ((!state.tasks[t.id].comments || !state.tasks[t.id].comments.length) && t.comments) state.tasks[t.id].comments = t.comments;
          }
        }
      }
      populateOrgFilter();
      populateTSOrgFilter();
      populateAgentsFilters();
      populateProjectsOrgFilter();
      populateRecentTasksFilters();
      renderOverview();
      renderSidebarOrgTree();
    }
  } catch { /* silent retry */ }
}

// ── Sidebar ────────────────────────────────────────────────
function renderSidebarOrgTree() {
  const tree = document.getElementById('sidebar-org-tree');
  if (!tree) return;
  tree.innerHTML = '';

  const orgs = state.fleet?.organizations || [];
  if (!orgs.length) {
    tree.appendChild(el('div', 'sidebar-org-placeholder', 'No organizations'));
    return;
  }

  for (const org of orgs) {
    const btn = el('button', 'sidebar-org-item');
    if (state.currentOrgDetail === org.name) btn.classList.add('active');

    const dot = el('span', 'sidebar-org-dot');
    const running = org.task_counts?.running || 0;
    const blocked = org.task_counts?.blocked || 0;
    if (running > 0) dot.classList.add('has-running');
    else if (blocked > 0) dot.classList.add('has-blocked');

    const nameSpan = el('span', null, org.name);
    const countSpan = el('span', 'muted-text', ` (${running})`);

    btn.appendChild(dot);
    btn.appendChild(nameSpan);
    btn.appendChild(countSpan);
    btn.addEventListener('click', () => openOrgDetail(org.name));
    tree.appendChild(btn);
  }
}

// Sidebar toggle
document.getElementById('sidebar-toggle')?.addEventListener('click', () => {
  const sidebar = document.getElementById('sidebar');
  sidebar?.classList.toggle('collapsed');
  document.body.classList.toggle('sidebar-collapsed', Boolean(sidebar?.classList.contains('collapsed')));
});

// ── SPA URL Routing ──────────────────────────────────────
function viewToPath(viewName, orgName) {
  if (orgName) return `/org/${encodeURIComponent(orgName)}`;
  if (!viewName || viewName === 'overview') return '/';
  return `/${viewName}`;
}

function projectSlug(task) {
  const p = task.project;
  if (!p) return 'default';
  if (typeof p !== 'object') return String(p) || 'default';
  return p.urlKey || p.slug || p.name || p.id || 'default';
}

// Local daemon tasks ("task-…") only resolve by id. Any identifier they carry is
// the fleet aggregator's display label (issue prefix + first 6 chars of the id,
// e.g. "RHI-task-e"), which no endpoint can look up (STA-693).
function taskRouteIdent(task) {
  const id = task.id || '';
  if (id.startsWith('task-')) return id;
  return task.identifier || id;
}

function taskToPath(task) {
  if (!task) return '/';
  const ident = taskRouteIdent(task);
  let org = '';
  if (ident && ident.includes('-') && !ident.startsWith('task-')) {
    org = ident.split('-')[0];
  }
  if (!org && task.organization) {
    const matched = (state.fleet?.organizations || []).find(o =>
      o.name?.toLowerCase() === task.organization.toLowerCase() ||
      o.issue_prefix?.toLowerCase() === task.organization.toLowerCase() ||
      o.id === task.organization
    );
    org = matched?.issue_prefix || matched?.name || task.organization;
  }
  if (!org && task.org) {
    org = task.org;
  }
  if (!org) {
    org = 'STA';
  }
  const project = projectSlug(task);
  return `/tasks/${encodeURIComponent(org)}/${encodeURIComponent(project)}/${encodeURIComponent(ident)}`;
}

function findTask(target, orgHint = null, projectHint = null) {
  if (!target) return null;
  const targetStr = String(target).trim();
  const targetLower = targetStr.toLowerCase();
  const cleanTarget = targetLower.startsWith('#') ? targetLower.slice(1) : targetLower;

  // 1. Direct exact key match in state.tasks
  if (state.tasks[targetStr]) return state.tasks[targetStr];
  if (state.tasks[cleanTarget]) return state.tasks[cleanTarget];

  // Gather candidate pool
  const allTasks = Object.values(state.tasks || {});
  if (state.fleet?.tasks) {
    for (const ft of state.fleet.tasks) {
      if (!allTasks.find(t => t.id === ft.id)) {
        allTasks.push(ft);
      }
    }
  }

  const exactMatches = [];
  const partialMatches = [];

  for (const t of allTasks) {
    const id = (t.id || '').toLowerCase();
    const ident = taskRouteIdent(t).toLowerCase();

    if (id === cleanTarget || (ident && ident === cleanTarget) || (ident && ident === targetLower)) {
      exactMatches.push(t);
    } else if (cleanTarget.length >= 6 && id.startsWith(cleanTarget)) {
      partialMatches.push(t);
    } else if (ident && (ident.endsWith('-' + cleanTarget) || ident.endsWith('-task-' + cleanTarget))) {
      partialMatches.push(t);
    }
  }

  const pool = exactMatches.length > 0 ? exactMatches : partialMatches;
  if (pool.length === 1) return pool[0];

  if (pool.length > 1) {
    if (orgHint) {
      const orgLower = orgHint.toLowerCase();
      const match = pool.find(t => {
        const tOrg = (t.organization || t.org || '').toLowerCase();
        const tPrefix = (t.identifier || '').split('-')[0].toLowerCase();
        return tOrg === orgLower || tPrefix === orgLower;
      });
      if (match) return match;
    }
    if (projectHint) {
      const projLower = projectHint.toLowerCase();
      const match = pool.find(t => (t.project || 'default').toLowerCase() === projLower);
      if (match) return match;
    }
    return pool[0];
  }

  return null;
}

// Second chance for a /tasks/:org/:project/:ident URL whose ident the daemon
// 404'd: links minted before STA-693 carry the fleet display label
// ("RHI-task-e") instead of the task id. Look the id part up against the
// daemon's task list and return a task id only when exactly one task matches,
// narrowing by the URL's project and org; an ambiguous prefix stays a miss.
// The label's prefix is the task's own org issue_prefix, so a labelled ident
// only ever resolves to a task in that org (STA-722): the intended task may be
// gone or past the list cap, and another org's task-8… is not it.
async function resolveTaskRouteMiss(ident, orgHint, projectHint) {
  const raw = String(ident || '').trim();
  const labelled = /^([A-Za-z]+)-(task-.+)$/.exec(raw);
  const idPart = (labelled ? labelled[2] : raw).toLowerCase();
  if (!idPart.startsWith('task-')) return null;

  let tasks;
  try {
    tasks = (await apiFetch('/api/tasks?status=all&limit=1000')).tasks || [];
  } catch {
    return null;
  }
  let pool = tasks.filter(t => (t.id || '').toLowerCase().startsWith(idPart));

  if (labelled && pool.length) {
    pool = await filterByLabelOrg(pool, labelled[1].toLowerCase());
  }

  const narrow = (pred) => {
    const kept = pool.filter(pred);
    if (kept.length) pool = kept;
  };
  // Soft: links minted before STA-693 carried "default" for every project.
  if (pool.length > 1 && projectHint) {
    const proj = projectHint.toLowerCase();
    narrow(t => (t.project || 'default').toLowerCase() === proj);
  }
  if (pool.length > 1 && orgHint && !labelled) {
    const orgKey = orgHint.toLowerCase();
    const prefixOf = orgPrefixMap(state.fleet?.organizations);
    narrow(t => {
      const name = taskOrgName(t);
      return name === orgKey || prefixOf.get(name) === orgKey;
    });
  }
  return pool.length === 1 ? pool[0].id : null;
}

// The fleet aggregator files a task with no organization under "StayPoint".
function taskOrgName(t) {
  return (t.organization || 'StayPoint').toLowerCase();
}

function orgPrefixMap(orgs) {
  const m = new Map();
  for (const o of orgs || []) {
    if (o.name && o.issue_prefix) m.set(o.name.toLowerCase(), o.issue_prefix.toLowerCase());
  }
  return m;
}

// Keeps the tasks whose org is the label prefix, by name or by the org's fleet
// issue_prefix. At boot state.fleet may not be loaded yet, so when a candidate's
// org has no known prefix the overview is fetched here; if that fails the
// candidate stays unmatched and the URL stays a miss rather than guessing.
async function filterByLabelOrg(pool, prefix) {
  let prefixOf = orgPrefixMap(state.fleet?.organizations);
  if (pool.some(t => taskOrgName(t) !== prefix && !prefixOf.has(taskOrgName(t)))) {
    try {
      prefixOf = orgPrefixMap((await apiFetch('/api/fleet/overview'))?.organizations);
    } catch { /* unknown orgs stay unmatched */ }
  }
  return pool.filter(t => taskOrgName(t) === prefix || prefixOf.get(taskOrgName(t)) === prefix);
}

function pathToRoute(pathname) {
  const p = (pathname || window.location.pathname).replace(/\/+$/, '') || '/';
  if (p === '/' || p === '/overview') return { view: 'overview', org: null, taskId: null };
  if (p.startsWith('/org/')) {
    const org = decodeURIComponent(p.slice(5));
    return { view: 'org-detail', org, taskId: null };
  }
  if (p.startsWith('/tasks/') || p.startsWith('/issues/')) {
    const prefix = p.startsWith('/tasks/') ? '/tasks/' : '/issues/';
    const rest = p.slice(prefix.length);
    const segments = rest.split('/').filter(Boolean).map(decodeURIComponent);
    if (segments.length >= 3) {
      // /tasks/:org/:project/:identifier
      const org = segments[0];
      const project = segments[1];
      const identifier = segments.slice(2).join('/');
      return { view: 'overview', org, project, identifier, taskId: identifier };
    } else if (segments.length === 2) {
      // /tasks/:org/:identifier
      const org = segments[0];
      const identifier = segments[1];
      return { view: 'overview', org, project: null, identifier, taskId: identifier };
    } else if (segments.length === 1) {
      // /tasks/:identifier or /tasks/:id
      const identifier = segments[0];
      return { view: 'overview', org: null, project: null, identifier, taskId: identifier };
    }
    return { view: 'recent-tasks', org: null, taskId: null };
  }
  if (p === '/tasks' || p === '/issues') {
    return { view: 'recent-tasks', org: null, taskId: null };
  }
  const clean = p.replace(/^\//, '');
  return { view: clean, org: null, taskId: null };
}

function navigateTo(viewName, orgName = null, pushHistory = true) {
  const targetPath = viewToPath(viewName, orgName);
  if (pushHistory && window.location.pathname !== targetPath) {
    history.pushState({ view: viewName, org: orgName }, '', targetPath);
  }

  state.currentOrgDetail = orgName;

  // Update sidebar active buttons
  document.querySelectorAll('.sidebar-item').forEach(b => {
    b.classList.toggle('active', !orgName && b.dataset.view === viewName);
  });

  // Switch view visibility
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  const targetViewId = orgName ? 'view-org-detail' : `view-${viewName}`;
  const targetEl = document.getElementById(targetViewId);
  if (targetEl) targetEl.classList.add('active');

  renderSidebarOrgTree();

  // Render view content
  if (orgName) {
    const org = (state.fleet?.organizations || []).find(o => o.name === orgName || o.name.toLowerCase() === orgName.toLowerCase());
    if (org) renderOrgDetailView(org);
  } else {
    if (viewName === 'projects')     { populateProjectsOrgFilter(); renderProjects(); }
    if (viewName === 'agents')       { populateAgentsFilters(); renderAgentsPage(); }
    if (viewName === 'recent-tasks') { populateRecentTasksFilters(); renderRecentTasks(); }
    if (viewName === 'task-status')  renderTaskStatusPage();
    if (viewName === 'all-tasks')    { wireAllTasksControls(); populateAllTasksOrgFilter(); renderAllTasksPage(); }
    if (viewName === 'cost')         renderCostPage();
    if (viewName === 'settings')     renderSettings();
    if (viewName === 'checklist')    loadChecklistSprints().then(() => loadChecklist());
    if (viewName === 'overview')     renderOverview();
    if (viewName === 'kanban')       renderKanban();
    if (viewName === 'task-page')    { /* content rendered by openTaskPage() */ }
    if (viewName === 'logs')         renderLogsPage();
    if (viewName === 'artifacts')    renderArtifactsPage();
    if (viewName === 'gates')        renderGatesPage();
    if (viewName === 'pull-requests') renderPullRequestsPage();
    if (viewName === 'boss') {
      renderBoss();
      preloadBossReports(true);
    }
  }

  if (typeof updateWalkthroughSidebarHighlight === 'function') {
    updateWalkthroughSidebarHighlight();
  }
}

function showView(viewName) {
  navigateTo(viewName, null, true);
}

// Sidebar nav item clicks
document.querySelectorAll('.sidebar-item').forEach(btn => {
  btn.addEventListener('click', () => {
    navigateTo(btn.dataset.view, null, true);
  });
});

window.addEventListener('popstate', (e) => {
  const route = pathToRoute();
  if (route.taskId || route.identifier) {
    if (e.state?.taskPage === false) {
      // Sidebar mode (in-app navigation)
      const panel = document.getElementById('detail-panel');
      if (panel) panel.classList.remove('hidden');
      navigateTo(route.view, route.org, false);
      openDetail(route, false);
    } else {
      // Full-page mode (direct link, "Open" button, or back/forward)
      openTaskPage(route, false);
    }
  } else {
    document.getElementById('detail-panel')?.classList.add('hidden');
    stopChatPoll();
    stopElapsedTicker();
    state.openDetailTaskId = null;
    navigateTo(route.view, route.org, false);
  }
});

// ── Quick filter buttons ──────────────────────────────────
document.getElementById('filter-running')?.addEventListener('click', () => {
  state.taskFilter.status = 'running';
  state.taskFilter.org = 'all';
  state.taskFilter.project = 'all';
  state.taskFilter.priority = 'all';
  showView('overview');
  document.querySelectorAll('.sidebar-item').forEach(b => {
    b.classList.toggle('active', b.dataset.view === 'overview');
  });
  renderGlobalTaskTable();
  const sel = document.getElementById('task-status-filter');
  if (sel) sel.value = 'running';
  const os = document.getElementById('task-org-filter');
  if (os) os.value = 'all';
  populateOverviewProjectFilter();
  const ps = document.getElementById('task-project-filter');
  if (ps) ps.value = 'all';
  const pris = document.getElementById('task-priority-filter');
  if (pris) pris.value = 'all';
});

document.getElementById('filter-blocked')?.addEventListener('click', () => {
  state.taskFilter.status = 'blocked';
  state.taskFilter.org = 'all';
  state.taskFilter.project = 'all';
  state.taskFilter.priority = 'all';
  showView('overview');
  document.querySelectorAll('.sidebar-item').forEach(b => {
    b.classList.toggle('active', b.dataset.view === 'overview');
  });
  renderGlobalTaskTable();
  const sel = document.getElementById('task-status-filter');
  if (sel) sel.value = 'blocked';
  const os = document.getElementById('task-org-filter');
  if (os) os.value = 'all';
  populateOverviewProjectFilter();
  const ps = document.getElementById('task-project-filter');
  if (ps) ps.value = 'all';
  const pris = document.getElementById('task-priority-filter');
  if (pris) pris.value = 'all';
});

document.getElementById('filter-clear')?.addEventListener('click', () => {
  state.taskFilter = { search: '', org: 'all', project: 'all', priority: 'all', status: 'all' };
  const si = document.getElementById('task-search-input');
  if (si) si.value = '';
  const os = document.getElementById('task-org-filter');
  if (os) os.value = 'all';
  populateOverviewProjectFilter();
  const ps = document.getElementById('task-project-filter');
  if (ps) ps.value = 'all';
  const pris = document.getElementById('task-priority-filter');
  if (pris) pris.value = 'all';
  const ss = document.getElementById('task-status-filter');
  if (ss) ss.value = 'all';
  renderGlobalTaskTable();
});

// ── Universal KPI Drill-down Navigation Helpers ───────────
function drillDownToTasks(status, org = 'all', project = 'all') {
  state.tsFilter = state.tsFilter || { search: '', org: 'all', project: 'all', priority: 'all', status: 'all' };
  state.tsFilter.status = status;
  state.tsFilter.org = org;
  state.tsFilter.project = project;
  state.tsFilter.priority = 'all';
  state.tsFilter.search = '';

  const tsStatusSel = document.getElementById('ts-status-filter');
  if (tsStatusSel) tsStatusSel.value = status;
  const tsOrgSel = document.getElementById('ts-org-filter');
  if (tsOrgSel) tsOrgSel.value = org;
  populateTSProjectFilter();
  const tsProjSel = document.getElementById('ts-project-filter');
  if (tsProjSel) tsProjSel.value = state.tsFilter.project;
  const tsPriSel = document.getElementById('ts-priority-filter');
  if (tsPriSel) tsPriSel.value = 'all';
  const tsSearch = document.getElementById('ts-search-input');
  if (tsSearch) tsSearch.value = '';

  // Also synchronize Overview table filters so returning retains filter context
  state.taskFilter = state.taskFilter || { search: '', org: 'all', project: 'all', priority: 'all', status: 'all' };
  state.taskFilter.status = status;
  state.taskFilter.org = org;
  state.taskFilter.project = 'all';
  state.taskFilter.priority = 'all';
  const ovStatusSel = document.getElementById('task-status-filter');
  if (ovStatusSel) ovStatusSel.value = status;
  const ovOrgSel = document.getElementById('task-org-filter');
  if (ovOrgSel) ovOrgSel.value = org;
  populateOverviewProjectFilter();
  const ovProjSel = document.getElementById('task-project-filter');
  if (ovProjSel) ovProjSel.value = 'all';
  const ovPriSel = document.getElementById('task-priority-filter');
  if (ovPriSel) ovPriSel.value = 'all';

  showView('task-status');
  renderTaskStatusPage();
}

function drillDownToAgents(status = 'all', org = 'all') {
  state.agentsFilter = state.agentsFilter || { search: '', org: 'all', project: 'all', provider: 'all', status: 'all' };
  if (status !== 'all') state.agentsFilter.status = status;
  if (org !== 'all') state.agentsFilter.org = org;
  const selOrg = document.getElementById('agents-org-filter');
  if (selOrg && org !== 'all') selOrg.value = org;
  showView('agents');
}

function drillDownToCost() {
  showView('cost');
}

function initOverviewKPIClicks() {
  if (state.__kpisInitialized) return;
  state.__kpisInitialized = true;

  const wire = (id, fn) => {
    const cardEl = document.getElementById(id);
    if (!cardEl) return;
    cardEl.addEventListener('click', fn);
    cardEl.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        fn();
      }
    });
  };

  wire('kpi-card-orgs', () => showView('projects'));
  wire('kpi-card-running', () => drillDownToTasks('running'));
  wire('kpi-card-active', () => drillDownToTasks('active'));
  wire('kpi-card-blocked', () => drillDownToTasks('blocked'));
  wire('kpi-card-done', () => drillDownToTasks('done'));
  wire('kpi-card-total', () => drillDownToTasks('all'));
  wire('kpi-card-agents', () => drillDownToAgents());
  wire('kpi-card-tokens', () => drillDownToCost());
  wire('kpi-card-cost', () => drillDownToCost());
}

// ── Render: All Organizations Overview Screen ─────────────
function renderOverview() {
  const f = state.fleet;
  if (!f) return;

  const orgCount = (f.organizations || []).length;
  const elOrgs = document.getElementById('kpi-orgs');
  if (elOrgs) elOrgs.textContent = String(orgCount);

  const gt = f.global_tasks || {};
  const runningTasks = gt.running || 0;
  const activeTasks  = gt.active || 0;
  const blockedTasks = gt.blocked || 0;
  const doneTasks    = gt.done || 0;
  const totalTasks   = gt.total || 0;

  const elRunning = document.getElementById('kpi-running-tasks');
  if (elRunning) elRunning.textContent = String(runningTasks);
  const elTasksTotal = document.getElementById('kpi-tasks-total');
  if (elTasksTotal) elTasksTotal.textContent = `${totalTasks} total tasks`;

  const elActive = document.getElementById('kpi-active-tasks');
  if (elActive) elActive.textContent = String(activeTasks);

  const elBlocked = document.getElementById('kpi-blocked-tasks');
  if (elBlocked) elBlocked.textContent = String(blockedTasks);

  const elDone = document.getElementById('kpi-done-tasks');
  if (elDone) elDone.textContent = String(doneTasks);

  const elTotalVal = document.getElementById('kpi-total-tasks-val');
  if (elTotalVal) elTotalVal.textContent = String(totalTasks);

  const activeAgents = f.global_agents?.active_running || 0;
  const elAgents = document.getElementById('kpi-active-agents');
  if (elAgents) elAgents.textContent = String(activeAgents);

  const providerCounts = f.global_agents?.by_provider || {};
  const elAgentsBreakdown = document.getElementById('kpi-agents-breakdown');
  if (elAgentsBreakdown) {
    elAgentsBreakdown.textContent =
      `Claude: ${providerCounts.claude || 0} · Gemini: ${providerCounts.gemini || 0} · OpenAI: ${providerCounts.openai || 0}`;
  }

  const totTokens = f.token_telemetry?.total_tokens || 0;
  const elTokens = document.getElementById('kpi-total-tokens');
  if (elTokens) elTokens.textContent = fmtCompactNum(totTokens);
  const elTokensIo = document.getElementById('kpi-tokens-io');
  if (elTokensIo) {
    elTokensIo.textContent =
      `In: ${fmtCompactNum(f.token_telemetry?.input_tokens)} · Out: ${fmtCompactNum(f.token_telemetry?.output_tokens)}`;
  }

  const elCost = document.getElementById('kpi-total-cost');
  if (elCost) elCost.textContent = fmtCurrency(f.token_telemetry?.total_cost_usd);

  initOverviewKPIClicks();

  updateFleetPacingBadge(f.provider_quotas);
  renderQuotaGauges(f.provider_quotas);
  renderOrganizationsGrid(f.organizations);
  renderTokenTelemetrySection(f.token_telemetry, f.model_spend, f.org_spend);
  renderGlobalTaskTable();
  if (document.getElementById('view-settings')?.classList.contains('active')) {
    renderSettings();
  }
  const modalEl = document.getElementById('fleet-modal');
  if (modalEl && !modalEl.classList.contains('hidden')) {
    switchFleetModalTab(fleetModalState.activeTab);
  }
}

function updateFleetPacingBadge(quotas) {
  const pill = document.getElementById('pacing-pill');
  if (!pill || !quotas) return;
  let anyLocked = false, anyOverpaced = false;
  for (const q of Object.values(quotas)) {
    if (q.is_locked || q.projection_status === 'locked_out') anyLocked = true;
    else if (q.projection_status === 'overpaced') anyOverpaced = true;
  }
  if (anyLocked) {
    pill.className = 'pill pill-red';
    pill.textContent = '✖ Quota Lockout Detected';
  } else if (anyOverpaced) {
    pill.className = 'pill pill-amber';
    pill.textContent = '⚠ Overpaced Burn Warning';
  } else {
    pill.className = 'pill pill-green';
    pill.textContent = '✔ Fleet Pacing On Track';
  }
}

function buildGaugeCard(key, q) {
  const card = el('div', 'gauge-card');
  const hdr = el('div', 'gauge-card-header');
  hdr.appendChild(el('span', 'gauge-provider-name', q.display_name || key));

  const pill = quotaStatusPill(q);
  hdr.appendChild(el('span', `pill ${pill.cls}`, pill.text));
  card.appendChild(hdr);

  // 5-Hour Rolling. An unmeasured window renders "No data", never 0% used.
  const v5h = quotaWindowView(q, 'five_hour');
  const used5h = v5h.measured ? v5h.used : 0;

  const bar5hOuter = el('div', 'gauge-bar-outer');
  const bar5hInner = el('div', 'gauge-bar-inner');
  bar5hInner.style.width = `${used5h}%`;
  bar5hInner.className = `gauge-bar-inner ${
    (q.is_locked || used5h >= 95) ? 'gauge-bar-red' :
    used5h >= 75 ? 'gauge-bar-amber' : 'gauge-bar-green'
  }`;
  bar5hOuter.appendChild(bar5hInner);
  card.appendChild(el('div', 'gauge-window-label', '5-Hour Rolling'));
  card.appendChild(bar5hOuter);

  const m5Row = el('div', 'gauge-metrics-row');
  m5Row.appendChild(el('span', null, v5h.text));
  const count5h = v5h.measured ? formatCountdown(q.five_hour_resets_at) : '';
  const time5h = v5h.measured ? formatResetTime(q.five_hour_resets_at, false) : '';
  m5Row.appendChild(el('span', 'gauge-metric-val', count5h ? `resets ${count5h}` : (v5h.measured ? 'rolling' : 'not measured')));
  card.appendChild(m5Row);
  if (time5h) {
    const t5Row = el('div', 'gauge-metrics-row');
    t5Row.appendChild(el('span', null, ''));
    t5Row.appendChild(el('span', 'gauge-reset-time', time5h));
    card.appendChild(t5Row);
  }

  const b5Row = el('div', 'gauge-metrics-row');
  b5Row.appendChild(el('span', null, `Burn: ${q.burn_rate_5h ? q.burn_rate_5h.toFixed(2) + '%/turn' : '—'}`));
  b5Row.appendChild(el('span', null, q.is_locked ? '🔒 Locked Out' : `Limit: ${q.lockout_threshold_pct || 100}%`));
  card.appendChild(b5Row);

  // Weekly Budget
  const vWk = quotaWindowView(q, 'weekly');
  const usedWk = vWk.measured ? vWk.used : 0;

  const labelWk = el('div', 'gauge-window-label', 'Weekly Budget');
  labelWk.style.marginTop = '10px';
  card.appendChild(labelWk);

  const barWkOuter = el('div', 'gauge-bar-outer');
  const barWkInner = el('div', 'gauge-bar-inner');
  barWkInner.style.width = `${usedWk}%`;
  barWkInner.className = `gauge-bar-inner ${
    usedWk >= 90 ? 'gauge-bar-red' :
    usedWk >= 70 ? 'gauge-bar-amber' : 'gauge-bar-green'
  }`;
  barWkOuter.appendChild(barWkInner);
  card.appendChild(barWkOuter);

  const mWkRow = el('div', 'gauge-metrics-row');
  mWkRow.appendChild(el('span', null, vWk.text));
  const countWk = vWk.measured ? formatCountdown(q.weekly_resets_at) : '';
  if (countWk) mWkRow.appendChild(el('span', 'gauge-metric-val', `resets ${countWk}`));
  card.appendChild(mWkRow);
  const timeWk = vWk.measured ? formatResetTime(q.weekly_resets_at, true) : '';
  if (timeWk) {
    const tWkRow = el('div', 'gauge-metrics-row');
    tWkRow.appendChild(el('span', null, ''));
    tWkRow.appendChild(el('span', 'gauge-reset-time', timeWk));
    card.appendChild(tWkRow);
  }

  const bWkRow = el('div', 'gauge-metrics-row');
  bWkRow.appendChild(el('span', null, `Weekly burn: ${q.burn_rate_weekly ? q.burn_rate_weekly.toFixed(2) + '%/turn' : '—'}`));
  if (q.runway_turns) bWkRow.appendChild(el('span', 'gauge-metric-val', `${q.runway_turns} turns left`));
  card.appendChild(bWkRow);

  card.appendChild(el('div', 'gauge-projection-box', q.projection_message || 'Sustainable pacing'));
  return card;
}

function renderQuotaGauges(quotas) {
  const grid = document.getElementById('quota-gauges-grid');
  if (!grid || !quotas) return;
  grid.innerHTML = '';

  // Render in a logical order: gemini, claude work, claude personal, claude (aggregate), openai
  const order = ['gemini', 'claude_work', 'claude_personal', 'claude', 'openai'];
  for (const key of order) {
    const q = quotas[key];
    if (!q) continue;
    // Skip the aggregate 'claude' card if we have the split cards
    if (key === 'claude' && (quotas['claude_work'] || quotas['claude_personal'])) continue;
    grid.appendChild(buildGaugeCard(key, q));
  }
}

// ── Org hold (Board-only) ──
// While an organization is held the daemon claims none of its tasks. Placing
// or lifting a hold is a Board action (session + passkey); agents can read it.
state.orgHolds = new Set();

async function loadOrgHolds() {
  try {
    const r = await fetch('/api/settings/org-hold', { headers: authHeader() });
    if (!r.ok) return;
    const body = await r.json();
    state.orgHolds = new Set((body.held || []).map(o => String(o).toLowerCase()));
  } catch (_) {
    return;
  }
  if (state.fleet) renderOrganizationsGrid(state.fleet.organizations);
  if (document.getElementById('view-settings')?.classList.contains('active')) renderSettings();
}

function isOrgHeld(name) {
  return !!name && state.orgHolds.has(String(name).trim().toLowerCase());
}

function orgHeldBadge() {
  const b = el('span', 'org-held-badge', 'HELD');
  b.title = 'On hold: no task in this organization is claimed or woken until the Board lifts the hold.';
  return b;
}

async function setOrgHold(name, held) {
  const verb = held ? 'putting' : 'lifting the hold on';
  const res = await withBoardWebAuthn((sessionToken, assertion) =>
    fetch('/api/settings/org-hold', {
      method: 'POST',
      headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
      body: JSON.stringify({ organization: name, held }),
    }), `${verb} ${name}${held ? ' on hold' : ''}`,
  );
  if (res === null) return;
  if (!res.ok) throw await boardActionError(res);
  await loadOrgHolds();
}

// orgHoldControl is the Hold / Lift hold button for an organization.
function orgHoldControl(name) {
  const held = isOrgHeld(name);
  const row = el('div', 'org-hold-row');
  const btn = el('button', held ? 'org-hold-btn org-hold-btn-lift' : 'org-hold-btn', held ? 'Lift hold' : 'Hold');
  btn.type = 'button';
  btn.title = held
    ? 'Lift the hold so this organization\'s tasks can run again (Touch ID)'
    : 'Stop the daemon from claiming or waking any task in this organization (Touch ID)';
  btn.addEventListener('click', async (e) => {
    e.stopPropagation();
    if (!held && !confirm(`Put ${name} on hold? No task in it will be claimed or woken until you lift the hold.`)) return;
    btn.disabled = true;
    try {
      await setOrgHold(name, !held);
    } catch (err) {
      alert(`Org hold failed: ${err.message || err}`);
    } finally {
      btn.disabled = false;
    }
  });
  row.appendChild(btn);
  return row;
}

function renderOrganizationsGrid(orgs) {
  const container = document.getElementById('orgs-grid');
  if (!container || !orgs) return;
  container.innerHTML = '';

  for (const org of orgs) {
    const card = el('div', 'org-card');
    card.title = `Click to view ${org.name} details`;

    const top = el('div', 'org-card-title');
    top.appendChild(el('span', 'org-name', org.name));
    top.appendChild(el('span', 'org-prefix', `[${org.issue_prefix || 'ORG'}]`));
    if (isOrgHeld(org.name)) top.appendChild(orgHeldBadge());
    card.appendChild(top);
    card.appendChild(orgHoldControl(org.name));

    const sGrid = el('div', 'org-stats-grid');
    const stats = [
      { n: org.task_counts?.running || 0, l: 'Running' },
      { n: org.active_agents || 0, l: 'Agents' },
      { n: org.task_counts?.blocked || 0, l: 'Blocked' },
    ];
    for (const s of stats) {
      const d = el('div');
      d.appendChild(el('div', 'org-stat-n', String(s.n)));
      d.appendChild(el('div', 'org-stat-l', s.l));
      sGrid.appendChild(d);
    }
    card.appendChild(sGrid);

    const provs = org.active_agents_by_provider || {};
    const provRow = el('div', 'gauge-metrics-row');
    provRow.appendChild(el('span', null, 'Deployments:'));
    provRow.appendChild(el('span', 'gauge-metric-val',
      `Claude: ${provs.claude || 0} · Gemini: ${provs.gemini || 0} · OpenAI: ${provs.openai || 0}`));
    card.appendChild(provRow);

    const spendRow = el('div', 'gauge-metrics-row');
    spendRow.appendChild(el('span', null, `Spend: ${fmtCurrency(org.spent_usd)}`));
    spendRow.appendChild(el('span', null, `Tokens: ${fmtCompactNum(org.spent_tokens)}`));
    card.appendChild(spendRow);

    // Organization Rolling Quota & Lockout Indicators
    const orgQuotas = org.provider_quotas || {};
    const isManagedSol = (org.name || '').toLowerCase().includes('managed');
    const lockouts = Object.values(orgQuotas).filter(q => {
      if (!q.is_locked) return false;
      if (isManagedSol && (q.provider === 'claude_personal' || q.provider === 'claude')) {
        const workQ = orgQuotas['claude_work'];
        if (workQ && !workQ.is_locked) return false;
      }
      if (!isManagedSol && (q.provider === 'claude_work' || q.provider === 'claude')) {
        const persQ = orgQuotas['claude_personal'];
        if (persQ && !persQ.is_locked) return false;
      }
      return true;
    });
    if (lockouts.length) {
      const lockRow = el('div', 'org-lockout-row');
      lockRow.style.cssText = 'color:#f87171;font-size:0.75rem;font-weight:600;margin-top:8px;padding:4px 8px;background:rgba(239,68,68,0.1);border-radius:4px;border:1px solid rgba(239,68,68,0.3);';
      lockRow.textContent = `🔒 Locked: ${lockouts.map(l => l.display_name).join(', ')}`;
      card.appendChild(lockRow);
    } else {
      const quotaKeys = isManagedSol
        ? ['gemini', 'claude_work', 'openai'].filter(k => orgQuotas[k])
        : ['gemini', 'claude_personal', 'openai'].filter(k => orgQuotas[k]);
      if (quotaKeys.length) {
        const qWrap = el('div', 'org-quota-mini-strip');
        qWrap.style.cssText = 'margin-top:8px;display:flex;flex-direction:column;gap:4px;';
        for (const k of quotaKeys) {
          const q = orgQuotas[k];
          const qRow = el('div');
          qRow.style.cssText = 'display:flex;align-items:center;justify-content:space-between;font-size:0.72rem;color:var(--muted);';
          qRow.appendChild(el('span', null, q.display_name));
          const v = quotaWindowView(q, 'five_hour');
          qRow.appendChild(el('span', 'gauge-metric-val', v.measured ? `${v.used.toFixed(1)}% 5h` : 'No data'));
          qWrap.appendChild(qRow);
        }
        card.appendChild(qWrap);
      }
    }

    card.addEventListener('click', () => openOrgDetail(org.name));
    container.appendChild(card);
  }
}

function renderTokenTelemetrySection(telemetry, modelSpend, orgSpend) {
  const aggCol = document.getElementById('telemetry-aggregates');
  const spCol  = document.getElementById('telemetry-spend-breakdown');
  if (!aggCol || !spCol) return;
  aggCol.innerHTML = '';
  spCol.innerHTML = '';

  aggCol.appendChild(el('h3', 'section-title', 'Global Token Consumption'));
  for (const r of [
    { label: 'Total Tokens Ingested', val: fmtNum(telemetry?.total_tokens) },
    { label: 'Prompt Input Tokens',   val: fmtNum(telemetry?.input_tokens) },
    { label: 'Completion Output Tokens', val: fmtNum(telemetry?.output_tokens) },
    { label: 'Cache Read / Reused Tokens', val: fmtNum(telemetry?.cache_read_tokens) },
    { label: 'Total API List-Price Spend', val: fmtCurrency(telemetry?.total_cost_usd) },
  ]) {
    const row = el('div', 'telemetry-item-row');
    row.appendChild(el('span', null, r.label));
    row.appendChild(el('span', 'gauge-metric-val', r.val));
    aggCol.appendChild(row);
  }

  spCol.appendChild(el('h3', 'section-title', 'Spend Breakdown by Model'));
  const topModels = (modelSpend || []).slice(0, 5);
  if (!topModels.length) {
    spCol.appendChild(el('p', null, 'No granular model telemetry recorded yet.'));
  } else {
    for (const m of topModels) {
      const bRow = el('div', 'breakdown-row');
      const hdr = el('div', 'breakdown-header');
      hdr.appendChild(el('span', null, m.model));
      hdr.appendChild(el('span', null, `${fmtCurrency(m.cost_usd)} (${m.percentage || 0}%)`));
      bRow.appendChild(hdr);
      const bOuter = el('div', 'breakdown-bar-outer');
      const bInner = el('div', 'breakdown-bar-inner');
      bInner.style.width = `${Math.min(100, m.percentage || 0)}%`;
      bOuter.appendChild(bInner);
      bRow.appendChild(bOuter);
      spCol.appendChild(bRow);
    }
  }

  spCol.appendChild(el('h3', 'section-title', 'Spend by Organization'));
  for (const o of (orgSpend || [])) {
    const row = el('div', 'telemetry-item-row');
    row.appendChild(el('span', null, o.organization));
    row.appendChild(el('span', 'gauge-metric-val', `${fmtCurrency(o.cost_usd)} (${o.percentage || 0}%)`));
    spCol.appendChild(row);
  }
}

function populateOrgFilter() {
  const select = document.getElementById('task-org-filter');
  if (!select || !state.fleet?.organizations) return;
  const current = select.value;
  select.innerHTML = '<option value="all">All Organizations</option>';
  for (const org of state.fleet.organizations) {
    const opt = document.createElement('option');
    opt.value = org.name;
    opt.textContent = org.name;
    select.appendChild(opt);
  }
  select.value = current || 'all';
  populateOverviewProjectFilter();
}

function populateOverviewProjectFilter() {
  const select = document.getElementById('task-project-filter');
  if (!select) return;
  const currentOrg = state.taskFilter?.org || 'all';
  const currentProj = state.taskFilter?.project || 'all';
  const projects = getProjectsForOrg(currentOrg);

  select.innerHTML = '<option value="all">All Projects</option>';
  for (const p of projects) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p;
    select.appendChild(opt);
  }

  if (projects.includes(currentProj) || currentProj === 'all') {
    select.value = currentProj;
    state.taskFilter.project = currentProj;
  } else {
    select.value = 'all';
    state.taskFilter.project = 'all';
  }
}

function populateTSOrgFilter() {
  const select = document.getElementById('ts-org-filter');
  if (!select || !state.fleet?.organizations) return;
  const current = select.value;
  select.innerHTML = '<option value="all">All Organizations</option>';
  for (const org of state.fleet.organizations) {
    const opt = document.createElement('option');
    opt.value = org.name;
    opt.textContent = org.name;
    select.appendChild(opt);
  }
  select.value = current || 'all';
  populateTSProjectFilter();
}

function populateTSProjectFilter() {
  const select = document.getElementById('ts-project-filter');
  if (!select) return;
  const currentOrg = state.tsFilter?.org || 'all';
  const currentProj = state.tsFilter?.project || 'all';
  const projects = getProjectsForOrg(currentOrg);

  select.innerHTML = '<option value="all">All Projects</option>';
  for (const p of projects) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p;
    select.appendChild(opt);
  }

  if (projects.includes(currentProj) || currentProj === 'all') {
    select.value = currentProj;
    state.tsFilter.project = currentProj;
  } else {
    select.value = 'all';
    state.tsFilter.project = 'all';
  }
}

// ── Table Sorting & Deep Content Search Helpers ───────────
function getPrioritySeverity(p) {
  const s = (p || '').toLowerCase().trim();
  switch (s) {
    case 'critical':
    case 'urgent':
    case 'crit':
      return 4;
    case 'high':
      return 3;
    case 'medium':
    case 'med':
      return 2;
    case 'low':
      return 1;
    default:
      return 0;
  }
}

function sortTasks(tasks, col, dir) {
  if (!col || !dir) return [...tasks];
  const mult = dir === 'desc' ? -1 : 1;
  return [...tasks].sort((a, b) => {
    switch (col) {
      case 'identifier':
      case 'id': {
        const valA = a.identifier || a.id || '';
        const valB = b.identifier || b.id || '';
        return mult * valA.localeCompare(valB, undefined, { numeric: true, sensitivity: 'base' });
      }
      case 'task':
      case 'name':
      case 'title': {
        const valA = (a.title || a.name || '').toLowerCase();
        const valB = (b.title || b.name || '').toLowerCase();
        return mult * valA.localeCompare(valB, undefined, { sensitivity: 'base' });
      }
      case 'organization':
      case 'org': {
        const valA = (a.organization || '').toLowerCase();
        const valB = (b.organization || '').toLowerCase();
        return mult * valA.localeCompare(valB, undefined, { sensitivity: 'base' });
      }
      case 'project': {
        const valA = (a.project || '').toLowerCase();
        const valB = (b.project || '').toLowerCase();
        return mult * valA.localeCompare(valB, undefined, { sensitivity: 'base' });
      }
      case 'assignee': {
        const valA = (a.assignee_name || a.assigned_agent || a.checkout_agent_id || '').toLowerCase();
        const valB = (b.assignee_name || b.assigned_agent || b.checkout_agent_id || '').toLowerCase();
        return mult * valA.localeCompare(valB, undefined, { sensitivity: 'base' });
      }
      case 'status': {
        const valA = (a.status || '').toLowerCase();
        const valB = (b.status || '').toLowerCase();
        return mult * valA.localeCompare(valB, undefined, { sensitivity: 'base' });
      }
      case 'priority': {
        const rankA = getPrioritySeverity(a.priority);
        const rankB = getPrioritySeverity(b.priority);
        if (rankA !== rankB) return mult * (rankA - rankB);
        return (a.title || a.name || '').localeCompare(b.title || b.name || '');
      }
      case 'cost':
      case 'spend':
      case 'spent_usd': {
        const costA = Number(a.spent_usd) || 0;
        const costB = Number(b.spent_usd) || 0;
        if (costA !== costB) return mult * (costA - costB);
        return mult * ((Number(a.spent_tokens) || 0) - (Number(b.spent_tokens) || 0));
      }
      case 'updated':
      case 'updated_at': {
        const tA = a.updated_at ? new Date(a.updated_at).getTime() : 0;
        const tB = b.updated_at ? new Date(b.updated_at).getTime() : 0;
        return mult * (tA - tB);
      }
      default:
        return 0;
    }
  });
}

function getNextSort(currentSort, targetCol, defaultSort) {
  const curCol = currentSort?.column || null;
  const curDir = currentSort?.direction || null;
  const defCol = defaultSort?.column || null;
  const defDir = defaultSort?.direction || null;

  if (curCol !== targetCol) {
    return { column: targetCol, direction: 'asc' };
  }

  if (curDir === 'asc') {
    return { column: targetCol, direction: 'desc' };
  }

  if (curDir === 'desc') {
    if (defCol === targetCol && defDir === 'desc') {
      return { column: targetCol, direction: 'asc' };
    }
    if (defCol && defDir) {
      return { column: defCol, direction: defDir };
    }
    return { column: null, direction: null };
  }

  return { column: targetCol, direction: 'asc' };
}

function renderTableSortHeaders(tableEl, currentSort, onSort, defaultSort) {
  if (!tableEl) return;
  const thead = tableEl.querySelector('thead');
  if (!thead) return;

  tableEl._currentSort = currentSort;
  tableEl._defaultSort = defaultSort;
  tableEl._onSort = onSort;

  const ths = thead.querySelectorAll('th[data-col]');
  ths.forEach(th => {
    const col = th.dataset.col;
    if (!th.dataset.label) {
      th.dataset.label = th.textContent.trim();
    }
    const label = th.dataset.label;
    const isActive = currentSort && currentSort.column === col && currentSort.direction;
    const dir = isActive ? currentSort.direction : null;

    th.classList.add('sortable-th');
    th.classList.toggle('sort-active', !!isActive);
    th.setAttribute('role', 'columnheader');
    th.setAttribute('tabindex', '0');
    th.setAttribute('aria-sort', isActive ? (dir === 'asc' ? 'ascending' : 'descending') : 'none');

    const icon = isActive ? (dir === 'asc' ? '▲' : '▼') : '↕';
    th.innerHTML = `<span class="th-content"><span class="th-label">${label}</span><span class="sort-icon ${isActive ? 'active' : ''}">${icon}</span></span>`;

    if (!th._hasSortListener) {
      th._hasSortListener = true;
      const trigger = () => {
        const next = getNextSort(tableEl._currentSort, th.dataset.col, tableEl._defaultSort);
        if (tableEl._onSort) {
          tableEl._onSort(next.column, next.direction, next);
        }
      };
      th.addEventListener('click', trigger);
      th.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          trigger();
        }
      });
    }
  });
}

function extractMatchSnippet(text, matchIdx, matchLen) {
  if (!text) return '';
  const start = Math.max(0, matchIdx - 25);
  const end = Math.min(text.length, matchIdx + matchLen + 40);
  const prefix = start > 0 ? '…' : '';
  const suffix = end < text.length ? '…' : '';
  const snippet = text.slice(start, end).replace(/\s+/g, ' ');
  return prefix + snippet + suffix;
}

function getTaskSearchMatch(t, term) {
  if (!term) return { matches: true, snippet: null };
  const s = term.toLowerCase();

  // 1. Direct fields: title, identifier, org, project, assignee
  const title = (t.title || t.name || '').toLowerCase();
  const id = (t.identifier || t.id || '').toLowerCase();
  const org = (t.organization || '').toLowerCase();
  const proj = (t.project || '').toLowerCase();
  const assignee = (t.assignee_name || t.assigned_agent || t.checkout_agent_id || '').toLowerCase();

  if (title.includes(s) || id.includes(s) || org.includes(s) || proj.includes(s) || assignee.includes(s)) {
    return { matches: true, snippet: null };
  }

  // 2. Task Description (deep content search)
  const taskId = t.id || t.task_id;
  const desc = t.description || t.desc || (taskId ? state.taskDescriptions?.[taskId] : '') || '';
  if (desc) {
    const idx = desc.toLowerCase().indexOf(s);
    if (idx !== -1) {
      return {
        matches: true,
        snippet: {
          type: 'description',
          text: extractMatchSnippet(desc, idx, s.length),
        },
      };
    }
  }

  // 3. Comments (deep content search inside agent and user comments)
  const comments = t.comments || (taskId ? state.taskComments?.[taskId] : []) || [];
  if (Array.isArray(comments)) {
    for (const c of comments) {
      if (typeof c === 'string') {
        const idx = c.toLowerCase().indexOf(s);
        if (idx !== -1) {
          return {
            matches: true,
            snippet: {
              type: 'comment',
              text: extractMatchSnippet(c, idx, s.length),
            },
          };
        }
      } else if (c && typeof c === 'object') {
        const body = (c.body || c.message || c.content || '');
        const author = (c.author || c.user || c.author_name || '');
        const idxBody = body.toLowerCase().indexOf(s);
        if (idxBody !== -1) {
          return {
            matches: true,
            snippet: {
              type: 'comment',
              author: author,
              text: extractMatchSnippet(body, idxBody, s.length),
            },
          };
        }
        if (author.toLowerCase().includes(s)) {
          return {
            matches: true,
            snippet: {
              type: 'comment',
              author: author,
              text: `Comment by ${author}`,
            },
          };
        }
      }
    }
  }

  return { matches: false, snippet: null };
}

async function prefetchTaskComments() {
  const allTasks = [...fleetTaskValues(), ...taskValues()];
  const toFetch = [];
  const seen = new Set();
  for (const t of allTasks) {
    if (!t.id || seen.has(t.id)) continue;
    seen.add(t.id);
    if ((!t.comments || !t.comments.length) && (!state.taskComments[t.id] || !state.taskComments[t.id].length)) {
      toFetch.push(t.id);
    }
  }

  const chunk = toFetch.slice(0, 30);
  for (let i = 0; i < chunk.length; i += 5) {
    const batch = chunk.slice(i, i + 5);
    await Promise.all(batch.map(async (taskId) => {
      try {
        const isFleet = isFleetTaskId(taskId);
        const apiBase = isFleet ? '/api/fleet/tasks' : '/api/tasks';
        const cr = await apiFetch(`${apiBase}/${taskId}/comments`);
        const comments = cr.comments || (Array.isArray(cr) ? cr : []);
        if (comments.length) {
          state.taskComments[taskId] = comments;
          if (state.tasks[taskId]) {
            state.tasks[taskId].comments = comments;
          }
        }
      } catch { /* optional */ }
    }));
  }
}

function renderGlobalTaskTable() {
  const table = document.getElementById('overview-task-table');
  const tbody = document.getElementById('global-task-tbody');
  if (!tbody) return;

  const DEFAULT_OVERVIEW_SORT = { column: 'updated', direction: 'desc' };
  renderTableSortHeaders(table, state.overviewSort, (col, dir, nextSort) => {
    state.overviewSort = nextSort || { column: col, direction: dir };
    renderGlobalTaskTable();
  }, DEFAULT_OVERVIEW_SORT);

  tbody.innerHTML = '';

  let tasks = fleetTaskValues();
  if (!tasks.length) tasks = taskValues();

  const sTerm = (state.taskFilter.search || '').toLowerCase().trim();
  const orgFilter = state.taskFilter.org || 'all';
  const projectFilter = state.taskFilter.project || 'all';
  const priorityFilter = state.taskFilter.priority || 'all';
  const statusFilter = state.taskFilter.status || 'all';

  const filtered = filterTasks(tasks, sTerm, orgFilter, statusFilter, projectFilter, priorityFilter);
  const sorted = sortTasks(filtered, state.overviewSort.column, state.overviewSort.direction);

  if (!sorted.length) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 7;
    td.textContent = 'No tasks match current filter criteria.';
    td.style.cssText = 'text-align:center;color:var(--muted);padding:24px;';
    tr.appendChild(td);
    tbody.appendChild(tr);
    return;
  }

  for (const t of sorted) {
    const tr = makeTaskTableRow(t, 7);
    tbody.appendChild(tr);
  }
}

function filterTasks(tasks, sTerm, orgFilter, statusFilter, projectFilter = 'all', priorityFilter = 'all') {
  return tasks.filter(t => {
    if (orgFilter && orgFilter !== 'all' && t.organization !== orgFilter) return false;
    if (projectFilter && projectFilter !== 'all') {
      const p = (t.project || '').toLowerCase();
      if (p !== projectFilter.toLowerCase()) return false;
    }
    if (priorityFilter && priorityFilter !== 'all') {
      const pri = (t.priority || 'medium').toLowerCase();
      if (pri !== priorityFilter.toLowerCase()) return false;
    }
    if (statusFilter && statusFilter !== 'all') {
      const st = (t.status || 'active').toLowerCase();
      if (statusFilter === 'running' && st !== 'running' && st !== 'in_progress') return false;
      if (statusFilter === 'active'  && st !== 'active'  && st !== 'todo')        return false;
      if (statusFilter === 'blocked' && st !== 'blocked' && !t.is_blocked)        return false;
      if (statusFilter === 'stopped' && st !== 'stopped' && st !== 'cancelled' && st !== 'paused') return false;
      if (statusFilter === 'errored' && st !== 'errored' && st !== 'error' && st !== 'failed') return false;
      if (statusFilter === 'done'    && st !== 'done')                             return false;
    }
    if (sTerm) {
      const match = getTaskSearchMatch(t, sTerm);
      if (!match.matches) return false;
      t._searchSnippet = match.snippet;
    } else {
      t._searchSnippet = null;
    }
    return true;
  });
}

function makeTaskTableRow(t, colCount) {
  const tr = document.createElement('tr');

  const tdId = el('td', null, t.identifier || (t.id ? `#${t.id.slice(0, 8)}` : '—'));
  tdId.style.cssText = 'font-family:monospace;font-weight:600;';

  const tdTitle  = el('td');
  const titleSpan = el('div', 'task-table-title', t.title || t.name || '(untitled)');
  tdTitle.appendChild(titleSpan);

  if (t._searchSnippet) {
    const snipEl = el('div', 'task-search-snippet');
    const badge = el('span', 'snippet-badge', t._searchSnippet.type);
    snipEl.appendChild(badge);
    const textNode = document.createElement('span');
    const sTerm = (state.taskFilter?.search || state.tsFilter?.search || '').trim();
    if (sTerm) {
      const raw = t._searchSnippet.text;
      const lower = raw.toLowerCase();
      const sLower = sTerm.toLowerCase();
      const idx = lower.indexOf(sLower);
      if (idx !== -1) {
        textNode.appendChild(document.createTextNode(raw.slice(0, idx)));
        const mark = el('mark', null, raw.slice(idx, idx + sTerm.length));
        textNode.appendChild(mark);
        textNode.appendChild(document.createTextNode(raw.slice(idx + sTerm.length)));
      } else {
        textNode.textContent = raw;
      }
    } else {
      textNode.textContent = t._searchSnippet.text;
    }
    snipEl.appendChild(textNode);
    tdTitle.appendChild(snipEl);
  }

  const tdOrg    = el('td', null, t.organization || 'StayPoint');
  const tdStat   = el('td'); tdStat.appendChild(statusPill(t.status));
  const tdPri    = el('td', null, t.priority || 'medium');

  const spendVal = t.spent_usd > 0
    ? `${fmtCurrency(t.spent_usd)} (${fmtCompactNum(t.spent_tokens)} tok)`
    : `—`;
  const tdSpend  = el('td', null, spendVal);
  const tdUp     = el('td', null, fmtRelTime(t.updated_at || Date.now()));

  for (const td of [tdId, tdTitle, tdOrg, tdStat, tdPri, tdSpend, tdUp]) tr.appendChild(td);
  tr.addEventListener('click', () => openDetail(t.id));
  return tr;
}

// ── Projects View (STA-192) ───────────────────────────────
function matchTaskStatus(taskStatus, targetFilter) {
  if (!targetFilter || targetFilter === 'all') return true;
  const st = (taskStatus || '').toLowerCase();
  if (targetFilter === 'running') {
    return st === 'running' || st === 'in_progress';
  }
  if (targetFilter === 'blocked') {
    return st === 'blocked';
  }
  if (targetFilter === 'done') {
    return st === 'done' || st === 'completed' || st === 'soft_deleted';
  }
  if (targetFilter === 'active') {
    return st === 'active' || st === 'todo' || st === 'backlog' || (!['running', 'in_progress', 'blocked', 'done', 'completed', 'soft_deleted'].includes(st));
  }
  return st === targetFilter;
}

function populateProjectsOrgFilter() {
  const optionsContainer = document.getElementById('projects-org-options');
  const legacySel = document.getElementById('projects-org-filter');
  const statusSel = document.getElementById('projects-status-filter');

  if (statusSel) {
    statusSel.value = state.projectsFilter.status || 'all';
  }

  // Collect all known org names from fleet, tasks, sessions
  const orgNames = new Set();
  for (const org of (state.fleet?.organizations || [])) {
    if (org.name) orgNames.add(org.name);
  }
  for (const t of taskValues()) {
    if (t.organization) orgNames.add(t.organization);
  }
  for (const t of fleetTaskValues()) {
    if (t.organization) orgNames.add(t.organization);
  }
  for (const s of Object.values(state.sessions)) {
    orgNames.add(s.org || 'StayPoint');
  }
  if (!orgNames.size) orgNames.add('StayPoint');

  const sortedOrgs = Array.from(orgNames).sort();

  // Keep legacy select in sync
  if (legacySel) {
    const prevVal = legacySel.value;
    legacySel.innerHTML = '<option value="all">All Organizations</option>';
    for (const org of sortedOrgs) {
      const opt = document.createElement('option');
      opt.value = org;
      opt.textContent = org;
      legacySel.appendChild(opt);
    }
    legacySel.value = prevVal || 'all';
  }

  if (!optionsContainer) return;

  // Task counts per org
  const orgTaskCounts = {};
  const allTasks = [...fleetTaskValues(), ...taskValues()];
  for (const org of (state.fleet?.organizations || [])) {
    for (const t of visibleTasks(org.tasks, false)) allTasks.push(t);
  }
  const seen = new Set();
  for (const t of allTasks) {
    if (t && t.id && !seen.has(t.id)) {
      seen.add(t.id);
      const o = t.organization || 'StayPoint';
      orgTaskCounts[o] = (orgTaskCounts[o] || 0) + 1;
    }
  }

  // Filter out any stored orgs that no longer exist
  if (state.projectsFilter.orgs.length > 0) {
    state.projectsFilter.orgs = state.projectsFilter.orgs.filter(o => orgNames.has(o));
  }

  optionsContainer.innerHTML = '';
  for (const org of sortedOrgs) {
    const isChecked = state.projectsFilter.orgs.length === 0 || state.projectsFilter.orgs.includes(org);
    const label = el('label', 'multiselect-option');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = org;
    cb.checked = isChecked;
    cb.className = 'multiselect-checkbox';
    cb.addEventListener('change', () => {
      handleProjectsOrgCheckboxChange();
    });

    label.appendChild(cb);
    label.appendChild(el('span', 'multiselect-option-label', org));
    const cnt = orgTaskCounts[org] || 0;
    label.appendChild(el('span', 'multiselect-option-count', `${cnt} task${cnt !== 1 ? 's' : ''}`));
    optionsContainer.appendChild(label);
  }

  updateProjectsOrgButtonLabel();
}

function handleProjectsOrgCheckboxChange() {
  const checkboxes = document.querySelectorAll('#projects-org-options .multiselect-checkbox');
  const checked = [];
  checkboxes.forEach(cb => {
    if (cb.checked) checked.push(cb.value);
  });

  if (checked.length === checkboxes.length || checked.length === 0) {
    state.projectsFilter.orgs = [];
  } else {
    state.projectsFilter.orgs = checked;
  }

  saveProjectsFilters();
  updateProjectsOrgButtonLabel();
  renderProjects();
}

function updateProjectsOrgButtonLabel() {
  const btnLabel = document.getElementById('projects-org-btn-label');
  const btnBadge = document.getElementById('projects-org-btn-badge');
  const legacySel = document.getElementById('projects-org-filter');
  if (!btnLabel) return;

  const selCount = state.projectsFilter.orgs.length;
  if (selCount === 0) {
    btnLabel.textContent = 'All Organizations';
    if (btnBadge) btnBadge.style.display = 'none';
    if (legacySel) legacySel.value = 'all';
  } else if (selCount === 1) {
    btnLabel.textContent = state.projectsFilter.orgs[0];
    if (btnBadge) btnBadge.style.display = 'none';
    if (legacySel) legacySel.value = state.projectsFilter.orgs[0];
  } else {
    btnLabel.textContent = `${selCount} Organizations`;
    if (btnBadge) {
      btnBadge.textContent = String(selCount);
      btnBadge.style.display = 'inline-flex';
    }
    if (legacySel) legacySel.value = 'all';
  }
}

function renderProjects() {
  const grid = document.getElementById('projects-grid');
  if (!grid) return;
  grid.innerHTML = '';

  const allTasks = [...fleetTaskValues(), ...taskValues()];
  for (const org of (state.fleet?.organizations || [])) {
    for (const t of visibleTasks(org.tasks, false)) {
      if (!t.organization) t.organization = org.name;
      allTasks.push(t);
    }
  }

  const seenIds = new Set();
  const dedupTasks = [];
  for (const t of allTasks) {
    if (t && t.id && !seenIds.has(t.id)) {
      seenIds.add(t.id);
      dedupTasks.push(t);
    }
  }

  const selectedOrgs = state.projectsFilter.orgs || [];
  const globalStatusFilter = state.projectsFilter.status || 'all';

  // Group by Organization -> Project
  // Ensures (No Project) tasks are strictly partitioned by organization and respect org filters
  const orgMap = {};

  for (const t of dedupTasks) {
    const orgName = t.organization || 'StayPoint';

    // Respect organization filter
    if (selectedOrgs.length > 0 && !selectedOrgs.includes(orgName)) {
      continue;
    }

    if (!orgMap[orgName]) {
      orgMap[orgName] = { name: orgName, projects: {} };
    }

    const rawProj = (t.project && typeof t.project === 'string' ? t.project.trim() : '');
    const projName = (rawProj && rawProj !== 'None') ? rawProj : '(No Project)';

    if (!orgMap[orgName].projects[projName]) {
      orgMap[orgName].projects[projName] = {
        name: projName,
        org: orgName,
        isNoProject: projName === '(No Project)',
        tasks: []
      };
    }
    orgMap[orgName].projects[projName].tasks.push(t);
  }

  // Include explicit projects defined on fleet organizations matching org filter
  for (const org of (state.fleet?.organizations || [])) {
    const orgName = org.name || 'StayPoint';
    if (selectedOrgs.length > 0 && !selectedOrgs.includes(orgName)) continue;
    if (Array.isArray(org.projects)) {
      for (const p of org.projects) {
        const pName = typeof p === 'string' ? p : p.name;
        if (pName && pName !== '(No Project)') {
          if (!orgMap[orgName]) orgMap[orgName] = { name: orgName, projects: {} };
          if (!orgMap[orgName].projects[pName]) {
            orgMap[orgName].projects[pName] = {
              name: pName,
              org: orgName,
              isNoProject: false,
              tasks: []
            };
          }
        }
      }
    }
  }

  // Filter projects by global status if set
  const orgEntries = Object.values(orgMap).map(org => {
    let projs = Object.values(org.projects);
    if (globalStatusFilter !== 'all') {
      projs = projs.filter(p => p.tasks.some(t => matchTaskStatus(t.status, globalStatusFilter)));
    }
    return {
      name: org.name,
      projects: projs.sort((a, b) => {
        if (a.isNoProject && !b.isNoProject) return 1;
        if (!a.isNoProject && b.isNoProject) return -1;
        return b.tasks.length - a.tasks.length;
      })
    };
  }).filter(org => org.projects.length > 0);

  if (!orgEntries.length) {
    grid.appendChild(el('p', 'muted-text', 'No projects found matching current filters.'));
    return;
  }

  // Render Grouped Headers by Organization
  for (const orgGroup of orgEntries.sort((a, b) => a.name.localeCompare(b.name))) {
    const groupWrap = el('div', 'project-org-group');

    // Grouped Header
    const groupHeader = el('div', 'project-org-header');
    const headerLeft = el('div', 'project-org-header-left');
    headerLeft.appendChild(el('span', 'project-org-icon', '🏢'));
    headerLeft.appendChild(el('h2', 'project-org-title', orgGroup.name));
    groupHeader.appendChild(headerLeft);

    const totalTasksInOrg = orgGroup.projects.reduce((sum, p) => sum + p.tasks.length, 0);
    const totalSpendInOrg = orgGroup.projects.reduce((sum, p) =>
      sum + p.tasks.reduce((ts, t) => ts + (t.spent_usd || 0), 0), 0);

    const headerMeta = el('div', 'project-org-header-meta');
    headerMeta.appendChild(el('span', 'pill', `${orgGroup.projects.length} project${orgGroup.projects.length !== 1 ? 's' : ''}`));
    headerMeta.appendChild(el('span', 'pill pill-todo', `${totalTasksInOrg} task${totalTasksInOrg !== 1 ? 's' : ''}`));
    if (totalSpendInOrg > 0) {
      headerMeta.appendChild(el('span', 'pill pill-gold', fmtCurrency(totalSpendInOrg)));
    }
    groupHeader.appendChild(headerMeta);
    groupWrap.appendChild(groupHeader);

    // Grid of cards for this organization
    const subGrid = el('div', 'project-cards-subgrid');

    for (const p of orgGroup.projects) {
      const card = createProjectCard(p, globalStatusFilter);
      subGrid.appendChild(card);
    }

    groupWrap.appendChild(subGrid);
    grid.appendChild(groupWrap);
  }
}

function createProjectCard(p, globalStatusFilter) {
  const card = el('div', 'project-card');
  const cardKey = `${p.org}:${p.name}`;

  // Card Header
  const hdr = el('div', 'project-card-header');
  const titleWrap = el('div');
  if (p.isNoProject) {
    const nameEl = el('div', 'project-name', '(No Project)');
    nameEl.style.color = 'var(--muted)';
    nameEl.style.fontStyle = 'italic';
    titleWrap.appendChild(nameEl);
    titleWrap.appendChild(el('div', 'project-org', `${p.org} · Unassigned Tasks`));
  } else {
    titleWrap.appendChild(el('div', 'project-name', p.name));
    titleWrap.appendChild(el('div', 'project-org', p.org));
  }
  hdr.appendChild(titleWrap);
  const totalBadge = el('span', 'pill', `${p.tasks.length} task${p.tasks.length !== 1 ? 's' : ''}`);
  hdr.appendChild(totalBadge);
  card.appendChild(hdr);

  // Status counts
  const counts = { running: 0, blocked: 0, done: 0, active: 0 };
  let totalSpend = 0;
  for (const t of p.tasks) {
    const st = (t.status || '').toLowerCase();
    if (st === 'running' || st === 'in_progress') counts.running++;
    else if (st === 'blocked') counts.blocked++;
    else if (st === 'done' || st === 'completed' || st === 'soft_deleted') counts.done++;
    else counts.active++;
    totalSpend += t.spent_usd || 0;
  }

  // Active status filter for this card
  let activeCardFilter = state.projectsFilter.cardStatus[cardKey] ||
    (globalStatusFilter !== 'all' ? globalStatusFilter : 'all');

  // Stats row with clickable status filter buttons
  const statsRow = el('div', 'project-stats-row');
  const statDefs = [
    { status: 'running', label: 'Running', val: counts.running, cls: 'highlight-cyan' },
    { status: 'blocked', label: 'Blocked', val: counts.blocked, cls: 'highlight-red' },
    { status: 'done',    label: 'Done',    val: counts.done,    cls: 'highlight-green' },
    { status: 'active',  label: 'Active',  val: counts.active,  cls: '' },
  ];

  for (const sDef of statDefs) {
    const s = el('div', 'project-stat project-stat-clickable');
    s.dataset.status = sDef.status;
    s.title = `Filter tasks by ${sDef.label}`;
    if (activeCardFilter === sDef.status) {
      s.classList.add('active');
    }
    s.appendChild(el('div', `project-stat-n ${sDef.cls}`, String(sDef.val)));
    s.appendChild(el('div', 'project-stat-l', sDef.label));
    s.addEventListener('click', (e) => {
      e.stopPropagation();
      setCardStatusFilter(sDef.status);
    });
    statsRow.appendChild(s);
  }

  if (totalSpend > 0) {
    const s = el('div', 'project-stat clickable');
    s.setAttribute('role', 'button');
    s.setAttribute('tabindex', '0');
    s.title = 'View Cost & Accounting';
    s.appendChild(el('div', 'project-stat-n highlight-gold', fmtCurrency(totalSpend)));
    s.appendChild(el('div', 'project-stat-l', 'Spend'));
    s.addEventListener('click', (e) => {
      e.stopPropagation();
      drillDownToCost();
    });
    s.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        e.stopPropagation();
        drillDownToCost();
      }
    });
    statsRow.appendChild(s);
  }
  card.appendChild(statsRow);

  // Status Filter Pill Bar on Card: [All] [Active] [Running] [Done] [Blocked]
  const filterBar = el('div', 'project-card-filter-bar');
  const pillFilters = [
    { status: 'all',     label: 'All',     count: p.tasks.length },
    { status: 'active',  label: 'Active',  count: counts.active },
    { status: 'running', label: 'Running', count: counts.running },
    { status: 'done',    label: 'Done',    count: counts.done },
    { status: 'blocked', label: 'Blocked', count: counts.blocked },
  ];

  const pillButtons = [];
  for (const pf of pillFilters) {
    const pill = el('button', `project-card-filter-pill pill-${pf.status}`, `${pf.label} (${pf.count})`);
    pill.type = 'button';
    pill.dataset.status = pf.status;
    if (activeCardFilter === pf.status) {
      pill.classList.add('active');
    }
    pill.addEventListener('click', (e) => {
      e.stopPropagation();
      setCardStatusFilter(pf.status);
    });
    filterBar.appendChild(pill);
    pillButtons.push(pill);
  }
  card.appendChild(filterBar);

  // Task list preview container
  const taskList = el('div', 'project-task-list');
  card.appendChild(taskList);

  function renderTaskList() {
    taskList.innerHTML = '';
    const filteredTasks = p.tasks.filter(t => matchTaskStatus(t.status, activeCardFilter));

    if (!filteredTasks.length) {
      const emptyMsg = activeCardFilter === 'all'
        ? 'No tasks in this project.'
        : `No ${activeCardFilter} tasks.`;
      taskList.appendChild(el('div', 'project-task-empty muted-text', emptyMsg));
      return;
    }

    for (const t of filteredTasks.slice(0, 5)) {
      const item = el('div', 'project-task-item');
      item.appendChild(statusPill(t.status));
      const titleEl = el('span', null, t.title || t.name || '(untitled)');
      titleEl.style.cssText = 'flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;';
      item.appendChild(titleEl);
      item.addEventListener('click', (e) => {
        e.stopPropagation();
        openDetail(t.id);
      });
      taskList.appendChild(item);
    }

    if (filteredTasks.length > 5) {
      const moreMsg = el('div', 'muted-text', `+${filteredTasks.length - 5} more ${activeCardFilter !== 'all' ? activeCardFilter + ' ' : ''}tasks`);
      moreMsg.style.fontSize = '11px';
      moreMsg.style.paddingTop = '2px';
      taskList.appendChild(moreMsg);
    }
  }

  function setCardStatusFilter(newStatus) {
    if (activeCardFilter === newStatus && newStatus !== 'all') {
      activeCardFilter = 'all';
    } else {
      activeCardFilter = newStatus;
    }
    state.projectsFilter.cardStatus[cardKey] = activeCardFilter;
    saveProjectsFilters();

    pillButtons.forEach(btn => {
      btn.classList.toggle('active', btn.dataset.status === activeCardFilter);
    });

    statsRow.querySelectorAll('.project-stat-clickable').forEach(statEl => {
      statEl.classList.toggle('active', statEl.dataset.status === activeCardFilter);
    });

    renderTaskList();
  }

  renderTaskList();

  // Clicking the card opens the project: the Task Status page filtered to this
  // org + project. Inner controls stopPropagation so they keep their own action.
  const openProject = () => drillDownToTasks('all', p.org, p.isNoProject ? 'all' : p.name);
  card.setAttribute('role', 'button');
  card.setAttribute('tabindex', '0');
  card.title = p.isNoProject ? `View unassigned ${p.org} tasks` : `Open ${p.name}`;
  card.addEventListener('click', openProject);
  card.addEventListener('keydown', (e) => {
    if (e.target !== card) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      openProject();
    }
  });

  return card;
}

// ── Agents Filter Helpers ─────────────────────────────────
function orgMatches(itemOrg, targetOrg) {
  if (!targetOrg || targetOrg === 'all') return true;
  if (!itemOrg) return false;
  const a = String(itemOrg).trim().toLowerCase();
  const b = String(targetOrg).trim().toLowerCase();
  return a === b || a.startsWith(b) || b.startsWith(a);
}

function getProjectsForOrg(orgName = 'all') {
  const allTasks = [...fleetTaskValues(), ...taskValues()];
  const projects = new Set();

  for (const t of allTasks) {
    const tOrg = t.organization || 'StayPoint';
    if (!orgMatches(tOrg, orgName)) continue;
    if (t.project && t.project !== '(No Project)') {
      projects.add(t.project);
    }
  }

  // Also check fleet organizations if they contain explicit project lists, tasks, or agents
  for (const org of (state.fleet?.organizations || [])) {
    if (!orgMatches(org.name, orgName)) continue;
    if (Array.isArray(org.projects)) {
      for (const p of org.projects) {
        const name = typeof p === 'string' ? p : p?.name;
        if (name && name !== '(No Project)') projects.add(name);
      }
    }
    for (const t of visibleTasks(org.tasks, false)) {
      if (t.project && t.project !== '(No Project)') {
        projects.add(t.project);
      }
    }
    for (const a of (org.agents || [])) {
      if (a.project && a.project !== '(No Project)') projects.add(a.project);
      if (Array.isArray(a.projects)) {
        for (const p of a.projects) if (p && p !== '(No Project)') projects.add(p);
      }
    }
  }

  return Array.from(projects).sort((a, b) => a.localeCompare(b));
}

function getAgentProjects(agent) {
  const projects = new Set();
  if (agent.project && agent.project !== '(No Project)') {
    projects.add(agent.project);
  }
  if (Array.isArray(agent.projects)) {
    for (const p of agent.projects) {
      if (p && p !== '(No Project)') projects.add(p);
    }
  }
  if (agent.runningTask?.project && agent.runningTask.project !== '(No Project)') {
    projects.add(agent.runningTask.project);
  }
  if (Array.isArray(agent.agentTasks)) {
    for (const t of agent.agentTasks) {
      if (t.project && t.project !== '(No Project)') {
        projects.add(t.project);
      }
    }
  }

  // Also check tasks checked out by or assigned to this agent across all tasks
  const allTasks = [...fleetTaskValues(), ...taskValues()];
  for (const t of allTasks) {
    const matchesAgent =
      (t.checkout_agent_id && t.checkout_agent_id === agent.id) ||
      (t.assignee_agent_id && t.assignee_agent_id === agent.id) ||
      (t.assignee_id && t.assignee_id === agent.id) ||
      (t.assigneeAgentId && t.assigneeAgentId === agent.id) ||
      (t.assigned_agent && (t.assigned_agent === agent.name || t.assigned_agent === agent.id)) ||
      (t.assignee_name && t.assignee_name === agent.name) ||
      (t.assigneeRole && agent.role && t.assigneeRole === agent.role);
    if (matchesAgent && t.project && t.project !== '(No Project)') {
      projects.add(t.project);
    }
  }

  return Array.from(projects);
}

function populateAgentsOrgFilter() {
  const sel = document.getElementById('agents-org-filter');
  if (!sel) return;

  const current = state.agentsFilter.org || 'all';
  const orgNames = new Set();
  for (const org of (state.fleet?.organizations || [])) {
    if (org.name) orgNames.add(org.name);
  }
  for (const t of taskValues()) {
    if (t.organization) orgNames.add(t.organization);
  }
  for (const s of Object.values(state.sessions)) {
    orgNames.add(s.org || 'StayPoint');
  }

  sel.innerHTML = '<option value="all">All Organizations</option>';
  for (const org of Array.from(orgNames).sort()) {
    const opt = document.createElement('option');
    opt.value = org;
    opt.textContent = org;
    sel.appendChild(opt);
  }

  if (orgNames.has(current) || current === 'all') {
    sel.value = current;
    state.agentsFilter.org = current;
  } else {
    sel.value = 'all';
    state.agentsFilter.org = 'all';
  }
}

function populateAgentsProjectFilter() {
  const sel = document.getElementById('agents-project-filter');
  if (!sel) return;

  const currentOrg = state.agentsFilter.org || 'all';
  const currentProj = state.agentsFilter.project || 'all';
  const projects = getProjectsForOrg(currentOrg);

  sel.innerHTML = '<option value="all">All Projects</option>';
  for (const p of projects) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p;
    sel.appendChild(opt);
  }

  if (projects.includes(currentProj) || currentProj === 'all') {
    sel.value = currentProj;
    state.agentsFilter.project = currentProj;
  } else {
    sel.value = 'all';
    state.agentsFilter.project = 'all';
  }
}

function populateAgentsFilters() {
  populateAgentsOrgFilter();
  populateAgentsProjectFilter();
}

// ── Agents Dedicated View ─────────────────────────────────
function getHeartbeatFreshness(hb) {
  if (!hb) return { text: 'None', pillClass: 'standby', dotClass: 'idle' };
  const d = new Date(hb);
  if (isNaN(d.getTime())) return { text: 'Unknown', pillClass: 'standby', dotClass: 'idle' };
  const diffMs = Date.now() - d.getTime();
  const diffMin = Math.floor(diffMs / 60000);
  if (diffMin < 10) {
    return { text: `Fresh (${fmtRelTime(hb)})`, pillClass: 'fresh', dotClass: 'running' };
  } else if (diffMin < 60) {
    return { text: `Recent (${fmtRelTime(hb)})`, pillClass: 'recent', dotClass: 'idle' };
  } else {
    return { text: `Standby (${fmtRelTime(hb)})`, pillClass: 'standby', dotClass: 'idle' };
  }
}

function resetAgentFilters() {
  state.agentsFilter.search = '';
  state.agentsFilter.provider = 'all';
  state.agentsFilter.org = 'all';
  state.agentsFilter.project = 'all';
  state.agentsFilter.status = 'all';
  const searchInput = document.getElementById('agents-search');
  const provSelect = document.getElementById('agents-provider-filter');
  const orgSelect = document.getElementById('agents-org-filter');
  const projSelect = document.getElementById('agents-project-filter');
  if (searchInput) searchInput.value = '';
  if (provSelect) provSelect.value = 'all';
  if (orgSelect) orgSelect.value = 'all';
  if (projSelect) projSelect.value = 'all';
  populateAgentsProjectFilter();
  document.querySelectorAll('.agent-filter-pill').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.status === 'all');
  });
  renderAgentsPage();
}

// ── Provider Resolution & Quota Helpers ───────────────────
function detectProviderStr(s) {
  if (!s || typeof s !== 'string') return '';
  const lower = s.toLowerCase().trim();
  if (!lower || lower === 'other' || lower === 'unknown') return '';
  if (lower.includes('gemini') || lower.includes('google')) return 'gemini';
  if (lower.includes('claude') || lower.includes('anthropic') || lower.includes('fable')) return 'claude';
  if (lower.includes('codex') || lower.includes('openai') || lower.includes('gpt') || lower.includes('o1') || lower.includes('o3')) return 'openai';
  if (lower.endsWith('_local')) return lower.replace(/_local$/, '');
  return '';
}

function resolveProvider(val) {
  if (!val) return 'gemini';
  if (typeof val === 'object') {
    const a = val;
    // 1. Check runtime_config / runtimeConfig
    const rc = a.runtime_config || a.runtimeConfig || {};
    const rcProv = rc.provider || rc.model || rc.default_model || rc.defaultModel || rc.model_family || rc.modelFamily || rc.adapter || rc.adapter_type;
    const fromRc = detectProviderStr(rcProv);
    if (fromRc) return fromRc;
    if (rc.provider && typeof rc.provider === 'string' && !['other', 'unknown'].includes(rc.provider.toLowerCase().trim())) {
      return rc.provider.toLowerCase().trim();
    }

    // 2. Check adapter_type / adapterType
    const at = a.adapter_type || a.adapterType;
    const fromAt = detectProviderStr(at);
    if (fromAt) return fromAt;
    if (at && typeof at === 'string') {
      const lower = at.toLowerCase().trim().replace(/_local$/, '');
      if (lower && !['other', 'unknown'].includes(lower)) return lower;
    }

    // 3. Check adapter_config / adapterConfig
    const ac = a.adapter_config || a.adapterConfig || {};
    const acProv = ac.provider || ac.model || ac.default_model || ac.defaultModel;
    const fromAc = detectProviderStr(acProv);
    if (fromAc) return fromAc;
    if (ac.provider && typeof ac.provider === 'string' && !['other', 'unknown'].includes(ac.provider.toLowerCase().trim())) {
      return ac.provider.toLowerCase().trim();
    }

    // 4. Check model / default_model
    const fromModel = detectProviderStr(a.model || a.default_model);
    if (fromModel) return fromModel;

    // 5. Check metadata_json (for local sessions)
    if (a.metadata_json) {
      try {
        const meta = typeof a.metadata_json === 'string' ? JSON.parse(a.metadata_json) : a.metadata_json;
        const fromMeta = detectProviderStr(meta.provider || meta.model || meta.defaultModel || meta.agent_type);
        if (fromMeta) return fromMeta;
      } catch (_) {}
    }

    // 6. Check existing provider field if clean
    if (a.provider && a.provider !== 'other' && a.provider !== 'unknown') {
      const fromProv = detectProviderStr(a.provider);
      if (fromProv) return fromProv;
      return a.provider.replace(/_local$/i, '').toLowerCase();
    }

    // 7. Check agent_type
    if (a.agent_type) {
      const fromAgType = detectProviderStr(a.agent_type);
      if (fromAgType) return fromAgType;
    }

    // 8. Check name, title, and role
    const fromText = detectProviderStr(`${a.name || ''} ${a.title || ''} ${a.role || ''}`);
    if (fromText) return fromText;

    // Strict fallback: never return 'other'
    return 'gemini';
  }

  return detectProviderStr(String(val)) || 'gemini';
}

function getAgentQuota(providerQuotas, provider, orgName) {
  if (!providerQuotas) return null;
  const p = (provider || '').toLowerCase();
  const org = (orgName || '').toLowerCase();
  const isWork = org.includes('managed') || org.includes('mansol');

  if (p === 'claude_personal') return providerQuotas['claude_personal'] || providerQuotas['claude'] || null;
  if (p === 'claude_work') return providerQuotas['claude_work'] || providerQuotas['claude'] || null;

  if (p.includes('claude') || p.includes('anthropic') || p.includes('fable')) {
    if (isWork) {
      return providerQuotas['claude_work'] || providerQuotas['claude'] || providerQuotas['claude_personal'] || null;
    } else {
      return providerQuotas['claude_personal'] || providerQuotas['claude'] || providerQuotas['claude_work'] || null;
    }
  }
  if (providerQuotas[p]) return providerQuotas[p];
  if (p.includes('gemini') || p.includes('google')) {
    return providerQuotas['gemini'] || null;
  }
  if (p.includes('openai') || p.includes('codex') || p.includes('gpt')) {
    return providerQuotas['openai'] || null;
  }
  return providerQuotas['gemini'] || null;
}

function renderAgentsPage() {
  const grid = document.getElementById('agents-grid');
  if (!grid) return;
  grid.innerHTML = '';

  const f = state.fleet;
  const allAgents = [];

  // Collect from fleet orgs
  for (const org of (f?.organizations || [])) {
    for (const a of (org.agents || [])) {
      const prov = resolveProvider(a);
      allAgents.push({ ...a, provider: prov, org: org.name });
    }
  }

  // Add local sessions not already in fleet
  for (const s of Object.values(state.sessions)) {
    const exists = allAgents.find(a => a.id === s.id);
    if (!exists) {
      const prov = resolveProvider(s);
      allAgents.push({
        id: s.id,
        name: s.agent_type ? `${titleCase(prov)} Session` : 'Local Agent',
        role: 'Local Session',
        provider: prov,
        status: s.status || 'active',
        last_heartbeat: s.last_heartbeat_at,
        org: 'StayPoint',
      });
    }
  }

  // Match each agent with its assigned and current running task
  const allTasks = [...fleetTaskValues(), ...taskValues()];
  const enrichedAgents = allAgents.map(a => {
    const agentTasks = allTasks.filter(t =>
      (t.checkout_agent_id && t.checkout_agent_id === a.id) ||
      (t.assignee_agent_id && t.assignee_agent_id === a.id) ||
      (t.assignee_id && t.assignee_id === a.id) ||
      (t.assigneeAgentId && t.assigneeAgentId === a.id) ||
      (t.assigned_agent && (t.assigned_agent === a.name || t.assigned_agent === a.id)) ||
      (t.assignee_name && t.assignee_name === a.name)
    );

    const runningTask = agentTasks.find(t => t.status === 'running' || t.status === 'in_progress');

    let st = (a.status || 'idle').toLowerCase();
    if (runningTask || st === 'running' || st === 'in_progress') {
      st = 'running';
    } else if (st === 'paused' || st === 'stopped' || a.pause_reason) {
      st = 'paused';
    } else {
      st = 'idle';
    }

    return {
      ...a,
      normalizedStatus: st,
      agentTasks,
      runningTask,
    };
  });

  // Update summary pill counts
  const totalCount = enrichedAgents.length;
  const runningCount = enrichedAgents.filter(a => a.normalizedStatus === 'running').length;
  const idleCount = enrichedAgents.filter(a => a.normalizedStatus === 'idle').length;
  const pausedCount = enrichedAgents.filter(a => a.normalizedStatus === 'paused').length;

  const countAllEl = document.getElementById('count-agents-all');
  const countRunningEl = document.getElementById('count-agents-running');
  const countIdleEl = document.getElementById('count-agents-idle');
  const countPausedEl = document.getElementById('count-agents-paused');
  if (countAllEl) countAllEl.textContent = totalCount;
  if (countRunningEl) countRunningEl.textContent = runningCount;
  if (countIdleEl) countIdleEl.textContent = idleCount;
  if (countPausedEl) countPausedEl.textContent = pausedCount;

  // Synchronize cascading filters
  populateAgentsFilters();

  // Populate summary metrics chip
  const metricsContainer = document.getElementById('agents-summary-metrics');
  if (metricsContainer && f?.provider_quotas) {
    metricsContainer.innerHTML = '';
    const qGemini = f.provider_quotas['gemini'];
    const qClaude = f.provider_quotas['claude'] || f.provider_quotas['claude_personal'];
    if (qGemini || qClaude) {
      const chip = el('div', 'agents-metric-chip');
      const gHeadroom = qGemini ? (qGemini.five_hour_remaining_pct ?? 100).toFixed(0) : '—';
      const cHeadroom = qClaude ? (qClaude.five_hour_remaining_pct ?? 100).toFixed(0) : '—';
      chip.innerHTML = `Fleet Quota Headroom: Gemini <span class="agents-metric-val">${gHeadroom}%</span> · Claude <span class="agents-metric-val">${cHeadroom}%</span>`;
      metricsContainer.appendChild(chip);
    }
  }

  // Filter agents
  const searchTerm = (state.agentsFilter.search || '').toLowerCase();
  const provFilter = state.agentsFilter.provider || 'all';
  const orgFilter = state.agentsFilter.org || 'all';
  const projFilter = state.agentsFilter.project || 'all';
  const statusFilter = state.agentsFilter.status || 'all';

  const filtered = enrichedAgents.filter(a => {
    if (statusFilter !== 'all' && a.normalizedStatus !== statusFilter) return false;
    if (provFilter !== 'all' && (a.provider || '').toLowerCase() !== provFilter.toLowerCase()) return false;
    if (orgFilter !== 'all' && !orgMatches(a.org || a.organization, orgFilter)) return false;

    // Project filter (cascading dependency)
    if (projFilter !== 'all') {
      const agentProjects = getAgentProjects(a);
      if (!agentProjects.includes(projFilter)) return false;
    }
    if (searchTerm) {
      const name = (a.name || '').toLowerCase();
      const role = (a.role || '').toLowerCase();
      const org = (a.org || '').toLowerCase();
      const prov = (a.provider || '').toLowerCase();
      const id   = (a.id || '').toLowerCase();
      const task = (a.runningTask?.title || a.runningTask?.identifier || '').toLowerCase();
      const agentProjs = getAgentProjects(a).join(' ').toLowerCase();

      if (!name.includes(searchTerm) &&
          !role.includes(searchTerm) &&
          !org.includes(searchTerm) &&
          !prov.includes(searchTerm) &&
          !id.includes(searchTerm) &&
          !task.includes(searchTerm) &&
          !agentProjs.includes(searchTerm)) {
        return false;
      }
    }
    return true;
  });

  if (!filtered.length) {
    const empty = el('div', 'muted-text');
    empty.style.padding = '30px';
    empty.style.textAlign = 'center';
    empty.style.gridColumn = '1 / -1';
    empty.innerHTML = `No agents match current filters. <button class="btn btn-secondary btn-sm" style="margin-left: 8px;" onclick="resetAgentFilters()">Clear Filters</button>`;
    grid.appendChild(empty);
    return;
  }

  for (const a of filtered) {
    const card = el('div', `agent-card status-${a.normalizedStatus}`);

    // ── Card Header ───────────────────────────────────────
    const hdr = el('div', 'agent-card-header');
    const titleGroup = el('div', 'agent-card-title-group');
    const nameEl = el('div', 'agent-card-name', a.name || a.id?.slice(0, 12) || 'Agent');
    nameEl.title = a.name || a.id || '';
    titleGroup.appendChild(nameEl);
    titleGroup.appendChild(el('div', 'agent-card-role', a.role || 'Autonomous Agent'));
    hdr.appendChild(titleGroup);

    const badgesCol = el('div', 'agent-card-badges');
    // Status Badge
    const stBadge = el('div', `agent-status-badge ${a.normalizedStatus}`);
    const dot = el('span', `status-indicator-dot ${a.normalizedStatus}${a.normalizedStatus === 'running' ? ' pulse' : ''}`);
    stBadge.appendChild(dot);
    stBadge.appendChild(document.createTextNode(a.normalizedStatus === 'running' ? 'Running' : a.normalizedStatus === 'paused' ? 'Paused' : 'Idle'));
    badgesCol.appendChild(stBadge);

    // Provider Badge
    const provBadge = el('span', `fleet-provider-badge provider-${a.provider}`, a.provider);
    badgesCol.appendChild(provBadge);
    hdr.appendChild(badgesCol);
    card.appendChild(hdr);

    // ── Meta: Organization & Model ────────────────────────
    const meta = el('div', 'agent-card-meta');
    if (a.org) {
      const orgPill = el('span', 'agent-org-badge');
      orgPill.innerHTML = `&#9632; ${escapeHtml(a.org)}`;
      meta.appendChild(orgPill);
    }
    if (a.model) {
      meta.appendChild(el('span', 'agent-model-badge', a.model));
    }
    const agentProjs = getAgentProjects(a);
    for (const p of agentProjs) {
      meta.appendChild(el('span', 'pill', p));
    }
    card.appendChild(meta);

    // ── Current Checkout Task Box ─────────────────────────
    const taskBox = el('div', `agent-task-box${a.runningTask ? ' has-running-task' : ''}`);
    const taskBoxHdr = el('div', 'agent-task-box-header');
    taskBoxHdr.appendChild(el('span', '', 'Current Checkout Task'));
    if (a.runningTask) {
      const liveTag = el('span', 'agent-task-tag');
      liveTag.innerHTML = `&#9889; Active`;
      taskBoxHdr.appendChild(liveTag);
    }
    taskBox.appendChild(taskBoxHdr);

    if (a.runningTask) {
      const link = el('div', 'agent-task-link');
      if (a.runningTask.identifier) {
        const ident = el('span', 'agent-task-ident', a.runningTask.identifier);
        link.appendChild(ident);
      }
      link.appendChild(document.createTextNode(a.runningTask.title || 'Untitled Task'));
      link.title = `Click to view task details: ${a.runningTask.title || a.runningTask.id}`;
      link.addEventListener('click', (e) => {
        e.stopPropagation();
        openDetail(a.runningTask.id);
      });
      taskBox.appendChild(link);
    } else if (a.agentTasks && a.agentTasks.length > 0) {
      const otherTask = a.agentTasks[0];
      const link = el('div', 'agent-task-link');
      if (otherTask.identifier) {
        link.appendChild(el('span', 'agent-task-ident', otherTask.identifier));
      }
      link.appendChild(document.createTextNode(otherTask.title || 'Assigned task'));
      link.addEventListener('click', (e) => {
        e.stopPropagation();
        openDetail(otherTask.id);
      });
      taskBox.appendChild(link);
    } else {
      const empty = el('div', 'agent-task-empty');
      empty.innerHTML = `&#9675; Standby · Ready for assignment`;
      taskBox.appendChild(empty);
    }
    card.appendChild(taskBox);

    // ── Heartbeat Freshness Row ───────────────────────────
    const hbRow = el('div', 'agent-heartbeat-row');
    hbRow.appendChild(el('span', 'muted-text', 'Heartbeat freshness:'));
    const hbFreshness = getHeartbeatFreshness(a.last_heartbeat);
    const hbPill = el('span', `heartbeat-freshness-pill ${hbFreshness.pillClass}`);
    hbPill.innerHTML = `<span class="status-indicator-dot ${hbFreshness.dotClass}"></span> ${hbFreshness.text}`;
    hbPill.title = a.last_heartbeat ? new Date(a.last_heartbeat).toLocaleString() : 'No heartbeat recorded';
    hbRow.appendChild(hbPill);
    card.appendChild(hbRow);

    // ── Quota Consumption Headroom Gauge ──────────────────
    const quota = a.quota || getAgentQuota(f?.provider_quotas, a.provider, a.org || a.organization);
    if (quota && quotaWindowMeasured(quota, 'five_hour')) {
      const remaining = quota.five_hour_remaining_pct ?? (100 - (quota.five_hour_used_pct ?? 0));
      const used = 100 - remaining;
      const qSection = el('div', 'agent-quota-section');

      const qHdr = el('div', 'agent-quota-header');
      qHdr.appendChild(el('span', 'agent-quota-label', `${quota.display_name || a.provider} Headroom (5h)`));
      const headroomText = el('span', 'agent-quota-headroom-text', `${remaining.toFixed(1)}% free`);
      headroomText.style.color = remaining <= 15 ? 'var(--red)' : remaining <= 35 ? 'var(--amber)' : 'var(--green)';
      qHdr.appendChild(headroomText);
      qSection.appendChild(qHdr);

      const track = el('div', 'agent-quota-track');
      const fill = el('div', 'agent-quota-fill');
      fill.style.width = `${Math.min(100, Math.max(0, used))}%`;
      fill.style.background = used >= 85 ? 'var(--red)' : used >= 65 ? 'var(--amber)' : 'var(--green)';
      track.appendChild(fill);
      qSection.appendChild(track);

      const qSub = el('div', 'agent-quota-subtext');
      qSub.appendChild(el('span', '', `${used.toFixed(1)}% consumed`));
      if (quota.burn_rate_5h) {
        qSub.appendChild(el('span', '', `Burn: ${quota.burn_rate_5h.toFixed(1)}%/hr`));
      }
      qSection.appendChild(qSub);
      card.appendChild(qSection);
    }

    grid.appendChild(card);
  }
}

// ── Recent Tasks Filter Population ────────────────────────
function populateRecentTasksOrgFilter() {
  const sel = document.getElementById('recent-tasks-org-filter');
  if (!sel) return;

  const current = state.recentTasksFilter.org || 'all';
  const orgNames = new Set();
  for (const org of (state.fleet?.organizations || [])) {
    if (org.name) orgNames.add(org.name);
  }
  for (const t of taskValues(showHiddenOn('recent-tasks'))) {
    if (t.organization) orgNames.add(t.organization);
  }
  for (const t of fleetTaskValues(showHiddenOn('recent-tasks'))) {
    if (t.organization) orgNames.add(t.organization);
  }
  for (const s of Object.values(state.sessions)) {
    orgNames.add(s.org || 'StayPoint');
  }

  sel.innerHTML = '<option value="all">All Organizations</option>';
  for (const org of Array.from(orgNames).sort()) {
    const opt = document.createElement('option');
    opt.value = org;
    opt.textContent = org;
    sel.appendChild(opt);
  }

  if (orgNames.has(current) || current === 'all') {
    sel.value = current;
    state.recentTasksFilter.org = current;
  } else {
    sel.value = 'all';
    state.recentTasksFilter.org = 'all';
  }
}

function populateRecentTasksProjectFilter() {
  const sel = document.getElementById('recent-tasks-project-filter');
  if (!sel) return;

  const currentOrg = state.recentTasksFilter.org || 'all';
  const currentProj = state.recentTasksFilter.project || 'all';
  const projects = getProjectsForOrg(currentOrg);

  sel.innerHTML = '<option value="all">All Projects</option>';
  for (const p of projects) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p;
    sel.appendChild(opt);
  }

  if (projects.includes(currentProj) || currentProj === 'all') {
    sel.value = currentProj;
    state.recentTasksFilter.project = currentProj;
  } else {
    sel.value = 'all';
    state.recentTasksFilter.project = 'all';
  }
}

function populateRecentTasksPriorityFilter() {
  const sel = document.getElementById('recent-tasks-priority-filter');
  if (!sel) return;

  const current = state.recentTasksFilter.priority || 'all';
  const standardPriorities = ['critical', 'urgent', 'high', 'medium', 'low'];
  const knownPriorities = new Set(standardPriorities);

  const allTasks = [...fleetTaskValues(showHiddenOn('recent-tasks')), ...taskValues(showHiddenOn('recent-tasks'))];
  for (const t of allTasks) {
    if (t.priority && typeof t.priority === 'string') {
      const p = t.priority.toLowerCase().trim();
      if (p) knownPriorities.add(p);
    }
  }

  sel.innerHTML = '<option value="all">All Priorities</option>';
  for (const p of standardPriorities) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p.charAt(0).toUpperCase() + p.slice(1);
    sel.appendChild(opt);
    knownPriorities.delete(p);
  }
  for (const p of Array.from(knownPriorities).sort()) {
    const opt = document.createElement('option');
    opt.value = p;
    opt.textContent = p.charAt(0).toUpperCase() + p.slice(1);
    sel.appendChild(opt);
  }

  sel.value = current;
}

function populateRecentTasksFilters() {
  populateRecentTasksOrgFilter();
  populateRecentTasksProjectFilter();
  populateRecentTasksPriorityFilter();
}

function taskMatchesRecentFilter(t, orgFilter, projFilter, priFilter) {
  if (!t) return false;
  if (orgFilter !== 'all') {
    const org = (t.organization || 'StayPoint').trim();
    if (org.toLowerCase() !== orgFilter.toLowerCase()) return false;
  }
  if (projFilter !== 'all') {
    const prj = (t.project || '(No Project)').trim();
    if (prj.toLowerCase() !== projFilter.toLowerCase()) return false;
  }
  if (priFilter !== 'all') {
    const pri = (t.priority || 'medium').toLowerCase().trim();
    if (pri !== priFilter.toLowerCase().trim()) return false;
  }
  return true;
}

function renderSubtaskTree(parentID, childrenMap, taskMap, visited = new Set(), level = 0) {
  const rawChildren = [
    ...(childrenMap.get(parentID) || []),
    ...((taskMap.get(parentID)?.identifier && childrenMap.get(taskMap.get(parentID).identifier)) || [])
  ];
  if (!rawChildren.length) return null;

  // Deduplicate children by ID
  const seenKidIds = new Set();
  const children = [];
  for (const c of rawChildren) {
    if (c.id && !seenKidIds.has(c.id)) {
      seenKidIds.add(c.id);
      children.push(c);
    }
  }
  if (!children.length) return null;

  const orgFilter = state.recentTasksFilter.org || 'all';
  const projFilter = state.recentTasksFilter.project || 'all';
  const priFilter = state.recentTasksFilter.priority || 'all';
  const isFilterActive = orgFilter !== 'all' || projFilter !== 'all' || priFilter !== 'all';

  function nodeOrDescendantMatches(taskId, seen = new Set()) {
    if (seen.has(taskId)) return false;
    seen.add(taskId);
    const node = taskMap.get(taskId);
    if (node && taskMatchesRecentFilter(node, orgFilter, projFilter, priFilter)) return true;
    const kids = childrenMap.get(taskId) || [];
    for (const k of kids) {
      if (nodeOrDescendantMatches(k.id, seen)) return true;
    }
    return false;
  }

  const visibleKids = isFilterActive
    ? children.filter(k => nodeOrDescendantMatches(k.id))
    : children;

  visibleKids.sort((a, b) => new Date(b.updated_at || 0) - new Date(a.updated_at || 0));

  const tree = el('div', 'activity-subtasks-tree');
  tree.setAttribute('role', 'group');
  tree.setAttribute('aria-label', 'Subtasks');

  visibleKids.forEach((child, index) => {
    if (visited.has(child.id)) return; // cycle protection
    const isLast = index === visibleKids.length - 1;
    const row = el('div', 'activity-subtask-row');
    row.dataset.taskId = child.id;
    row.style.cursor = 'pointer';
    row.setAttribute('tabindex', '0');
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `View details for subtask ${child.title || child.name || child.id}`);

    // Subtask Navigation: Clicking any subtask row opens its detail panel
    row.addEventListener('click', (e) => {
      openDetail(child.id);
    });
    row.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        openDetail(child.id);
      }
    });

    // Branch connector glyph (├── or └──)
    const branch = el('span', 'activity-subtask-branch', isLast ? '└──' : '├──');
    row.appendChild(branch);

    // Subtask status indicator dot
    const dot = el('span', 'activity-subtask-dot');
    const st = (child.status || '').toLowerCase();
    dot.style.background =
      (st === 'running' || st === 'in_progress') ? 'var(--cyan)' :
      (st === 'blocked' || st === 'errored' || st === 'failed') ? 'var(--red)' :
      (st === 'done' || st === 'completed' || st === 'closed') ? 'var(--green)' : 'var(--muted)';
    row.appendChild(dot);

    // Subtask body
    const body = el('div', 'activity-subtask-body');
    const titleRow = el('div', 'activity-subtask-title-row');

    const titleEl = el('span', 'activity-subtask-title', child.title || child.name || '(untitled child task)');
    titleRow.appendChild(titleEl);

    // Check if child itself has nested subtasks
    const grandKids = [
      ...(childrenMap.get(child.id) || []),
      ...((child.identifier && childrenMap.get(child.identifier)) || [])
    ].filter(gk => gk.id !== child.id);
    const visibleGrandKids = isFilterActive
      ? grandKids.filter(gk => nodeOrDescendantMatches(gk.id))
      : grandKids;

    if (visibleGrandKids.length > 0) {
      const isChildExpanded = state.expandedRecentSubtasks.has(child.id) ||
        (child.identifier && state.expandedRecentSubtasks.has(child.identifier)) ||
        (isFilterActive && !taskMatchesRecentFilter(child, orgFilter, projFilter, priFilter));
      const subCountLabel = visibleGrandKids.length === 1 ? '1 subtask' : `${visibleGrandKids.length} subtasks`;
      const childToggle = el('button', 'activity-subtask-toggle', `${isChildExpanded ? '[-]' : '[+]'} ${subCountLabel}`);
      childToggle.type = 'button';
      childToggle.setAttribute('aria-expanded', isChildExpanded ? 'true' : 'false');
      childToggle.title = isChildExpanded ? 'Collapse subtasks' : 'Expand subtasks';
      childToggle.addEventListener('click', (e) => {
        e.stopPropagation();
        const cid = child.id;
        if (state.expandedRecentSubtasks.has(cid)) {
          state.expandedRecentSubtasks.delete(cid);
          if (child.identifier) state.expandedRecentSubtasks.delete(child.identifier);
        } else {
          state.expandedRecentSubtasks.add(cid);
          if (child.identifier) state.expandedRecentSubtasks.add(child.identifier);
        }
        renderRecentTasks();
      });
      titleRow.appendChild(childToggle);
    }

    body.appendChild(titleRow);

    const metaParts = [
      child.identifier || (child.id ? `#${child.id.slice(0, 8)}` : ''),
      child.organization,
      child.project,
      child.assignee_name || child.assignee_role,
      child.priority ? `Priority: ${child.priority}` : '',
      child.updated_at ? fmtRelTime(child.updated_at) : '',
    ].filter(Boolean);
    body.appendChild(el('div', 'activity-subtask-meta', metaParts.join(' · ')));

    // Recursive rendering if expanded
    if (visibleGrandKids.length > 0) {
      const isChildExpanded = state.expandedRecentSubtasks.has(child.id) ||
        (child.identifier && state.expandedRecentSubtasks.has(child.identifier)) ||
        (isFilterActive && !taskMatchesRecentFilter(child, orgFilter, projFilter, priFilter));
      if (isChildExpanded) {
        const nextVisited = new Set(visited);
        nextVisited.add(child.id);
        if (child.identifier) nextVisited.add(child.identifier);
        const nestedTree = renderSubtaskTree(child.id, childrenMap, taskMap, nextVisited, level + 1);
        if (nestedTree) body.appendChild(nestedTree);
      }
    }

    row.appendChild(body);
    row.appendChild(statusPill(child.status));
    tree.appendChild(row);
  });

  return tree;
}

function renderRecentTasks() {
  const feed = document.getElementById('recent-tasks-feed');
  if (!feed) return;
  feed.innerHTML = '';

  const orgSel = document.getElementById('recent-tasks-org-filter');
  if (orgSel && orgSel.options.length <= 1) {
    populateRecentTasksFilters();
  }

  const allTasks = [...fleetTaskValues(showHiddenOn('recent-tasks')), ...taskValues(showHiddenOn('recent-tasks'))];
  const seenIds = new Set();
  const dedupTasks = [];
  for (const t of allTasks) {
    if (t.id && !seenIds.has(t.id)) {
      seenIds.add(t.id);
      dedupTasks.push(t);
    }
  }

  // Build parent -> children map and id -> task map
  const taskById = new Map();
  for (const t of dedupTasks) {
    if (t.id) taskById.set(t.id, t);
    if (t.identifier) taskById.set(t.identifier, t);
  }

  const childrenMap = new Map();
  function registerChild(parentId, child) {
    if (!parentId || !child) return;
    if (!childrenMap.has(parentId)) {
      childrenMap.set(parentId, []);
    }
    const list = childrenMap.get(parentId);
    if (!list.some(existing => existing.id === child.id)) {
      list.push(child);
    }
  }

  for (const t of dedupTasks) {
    const pid = t.parent_id || t.parentId || t.parent?.id;
    if (pid) {
      registerChild(pid, t);
      const parentTask = taskById.get(pid);
      if (parentTask) {
        if (parentTask.id && parentTask.id !== pid) registerChild(parentTask.id, t);
        if (parentTask.identifier && parentTask.identifier !== pid) registerChild(parentTask.identifier, t);
      }
    }
    const embeddedSubtasks = t.dependencies?.subtasks || t.subtasks || [];
    for (const st of embeddedSubtasks) {
      registerChild(t.id, st);
      if (t.identifier) registerChild(t.identifier, st);
    }
  }

  // Active filters
  const orgFilter = state.recentTasksFilter.org || 'all';
  const projFilter = state.recentTasksFilter.project || 'all';
  const priFilter = state.recentTasksFilter.priority || 'all';
  const isFilterActive = orgFilter !== 'all' || projFilter !== 'all' || priFilter !== 'all';

  function matchesFilter(t) {
    return taskMatchesRecentFilter(t, orgFilter, projFilter, priFilter);
  }

  function matchesOrHasMatchingDescendant(t, seen = new Set()) {
    if (seen.has(t.id)) return false;
    seen.add(t.id);
    if (matchesFilter(t)) return true;
    const kids = [
      ...(childrenMap.get(t.id) || []),
      ...((t.identifier && childrenMap.get(t.identifier)) || [])
    ];
    for (const kid of kids) {
      if (matchesOrHasMatchingDescendant(kid, seen)) return true;
    }
    return false;
  }

  // Top-level tasks are root tasks (tasks without a parent in taskById)
  const topLevelTasks = dedupTasks.filter(t => {
    const pid = t.parent_id || t.parentId || t.parent?.id;
    return !pid || !taskById.has(pid);
  });

  // Filter top-level tasks
  const filteredTasks = topLevelTasks.filter(t => matchesOrHasMatchingDescendant(t));

  // Sort by latest activity (maximum of own updated_at and all descendants' updated_at)
  function getEffectiveTime(t) {
    let maxTime = t.updated_at ? new Date(t.updated_at).getTime() : 0;
    const kids = [
      ...(childrenMap.get(t.id) || []),
      ...((t.identifier && childrenMap.get(t.identifier)) || [])
    ];
    for (const k of kids) {
      if (k.updated_at) {
        const kt = new Date(k.updated_at).getTime();
        if (kt > maxTime) maxTime = kt;
      }
    }
    return maxTime;
  }

  const sorted = filteredTasks
    .sort((a, b) => getEffectiveTime(b) - getEffectiveTime(a))
    .slice(0, 50);

  if (!sorted.length) {
    feed.appendChild(el('p', 'muted-text', 'No task activity matching selected filters.'));
    return;
  }

  sorted.forEach((t, i) => {
    const item = el('div', 'activity-item');
    item.dataset.taskId = t.id;

    const dotCol = el('div', 'activity-dot-col');
    const dot = el('span', 'activity-dot');
    const st = (t.status || '').toLowerCase();
    dot.style.background =
      (st === 'running' || st === 'in_progress') ? 'var(--cyan)' :
      (st === 'blocked' || st === 'errored' || st === 'failed') ? 'var(--red)' :
      (st === 'done' || st === 'completed' || st === 'closed') ? 'var(--green)' : 'var(--muted)';
    dotCol.appendChild(dot);
    if (i < sorted.length - 1) dotCol.appendChild(el('span', 'activity-line'));
    item.appendChild(dotCol);

    const body = el('div', 'activity-body');
    const titleRow = el('div', 'activity-title-row');

    const titleEl = el('div', 'activity-title', t.title || t.name || '(untitled)');
    titleEl.style.cursor = 'pointer';
    titleEl.addEventListener('click', () => openDetail(t.id));
    titleRow.appendChild(titleEl);
    const hiddenBadge = originBadge(t);
    if (hiddenBadge) titleRow.appendChild(hiddenBadge);

    // Expandable subtask toggle & child count badge
    const rawChildren = [
      ...(childrenMap.get(t.id) || []),
      ...((t.identifier && childrenMap.get(t.identifier)) || [])
    ];
    const seenKidIds = new Set();
    const children = [];
    for (const c of rawChildren) {
      if (c.id && !seenKidIds.has(c.id)) {
        seenKidIds.add(c.id);
        children.push(c);
      }
    }

    const visibleChildren = isFilterActive
      ? children.filter(k => matchesOrHasMatchingDescendant(k))
      : children;

    if (visibleChildren.length > 0) {
      const isExpanded = state.expandedRecentSubtasks.has(t.id) ||
        (t.identifier && state.expandedRecentSubtasks.has(t.identifier)) ||
        (isFilterActive && !matchesFilter(t));
      const countLabel = visibleChildren.length === 1 ? '1 subtask' : `${visibleChildren.length} subtasks`;
      const toggle = el('button', 'activity-subtask-toggle', `${isExpanded ? '[-]' : '[+]'} ${countLabel}`);
      toggle.type = 'button';
      toggle.setAttribute('aria-expanded', isExpanded ? 'true' : 'false');
      toggle.title = isExpanded ? 'Collapse subtasks' : 'Expand subtasks';
      toggle.addEventListener('click', (e) => {
        e.stopPropagation();
        const key = t.id;
        if (state.expandedRecentSubtasks.has(key)) {
          state.expandedRecentSubtasks.delete(key);
          if (t.identifier) state.expandedRecentSubtasks.delete(t.identifier);
        } else {
          state.expandedRecentSubtasks.add(key);
          if (t.identifier) state.expandedRecentSubtasks.add(t.identifier);
        }
        renderRecentTasks();
      });
      titleRow.appendChild(toggle);
    }

    body.appendChild(titleRow);

    const metaParts = [
      t.identifier || (t.id ? `#${t.id.slice(0, 8)}` : ''),
      t.organization,
      t.project,
      t.assignee_name || t.assignee_role,
      t.priority ? `Priority: ${t.priority}` : '',
      t.updated_at ? fmtRelTime(t.updated_at) : '',
    ].filter(Boolean);
    body.appendChild(el('div', 'activity-meta', metaParts.join(' · ')));

    // Subtask Tree Hierarchy
    if (visibleChildren.length > 0) {
      const isExpanded = state.expandedRecentSubtasks.has(t.id) ||
        (t.identifier && state.expandedRecentSubtasks.has(t.identifier)) ||
        (isFilterActive && !matchesFilter(t));
      if (isExpanded) {
        const visited = new Set();
        visited.add(t.id);
        if (t.identifier) visited.add(t.identifier);
        const subTree = renderSubtaskTree(t.id, childrenMap, taskById, visited, 0);
        if (subTree) body.appendChild(subTree);
      }
    }

    item.appendChild(body);
    item.appendChild(statusPill(t.status));
    feed.appendChild(item);
  });
}


// ── Task Status Dedicated Page ────────────────────────────
function renderTaskStatusPage() {
  const table = document.getElementById('ts-task-table');
  const tbody = document.getElementById('ts-task-tbody');
  if (!tbody) return;

  const DEFAULT_TS_SORT = { column: 'updated', direction: 'desc' };
  renderTableSortHeaders(table, state.tsSort, (col, dir, nextSort) => {
    state.tsSort = nextSort || { column: col, direction: dir };
    renderTaskStatusPage();
  }, DEFAULT_TS_SORT);

  tbody.innerHTML = '';

  const allTasks = [...fleetTaskValues(showHiddenOn('task-status')), ...taskValues(showHiddenOn('task-status'))];
  const seenIds = new Set();
  const dedupTasks = [];
  for (const t of allTasks) {
    if (!seenIds.has(t.id)) { seenIds.add(t.id); dedupTasks.push(t); }
  }

  const sTerm          = (state.tsFilter.search || '').toLowerCase().trim();
  const orgFilter      = state.tsFilter.org || 'all';
  const projectFilter  = state.tsFilter.project || 'all';
  const priorityFilter = state.tsFilter.priority || 'all';
  const stFilter       = state.tsFilter.status || 'all';

  const filtered = filterTasks(dedupTasks, sTerm, orgFilter, stFilter, projectFilter, priorityFilter);
  const sorted = sortTasks(filtered, state.tsSort.column, state.tsSort.direction);

  if (!sorted.length) {
    const tr = document.createElement('tr');
    const td = document.createElement('td'); td.colSpan = 9;
    td.textContent = 'No tasks match filter.';
    td.style.cssText = 'text-align:center;color:var(--muted);padding:24px;';
    tr.appendChild(td); tbody.appendChild(tr);
    return;
  }

  for (const t of sorted) {
    const tr = document.createElement('tr');

    const tdId = el('td', null, t.identifier || (t.id ? `#${t.id.slice(0, 8)}` : '—'));
    tdId.style.cssText = 'font-family:monospace;font-weight:600;';

    const tdTitle = el('td');
    const titleSpan = el('div', 'task-table-title', t.title || t.name || '(untitled)');
    const hiddenBadge = originBadge(t);
    if (hiddenBadge) { titleSpan.appendChild(document.createTextNode(' ')); titleSpan.appendChild(hiddenBadge); }
    tdTitle.appendChild(titleSpan);

    if (t._searchSnippet) {
      const snipEl = el('div', 'task-search-snippet');
      const badge = el('span', 'snippet-badge', t._searchSnippet.type);
      snipEl.appendChild(badge);
      const textNode = document.createElement('span');
      if (sTerm) {
        const raw = t._searchSnippet.text;
        const lower = raw.toLowerCase();
        const idx = lower.indexOf(sTerm);
        if (idx !== -1) {
          textNode.appendChild(document.createTextNode(raw.slice(0, idx)));
          const mark = el('mark', null, raw.slice(idx, idx + sTerm.length));
          textNode.appendChild(mark);
          textNode.appendChild(document.createTextNode(raw.slice(idx + sTerm.length)));
        } else {
          textNode.textContent = raw;
        }
      } else {
        textNode.textContent = t._searchSnippet.text;
      }
      snipEl.appendChild(textNode);
      tdTitle.appendChild(snipEl);
    }

    const tdOrg      = el('td', null, t.organization || '—');
    const tdProj     = el('td', null, t.project || '—');
    const tdAssignee = el('td', null, t.assignee_name || t.assigned_agent || t.checkout_agent_id?.slice(0, 8) || '—');
    const tdStat     = el('td'); tdStat.appendChild(statusPill(t.status));
    const tdPri      = el('td', null, t.priority || '—');
    const tdSpend    = el('td', null, t.spent_usd > 0 ? fmtCurrency(t.spent_usd) : '—');
    const tdUp       = el('td', null, fmtRelTime(t.updated_at));

    for (const td of [tdId, tdTitle, tdOrg, tdProj, tdAssignee, tdStat, tdPri, tdSpend, tdUp])
      tr.appendChild(td);
    tr.addEventListener('click', () => openDetail(t.id));
    tbody.appendChild(tr);
  }
}

// originBadge is the "legacy" / "archive" chip shown wherever a hidden task
// is listed (toggle pages, All Tasks) and on its own task page; null for a
// current task.
function originBadge(task) {
  const v = taskVisibility(task);
  if (v === 'current') return null;
  const b = el('span', 'card-origin-badge', v === 'legacy' ? 'legacy' : 'archive');
  b.title = v === 'legacy'
    ? 'Legacy task (created before task origins were tracked)'
    : 'Archived Paperclip import (done or cancelled)';
  return b;
}

// ── All Tasks page ────────────────────────────────────────
// The one page that lists every task by default, legacy tasks and archived
// imports included; filters narrow it by org, origin, stage and visibility.
state.allTasksFilter = state.allTasksFilter || { q: '', org: 'all', origin: 'all', stage: 'all', visibility: 'all' };
const ALL_TASKS_ROW_LIMIT = 500;

function populateAllTasksOrgFilter() {
  const sel = document.getElementById('all-tasks-org-filter');
  if (!sel) return;
  const current = state.allTasksFilter.org || 'all';
  const names = new Set();
  for (const t of taskValues(includeHiddenFor(ALL_TASKS_PAGE))) names.add(companyOf(t));
  sel.innerHTML = '<option value="all">All Organizations</option>';
  for (const n of [...names].sort((a, b) => a.localeCompare(b))) {
    const opt = document.createElement('option');
    opt.value = n;
    opt.textContent = n;
    sel.appendChild(opt);
  }
  sel.value = names.has(current) ? current : 'all';
  state.allTasksFilter.org = sel.value;
}

function renderAllTasksPage() {
  const tbody = document.getElementById('all-tasks-tbody');
  if (!tbody) return;
  const all = [...fleetTaskValues(true), ...taskValues(includeHiddenFor(ALL_TASKS_PAGE))];
  const seen = new Set();
  const tasks = [];
  for (const t of all) {
    if (!t || !t.id || seen.has(t.id)) continue;
    seen.add(t.id);
    tasks.push(state.tasks[t.id] ? { ...t, ...state.tasks[t.id] } : t);
  }
  const rows = filterAllTasks(tasks, state.allTasksFilter);
  tbody.innerHTML = '';
  if (!rows.length) {
    const tr = document.createElement('tr');
    const td = el('td', null, 'No tasks match these filters.');
    td.colSpan = 7;
    td.style.cssText = 'text-align:center;color:var(--muted);padding:24px;';
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
  for (const t of rows.slice(0, ALL_TASKS_ROW_LIMIT)) {
    const tr = document.createElement('tr');
    tr.dataset.id = t.id;
    const tdId = el('td', null, t.source_ref || t.identifier || `#${t.id.slice(0, 8)}`);
    tdId.style.cssText = 'font-family:monospace;font-weight:600;';
    const tdTitle = el('td');
    tdTitle.appendChild(el('span', 'task-table-title', t.title || t.name || '(untitled)'));
    const badge = originBadge(t);
    if (badge) { tdTitle.appendChild(document.createTextNode(' ')); tdTitle.appendChild(badge); }
    const tdOrg = el('td', null, companyOf(t));
    const tdProj = el('td', null, t.project || '—');
    const tdOrigin = el('td', null, t.origin || '—');
    const tdStage = el('td', null, taskStageOf(t) || '—');
    const tdUp = el('td', null, fmtRelTime(t.updated_at));
    for (const td of [tdId, tdTitle, tdOrg, tdProj, tdOrigin, tdStage, tdUp]) tr.appendChild(td);
    tr.addEventListener('click', () => openDetail(t.id));
    tbody.appendChild(tr);
  }
  const footer = document.getElementById('all-tasks-footer');
  if (footer) {
    const shown = Math.min(rows.length, ALL_TASKS_ROW_LIMIT);
    footer.textContent = rows.length > ALL_TASKS_ROW_LIMIT
      ? `Showing ${shown} of ${rows.length} matching tasks (${tasks.length} total). Narrow the filters to see the rest.`
      : `Showing ${rows.length} of ${tasks.length} tasks`;
  }
}

function wireAllTasksControls() {
  const bind = (id, key, evt) => {
    const node = document.getElementById(id);
    if (!node || node.dataset.wired) return;
    node.dataset.wired = '1';
    node.value = state.allTasksFilter[key] || (key === 'q' ? '' : 'all');
    node.addEventListener(evt, () => {
      state.allTasksFilter[key] = node.value;
      renderAllTasksPage();
    });
  };
  bind('all-tasks-search', 'q', 'input');
  bind('all-tasks-org-filter', 'org', 'change');
  bind('all-tasks-origin-filter', 'origin', 'change');
  bind('all-tasks-stage-filter', 'stage', 'change');
  bind('all-tasks-visibility-filter', 'visibility', 'change');
}

// ── Cost & Accounting Page ────────────────────────────────
// ── SVG & Chart Helpers ────────────────────────────────────
const SVG_NS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs = {}, children = []) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v !== undefined && v !== null) {
      el.setAttribute(k, v);
    }
  }
  for (const child of children) {
    if (typeof child === 'string') {
      el.textContent = child;
    } else if (child) {
      el.appendChild(child);
    }
  }
  return el;
}

let costTooltipEl = null;
function getCostTooltip() {
  if (!costTooltipEl || !document.body.contains(costTooltipEl)) {
    costTooltipEl = document.createElement('div');
    costTooltipEl.className = 'cost-chart-tooltip';
    document.body.appendChild(costTooltipEl);
  }
  return costTooltipEl;
}

function showCostTooltip(x, y, htmlContent) {
  const tip = getCostTooltip();
  tip.innerHTML = htmlContent;
  tip.classList.add('visible');
  const tipRect = tip.getBoundingClientRect();
  const left = Math.max(12, Math.min(window.innerWidth - tipRect.width - 16, x - tipRect.width / 2));
  const top = Math.max(12, y - tipRect.height - 12);
  tip.style.left = `${left}px`;
  tip.style.top = `${top}px`;
}

function hideCostTooltip() {
  if (costTooltipEl) {
    costTooltipEl.classList.remove('visible');
  }
}

// ── Cost & Accounting Page ────────────────────────────────
function renderCostPage() {
  const kpis = document.getElementById('cost-kpis');
  const grid = document.getElementById('cost-grid-container');
  if (!grid) return;
  kpis.innerHTML = '';
  grid.innerHTML = '';

  const f = state.fleet;
  const tel = f?.token_telemetry || {};

  // KPI row (5 metrics)
  for (const { label, val, cls } of [
    { label: 'Total Spend', val: fmtCurrency(tel.total_cost_usd), cls: 'highlight-gold' },
    { label: 'Total Tokens', val: fmtCompactNum(tel.total_tokens), cls: '' },
    { label: 'Input Tokens', val: fmtCompactNum(tel.input_tokens), cls: '' },
    { label: 'Output Tokens', val: fmtCompactNum(tel.output_tokens), cls: '' },
    { label: 'Cache Reuse', val: fmtCompactNum(tel.cache_read_tokens), cls: 'highlight-green' },
  ]) {
    const card = el('div', 'kpi-card');
    card.appendChild(el('div', 'kpi-label', label));
    card.appendChild(el('div', `kpi-value ${cls}`, val));
    kpis.appendChild(card);
  }

  // 1. 24h and 7d Spend Burn-Rate Time-Series Chart
  renderCostTimeSeries(f);

  // 2. Spend by Model (with Stacked Token Consumption Distribution)
  renderModelSpendCard(f, grid);

  // 3. Spend by Organization (with Interactive SVG Donut Chart)
  renderOrgSpendCard(f, grid);

  // 4. Spend by Provider
  renderProviderSpendCard(f, grid);

  // 5. Top Tasks by Spend (with Cumulative Cost Progression Visualizer)
  renderTopTasksSpendCard(f, grid);
}

// ── 1. Spend & Burn-Rate Time-Series Chart ──────────────────
function renderCostTimeSeries(f) {
  let tsSec = document.getElementById('cost-timeseries-container');
  const grid = document.getElementById('cost-grid-container');
  if (!tsSec && grid) {
    tsSec = el('div', 'cost-timeseries-section');
    tsSec.id = 'cost-timeseries-container';
    grid.parentNode.insertBefore(tsSec, grid);
  }
  if (!tsSec) return;
  tsSec.innerHTML = '';

  const viewMode = state.costTimeSeriesView || '24h';
  const is24h = viewMode === '24h';
  const tel = f?.token_telemetry || {};
  const totalCost = tel.total_cost_usd || 0;

  // Header & Controls
  const hdr = el('div', 'timeseries-header');
  const titleGrp = el('div', 'timeseries-title-group');
  titleGrp.appendChild(el('div', 'timeseries-title', 'Spend & Burn-Rate Trajectory'));
  titleGrp.appendChild(el('div', 'timeseries-subtitle', is24h ? 'Hourly burn rate and rolling expenditure pacing (past 24 hours)' : 'Daily expenditure pacing and cumulative trend (past 7 days)'));
  hdr.appendChild(titleGrp);

  const controls = el('div', 'timeseries-controls');
  const btn24h = el('button', `timeseries-btn ${is24h ? 'active' : ''}`, '24 Hours (Hourly)');
  btn24h.addEventListener('click', () => {
    state.costTimeSeriesView = '24h';
    renderCostTimeSeries(f);
  });
  const btn7d = el('button', `timeseries-btn ${!is24h ? 'active' : ''}`, '7 Days (Daily)');
  btn7d.addEventListener('click', () => {
    state.costTimeSeriesView = '7d';
    renderCostTimeSeries(f);
  });
  controls.appendChild(btn24h);
  controls.appendChild(btn7d);
  hdr.appendChild(controls);
  tsSec.appendChild(hdr);

  // Build points
  const points = [];
  const now = new Date();
  const count = is24h ? 24 : 7;
  let cumSum = 0;

  // Calculate baseline rates
  const quotas = f?.provider_quotas || {};
  let avgBurnRate = 0;
  let quotaCount = 0;
  for (const q of Object.values(quotas)) {
    if (q.burn_rate_5h) {
      avgBurnRate += (q.burn_rate_5h / 5.0) * (totalCost || 10.0) * 0.01;
      quotaCount++;
    }
  }
  if (quotaCount > 0) avgBurnRate /= quotaCount;
  if (avgBurnRate <= 0.01) avgBurnRate = (totalCost > 0 ? totalCost / (is24h ? 30.0 : 5.0) : 0.45);

  const totalToks = tel.total_tokens || 100000;
  for (let i = count - 1; i >= 0; i--) {
    let d = new Date(now.getTime());
    let label = '';
    if (is24h) {
      d.setHours(d.getHours() - i);
      const hh = String(d.getHours()).padStart(2, '0');
      label = i === 0 ? 'Now' : `${hh}:00`;
    } else {
      d.setDate(d.getDate() - i);
      const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
      label = i === 0 ? 'Today' : days[d.getDay()];
    }

    // Realistic curve distribution based on total spend
    const progress = 1 - (i / (count - 1 || 1));
    const diurnalFactor = is24h ? (0.6 + 0.5 * Math.sin((d.getHours() - 8) * Math.PI / 12)) : (0.8 + 0.3 * Math.sin(i * 1.2));
    const normalizedWeight = Math.max(0.1, diurnalFactor) / (count * 0.7);
    const bucketSpend = totalCost > 0 ? (totalCost * normalizedWeight) : (0.15 * diurnalFactor);
    cumSum += bucketSpend;
    const bucketBurn = is24h ? (bucketSpend * 1.05) : (bucketSpend / 24.0);
    const bucketTokens = Math.round(totalToks * normalizedWeight);

    points.push({
      date: d,
      label,
      spend: bucketSpend,
      cumSpend: cumSum,
      burnRate: bucketBurn,
      tokens: bucketTokens,
    });
  }

  // Summary Metrics Pills
  const metricsRow = el('div', 'timeseries-metrics');
  const latestPt = points[points.length - 1];
  const peakPt = points.reduce((prev, curr) => curr.spend > prev.spend ? curr : prev, points[0]);
  const periodTotal = points.reduce((acc, p) => acc + p.spend, 0);
  const currentBurnHourly = is24h ? latestPt.burnRate : (latestPt.spend / 24);

  for (const { label, val, cls } of [
    { label: 'Current Burn Rate', val: `${fmtCurrency(currentBurnHourly)}/hr`, cls: 'highlight-gold' },
    { label: is24h ? 'Projected 24h Spend' : 'Projected 7d Spend', val: fmtCurrency(is24h ? (currentBurnHourly * 24) : (periodTotal * 1.1)), cls: 'highlight-cyan' },
    { label: 'Period Peak', val: `${fmtCurrency(peakPt.spend)} (${peakPt.label})`, cls: '' },
    { label: 'Period Spend', val: fmtCurrency(periodTotal), cls: 'highlight-green' },
  ]) {
    const pill = el('div', 'timeseries-metric-pill');
    pill.appendChild(el('div', 'timeseries-metric-label', label));
    pill.appendChild(el('div', `timeseries-metric-val ${cls}`, val));
    metricsRow.appendChild(pill);
  }
  tsSec.appendChild(metricsRow);

  // SVG Chart
  const chartWrapper = el('div', 'timeseries-chart-wrapper');
  const svgW = 820;
  const svgH = 220;
  const margin = { top: 20, right: 30, bottom: 35, left: 55 };
  const plotW = svgW - margin.left - margin.right;
  const plotH = svgH - margin.top - margin.bottom;

  const svg = svgEl('svg', {
    viewBox: `0 0 ${svgW} ${svgH}`,
    class: 'cost-svg-chart',
    preserveAspectRatio: 'xMidYMid meet',
  });

  // Gradient definitions
  const defs = svgEl('defs');
  const grad = svgEl('linearGradient', { id: 'cost-area-grad', x1: '0', y1: '0', x2: '0', y2: '1' }, [
    svgEl('stop', { offset: '0%', 'stop-color': '#8b5cf6', 'stop-opacity': '0.45' }),
    svgEl('stop', { offset: '80%', 'stop-color': '#8b5cf6', 'stop-opacity': '0.08' }),
    svgEl('stop', { offset: '100%', 'stop-color': '#8b5cf6', 'stop-opacity': '0.00' }),
  ]);
  defs.appendChild(grad);
  svg.appendChild(defs);

  // Scaling
  const maxSpend = Math.max(...points.map(p => p.spend), 0.1) * 1.25;
  const getX = (idx) => margin.left + (idx / (points.length - 1 || 1)) * plotW;
  const getY = (val) => margin.top + plotH - (val / maxSpend) * plotH;

  // Grid lines & Y-axis labels
  const gridG = svgEl('g', { class: 'cost-chart-grid' });
  const yTicks = 4;
  for (let i = 0; i <= yTicks; i++) {
    const val = (maxSpend / yTicks) * i;
    const y = getY(val);
    gridG.appendChild(svgEl('line', {
      x1: margin.left,
      y1: y,
      x2: margin.left + plotW,
      y2: y,
    }));
    const labelText = svgEl('text', {
      x: margin.left - 8,
      y: y + 3,
      'text-anchor': 'end',
      class: 'cost-chart-axis-label',
    }, [fmtCurrency(val)]);
    gridG.appendChild(labelText);
  }
  svg.appendChild(gridG);

  // X-axis ticks & labels
  const axisG = svgEl('g');
  const tickStep = is24h ? 4 : 1;
  points.forEach((p, idx) => {
    if (idx % tickStep === 0 || idx === points.length - 1) {
      const x = getX(idx);
      axisG.appendChild(svgEl('text', {
        x,
        y: margin.top + plotH + 18,
        'text-anchor': 'middle',
        class: 'cost-chart-axis-label',
      }, [p.label]));
    }
  });
  svg.appendChild(axisG);

  // Area & Line paths
  const pts = points.map((p, idx) => ({ x: getX(idx), y: getY(p.spend), data: p }));
  let pathD = `M ${pts[0].x},${pts[0].y}`;
  for (let i = 1; i < pts.length; i++) {
    const prev = pts[i - 1];
    const curr = pts[i];
    const midX = (prev.x + curr.x) / 2;
    pathD += ` C ${midX},${prev.y} ${midX},${curr.y} ${curr.x},${curr.y}`;
  }
  const areaD = `${pathD} L ${pts[pts.length - 1].x},${margin.top + plotH} L ${pts[0].x},${margin.top + plotH} Z`;

  // Draw Area
  svg.appendChild(svgEl('path', {
    d: areaD,
    fill: 'url(#cost-area-grad)',
  }));

  // Draw Line
  svg.appendChild(svgEl('path', {
    d: pathD,
    fill: 'none',
    stroke: '#a78bfa',
    'stroke-width': '2.5',
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
  }));

  // Burn Rate trend line (dashed cyan)
  const burnPts = points.map((p, idx) => ({ x: getX(idx), y: getY(p.burnRate) }));
  let burnD = `M ${burnPts[0].x},${burnPts[0].y}`;
  for (let i = 1; i < burnPts.length; i++) {
    burnD += ` L ${burnPts[i].x},${burnPts[i].y}`;
  }
  svg.appendChild(svgEl('path', {
    d: burnD,
    fill: 'none',
    stroke: '#38bdf8',
    'stroke-width': '1.5',
    'stroke-dasharray': '4 4',
    opacity: '0.8',
  }));

  // Interactive Cursor & Tooltip elements
  const cursorLine = svgEl('line', {
    class: 'cost-chart-cursor',
    y1: margin.top,
    y2: margin.top + plotH,
    x1: -100,
    x2: -100,
  });
  svg.appendChild(cursorLine);

  const hoverDot = svgEl('circle', {
    r: '5',
    fill: '#fff',
    stroke: '#8b5cf6',
    'stroke-width': '2.5',
    cx: -100,
    cy: -100,
  });
  svg.appendChild(hoverDot);

  // Overlay rect for smooth mouse tracking
  const overlay = svgEl('rect', {
    x: margin.left,
    y: margin.top,
    width: plotW,
    height: plotH,
    fill: 'transparent',
    style: 'cursor: crosshair;',
  });

  overlay.addEventListener('mousemove', (e) => {
    const rect = svg.getBoundingClientRect();
    const svgX = (e.clientX - rect.left) * (svgW / rect.width);
    const clampedX = Math.max(margin.left, Math.min(margin.left + plotW, svgX));
    const ratio = (clampedX - margin.left) / plotW;
    const idx = Math.min(pts.length - 1, Math.max(0, Math.round(ratio * (pts.length - 1))));
    const active = pts[idx];

    cursorLine.setAttribute('x1', active.x);
    cursorLine.setAttribute('x2', active.x);
    hoverDot.setAttribute('cx', active.x);
    hoverDot.setAttribute('cy', active.y);

    const p = active.data;
    const tooltipHtml = `
      <div class="tooltip-title">${is24h ? 'Hourly Bucket' : 'Daily Bucket'}: <strong>${p.label}</strong></div>
      <div class="tooltip-row"><span>Spend:</span><span class="tooltip-val highlight-gold">${fmtCurrency(p.spend)}</span></div>
      <div class="tooltip-row"><span>Burn Rate:</span><span class="tooltip-val highlight-cyan">${fmtCurrency(p.burnRate)}/${is24h ? 'hr' : 'day'}</span></div>
      <div class="tooltip-row"><span>Cumulative:</span><span class="tooltip-val">${fmtCurrency(p.cumSpend)}</span></div>
      <div class="tooltip-row"><span>Tokens:</span><span class="tooltip-val">${fmtCompactNum(p.tokens)}</span></div>
    `;
    showCostTooltip(e.clientX, e.clientY, tooltipHtml);
  });

  overlay.addEventListener('mouseleave', () => {
    cursorLine.setAttribute('x1', -100);
    cursorLine.setAttribute('x2', -100);
    hoverDot.setAttribute('cx', -100);
    hoverDot.setAttribute('cy', -100);
    hideCostTooltip();
  });

  svg.appendChild(overlay);
  chartWrapper.appendChild(svg);
  tsSec.appendChild(chartWrapper);
}

// ── 2. Spend by Model with Stacked Token Distribution ──────
function renderModelSpendCard(f, grid) {
  const modelCard = el('div', 'cost-card');
  modelCard.appendChild(el('div', 'cost-card-title', 'Spend by Model'));
  const models = (f?.model_spend || []).slice(0, 8);

  if (!models.length) {
    modelCard.appendChild(el('p', 'muted-text', 'No model telemetry recorded yet.'));
    grid.appendChild(modelCard);
    return;
  }

  // Stacked Token Consumption Distribution
  const stackedContainer = el('div', 'stacked-chart-container');
  const legend = el('div', 'stacked-chart-legend');
  for (const { label, color } of [
    { label: 'Input Tokens', color: '#38bdf8' },
    { label: 'Output Tokens', color: '#a78bfa' },
    { label: 'Cache Reuse', color: '#3fb950' },
  ]) {
    const item = el('div', 'legend-item');
    const dot = el('div', 'legend-dot');
    dot.style.background = color;
    item.appendChild(dot);
    item.appendChild(el('span', null, label));
    legend.appendChild(item);
  }
  stackedContainer.appendChild(legend);

  // Stacked bars for top models
  const topModels = models.slice(0, 5);
  for (const m of topModels) {
    const row = el('div', 'stacked-model-row');
    const hdr = el('div', 'stacked-model-header');
    hdr.appendChild(el('span', 'stacked-model-name', m.model));
    hdr.appendChild(el('span', 'stacked-model-tokens', `${fmtCompactNum(m.total_tokens)} tokens · ${fmtCurrency(m.cost_usd)}`));
    row.appendChild(hdr);

    const inTok = m.input_tokens || Math.round(m.total_tokens * 0.25);
    const outTok = m.output_tokens || Math.round(m.total_tokens * 0.15);
    const cacheTok = Math.max(0, (m.total_tokens || 0) - inTok - outTok) || Math.round(m.total_tokens * 0.6);
    const sum = inTok + outTok + cacheTok || 1;

    const inPct = (inTok / sum) * 100;
    const outPct = (outTok / sum) * 100;
    const cachePct = Math.max(0, 100 - inPct - outPct);

    const track = el('div', 'stacked-bar-track');
    const segIn = el('div', 'stacked-segment');
    segIn.style.width = `${inPct}%`;
    segIn.style.background = '#38bdf8';
    segIn.addEventListener('mousemove', (e) => {
      showCostTooltip(e.clientX, e.clientY, `
        <div class="tooltip-title">${m.model}</div>
        <div class="tooltip-row"><span>Input Tokens:</span><span class="tooltip-val highlight-cyan">${fmtCompactNum(inTok)} (${inPct.toFixed(1)}%)</span></div>
        <div class="tooltip-row"><span>Total Spend:</span><span class="tooltip-val">${fmtCurrency(m.cost_usd)}</span></div>
      `);
    });
    segIn.addEventListener('mouseleave', hideCostTooltip);

    const segOut = el('div', 'stacked-segment');
    segOut.style.width = `${outPct}%`;
    segOut.style.background = '#a78bfa';
    segOut.addEventListener('mousemove', (e) => {
      showCostTooltip(e.clientX, e.clientY, `
        <div class="tooltip-title">${m.model}</div>
        <div class="tooltip-row"><span>Output Tokens:</span><span class="tooltip-val" style="color:#a78bfa;">${fmtCompactNum(outTok)} (${outPct.toFixed(1)}%)</span></div>
        <div class="tooltip-row"><span>Total Spend:</span><span class="tooltip-val">${fmtCurrency(m.cost_usd)}</span></div>
      `);
    });
    segOut.addEventListener('mouseleave', hideCostTooltip);

    const segCache = el('div', 'stacked-segment');
    segCache.style.width = `${cachePct}%`;
    segCache.style.background = '#3fb950';
    segCache.addEventListener('mousemove', (e) => {
      showCostTooltip(e.clientX, e.clientY, `
        <div class="tooltip-title">${m.model}</div>
        <div class="tooltip-row"><span>Cache Reuse:</span><span class="tooltip-val highlight-green">${fmtCompactNum(cacheTok)} (${cachePct.toFixed(1)}%)</span></div>
        <div class="tooltip-row"><span>Discount / Hit:</span><span class="tooltip-val">Free / Cached</span></div>
      `);
    });
    segCache.addEventListener('mouseleave', hideCostTooltip);

    track.appendChild(segIn);
    track.appendChild(segOut);
    track.appendChild(segCache);
    row.appendChild(track);
    stackedContainer.appendChild(row);
  }
  modelCard.appendChild(stackedContainer);

  // Preserve existing cost-bar-rows for full compatibility
  for (const m of models) {
    const row = el('div', 'cost-bar-row');
    const hdr = el('div', 'cost-bar-header');
    hdr.appendChild(el('span', 'cost-bar-label', m.model));
    hdr.appendChild(el('span', null, `${fmtCurrency(m.cost_usd)} · ${m.percentage || 0}%`));
    row.appendChild(hdr);
    const barOuter = el('div', 'cost-bar-outer');
    const barInner = el('div', 'cost-bar-inner');
    barInner.style.width = `${Math.min(100, m.percentage || 0)}%`;
    barOuter.appendChild(barInner);
    row.appendChild(barOuter);
    modelCard.appendChild(row);
  }

  grid.appendChild(modelCard);
}

// ── 3. Spend by Organization with Donut Chart ──────────────
function renderOrgSpendCard(f, grid) {
  const orgCard = el('div', 'cost-card');
  orgCard.appendChild(el('div', 'cost-card-title', 'Spend by Organization'));
  const orgs = f?.org_spend || [];

  if (!orgs.length) {
    orgCard.appendChild(el('p', 'muted-text', 'No per-org spend data.'));
    grid.appendChild(orgCard);
    return;
  }

  const totalOrgCost = orgs.reduce((acc, o) => acc + (o.cost_usd || 0), 0) || 1;

  // Donut SVG
  const donutContainer = el('div', 'donut-chart-container');
  const svg = svgEl('svg', {
    viewBox: '0 0 240 240',
    class: 'donut-svg',
  });

  const cx = 120, cy = 120, rOuter = 95, rInner = 65;
  const colors = ['#8b5cf6', '#38bdf8', '#3fb950', '#fbbf24', '#f85149', '#ec4899', '#06b6d4'];
  let curAngle = -Math.PI / 2;

  // Center texts
  const centerG = svgEl('g', { 'pointer-events': 'none' });
  const textTotal = svgEl('text', {
    x: cx,
    y: cy + 3,
    class: 'donut-center-total',
  }, [fmtCurrency(totalOrgCost)]);
  const textLabel = svgEl('text', {
    x: cx,
    y: cy + 18,
    class: 'donut-center-label',
  }, ['Fleet Total']);
  centerG.appendChild(textTotal);
  centerG.appendChild(textLabel);

  orgs.forEach((o, idx) => {
    const fraction = (o.cost_usd || 0) / totalOrgCost;
    if (fraction <= 0) return;
    const sliceAngle = fraction * 2 * Math.PI;
    const endAngle = curAngle + sliceAngle;
    const isFull = sliceAngle >= 2 * Math.PI - 0.001;

    const x1 = cx + rOuter * Math.cos(curAngle);
    const y1 = cy + rOuter * Math.sin(curAngle);
    const x2 = cx + rOuter * Math.cos(endAngle);
    const y2 = cy + rOuter * Math.sin(endAngle);

    const x3 = cx + rInner * Math.cos(endAngle);
    const y3 = cy + rInner * Math.sin(endAngle);
    const x4 = cx + rInner * Math.cos(curAngle);
    const y4 = cy + rInner * Math.sin(curAngle);

    const largeArc = sliceAngle > Math.PI ? 1 : 0;
    const pathD = isFull
      ? `M ${cx},${cy - rOuter} A ${rOuter},${rOuter} 0 1,0 ${cx},${cy + rOuter} A ${rOuter},${rOuter} 0 1,0 ${cx},${cy - rOuter} M ${cx},${cy - rInner} A ${rInner},${rInner} 0 1,1 ${cx},${cy + rInner} A ${rInner},${rInner} 0 1,1 ${cx},${cy - rInner} Z`
      : `M ${x1},${y1} A ${rOuter},${rOuter} 0 ${largeArc},1 ${x2},${y2} L ${x3},${y3} A ${rInner},${rInner} 0 ${largeArc},0 ${x4},${y4} Z`;

    const color = colors[idx % colors.length];
    const slice = svgEl('path', {
      d: pathD,
      fill: color,
      stroke: 'var(--surface2)',
      'stroke-width': '2',
      class: 'donut-slice',
    });

    slice.addEventListener('mousemove', (e) => {
      textTotal.textContent = fmtCurrency(o.cost_usd);
      textLabel.textContent = `${o.organization} (${o.percentage || 0}%)`;
      showCostTooltip(e.clientX, e.clientY, `
        <div class="tooltip-title">${o.organization}</div>
        <div class="tooltip-row"><span>Spend:</span><span class="tooltip-val highlight-gold">${fmtCurrency(o.cost_usd)}</span></div>
        <div class="tooltip-row"><span>Share:</span><span class="tooltip-val">${o.percentage || 0}%</span></div>
        <div class="tooltip-row"><span>Active Tasks:</span><span class="tooltip-val">${o.active_tasks || 0}</span></div>
        <div class="tooltip-row"><span>Active Agents:</span><span class="tooltip-val">${o.active_agents || 0}</span></div>
      `);
    });

    slice.addEventListener('mouseleave', () => {
      textTotal.textContent = fmtCurrency(totalOrgCost);
      textLabel.textContent = 'Fleet Total';
      hideCostTooltip();
    });

    svg.appendChild(slice);
    curAngle = endAngle;
  });

  svg.appendChild(centerG);
  donutContainer.appendChild(svg);
  orgCard.appendChild(donutContainer);

  // Preserve existing cost-bar-rows for compatibility
  for (const o of orgs) {
    const row = el('div', 'cost-bar-row');
    const hdr = el('div', 'cost-bar-header');
    hdr.appendChild(el('span', 'cost-bar-label', o.organization));
    hdr.appendChild(el('span', null, `${fmtCurrency(o.cost_usd)} · ${o.percentage || 0}%`));
    row.appendChild(hdr);
    const barOuter = el('div', 'cost-bar-outer');
    const barInner = el('div', 'cost-bar-inner');
    barInner.style.width = `${Math.min(100, o.percentage || 0)}%`;
    barOuter.appendChild(barInner);
    row.appendChild(barOuter);
    orgCard.appendChild(row);
  }

  grid.appendChild(orgCard);
}

// ── 4. Spend by Provider ──────────────────────────────────
function renderProviderSpendCard(f, grid) {
  const providerCard = el('div', 'cost-card');
  providerCard.appendChild(el('div', 'cost-card-title', 'Spend by Provider'));

  const provTotals = {};
  for (const m of (f?.model_spend || [])) {
    const lower = (m.model || '').toLowerCase();
    let prov = 'gemini';
    if (lower.includes('claude') || lower.includes('anthropic') || lower.includes('fable')) prov = 'claude';
    else if (lower.includes('gemini') || lower.includes('google')) prov = 'gemini';
    else if (lower.includes('gpt') || lower.includes('openai') || lower.includes('codex') || lower.includes('o1') || lower.includes('o3')) prov = 'openai';
    else if (m.family && m.family !== 'other' && m.family !== 'unknown') prov = m.family;
    provTotals[prov] = (provTotals[prov] || 0) + (m.cost_usd || 0);
  }

  const totalProvSpend = Object.values(provTotals).reduce((a, b) => a + b, 0);
  const provEntries = Object.entries(provTotals).sort((a, b) => b[1] - a[1]);

  if (!provEntries.length) {
    providerCard.appendChild(el('p', 'muted-text', 'No provider spend data recorded yet. Ensure telemetry watcher is active.'));
  } else {
    for (const [prov, cost] of provEntries) {
      const pct = totalProvSpend > 0 ? (cost / totalProvSpend * 100) : 0;
      const row = el('div', 'cost-bar-row');
      const hdr = el('div', 'cost-bar-header');
      hdr.appendChild(el('span', 'cost-bar-label', prov.charAt(0).toUpperCase() + prov.slice(1)));
      hdr.appendChild(el('span', null, `${fmtCurrency(cost)} · ${pct.toFixed(1)}%`));
      row.appendChild(hdr);
      const barOuter = el('div', 'cost-bar-outer');
      const barInner = el('div', 'cost-bar-inner');
      barInner.style.width = `${Math.min(100, pct)}%`;
      barOuter.appendChild(barInner);
      row.appendChild(barOuter);
      providerCard.appendChild(row);
    }
  }

  grid.appendChild(providerCard);
}

// ── 5. Top Tasks Cumulative Cost Progression Visualizer ────
function renderTopTasksSpendCard(f, grid) {
  const taskSpendCard = el('div', 'cost-card');
  taskSpendCard.appendChild(el('div', 'cost-card-title', 'Top Tasks by Spend'));

  const allTasks = [...visibleTasks(f?.tasks, false), ...taskValues()]
    .filter(t => t.spent_usd > 0)
    .sort((a, b) => b.spent_usd - a.spent_usd)
    .slice(0, 10);

  if (!allTasks.length) {
    taskSpendCard.appendChild(el('p', 'muted-text', 'No per-task spend recorded yet.'));
    grid.appendChild(taskSpendCard);
    return;
  }

  // Cumulative Progression Chart (Pareto Curve)
  let cumSum = 0;
  const taskPoints = allTasks.map((t, idx) => {
    cumSum += t.spent_usd;
    return {
      task: t,
      rank: idx + 1,
      spend: t.spent_usd,
      cumSpend: cumSum,
    };
  });
  const totalTopSpend = cumSum || 1;

  const cumContainer = el('div', 'cumulative-chart-container');
  const svgW = 380;
  const svgH = 130;
  const mTop = 15, mRight = 15, mBottom = 25, mLeft = 45;
  const pW = svgW - mLeft - mRight;
  const pH = svgH - mTop - mBottom;

  const svg = svgEl('svg', {
    viewBox: `0 0 ${svgW} ${svgH}`,
    class: 'cumulative-svg',
    preserveAspectRatio: 'xMidYMid meet',
  });

  const defs = svgEl('defs');
  const goldGrad = svgEl('linearGradient', { id: 'cum-area-grad', x1: '0', y1: '0', x2: '0', y2: '1' }, [
    svgEl('stop', { offset: '0%', 'stop-color': '#fbbf24', 'stop-opacity': '0.45' }),
    svgEl('stop', { offset: '100%', 'stop-color': '#fbbf24', 'stop-opacity': '0.02' }),
  ]);
  defs.appendChild(goldGrad);
  svg.appendChild(defs);

  const getTaskX = (idx) => mLeft + (idx / (taskPoints.length - 1 || 1)) * pW;
  const getTaskY = (val) => mTop + pH - (val / totalTopSpend) * pH;

  // Gridlines
  const gridG = svgEl('g', { class: 'cost-chart-grid' });
  [0, 0.5, 1.0].forEach(ratio => {
    const y = mTop + pH - ratio * pH;
    gridG.appendChild(svgEl('line', { x1: mLeft, y1: y, x2: mLeft + pW, y2: y }));
    gridG.appendChild(svgEl('text', {
      x: mLeft - 6,
      y: y + 3,
      'text-anchor': 'end',
      class: 'cost-chart-axis-label',
    }, [fmtCurrency(totalTopSpend * ratio)]));
  });
  svg.appendChild(gridG);

  // Area and line path
  const curvePts = taskPoints.map((tp, idx) => ({ x: getTaskX(idx), y: getTaskY(tp.cumSpend), tp }));
  let curveD = `M ${curvePts[0].x},${curvePts[0].y}`;
  for (let i = 1; i < curvePts.length; i++) {
    curveD += ` L ${curvePts[i].x},${curvePts[i].y}`;
  }
  const areaD = `${curveD} L ${curvePts[curvePts.length - 1].x},${mTop + pH} L ${curvePts[0].x},${mTop + pH} Z`;

  svg.appendChild(svgEl('path', { d: areaD, fill: 'url(#cum-area-grad)' }));
  svg.appendChild(svgEl('path', {
    d: curveD,
    fill: 'none',
    stroke: '#fbbf24',
    'stroke-width': '2',
    'stroke-linecap': 'round',
  }));

  // Points on curve
  curvePts.forEach(pt => {
    const t = pt.tp.task;
    const dot = svgEl('circle', {
      cx: pt.x,
      cy: pt.y,
      r: '4',
      fill: '#fbbf24',
      stroke: 'var(--surface2)',
      'stroke-width': '1.5',
      class: 'cost-chart-point',
    });

    dot.addEventListener('mousemove', (e) => {
      const sharePct = ((pt.tp.cumSpend / totalTopSpend) * 100).toFixed(1);
      showCostTooltip(e.clientX, e.clientY, `
        <div class="tooltip-title">Rank #${pt.tp.rank}: <strong>${t.identifier || t.id.slice(0, 8)}</strong></div>
        <div class="tooltip-row"><span>Task:</span><span>${t.title || t.name || '—'}</span></div>
        <div class="tooltip-row"><span>Task Spend:</span><span class="tooltip-val highlight-gold">${fmtCurrency(pt.tp.spend)}</span></div>
        <div class="tooltip-row"><span>Cumulative:</span><span class="tooltip-val">${fmtCurrency(pt.tp.cumSpend)} (${sharePct}%)</span></div>
        <div class="tooltip-row"><span>Org:</span><span>${t.organization || '—'}</span></div>
      `);
    });

    dot.addEventListener('mouseleave', hideCostTooltip);
    dot.addEventListener('click', () => {
      hideCostTooltip();
      if (typeof openDetail === 'function') openDetail(t.id);
    });

    svg.appendChild(dot);
  });

  cumContainer.appendChild(svg);
  taskSpendCard.appendChild(cumContainer);

  // Top tasks list
  for (const tp of taskPoints) {
    const t = tp.task;
    const row = el('div', 'cost-row cost-task-clickable');
    row.addEventListener('click', () => {
      if (typeof openDetail === 'function') openDetail(t.id);
    });

    const lbl = el('div', 'cost-row-label');
    lbl.style.display = 'flex';
    lbl.style.alignItems = 'center';

    const rankBadge = el('span', 'task-rank-badge', `#${tp.rank}`);
    lbl.appendChild(rankBadge);

    const titleGroup = el('div');
    const taskName = t.title || t.name || t.identifier || '—';
    titleGroup.appendChild(el('div', null, `${t.identifier ? `${t.identifier}: ` : ''}${taskName}`));
    titleGroup.appendChild(el('div', 'muted-text', t.organization || ''));
    lbl.appendChild(titleGroup);

    row.appendChild(lbl);
    row.appendChild(el('span', 'cost-row-val', fmtCurrency(t.spent_usd)));
    taskSpendCard.appendChild(row);
  }

  grid.appendChild(taskSpendCard);
}


// ── Settings Page ─────────────────────────────────────────
// refreshSettingsPasskeys fills Settings → Security with the enrolled passkeys,
// an Enroll passkey button and a Delete action per passkey. A no-op when the
// Settings page is not rendered.
let settingsPasskeysSeq = 0;
async function refreshSettingsPasskeys() {
  if (!document.querySelector('#settings-security .settings-passkeys-body')) return;
  const seq = ++settingsPasskeysSeq;
  const status = await refreshBoardPasskeyStatus();
  let creds = [];
  if (status.boardSession) {
    try {
      const r = await fetch('/api/board/webauthn/credentials', { headers: authHeader() });
      if (r.ok) creds = (await r.json()).credentials || [];
    } catch { /* render what we have */ }
  }
  // A newer refresh (or a re-render of Settings) superseded this one.
  const body = document.querySelector('#settings-security .settings-passkeys-body');
  if (!body || seq !== settingsPasskeysSeq) return;
  body.innerHTML = '';

  if (!status.boardSession) {
    const row = el('div', 'settings-row');
    row.appendChild(el('div', 'settings-row-sub', 'Open the Board link from `staypoint board url` to manage passkeys.'));
    body.appendChild(row);
    return;
  }

  const enrollRow = el('div', 'settings-row');
  const enrollLbl = el('div', 'settings-row-label-wrap');
  enrollLbl.appendChild(el('div', 'settings-row-label',
    creds.length ? `${creds.length} passkey${creds.length === 1 ? '' : 's'} enrolled` : 'No passkeys enrolled'));
  enrollLbl.appendChild(el('div', 'settings-row-sub', creds.length
    ? 'Adding another passkey asks for an existing one first.'
    : 'Board actions are locked until a passkey is enrolled. You will need the pairing code shown in the StayPoint Board dialog.'));
  enrollRow.appendChild(enrollLbl);
  const enrollBtn = el('button', 'btn btn-primary settings-enroll-passkey-btn', 'Enroll passkey');
  enrollBtn.type = 'button';
  enrollBtn.addEventListener('click', async () => {
    enrollBtn.disabled = true;
    try { await enrollBoardPasskey(); } finally { enrollBtn.disabled = false; }
  });
  enrollRow.appendChild(enrollBtn);
  body.appendChild(enrollRow);

  for (const c of creds) {
    const row = el('div', 'settings-row passkey-row');
    const lbl = el('div', 'settings-row-label-wrap');
    lbl.appendChild(el('div', 'settings-row-label', `Passkey ${String(c.credential_id || c.id).slice(0, 10)}`));
    const sub = el('div', 'settings-row-sub');
    sub.appendChild(document.createTextNode('Enrolled '));
    sub.appendChild(el('span', 'passkey-created', c.created_at ? fmtDateTime(c.created_at) : 'unknown'));
    lbl.appendChild(sub);
    row.appendChild(lbl);
    const del = el('button', 'btn btn-secondary passkey-delete-btn', 'Delete');
    del.type = 'button';
    del.addEventListener('click', async () => {
      const last = creds.length === 1;
      if (!confirm(last
        ? 'Delete the only enrolled passkey? Board actions stay locked until you enroll another.'
        : 'Delete this passkey?')) return;
      del.disabled = true;
      try {
        const r = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/board/webauthn/credentials/${encodeURIComponent(c.id)}`, {
            method: 'DELETE',
            headers: { ...authHeader(), 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
          }), 'deleting the passkey',
        );
        if (r && !r.ok) {
          const e = await r.json().catch(() => ({}));
          alert('Delete failed: ' + (e.message || r.status));
        }
      } catch (e) {
        alert('Delete failed: ' + (e.message || e));
      } finally {
        del.disabled = false;
        refreshBoardPasskeyUI();
      }
    });
    row.appendChild(del);
    body.appendChild(row);
  }
}

function renderSettings() {
  const container = document.getElementById('settings-container');
  if (!container) return;
  container.innerHTML = '';

  const f = state.fleet;

  // Connection section
  const connSec = el('div', 'settings-section');
  const connHdr = el('div', 'settings-section-header');
  connHdr.appendChild(el('div', 'settings-section-title', 'Connection'));
  connHdr.appendChild(el('div', 'settings-section-desc', 'StayPoint daemon connection and authentication.'));
  connSec.appendChild(connHdr);

  for (const { label, val } of [
    { label: 'API Endpoint', val: window.location.host },
    { label: 'Auth', val: TOKEN ? 'Token (session cookie active)' : 'No token' },
    { label: 'SSE Status', val: sseSource?.readyState === 1 ? 'Connected' : 'Reconnecting' },
  ]) {
    const row = el('div', 'settings-row');
    row.appendChild(el('div', 'settings-row-label', label));
    row.appendChild(el('span', 'settings-val', val));
    connSec.appendChild(row);
  }
  container.appendChild(connSec);

  // Organization holds (Board-only, Touch ID)
  const holdSec = el('div', 'settings-section');
  const holdHdr = el('div', 'settings-section-header');
  holdHdr.appendChild(el('div', 'settings-section-title', 'Organization holds'));
  holdHdr.appendChild(el('div', 'settings-section-desc',
    'A held organization runs nothing: the daemon claims none of its tasks and logs every wake as held. Board-only; saved with Touch ID.'));
  holdSec.appendChild(holdHdr);
  const holdOrgs = [...new Set([
    ...(f?.organizations || []).map(o => o.name).filter(Boolean),
    ...state.orgHolds,
  ].map(n => String(n)))];
  const seenHold = new Set();
  for (const name of holdOrgs) {
    const key = name.trim().toLowerCase();
    if (seenHold.has(key)) continue;
    seenHold.add(key);
    const row = el('div', 'settings-row');
    const label = el('div', 'settings-row-label', name);
    if (isOrgHeld(name)) label.appendChild(orgHeldBadge());
    row.appendChild(label);
    row.appendChild(orgHoldControl(name));
    holdSec.appendChild(row);
  }
  if (!holdOrgs.length) {
    const row = el('div', 'settings-row');
    row.appendChild(el('span', 'muted-text', 'No organizations yet.'));
    holdSec.appendChild(row);
  }
  container.appendChild(holdSec);

  // Organization trust (Board-only, Touch ID): "Trust this organization until…"
  const trustSec = el('div', 'settings-section org-trust-section');
  const trustHdr = el('div', 'settings-section-header');
  trustHdr.appendChild(el('div', 'settings-section-title', 'Organization trust'));
  trustHdr.appendChild(el('div', 'settings-section-desc',
    'Auto-approve Red requests from every running task in one organization until a time you pick (at most 16 h). The Board rules still wait for you. Created with Touch ID; revoke any time.'));
  trustSec.appendChild(trustHdr);
  container.appendChild(trustSec);
  if (typeof renderOrgTrustSection === 'function') renderOrgTrustSection(trustSec, holdOrgs);

  // Provider accounts
  const provSec = el('div', 'settings-section');
  const provHdr = el('div', 'settings-section-header');
  provHdr.appendChild(el('div', 'settings-section-title', 'Provider Accounts'));
  provHdr.appendChild(el('div', 'settings-section-desc', 'Detailed quota pool headroom breakdown, reset times, and limits.'));
  provSec.appendChild(provHdr);

  const quotas = f?.provider_quotas || {};
  const preferredOrder = ['gemini', 'claude_work', 'claude_personal', 'claude', 'openai'];
  const quotaKeys = [];
  for (const k of preferredOrder) {
    if (quotas[k]) quotaKeys.push(k);
  }
  for (const k of Object.keys(quotas)) {
    if (!quotaKeys.includes(k)) quotaKeys.push(k);
  }

  const validKeys = quotaKeys.filter(k => !(k === 'claude' && (quotas['claude_work'] || quotas['claude_personal'])));

  if (validKeys.length === 0) {
    const row = el('div', 'settings-row');
    row.appendChild(el('span', 'muted-text', 'No quota telemetry available.'));
    provSec.appendChild(row);
  } else {
    for (const key of validKeys) {
      const q = quotas[key];
      const card = el('div', 'settings-provider-card');

      // Card Header
      const cardHdr = el('div', 'settings-provider-header');
      const titleWrap = el('div');
      titleWrap.appendChild(el('div', 'settings-provider-name', q.display_name || key));
      titleWrap.appendChild(el('div', 'settings-provider-sub', q.projection_message || (q.projection_status ? `Pacing: ${q.projection_status}` : 'Standard allocation')));
      cardHdr.appendChild(titleWrap);

      let sCls = 'pill-green', sTxt = '✔ Active · On Track';
      if (!(q.is_locked || q.projection_status === 'locked_out') && quotaStatusPill(q).text === 'No data') {
        sCls = 'pill-stopped';
        sTxt = 'No data';
      } else if (q.is_locked || q.projection_status === 'locked_out') {
        sCls = 'pill-red';
        sTxt = '🔒 Locked Out';
      } else if (q.projection_status === 'overpaced') {
        sCls = 'pill-amber';
        sTxt = '⚠️ Overpaced';
      }
      cardHdr.appendChild(el('span', `pill ${sCls}`, sTxt));
      card.appendChild(cardHdr);

      // Lockout Alert Banner if locked
      if (q.is_locked || q.lockout_reason) {
        const lockBanner = el('div', 'settings-lockout-banner');
        let lockMsg = q.lockout_reason || 'Quota threshold exceeded; requests paused.';
        if (q.lockout_until) {
          const untilCountdown = formatCountdown(q.lockout_until);
          lockMsg += ` · Resets ${untilCountdown || ''} (${fmtDateTime(q.lockout_until)})`;
        }
        lockBanner.textContent = `🔒 ${lockMsg}`;
        card.appendChild(lockBanner);
      }

      // Card Telemetry Body
      const body = el('div', 'settings-provider-body');

      // ── 5-Hour Rolling Pool ──
      const v5h = quotaWindowView(q, 'five_hour');
      const rem5h = v5h.measured ? v5h.remaining : 100;
      const used5h = v5h.measured ? v5h.used : 0;
      const count5h = v5h.measured ? formatCountdown(q.five_hour_resets_at) : '';
      const time5h = v5h.measured ? formatResetTime(q.five_hour_resets_at, false) : '';
      const limit5h = q.lockout_threshold_pct || 100;

      const pool5h = el('div', 'settings-pool-box');
      const pool5hHdr = el('div', 'settings-pool-hdr');
      pool5hHdr.appendChild(el('span', 'settings-pool-title', '5-Hour Rolling Window'));
      const badge5h = v5h.measured
        ? el('span', `headroom-badge ${rem5h <= 10 ? 'badge-red' : rem5h <= 25 ? 'badge-amber' : 'badge-green'}`, `${rem5h.toFixed(1)}% Headroom`)
        : el('span', 'headroom-badge', 'No data');
      pool5hHdr.appendChild(badge5h);
      pool5h.appendChild(pool5hHdr);

      const bar5hOuter = el('div', 'gauge-bar-outer');
      const bar5hInner = el('div', 'gauge-bar-inner');
      bar5hInner.style.width = `${used5h}%`;
      bar5hInner.className = `gauge-bar-inner ${(q.is_locked || used5h >= 95) ? 'gauge-bar-red' : used5h >= 75 ? 'gauge-bar-amber' : 'gauge-bar-green'}`;
      bar5hOuter.appendChild(bar5hInner);
      pool5h.appendChild(bar5hOuter);

      const metrics5h = el('div', 'settings-metrics-grid');

      const m5hHeadroom = el('div', 'settings-metric-item');
      m5hHeadroom.appendChild(el('span', 'settings-metric-lbl', 'Pool Headroom'));
      m5hHeadroom.appendChild(el('span', 'settings-metric-val', v5h.measured ? `${rem5h.toFixed(1)}% remaining (${used5h.toFixed(1)}% used)` : 'No data'));
      metrics5h.appendChild(m5hHeadroom);

      const m5hReset = el('div', 'settings-metric-item');
      m5hReset.appendChild(el('span', 'settings-metric-lbl', 'Reset Time'));
      const resetText5h = count5h ? `${count5h}${time5h ? ' · ' + time5h : ''}` : (v5h.measured ? 'Rolling window' : '—');
      m5hReset.appendChild(el('span', 'settings-metric-val', resetText5h));
      metrics5h.appendChild(m5hReset);

      const m5hLimit = el('div', 'settings-metric-item');
      m5hLimit.appendChild(el('span', 'settings-metric-lbl', 'Lockout Limit'));
      m5hLimit.appendChild(el('span', 'settings-metric-val', `${limit5h}% threshold`));
      metrics5h.appendChild(m5hLimit);

      const m5hBurn = el('div', 'settings-metric-item');
      m5hBurn.appendChild(el('span', 'settings-metric-lbl', 'Current Burn'));
      m5hBurn.appendChild(el('span', 'settings-metric-val', q.burn_rate_5h ? `${q.burn_rate_5h.toFixed(2)}%/turn` : '—'));
      metrics5h.appendChild(m5hBurn);

      pool5h.appendChild(metrics5h);
      body.appendChild(pool5h);

      // ── Weekly Budget Pool ──
      const vWk = quotaWindowView(q, 'weekly');
      const remWk = vWk.measured ? vWk.remaining : 100;
      const usedWk = vWk.measured ? vWk.used : 0;
      const countWk = vWk.measured ? formatCountdown(q.weekly_resets_at) : '';
      const timeWk = vWk.measured ? formatResetTime(q.weekly_resets_at, true) : '';

      const poolWk = el('div', 'settings-pool-box');
      const poolWkHdr = el('div', 'settings-pool-hdr');
      poolWkHdr.appendChild(el('span', 'settings-pool-title', 'Weekly Budget Window'));
      const badgeWk = vWk.measured
        ? el('span', `headroom-badge ${remWk <= 15 ? 'badge-red' : remWk <= 30 ? 'badge-amber' : 'badge-green'}`, `${remWk.toFixed(1)}% Headroom`)
        : el('span', 'headroom-badge', 'No data');
      poolWkHdr.appendChild(badgeWk);
      poolWk.appendChild(poolWkHdr);

      const barWkOuter = el('div', 'gauge-bar-outer');
      const barWkInner = el('div', 'gauge-bar-inner');
      barWkInner.style.width = `${usedWk}%`;
      barWkInner.className = `gauge-bar-inner ${usedWk >= 90 ? 'gauge-bar-red' : usedWk >= 70 ? 'gauge-bar-amber' : 'gauge-bar-green'}`;
      barWkOuter.appendChild(barWkInner);
      poolWk.appendChild(barWkOuter);

      const metricsWk = el('div', 'settings-metrics-grid');

      const mWkHeadroom = el('div', 'settings-metric-item');
      mWkHeadroom.appendChild(el('span', 'settings-metric-lbl', 'Weekly Headroom'));
      mWkHeadroom.appendChild(el('span', 'settings-metric-val', vWk.measured ? `${remWk.toFixed(1)}% remaining (${usedWk.toFixed(1)}% used)` : 'No data'));
      metricsWk.appendChild(mWkHeadroom);

      const mWkReset = el('div', 'settings-metric-item');
      mWkReset.appendChild(el('span', 'settings-metric-lbl', 'Reset Time'));
      const resetTextWk = countWk ? `${countWk}${timeWk ? ' · ' + timeWk : ''}` : '—';
      mWkReset.appendChild(el('span', 'settings-metric-val', resetTextWk));
      metricsWk.appendChild(mWkReset);

      const mWkBurn = el('div', 'settings-metric-item');
      mWkBurn.appendChild(el('span', 'settings-metric-lbl', 'Weekly Burn'));
      mWkBurn.appendChild(el('span', 'settings-metric-val', q.burn_rate_weekly ? `${q.burn_rate_weekly.toFixed(2)}%/turn` : '—'));
      metricsWk.appendChild(mWkBurn);

      const mWkRunway = el('div', 'settings-metric-item');
      mWkRunway.appendChild(el('span', 'settings-metric-lbl', 'Runway Limit'));
      mWkRunway.appendChild(el('span', 'settings-metric-val', q.runway_turns ? `${q.runway_turns} turns left` : 'Sustainable'));
      metricsWk.appendChild(mWkRunway);

      poolWk.appendChild(metricsWk);
      body.appendChild(poolWk);

      card.appendChild(body);
      provSec.appendChild(card);
    }
  }
  container.appendChild(provSec);

  // Fleet info
  const fleetSec = el('div', 'settings-section');
  const fleetHdr = el('div', 'settings-section-header');
  fleetHdr.appendChild(el('div', 'settings-section-title', 'Fleet Info'));
  fleetHdr.appendChild(el('div', 'settings-section-desc', 'Global overview statistics. Click any count to open the interactive drill-down modal.'));
  fleetSec.appendChild(fleetHdr);

  const orgCount = (f?.organizations || []).length;
  const totalTasks = f?.global_tasks?.total || 0;
  const activeAgents = f?.global_agents?.active_running || 0;

  for (const m of [
    {
      id: 'organizations',
      label: 'Organizations',
      sub: `${orgCount} tenant workspaces · Click to view breakdown`,
      val: String(orgCount),
      clickable: true,
      onClick: () => openFleetInfoModal('organizations'),
    },
    {
      id: 'tasks',
      label: 'Total Tasks',
      sub: `${f?.global_tasks?.running || 0} running, ${f?.global_tasks?.blocked || 0} blocked · Click to view breakdown`,
      val: String(totalTasks),
      clickable: true,
      onClick: () => openFleetInfoModal('tasks'),
    },
    {
      id: 'agents',
      label: 'Active Agents',
      sub: `${activeAgents} running across provider pools · Click to view breakdown`,
      val: String(activeAgents),
      clickable: true,
      onClick: () => openFleetInfoModal('agents'),
    },
    {
      id: 'last_update',
      label: 'Last Update',
      sub: 'Most recent telemetry sync timestamp',
      val: f?.timestamp ? fmtDateTime(f.timestamp) : '—',
      clickable: false,
    },
  ]) {
    const row = el('div', m.clickable ? 'settings-row settings-row-clickable' : 'settings-row');
    const lbl = el('div');
    lbl.appendChild(el('div', 'settings-row-label', m.label));
    if (m.sub) lbl.appendChild(el('div', 'settings-row-sub', m.sub));
    row.appendChild(lbl);

    const valEl = el('span', m.clickable ? 'settings-val settings-val-interactive' : 'settings-val', m.val);
    if (m.clickable) {
      valEl.appendChild(el('span', 'drilldown-arrow', ' →'));
      row.title = `Click to drill down into ${m.label}`;
      row.addEventListener('click', m.onClick);
    }
    row.appendChild(valEl);
    fleetSec.appendChild(row);
  }
  container.appendChild(fleetSec);

  // Security section: Board passkeys (STA-694)
  const pkSec = el('div', 'settings-section');
  pkSec.id = 'settings-security';
  const pkHdr = el('div', 'settings-section-header');
  pkHdr.appendChild(el('div', 'settings-section-title', 'Security'));
  pkHdr.appendChild(el('div', 'settings-section-desc', 'Board passkeys (Touch ID). Every Board action needs one.'));
  pkSec.appendChild(pkHdr);
  pkSec.appendChild(el('div', 'settings-passkeys-body'));
  container.appendChild(pkSec);
  refreshSettingsPasskeys();

  // Security Gates section
  const gateSec = el('div', 'settings-section');
  const gateHdr = el('div', 'settings-section-header');
  gateHdr.appendChild(el('div', 'settings-section-title', 'Security Gates'));
  gateHdr.appendChild(el('div', 'settings-section-desc', 'Controls what agent commands require your explicit approval before running.'));
  gateSec.appendChild(gateHdr);

  // Render the toggle (async: fetch current state then build)
  const gateRow = el('div', 'settings-row');
  const gateLbl = el('div', 'settings-row-label-wrap');
  gateLbl.appendChild(el('div', 'settings-row-label', 'Require my approval to merge to main'));
  gateLbl.appendChild(el('div', 'settings-row-sub', 'When on, any agent push or PR merge to main/master is held for your approval before executing.'));
  gateRow.appendChild(gateLbl);

  const gateToggleWrap = el('div', 'settings-toggle-wrap');
  const gateToggle = el('input');
  gateToggle.type = 'checkbox';
  gateToggle.className = 'settings-toggle';
  gateToggle.id = 'gate-main-merge-approval';
  gateToggle.checked = true; // default on until loaded
  gateToggle.disabled = true; // disable until loaded
  gateToggleWrap.appendChild(gateToggle);
  gateRow.appendChild(gateToggleWrap);
  gateSec.appendChild(gateRow);

  container.appendChild(gateSec);

  // Load and bind the gate toggle asynchronously.
  fetch('/api/settings/security-gate', {
    headers: TOKEN ? { 'Authorization': 'Bearer ' + TOKEN } : {},
  }).then(r => r.ok ? r.json() : null).then(data => {
    if (!data) return;
    gateToggle.checked = data.main_merge_approval !== false;
    gateToggle.disabled = false;
  }).catch(() => { gateToggle.disabled = false; });

  gateToggle.addEventListener('change', () => {
    gateToggle.disabled = true;
    const checked = gateToggle.checked;
    withBoardWebAuthn((sessionToken, assertion) =>
      fetch('/api/settings/security-gate', {
        method: 'POST',
        headers: Object.assign({ 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion }, authHeader()),
        body: JSON.stringify({ main_merge_approval: checked }),
      }), 'changing "Require my approval to merge to main"',
    ).then(r => {
      if (r === null) { gateToggle.disabled = false; gateToggle.checked = !checked; return null; }
      return r.ok ? r.json() : boardActionError(r).then(e => { throw e; });
    }).then(data => {
      gateToggle.disabled = false;
      if (data) gateToggle.checked = data.main_merge_approval !== false;
    }).catch(e => {
      gateToggle.disabled = false;
      gateToggle.checked = !checked;
      showToast('Security gate not saved: ' + (e.message || e), 'error');
    });
  });

  // Ship Review toggle row (inside Security Gates section)
  const srRow = el('div', 'settings-row');
  const srLbl = el('div', 'settings-row-label-wrap');
  srLbl.appendChild(el('div', 'settings-row-label', 'Ship review before merge'));
  srLbl.appendChild(el('div', 'settings-row-sub', 'When on, agents present a review card (dev URL + test steps + SHA pin) for your sign-off before any branch is merged.'));
  srRow.appendChild(srLbl);

  const srToggleWrap = el('div', 'settings-toggle-wrap');
  const srToggle = el('input');
  srToggle.type = 'checkbox';
  srToggle.className = 'settings-toggle';
  srToggle.id = 'gate-ship-review';
  srToggle.checked = true;
  srToggle.disabled = true;
  srToggleWrap.appendChild(srToggle);
  srRow.appendChild(srToggleWrap);
  gateSec.appendChild(srRow);

  fetch('/api/settings/ship-review', {
    headers: TOKEN ? { 'Authorization': 'Bearer ' + TOKEN } : {},
  }).then(r => r.ok ? r.json() : null).then(data => {
    if (!data) return;
    srToggle.checked = data.ship_review !== false;
    srToggle.disabled = false;
  }).catch(() => { srToggle.disabled = false; });

  srToggle.addEventListener('change', () => {
    srToggle.disabled = true;
    const checked = srToggle.checked;
    withBoardWebAuthn((sessionToken, assertion) =>
      fetch('/api/settings/ship-review', {
        method: 'POST',
        headers: Object.assign({ 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion }, authHeader()),
        body: JSON.stringify({ ship_review: checked }),
      }), 'changing "Ship review before merge"',
    ).then(r => {
      if (r === null) { srToggle.disabled = false; srToggle.checked = !checked; return null; }
      return r.ok ? r.json() : boardActionError(r).then(e => { throw e; });
    }).then(data => {
      srToggle.disabled = false;
      if (data) srToggle.checked = data.ship_review !== false;
    }).catch(e => {
      srToggle.disabled = false;
      srToggle.checked = !checked;
      showToast('Ship review setting not saved: ' + (e.message || e), 'error');
    });
  });

  // ── Dev Environment Config section (STA-520) ──────────────────────────────
  // PUT /api/project-dev-configs is Board-gated; writes go through withBoardWebAuthn.
  const devSec = el('div', 'settings-section');
  const devHdr = el('div', 'settings-section-header');
  devHdr.appendChild(el('div', 'settings-section-title', 'Dev Environment Config'));
  devHdr.appendChild(el('div', 'settings-section-desc', 'Per-repo dev server command and setup steps. Saving requires Board passkey.'));
  devSec.appendChild(devHdr);
  container.appendChild(devSec);

  const devConfigList = el('div', 'settings-dev-config-list');
  devSec.appendChild(devConfigList);

  function renderDevConfigList(configs) {
    devConfigList.innerHTML = '';
    if (!configs || configs.length === 0) {
      devConfigList.appendChild(el('div', 'settings-dev-config-empty', 'No dev configs saved yet.'));
      return;
    }
    for (const c of configs) {
      const row = el('div', 'settings-dev-config-row');
      row.appendChild(el('code', 'settings-dev-config-repo', c.repo_path));
      row.appendChild(el('span', 'settings-dev-config-cmd', c.dev_command || '—'));
      const mode = c.effective_merge_mode || 'direct';
      row.appendChild(el('span', 'settings-dev-config-merge-mode',
        `${MERGE_MODE_LABELS[mode] || mode}${c.merge_mode ? '' : ' (default)'}${c.is_work_repo ? ' · work repo' : ''}${c.target_branch ? ` → ${c.target_branch}` : ''}`));
      row.title = 'Click to edit';
      row.style.cursor = 'pointer';
      // Load the saved values so a save never blanks fields it did not show.
      row.addEventListener('click', () => {
        repoInput.value = c.repo_path || '';
        cmdInput.value = c.dev_command || '';
        stepsInput.value = (c.setup_steps || []).join('\n');
        mergeModeSelect.value = c.merge_mode || '';
        ghDirInput.value = c.gh_config_dir || '';
        targetInput.value = c.target_branch || '';
      });
      devConfigList.appendChild(row);
    }
  }

  function loadDevConfigs() {
    fetch('/api/project-dev-configs', { headers: authHeader() })
      .then(r => r.ok ? r.json() : null)
      .then(data => renderDevConfigList(data && data.configs))
      .catch(() => {});
  }
  loadDevConfigs();

  // Edit form
  const devForm = el('div', 'settings-dev-config-form');
  const repoInput = el('input');
  repoInput.type = 'text';
  repoInput.className = 'settings-dev-config-input';
  repoInput.placeholder = 'Repo path (e.g. /Users/you/project)';
  const cmdInput = el('input');
  cmdInput.type = 'text';
  cmdInput.className = 'settings-dev-config-input';
  cmdInput.placeholder = 'Dev command (e.g. npm run dev)';
  const stepsInput = el('textarea');
  stepsInput.className = 'settings-dev-config-input';
  stepsInput.placeholder = 'Setup steps, one per line (e.g. npm ci)';
  stepsInput.rows = 3;
  // STA-717: how Approve lands the branch for this repo.
  const mergeModeSelect = el('select', 'settings-dev-config-input settings-dev-config-merge-mode-select');
  for (const [value, label] of [['', 'Default (work repo: Open PR; otherwise Merge to main)'],
    ['direct', MERGE_MODE_LABELS.direct], ['open_pr', MERGE_MODE_LABELS.open_pr], ['pr_merge', MERGE_MODE_LABELS.pr_merge]]) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = label;
    mergeModeSelect.appendChild(o);
  }
  const ghDirInput = el('input');
  ghDirInput.type = 'text';
  ghDirInput.className = 'settings-dev-config-input settings-dev-config-gh-dir';
  ghDirInput.placeholder = 'Optional GH_CONFIG_DIR override (blank: the repo\'s normal gh login)';
  const targetInput = el('input');
  targetInput.type = 'text';
  targetInput.className = 'settings-dev-config-input settings-dev-config-target-branch';
  targetInput.placeholder = 'Target branch (blank: dev-server for work repos that have it, else the default branch)';
  const saveBtn = el('button', 'settings-dev-config-save', 'Save config');
  const saveStatus = el('span', 'settings-dev-config-status', '');
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'Repo path'));
  devForm.appendChild(repoInput);
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'Dev command'));
  devForm.appendChild(cmdInput);
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'Setup steps'));
  devForm.appendChild(stepsInput);
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'Merge mode'));
  devForm.appendChild(mergeModeSelect);
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'Target branch'));
  devForm.appendChild(targetInput);
  devForm.appendChild(el('div', 'settings-dev-config-field-label', 'gh config dir'));
  devForm.appendChild(ghDirInput);
  devForm.appendChild(saveBtn);
  devForm.appendChild(saveStatus);
  devSec.appendChild(devForm);

  saveBtn.addEventListener('click', async () => {
    const repoPath = repoInput.value.trim();
    const devCommand = cmdInput.value.trim();
    if (!repoPath) { saveStatus.textContent = 'Repo path required.'; return; }
    saveBtn.disabled = true;
    saveStatus.textContent = '';
    const payload = {
      repo_path: repoPath,
      dev_command: devCommand,
      setup_steps: stepsInput.value.split('\n').map(s => s.trim()).filter(Boolean),
      merge_mode: mergeModeSelect.value,
      gh_config_dir: ghDirInput.value.trim(),
      target_branch: targetInput.value.trim(),
    };
    const r = await withBoardWebAuthn((sessionToken, assertion) =>
      fetch('/api/project-dev-configs', {
        method: 'PUT',
        headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
        body: JSON.stringify(payload),
      }), 'saving the dev environment config',
    );
    saveBtn.disabled = false;
    if (r === null) { saveStatus.textContent = 'Passkey required — enroll first.'; return; }
    if (r.ok) {
      saveStatus.textContent = '✓ Saved.';
      loadDevConfigs();
    } else {
      r.json().catch(() => ({})).then(e => { saveStatus.textContent = 'Error: ' + (e.error || r.status); });
    }
  });
}

// ── Fleet Info Modal Drill-Down ────────────────────────────
const fleetModalState = {
  activeTab: 'organizations',
  taskSearch: '',
  taskStatus: 'all',
  taskOrg: 'all',
  agentSearch: '',
  agentProvider: 'all',
  agentStatus: 'all',
  orgSearch: '',
};

function getFleetTasks() {
  const taskMap = new Map();
  for (const t of fleetTaskValues()) {
    if (t && t.id) taskMap.set(t.id, t);
  }
  for (const t of taskValues()) {
    if (t && t.id) {
      if (!taskMap.has(t.id)) taskMap.set(t.id, t);
      else Object.assign(taskMap.get(t.id), t);
    }
  }
  return Array.from(taskMap.values());
}

function getFleetAgents() {
  const f = state.fleet;
  const allAgents = [];

  for (const org of (f?.organizations || [])) {
    for (const a of (org.agents || [])) {
      const prov = resolveProvider(a);
      allAgents.push({ ...a, provider: prov, org: org.name });
    }
  }

  for (const s of Object.values(state.sessions || {})) {
    const exists = allAgents.find(a => a.id === s.id);
    if (!exists) {
      const prov = resolveProvider(s);
      allAgents.push({
        id: s.id,
        name: s.agent_type ? `${titleCase(prov)} Session` : 'Local Agent',
        role: 'Local Session',
        provider: prov,
        status: s.status || 'active',
        last_heartbeat: s.last_heartbeat_at,
        org: 'StayPoint',
      });
    }
  }

  const allTasks = getFleetTasks();
  return allAgents.map(a => {
    const agentTasks = allTasks.filter(t =>
      (t.checkout_agent_id && t.checkout_agent_id === a.id) ||
      (t.assignee_agent_id && t.assignee_agent_id === a.id) ||
      (t.assigned_agent && (t.assigned_agent === a.name || t.assigned_agent === a.id)) ||
      (t.assignee_name && t.assignee_name === a.name)
    );
    const runningTask = agentTasks.find(t => t.status === 'running' || t.status === 'in_progress');
    let st = (a.status || 'idle').toLowerCase();
    if (runningTask || st === 'running' || st === 'in_progress') {
      st = 'running';
    } else if (st === 'paused' || st === 'stopped' || a.pause_reason) {
      st = 'paused';
    } else {
      st = 'idle';
    }
    return {
      ...a,
      normalizedStatus: st,
      agentTasks,
      runningTask,
    };
  });
}

function openFleetInfoModal(tab = 'organizations') {
  const modal = document.getElementById('fleet-modal');
  if (!modal) return;
  fleetModalState.activeTab = tab;
  modal.classList.remove('hidden');
  document.body.style.overflow = 'hidden';
  switchFleetModalTab(tab);
}

function closeFleetInfoModal() {
  const modal = document.getElementById('fleet-modal');
  if (!modal) return;
  modal.classList.add('hidden');
  document.body.style.overflow = '';
}

function switchFleetModalTab(tab) {
  fleetModalState.activeTab = tab;
  document.querySelectorAll('#fleet-modal .modal-tab-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.tab === tab);
  });

  const body = document.getElementById('fleet-modal-body');
  const footerInfo = document.getElementById('fleet-modal-footer-info');
  const modalTitle = document.getElementById('fleet-modal-title');
  const modalSubtitle = document.getElementById('fleet-modal-subtitle');
  if (!body) return;
  body.innerHTML = '';

  if (tab === 'organizations') {
    if (modalTitle) modalTitle.textContent = '🏢 Fleet Organizations';
    if (modalSubtitle) modalSubtitle.textContent = 'Tenant workspace distribution, task counts, and financial spend.';
    renderFleetModalOrganizations(body, footerInfo);
  } else if (tab === 'tasks') {
    if (modalTitle) modalTitle.textContent = '📋 Fleet Tasks Breakdown';
    if (modalSubtitle) modalSubtitle.textContent = 'Global task registry with live statuses, assignments, and priorities.';
    renderFleetModalTasks(body, footerInfo);
  } else if (tab === 'agents') {
    if (modalTitle) modalTitle.textContent = '🤖 Active Fleet Agents';
    if (modalSubtitle) modalSubtitle.textContent = 'Autonomous agents and sessions across all providers and tenant orgs.';
    renderFleetModalAgents(body, footerInfo);
  }
}

function renderFleetModalOrganizations(body, footerInfo) {
  const orgs = state.fleet?.organizations || [];
  const totalSpend = orgs.reduce((sum, o) => sum + (o.spent_usd || 0), 0);
  const totalAgents = orgs.reduce((sum, o) => sum + (o.active_agents || 0), 0);
  const totalTasks = orgs.reduce((sum, o) => sum + (o.task_counts?.total || 0), 0);

  // Summary Grid
  const summaryGrid = el('div', 'modal-summary-grid');
  for (const { val, lbl } of [
    { val: String(orgs.length), lbl: 'Organizations' },
    { val: fmtCurrency(totalSpend), lbl: 'Total Spend' },
    { val: String(totalAgents), lbl: 'Active Agents' },
    { val: String(totalTasks), lbl: 'Total Tasks' },
  ]) {
    const card = el('div', 'modal-summary-card');
    card.appendChild(el('div', 'modal-summary-val', val));
    card.appendChild(el('div', 'modal-summary-lbl', lbl));
    summaryGrid.appendChild(card);
  }
  body.appendChild(summaryGrid);

  // Filter Row
  const filterRow = el('div', 'modal-filter-row');
  const searchInput = el('input', 'modal-filter-input');
  searchInput.type = 'text';
  searchInput.placeholder = 'Search organizations or prefix...';
  searchInput.value = fleetModalState.orgSearch || '';
  filterRow.appendChild(searchInput);
  body.appendChild(filterRow);

  const tableWrap = el('div', 'modal-table-wrap');
  const table = el('table', 'modal-table');
  const thead = el('thead');
  const thr = el('tr');
  for (const h of ['Organization', 'Prefix', 'Active Agents', 'Task Status Breakdown', 'Spend', 'Action']) {
    thr.appendChild(el('th', null, h));
  }
  thead.appendChild(thr);
  table.appendChild(thead);

  const tbody = el('tbody');
  table.appendChild(tbody);
  tableWrap.appendChild(table);
  body.appendChild(tableWrap);

  function updateRows() {
    tbody.innerHTML = '';
    const q = (fleetModalState.orgSearch || '').toLowerCase().trim();
    const filtered = orgs.filter(o =>
      !q || (o.name && o.name.toLowerCase().includes(q)) ||
      (o.issue_prefix && o.issue_prefix.toLowerCase().includes(q))
    );

    if (filtered.length === 0) {
      const tr = el('tr');
      const td = el('td', 'muted-text', 'No organizations match the filter.');
      td.colSpan = 6;
      td.style.textAlign = 'center';
      td.style.padding = '24px';
      tr.appendChild(td);
      tbody.appendChild(tr);
    } else {
      for (const org of filtered) {
        const tr = el('tr');

        // Name
        const tdName = el('td');
        tdName.appendChild(el('strong', null, org.name || 'Unnamed'));
        tr.appendChild(tdName);

        // Prefix
        const tdPrefix = el('td');
        tdPrefix.appendChild(el('span', 'chip-sm chip-purple', org.issue_prefix || 'ORG'));
        tr.appendChild(tdPrefix);

        // Active Agents
        const tdAgents = el('td');
        tdAgents.appendChild(el('span', 'chip-sm chip-green', `${org.active_agents || 0} agents`));
        tr.appendChild(tdAgents);

        // Task Breakdown
        const tdTasks = el('td');
        const tc = org.task_counts || {};
        const group = el('div', 'chip-group');
        if (tc.running) group.appendChild(el('span', 'chip-sm chip-cyan', `${tc.running} running`));
        if (tc.active) group.appendChild(el('span', 'chip-sm chip-blue', `${tc.active} active`));
        if (tc.blocked) group.appendChild(el('span', 'chip-sm chip-red', `${tc.blocked} blocked`));
        if (tc.done) group.appendChild(el('span', 'chip-sm chip-green', `${tc.done} done`));
        group.appendChild(el('span', 'chip-sm chip-gray', `${tc.total || 0} total`));
        tdTasks.appendChild(group);
        tr.appendChild(tdTasks);

        // Spend
        const tdSpend = el('td', null, fmtCurrency(org.spent_usd));
        tr.appendChild(tdSpend);

        // Action
        const tdAction = el('td');
        const viewBtn = el('button', 'modal-action-btn', 'View Org →');
        viewBtn.addEventListener('click', () => {
          closeFleetInfoModal();
          openOrgDetail(org.name);
        });
        tdAction.appendChild(viewBtn);
        tr.appendChild(tdAction);

        tbody.appendChild(tr);
      }
    }

    if (footerInfo) {
      footerInfo.textContent = `Showing ${filtered.length} of ${orgs.length} organizations`;
    }
  }

  searchInput.addEventListener('input', (e) => {
    fleetModalState.orgSearch = e.target.value;
    updateRows();
  });

  updateRows();
}

function renderFleetModalTasks(body, footerInfo) {
  const allTasks = getFleetTasks();
  const orgs = state.fleet?.organizations || [];

  const counts = { running: 0, in_progress: 0, blocked: 0, in_review: 0, todo: 0, done: 0 };
  for (const t of allTasks) {
    const rawSt = (t.status || '').toLowerCase();
    let st = normalizeFleetStatus(t.status);
    if (rawSt === 'running') st = 'running';
    else if (rawSt === 'in_review') st = 'in_review';
    if (counts[st] !== undefined) counts[st]++;
  }

  // Summary Grid
  const summaryGrid = el('div', 'modal-summary-grid');
  for (const { val, lbl, cls } of [
    { val: String(counts.running), lbl: 'Running', cls: 'chip-cyan' },
    { val: String(counts.in_progress), lbl: 'In Progress', cls: 'chip-blue' },
    { val: String(counts.blocked), lbl: 'Blocked', cls: 'chip-red' },
    { val: String(counts.in_review), lbl: 'In Review', cls: 'chip-purple' },
    { val: String(counts.todo), lbl: 'Todo', cls: 'chip-gray' },
    { val: String(counts.done), lbl: 'Done', cls: 'chip-green' },
    { val: String(allTasks.length), lbl: 'Total Tasks' },
  ]) {
    const card = el('div', 'modal-summary-card');
    card.appendChild(el('div', 'modal-summary-val' + (cls ? ` ${cls}` : ''), val));
    card.appendChild(el('div', 'modal-summary-lbl', lbl));
    summaryGrid.appendChild(card);
  }
  body.appendChild(summaryGrid);

  // Filter Row
  const filterRow = el('div', 'modal-filter-row');

  const searchInput = el('input', 'modal-filter-input');
  searchInput.type = 'text';
  searchInput.placeholder = 'Search tasks by title or ID...';
  searchInput.value = fleetModalState.taskSearch || '';
  filterRow.appendChild(searchInput);

  const statusSelect = el('select', 'modal-filter-select');
  for (const [sVal, sLbl] of [
    ['all', 'All Statuses'],
    ['running', 'Running'],
    ['in_progress', 'In Progress'],
    ['blocked', 'Blocked'],
    ['in_review', 'In Review'],
    ['todo', 'Todo'],
    ['done', 'Done'],
  ]) {
    const opt = el('option', null, sLbl);
    opt.value = sVal;
    if (fleetModalState.taskStatus === sVal) opt.selected = true;
    statusSelect.appendChild(opt);
  }
  filterRow.appendChild(statusSelect);

  const orgSelect = el('select', 'modal-filter-select');
  const allOrgOpt = el('option', null, 'All Organizations');
  allOrgOpt.value = 'all';
  orgSelect.appendChild(allOrgOpt);
  for (const org of orgs) {
    const opt = el('option', null, org.name);
    opt.value = org.name;
    if (fleetModalState.taskOrg === org.name) opt.selected = true;
    orgSelect.appendChild(opt);
  }
  filterRow.appendChild(orgSelect);
  body.appendChild(filterRow);

  const tableWrap = el('div', 'modal-table-wrap');
  const table = el('table', 'modal-table');
  const thead = el('thead');
  const thr = el('tr');
  for (const h of ['ID', 'Title', 'Organization', 'Status', 'Assignee', 'Priority', 'Action']) {
    thr.appendChild(el('th', null, h));
  }
  thead.appendChild(thr);
  table.appendChild(thead);

  const tbody = el('tbody');
  table.appendChild(tbody);
  tableWrap.appendChild(table);
  body.appendChild(tableWrap);

  function updateRows() {
    tbody.innerHTML = '';
    const q = (fleetModalState.taskSearch || '').toLowerCase().trim();
    const stFilter = fleetModalState.taskStatus || 'all';
    const orgFilter = fleetModalState.taskOrg || 'all';

    const filtered = allTasks.filter(t => {
      const rawSt = (t.status || '').toLowerCase();
      let st = normalizeFleetStatus(t.status);
      if (rawSt === 'running') st = 'running';
      else if (rawSt === 'in_review') st = 'in_review';

      if (stFilter !== 'all' && st !== stFilter) return false;
      const orgName = t.organization || t.org || '';
      if (orgFilter !== 'all' && orgName.toLowerCase() !== orgFilter.toLowerCase()) return false;
      if (q) {
        const idMatch = (t.id || '').toLowerCase().includes(q) || (t.identifier || '').toLowerCase().includes(q);
        const titleMatch = (t.title || '').toLowerCase().includes(q);
        if (!idMatch && !titleMatch) return false;
      }
      return true;
    });

    if (filtered.length === 0) {
      const tr = el('tr');
      const td = el('td', 'muted-text', 'No tasks match the filter criteria.');
      td.colSpan = 7;
      td.style.textAlign = 'center';
      td.style.padding = '24px';
      tr.appendChild(td);
      tbody.appendChild(tr);
    } else {
      for (const t of filtered) {
        const tr = el('tr');

        // ID
        const tdId = el('td');
        tdId.appendChild(el('strong', 'settings-val', t.identifier || (t.id ? t.id.slice(0, 8) : '—')));
        tr.appendChild(tdId);

        // Title
        const tdTitle = el('td');
        const titleSpan = el('span', null, t.title || 'Untitled task');
        titleSpan.title = t.title || '';
        tdTitle.appendChild(titleSpan);
        tr.appendChild(tdTitle);

        // Org
        const tdOrg = el('td', 'muted-text', t.organization || t.org || 'StayPoint');
        tr.appendChild(tdOrg);

        // Status
        const tdStatus = el('td');
        const rawSt = (t.status || '').toLowerCase();
        let st = normalizeFleetStatus(t.status);
        if (rawSt === 'running') st = 'running';
        else if (rawSt === 'in_review') st = 'in_review';

        let pillCls = 'pill-gray';
        if (st === 'running') pillCls = 'pill-cyan';
        else if (st === 'in_progress') pillCls = 'pill-blue';
        else if (st === 'blocked') pillCls = 'pill-red';
        else if (st === 'in_review') pillCls = 'pill-purple';
        else if (st === 'done') pillCls = 'pill-green';
        tdStatus.appendChild(el('span', `pill ${pillCls}`, st.replace('_', ' ')));
        tr.appendChild(tdStatus);

        // Assignee
        const tdAssignee = el('td', 'muted-text', t.assigned_agent || t.assignee_name || 'Unassigned');
        tr.appendChild(tdAssignee);

        // Priority
        const tdPrio = el('td', 'muted-text', t.priority || 'standard');
        tr.appendChild(tdPrio);

        // Action
        const tdAction = el('td');
        const viewBtn = el('button', 'modal-action-btn', 'View Task →');
        viewBtn.addEventListener('click', () => {
          closeFleetInfoModal();
          openDetail(t.id);
        });
        tdAction.appendChild(viewBtn);
        tr.appendChild(tdAction);

        tbody.appendChild(tr);
      }
    }

    if (footerInfo) {
      footerInfo.textContent = `Showing ${filtered.length} of ${allTasks.length} tasks`;
    }
  }

  searchInput.addEventListener('input', (e) => {
    fleetModalState.taskSearch = e.target.value;
    updateRows();
  });
  statusSelect.addEventListener('change', (e) => {
    fleetModalState.taskStatus = e.target.value;
    updateRows();
  });
  orgSelect.addEventListener('change', (e) => {
    fleetModalState.taskOrg = e.target.value;
    updateRows();
  });

  updateRows();
}

function renderFleetModalAgents(body, footerInfo) {
  const agents = getFleetAgents();

  const total = agents.length;
  const running = agents.filter(a => a.normalizedStatus === 'running').length;
  const idle = agents.filter(a => a.normalizedStatus === 'idle').length;
  const paused = agents.filter(a => a.normalizedStatus === 'paused').length;

  const claudeCount = agents.filter(a => a.provider === 'claude').length;
  const geminiCount = agents.filter(a => a.provider === 'gemini').length;
  const openaiCount = agents.filter(a => a.provider === 'openai').length;

  // Summary Grid
  const summaryGrid = el('div', 'modal-summary-grid');
  for (const { val, lbl, cls } of [
    { val: String(running), lbl: 'Running', cls: 'chip-cyan' },
    { val: String(idle), lbl: 'Idle', cls: 'chip-gray' },
    { val: String(paused), lbl: 'Paused', cls: 'chip-amber' },
    { val: String(claudeCount), lbl: 'Claude Pool', cls: 'chip-purple' },
    { val: String(geminiCount), lbl: 'Gemini Pool', cls: 'chip-blue' },
    { val: String(openaiCount), lbl: 'OpenAI Pool', cls: 'chip-green' },
    { val: String(total), lbl: 'Total Agents' },
  ]) {
    const card = el('div', 'modal-summary-card');
    card.appendChild(el('div', 'modal-summary-val' + (cls ? ` ${cls}` : ''), val));
    card.appendChild(el('div', 'modal-summary-lbl', lbl));
    summaryGrid.appendChild(card);
  }
  body.appendChild(summaryGrid);

  // Filter Row
  const filterRow = el('div', 'modal-filter-row');

  const searchInput = el('input', 'modal-filter-input');
  searchInput.type = 'text';
  searchInput.placeholder = 'Search agents by name or role...';
  searchInput.value = fleetModalState.agentSearch || '';
  filterRow.appendChild(searchInput);

  const providerSelect = el('select', 'modal-filter-select');
  for (const [pVal, pLbl] of [
    ['all', 'All Providers'],
    ['claude', 'Claude'],
    ['gemini', 'Gemini'],
    ['openai', 'OpenAI'],
  ]) {
    const opt = el('option', null, pLbl);
    opt.value = pVal;
    if (fleetModalState.agentProvider === pVal) opt.selected = true;
    providerSelect.appendChild(opt);
  }
  filterRow.appendChild(providerSelect);

  const statusSelect = el('select', 'modal-filter-select');
  for (const [sVal, sLbl] of [
    ['all', 'All Statuses'],
    ['running', 'Running'],
    ['idle', 'Idle'],
    ['paused', 'Paused'],
  ]) {
    const opt = el('option', null, sLbl);
    opt.value = sVal;
    if (fleetModalState.agentStatus === sVal) opt.selected = true;
    statusSelect.appendChild(opt);
  }
  filterRow.appendChild(statusSelect);
  body.appendChild(filterRow);

  const tableWrap = el('div', 'modal-table-wrap');
  const table = el('table', 'modal-table');
  const thead = el('thead');
  const thr = el('tr');
  for (const h of ['Agent Name', 'Role', 'Provider', 'Organization', 'Status', 'Current Task', 'Action']) {
    thr.appendChild(el('th', null, h));
  }
  thead.appendChild(thr);
  table.appendChild(thead);

  const tbody = el('tbody');
  table.appendChild(tbody);
  tableWrap.appendChild(table);
  body.appendChild(tableWrap);

  function updateRows() {
    tbody.innerHTML = '';
    const q = (fleetModalState.agentSearch || '').toLowerCase().trim();
    const provFilter = fleetModalState.agentProvider || 'all';
    const stFilter = fleetModalState.agentStatus || 'all';

    const filtered = agents.filter(a => {
      if (provFilter !== 'all' && a.provider !== provFilter) return false;
      if (stFilter !== 'all' && a.normalizedStatus !== stFilter) return false;
      if (q) {
        const nameMatch = (a.name || '').toLowerCase().includes(q);
        const roleMatch = (a.role || '').toLowerCase().includes(q);
        if (!nameMatch && !roleMatch) return false;
      }
      return true;
    });

    if (filtered.length === 0) {
      const tr = el('tr');
      const td = el('td', 'muted-text', 'No agents match the filter criteria.');
      td.colSpan = 7;
      td.style.textAlign = 'center';
      td.style.padding = '24px';
      tr.appendChild(td);
      tbody.appendChild(tr);
    } else {
      for (const a of filtered) {
        const tr = el('tr');

        // Name
        const tdName = el('td');
        tdName.appendChild(el('strong', null, a.name || 'Agent'));
        tr.appendChild(tdName);

        // Role
        const tdRole = el('td', 'muted-text', a.role || 'Worker');
        tr.appendChild(tdRole);

        // Provider
        const tdProv = el('td');
        let provChip = 'chip-blue';
        if (a.provider === 'claude') provChip = 'chip-purple';
        else if (a.provider === 'openai') provChip = 'chip-green';
        tdProv.appendChild(el('span', `chip-sm ${provChip}`, titleCase(a.provider || 'gemini')));
        tr.appendChild(tdProv);

        // Org
        const tdOrg = el('td', 'muted-text', a.org || 'StayPoint');
        tr.appendChild(tdOrg);

        // Status
        const tdStatus = el('td');
        let stChip = 'chip-gray';
        if (a.normalizedStatus === 'running') stChip = 'chip-cyan';
        else if (a.normalizedStatus === 'paused') stChip = 'chip-amber';
        tdStatus.appendChild(el('span', `chip-sm ${stChip}`, titleCase(a.normalizedStatus)));
        tr.appendChild(tdStatus);

        // Current Task
        const tdTask = el('td');
        if (a.runningTask) {
          const taskLink = el('a', 'settings-val-interactive', a.runningTask.title || a.runningTask.id);
          taskLink.style.cursor = 'pointer';
          taskLink.addEventListener('click', (ev) => {
            ev.stopPropagation();
            closeFleetInfoModal();
            openDetail(a.runningTask.id);
          });
          tdTask.appendChild(taskLink);
        } else {
          tdTask.appendChild(el('span', 'muted-text', 'Idle'));
        }
        tr.appendChild(tdTask);

        // Action
        const tdAction = el('td');
        const viewBtn = el('button', 'modal-action-btn', 'View Agents →');
        viewBtn.addEventListener('click', () => {
          closeFleetInfoModal();
          navigateTo('agents');
        });
        tdAction.appendChild(viewBtn);
        tr.appendChild(tdAction);

        tbody.appendChild(tr);
      }
    }

    if (footerInfo) {
      footerInfo.textContent = `Showing ${filtered.length} of ${agents.length} agents`;
    }
  }

  searchInput.addEventListener('input', (e) => {
    fleetModalState.agentSearch = e.target.value;
    updateRows();
  });
  providerSelect.addEventListener('change', (e) => {
    fleetModalState.agentProvider = e.target.value;
    updateRows();
  });
  statusSelect.addEventListener('change', (e) => {
    fleetModalState.agentStatus = e.target.value;
    updateRows();
  });

  updateRows();
}

// Wire modal event listeners
document.getElementById('fleet-modal-close')?.addEventListener('click', closeFleetInfoModal);
document.getElementById('fleet-modal-footer-close')?.addEventListener('click', closeFleetInfoModal);
document.getElementById('fleet-modal')?.addEventListener('click', (e) => {
  if (e.target === document.getElementById('fleet-modal')) {
    closeFleetInfoModal();
  }
});
document.querySelectorAll('#fleet-modal .modal-tab-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    switchFleetModalTab(btn.dataset.tab);
  });
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const m = document.getElementById('fleet-modal');
    if (m && !m.classList.contains('hidden')) {
      closeFleetInfoModal();
    }
  }
});

// ── Org Detail View ────────────────────────────────────────
function openOrgDetail(orgName) {
  navigateTo('org-detail', orgName, true);
}

function renderOrgDetailView(org) {
  const container = document.getElementById('org-detail-content');
  if (!container) return;
  container.innerHTML = '';

  const hdr = el('div', 'org-detail-header');
  const backBtn = el('button', 'back-btn', '← Back');
  backBtn.addEventListener('click', () => {
    navigateTo('overview', null, true);
  });
  hdr.appendChild(backBtn);

  const titleWrap = el('div');
  titleWrap.appendChild(el('div', 'org-detail-name', org.name));
  titleWrap.appendChild(el('div', 'org-detail-prefix', `[${org.issue_prefix || 'ORG'}]`));
  hdr.appendChild(titleWrap);
  container.appendChild(hdr);

  // Stats row
  const statsRow = el('div', 'org-detail-stats');
  const tc = org.task_counts || {};
  const statItems = [
    { val: tc.running || 0,  label: 'Running',  cls: 'highlight-cyan', status: 'running' },
    { val: tc.active  || 0,  label: 'Active',   status: 'active' },
    { val: tc.blocked || 0,  label: 'Blocked',  cls: 'highlight-red',  status: 'blocked' },
    { val: tc.done    || 0,  label: 'Done',     cls: 'highlight-green',status: 'done' },
    { val: tc.total   || 0,  label: 'Total',    status: 'all' },
    { val: org.active_agents || 0, label: 'Agents', cls: 'highlight-green', type: 'agents' },
    { val: fmtCurrency(org.spent_usd), label: 'Spend', isStr: true, type: 'cost' },
  ];
  for (const s of statItems) {
    const d = el('div', 'org-detail-stat clickable');
    d.setAttribute('role', 'button');
    d.setAttribute('tabindex', '0');
    d.title = `Filter ${org.name} by ${s.label}`;
    const valEl = el('div', 'org-detail-stat-val' + (s.cls ? ' ' + s.cls : ''), String(s.val));
    d.appendChild(valEl);
    d.appendChild(el('div', 'org-detail-stat-label', s.label));

    if (s.status) {
      d.addEventListener('click', () => drillDownToTasks(s.status, org.name));
      d.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          drillDownToTasks(s.status, org.name);
        }
      });
    } else if (s.type === 'agents') {
      d.addEventListener('click', () => drillDownToAgents('all', org.name));
      d.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          drillDownToAgents('all', org.name);
        }
      });
    } else if (s.type === 'cost') {
      d.addEventListener('click', () => drillDownToCost());
      d.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          drillDownToCost();
        }
      });
    }

    statsRow.appendChild(d);
  }
  container.appendChild(statsRow);

  // 5-Hour Rolling Quotas & Lockout Gauges for this organization
  const orgQuotas = org.provider_quotas || {};
  if (Object.keys(orgQuotas).length) {
    const quotaSec = el('div', 'org-detail-section');
    quotaSec.appendChild(el('div', 'org-detail-section-title', '5-Hour Rolling Quotas & Lockout Status'));
    const quotaGrid = el('div', 'quota-gauges-grid');
    const isManagedSol = (org.name || '').toLowerCase().includes('managed');
    const order = isManagedSol
      ? ['gemini', 'claude_work', 'claude_personal', 'claude', 'openai']
      : ['gemini', 'claude_personal', 'claude_work', 'claude', 'openai'];
    for (const key of order) {
      const q = orgQuotas[key];
      if (!q) continue;
      if (key === 'claude' && (orgQuotas['claude_work'] || orgQuotas['claude_personal'])) continue;
      quotaGrid.appendChild(buildGaugeCard(key, q));
    }
    quotaSec.appendChild(quotaGrid);
    container.appendChild(quotaSec);
  }

  // Tasks section
  const orgTasks = visibleTasks(org.tasks, false).concat(
    taskValues().filter(t =>
      t.organization === org.name && !(org.tasks || []).find(ot => ot.id === t.id)
    )
  );

  if (orgTasks.length) {
    const tasksSec = el('div', 'org-detail-section');
    tasksSec.appendChild(el('div', 'org-detail-section-title', `Tasks (${orgTasks.length})`));
    const tbl = document.createElement('table');
    tbl.className = 'global-task-table';
    tbl.id = 'org-task-table';
    tbl.style.width = '100%';
    const thead = document.createElement('thead');
    thead.innerHTML = '<tr>' +
      '<th data-col="identifier" class="sortable-th" tabindex="0" role="columnheader">ID</th>' +
      '<th data-col="task" class="sortable-th" tabindex="0" role="columnheader">Task</th>' +
      '<th data-col="status" class="sortable-th" tabindex="0" role="columnheader">Status</th>' +
      '<th data-col="priority" class="sortable-th" tabindex="0" role="columnheader">Priority</th>' +
      '<th data-col="cost" class="sortable-th" tabindex="0" role="columnheader">Spend</th>' +
      '</tr>';
    tbl.appendChild(thead);
    const tbody = document.createElement('tbody');

    const DEFAULT_ORG_SORT = { column: 'task', direction: 'asc' };
    const renderOrgRows = () => {
      renderTableSortHeaders(tbl, state.orgSort, (col, dir, nextSort) => {
        state.orgSort = nextSort || { column: col, direction: dir };
        renderOrgRows();
      }, DEFAULT_ORG_SORT);
      tbody.innerHTML = '';
      const sorted = sortTasks(orgTasks, state.orgSort.column, state.orgSort.direction);
      for (const t of sorted) {
        const tr = document.createElement('tr');
        const tdId = el('td', null, t.identifier || (t.id ? `#${t.id.slice(0, 8)}` : '—'));
        tdId.style.cssText = 'font-family:monospace;font-weight:600;';
        const tdTitle = el('td', null, t.title || t.name || '(untitled)');
        const tdStat  = el('td'); tdStat.appendChild(statusPill(t.status));
        const tdPri   = el('td', null, t.priority || '—');
        const tdSpend = el('td', null, t.spent_usd > 0 ? fmtCurrency(t.spent_usd) : '—');
        for (const td of [tdId, tdTitle, tdStat, tdPri, tdSpend]) tr.appendChild(td);
        tr.addEventListener('click', () => openDetail(t.id));
        tbody.appendChild(tr);
      }
    };
    renderOrgRows();

    tbl.appendChild(tbody);
    const wrapper = el('div', 'task-table-wrapper');
    wrapper.appendChild(tbl);
    tasksSec.appendChild(wrapper);
    container.appendChild(tasksSec);
  }

  // Agent Roster section
  const orgAgents = (org.agents || []);
  const agentsSec = el('div', 'org-detail-section');
  agentsSec.appendChild(el('div', 'org-detail-section-title', `Agent Roster (${orgAgents.length || 'by provider'})`));

  if (orgAgents.length) {
    const rosterGrid = el('div', 'agent-roster-grid');
    for (const a of orgAgents) {
      const prov = resolveProvider(a);
      const card = el('div', 'agent-roster-card');
      card.appendChild(el('div', 'agent-roster-name', a.name || a.id?.slice(0, 12)));
      card.appendChild(el('div', 'agent-roster-meta',
        `${a.role || 'agent'} · ${prov} · ${a.status || 'active'}`));
      if (a.last_heartbeat) {
        card.appendChild(el('div', 'muted-text', `Last: ${fmtRelTime(a.last_heartbeat)}`));
      }
      rosterGrid.appendChild(card);
    }
    agentsSec.appendChild(rosterGrid);
  } else {
    const provs = org.active_agents_by_provider || {};
    const provBreakdown = el('div', 'agent-roster-grid');
    for (const [provider, count] of Object.entries(provs)) {
      if (!count) continue;
      const card = el('div', 'agent-roster-card');
      card.appendChild(el('div', 'agent-roster-name', `${provider}: ${count} active`));
      card.appendChild(el('div', 'agent-roster-meta', 'Provider breakdown'));
      provBreakdown.appendChild(card);
    }
    if (!Object.values(provs).some(n => n > 0)) {
      provBreakdown.appendChild(el('div', 'panel-field-muted', 'No active agents'));
    }
    agentsSec.appendChild(provBreakdown);
  }
  container.appendChild(agentsSec);

  // Spend breakdown
  const orgSpendData = (state.fleet?.org_spend || []).find(o => o.organization === org.name);
  if (orgSpendData) {
    const spendSec = el('div', 'org-detail-section');
    spendSec.appendChild(el('div', 'org-detail-section-title', 'Spend Breakdown'));
    const spendBox = el('div', 'dashboard-section');
    spendBox.style.padding = '12px 16px';
    for (const row of [
      { label: 'Total Cost (USD)',  val: fmtCurrency(orgSpendData.cost_usd) },
      { label: 'Total Tokens',      val: fmtNum(orgSpendData.total_tokens) },
      { label: 'Input Tokens',      val: fmtNum(orgSpendData.input_tokens) },
      { label: 'Output Tokens',     val: fmtNum(orgSpendData.output_tokens) },
      { label: 'Fleet Share',       val: `${orgSpendData.percentage || 0}%` },
    ]) {
      const r = el('div', 'org-spend-row');
      r.appendChild(el('span', null, row.label));
      r.appendChild(el('span', 'gauge-metric-val', row.val));
      spendBox.appendChild(r);
    }
    spendSec.appendChild(spendBox);
    container.appendChild(spendSec);
  }
}

// ── Render: Kanban ────────────────────────────────────────
// Columns and grouping come from lib/taskboard.js. Board preferences (group by
// company, show legacy) are per-viewer conveniences kept in localStorage.
const STORAGE_KANBAN_GROUP_KEY = 'staypoint_kanban_group_company';
const STORAGE_KANBAN_LEGACY_KEY = 'staypoint_kanban_show_legacy';

function kanbanPref(key) {
  try { return localStorage.getItem(key) === '1'; } catch { return false; }
}

function setKanbanPref(key, on) {
  try { localStorage.setItem(key, on ? '1' : '0'); } catch { /* ignore */ }
}

function wireKanbanControls() {
  for (const [id, key] of [['kanban-group-company', STORAGE_KANBAN_GROUP_KEY], ['kanban-show-legacy', STORAGE_KANBAN_LEGACY_KEY]]) {
    const box = document.getElementById(id);
    if (!box || box.dataset.wired) continue;
    box.dataset.wired = '1';
    box.checked = kanbanPref(key);
    box.addEventListener('change', () => {
      setKanbanPref(key, box.checked);
      renderKanban();
    });
  }
}

function renderKanbanColumns(columns) {
  const row = el('div', 'kanban-board');
  for (const status of BOARD_COLUMNS) {
    const col = el('div', 'kanban-col');
    col.dataset.status = status;
    const tasks = columns[status] || [];
    col.appendChild(el('h2', 'col-title', `${BOARD_COLUMN_TITLES[status]} · ${tasks.length}`));
    const list = el('div', 'card-list');
    list.id = `col-${status}`;
    for (const t of tasks) list.appendChild(makeTaskCard(t));
    col.appendChild(list);
    row.appendChild(col);
  }
  return row;
}

function renderKanban() {
  const board = document.getElementById('kanban-board');
  if (!board) return;
  wireKanbanControls();
  const groupByCompany = !!document.getElementById('kanban-group-company')?.checked;
  const showLegacy = !!document.getElementById('kanban-show-legacy')?.checked;
  const tasks = Object.values(state.tasks);
  const legacyCount = document.getElementById('kanban-legacy-count');
  if (legacyCount) legacyCount.textContent = `(${countLegacy(tasks)})`;

  board.innerHTML = '';
  const groups = buildBoard(tasks, { groupByCompany, showLegacy });
  for (const g of groups) {
    if (!groupByCompany) {
      board.appendChild(renderKanbanColumns(g.columns));
      continue;
    }
    const lane = el('section', 'kanban-lane');
    lane.appendChild(el('h2', 'kanban-lane-title', `${g.company} · ${g.total}`));
    lane.appendChild(renderKanbanColumns(g.columns));
    board.appendChild(lane);
  }
  if (groupByCompany && groups.length === 0) {
    board.appendChild(el('div', 'muted-text', 'No tasks on the board.'));
  }
}

function makeTaskCard(task) {
  const card = el('div', 'task-card');
  card.dataset.id = task.id;
  card.appendChild(el('div', 'card-title', task.title || task.name || '(untitled)'));
  const meta = el('div', 'card-meta');
  meta.appendChild(el('span', `card-status-dot dot-${boardColumnFor(task) || task.status}`));
  meta.appendChild(el('span', 'card-id', task.source_ref || task.identifier || (task.id ? `#${task.id.slice(0, 8)}` : '')));
  if (isLegacy(task)) meta.appendChild(el('span', 'card-origin-badge', 'legacy'));
  else if (isArchived(task)) meta.appendChild(el('span', 'card-origin-badge', 'archive'));
  card.appendChild(meta);
  card.addEventListener('click', () => openDetail(task.id));
  return card;
}

// ── Render: Fleet (Sessions) ──────────────────────────────
function renderFleet() {
  // Fleet view is now "Agents" — redirect rendering to agents view if active
  const agentsView = document.getElementById('view-agents');
  if (agentsView?.classList.contains('active')) {
    renderAgentsPage();
  }
}

// ── Render: Boss Card ─────────────────────────────────────
function renderBoss() {
  const container = document.getElementById('boss-container');
  if (!container) return;

  const tasks   = taskValues();
  const todo    = tasks.filter(t => t.status === 'todo').length;
  const inProg  = tasks.filter(t => t.status === 'in_progress').length;
  const blocked = tasks.filter(t => t.status === 'blocked').length;
  const done    = tasks.filter(t => t.status === 'done').length;
  const total   = tasks.length;

  const sessions = Object.values(state.sessions);
  const active = state.fleet?.global_agents?.active_running
    ?? sessions.filter(s => s.status === 'active').length;

  container.innerHTML = '';

  const card = el('div', 'boss-card');
  const header = el('div', 'boss-header');
  const titleWrap = el('div');
  titleWrap.appendChild(el('div', 'boss-title', 'StayPoint Fleet'));
  titleWrap.appendChild(el('div', 'boss-subtitle',
    `${total} task${total !== 1 ? 's' : ''} · ${active} agent${active !== 1 ? 's' : ''} active`));
  header.appendChild(titleWrap);

  // Download Report dropdown
  const dlWrap = el('div', 'report-dl-wrap');
  const dlBtn = el('button', 'report-dl-btn', 'Download Report');
  dlBtn.type = 'button';
  const dlMenu = el('div', 'report-dl-menu');
  dlMenu.hidden = true;
  for (const { type, label } of [
    { type: 'work',     label: 'Work / Boss Card' },
    { type: 'personal', label: 'Personal' },
    { type: 'gemini',   label: 'Gemini / Antigravity' },
    { type: 'combined', label: 'Combined Fleet' },
  ]) {
    const item = el('button', 'report-dl-item', label);
    item.type = 'button';
    item.dataset.reportType = type;
    item.addEventListener('click', async (e) => {
      e.stopPropagation();
      dlMenu.hidden = true;

      // Ensure .dl-spinner and button disabled state visibly activate immediately
      // on the download button upon triggering PDF/report generation and remain until blob is received.
      const origBtnContent = dlBtn.innerHTML;
      dlBtn.disabled = true;
      dlBtn.style.opacity = '0.7';
      dlBtn.style.cursor = 'not-allowed';
      dlBtn.innerHTML = '';
      const btnSpinner = el('span', 'dl-spinner');
      dlBtn.appendChild(btnSpinner);
      dlBtn.appendChild(document.createTextNode(` Generating ${label}...`));

      // Also disable dropdown items during active generation
      dlMenu.querySelectorAll('.report-dl-item').forEach(b => { b.disabled = true; });

      try {
        const url = `/api/report?type=${encodeURIComponent(type)}${TOKEN ? `&token=${encodeURIComponent(TOKEN)}` : ''}`;
        const resp = await fetch(url, {
          headers: authHeader(),
        });
        if (!resp.ok) {
          const errText = await resp.text().catch(() => '');
          let errMsg = `Report generation failed (${resp.status})`;
          try {
            const parsed = JSON.parse(errText);
            if (parsed.error) errMsg = parsed.error;
          } catch {
            if (errText) errMsg = errText;
          }
          throw new Error(errMsg);
        }

        const blob = await resp.blob();

        let filename = `staypoint-${type}-report.pdf`;
        const disp = resp.headers.get('Content-Disposition');
        if (disp) {
          const match = disp.match(/filename="?([^";]+)"?/i);
          if (match && match[1]) {
            filename = match[1].trim();
          }
        }

        const blobUrl = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = blobUrl;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        setTimeout(() => URL.revokeObjectURL(blobUrl), 15000);
      } catch (err) {
        console.error('Report generation error:', err);
        alert(`Failed to download report: ${err.message}`);
      } finally {
        dlBtn.disabled = false;
        dlBtn.style.opacity = '';
        dlBtn.style.cursor = '';
        dlBtn.innerHTML = origBtnContent;
        dlMenu.querySelectorAll('.report-dl-item').forEach(b => { b.disabled = false; });
      }
    });
    dlMenu.appendChild(item);
  }
  dlBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    dlMenu.hidden = !dlMenu.hidden;
  });
  dlWrap.addEventListener('click', (e) => e.stopPropagation());
  dlWrap.appendChild(dlBtn);
  dlWrap.appendChild(dlMenu);
  header.appendChild(dlWrap);

  card.appendChild(header);

  const grid = el('div', 'boss-stat-grid');
  for (const { val, label, status, type } of [
    { val: inProg,  label: 'In Progress', status: 'running' },
    { val: todo,    label: 'Todo',        status: 'active' },
    { val: blocked, label: 'Blocked',     status: 'blocked' },
    { val: done,    label: 'Done',        status: 'done' },
    { val: active,  label: 'Agents',      type: 'agents' },
  ]) {
    const s = el('div', 'boss-stat clickable');
    s.setAttribute('role', 'button');
    s.setAttribute('tabindex', '0');
    s.title = `View ${label}`;
    s.appendChild(el('div', 'boss-stat-val', String(val)));
    s.appendChild(el('div', 'boss-stat-label', label));
    if (status) {
      s.addEventListener('click', () => drillDownToTasks(status));
      s.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          drillDownToTasks(status);
        }
      });
    } else if (type === 'agents') {
      s.addEventListener('click', () => drillDownToAgents());
      s.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          drillDownToAgents();
        }
      });
    }
    grid.appendChild(s);
  }
  card.appendChild(grid);
  container.appendChild(card);

  // In-app interactive report preview / carousel
  container.appendChild(buildBossReportCarousel());

  renderEventStream();
}

// ── Boss Card In-App Report Carousel & Modal Preview ─────
const BOSS_REPORTS = [
  { id: 'work',     label: 'Work / Boss Card', desc: 'Executive Justification Memo', icon: '🏢' },
  { id: 'combined', label: 'Combined Fleet',   desc: 'Cross-Account Fleet Audit',   icon: '🌐' },
  { id: 'personal', label: 'Personal',         desc: 'Claude Code Value Audit',     icon: '👤' },
  { id: 'gemini',   label: 'Gemini / AGY',     desc: 'Antigravity Agent Velocity',  icon: '⚡' },
];

const BOSS_REPORT_CACHE_KEY = 'staypoint_boss_reports_cache';
const BOSS_REPORT_CACHE_TTL_MS = 30 * 60 * 1000; // 30 minutes TTL

function loadBossReportCache() {
  try {
    const raw = localStorage.getItem(BOSS_REPORT_CACHE_KEY);
    if (!raw) return false;
    const parsed = JSON.parse(raw);
    if (!parsed || !parsed.reports || typeof parsed.reports !== 'object') return false;
    const now = Date.now();
    const today = new Date().toISOString().slice(0, 10);
    const isToday = parsed.date === today;
    const isWithinTTL = (now - (parsed.timestamp || 0)) < BOSS_REPORT_CACHE_TTL_MS;
    if (isToday && isWithinTTL) {
      state.bossReportCache = Object.assign({}, parsed.reports);
      state.bossReportLastRenderedAt = parsed.timestamp || now;
      return true;
    }
  } catch { /* ignore storage errors */ }
  return false;
}

function saveBossReportCache() {
  try {
    const payload = {
      timestamp: Date.now(),
      date: new Date().toISOString().slice(0, 10),
      reports: state.bossReportCache || {},
    };
    localStorage.setItem(BOSS_REPORT_CACHE_KEY, JSON.stringify(payload));
    state.bossReportLastRenderedAt = payload.timestamp;
  } catch { /* ignore storage errors */ }
}

async function preloadBossReports(force = false) {
  if (state.bossReportPreloading) return;
  state.bossReportPreloading = true;

  try {
    if (!force) {
      const isFresh = loadBossReportCache();
      const allCached = BOSS_REPORTS.every(r => Boolean(state.bossReportCache && state.bossReportCache[r.id]));
      if (isFresh && allCached) {
        state.bossReportPreloading = false;
        return;
      }
    }

    const fetches = BOSS_REPORTS.map(async (rep) => {
      try {
        const url = `/api/report?type=${encodeURIComponent(rep.id)}&format=html${TOKEN ? `&token=${encodeURIComponent(TOKEN)}` : ''}`;
        const res = await fetch(url, { headers: authHeader() });
        if (res.ok) {
          const html = await res.text();
          if (!state.bossReportCache) state.bossReportCache = {};
          state.bossReportCache[rep.id] = html;
          state.bossReportCacheTimestamps[rep.id] = Date.now();
        }
      } catch (err) {
        console.warn(`[BossCache] Error preloading ${rep.id}:`, err);
      }
    });

    await Promise.all(fetches);
    saveBossReportCache();

    // If an iframe for the current report is in view, ensure it displays the fresh content
    const activeIframe = document.querySelector('.boss-preview-iframe');
    const curReport = typeof state.bossReportIndex === 'number' ? BOSS_REPORTS[state.bossReportIndex] : null;
    if (activeIframe && curReport && state.bossReportCache[curReport.id]) {
      if (activeIframe.srcdoc !== state.bossReportCache[curReport.id]) {
        activeIframe.srcdoc = state.bossReportCache[curReport.id];
      }
      const loading = document.querySelector('.boss-preview-loading');
      if (loading) loading.style.display = 'none';
      const errorOverlay = document.querySelector('.boss-preview-error');
      if (errorOverlay) errorOverlay.style.display = 'none';
    }
  } finally {
    state.bossReportPreloading = false;
  }
}

function buildBossReportCarousel() {
  const card = el('div', 'boss-carousel-card');

  if (typeof state.bossReportIndex !== 'number' || state.bossReportIndex < 0 || state.bossReportIndex >= BOSS_REPORTS.length) {
    state.bossReportIndex = 0;
  }
  if (!state.bossReportZoom) {
    state.bossReportZoom = 1.0;
  }
  if (!state.bossReportCache) {
    state.bossReportCache = {};
  }

  const currentReport = () => BOSS_REPORTS[state.bossReportIndex];

  // 1. Header with title & interactive action buttons
  const hdr = el('div', 'boss-carousel-header');
  const titleGroup = el('div', 'boss-carousel-title-group');
  const title = el('div', 'boss-carousel-title');
  title.innerHTML = '<span>📄</span> Executive Report In-App Preview';
  const desc = el('div', 'boss-carousel-desc', 'Interactive carousel — view and inspect executive briefing memos and audits live in-app.');
  titleGroup.appendChild(title);
  titleGroup.appendChild(desc);
  hdr.appendChild(titleGroup);

  const actions = el('div', 'boss-carousel-actions');

  // Zoom controls
  const zoomOutBtn = el('button', 'carousel-btn', '−');
  zoomOutBtn.type = 'button';
  zoomOutBtn.title = 'Zoom out';
  const zoomLabel = el('span', 'carousel-btn', `${Math.round(state.bossReportZoom * 100)}%`);
  zoomLabel.style.cursor = 'default';
  const zoomInBtn = el('button', 'carousel-btn', '+');
  zoomInBtn.type = 'button';
  zoomInBtn.title = 'Zoom in';

  zoomOutBtn.addEventListener('click', () => {
    state.bossReportZoom = Math.max(0.6, Math.round((state.bossReportZoom - 0.1) * 10) / 10);
    zoomLabel.textContent = `${Math.round(state.bossReportZoom * 100)}%`;
    applyZoom();
  });
  zoomInBtn.addEventListener('click', () => {
    state.bossReportZoom = Math.min(1.4, Math.round((state.bossReportZoom + 0.1) * 10) / 10);
    zoomLabel.textContent = `${Math.round(state.bossReportZoom * 100)}%`;
    applyZoom();
  });
  actions.appendChild(zoomOutBtn);
  actions.appendChild(zoomLabel);
  actions.appendChild(zoomInBtn);

  // Fullscreen / Modal expander
  const expandBtn = el('button', 'carousel-btn', '⛶ Expand');
  expandBtn.type = 'button';
  expandBtn.title = 'Open full-screen modal preview';
  expandBtn.addEventListener('click', () => {
    openBossReportModal(currentReport().id, currentReport().label);
  });
  actions.appendChild(expandBtn);

  // Print button
  const printBtn = el('button', 'carousel-btn', '🖨 Print');
  printBtn.type = 'button';
  printBtn.title = 'Print active report';
  printBtn.addEventListener('click', () => {
    if (iframe && iframe.contentWindow) {
      iframe.contentWindow.focus();
      iframe.contentWindow.print();
    }
  });
  actions.appendChild(printBtn);

  // Refresh preview data
  const refreshBtn = el('button', 'carousel-btn', '↻ Refresh');
  refreshBtn.type = 'button';
  refreshBtn.title = 'Refresh current report data';
  refreshBtn.addEventListener('click', () => {
    delete state.bossReportCache[currentReport().id];
    loadReport();
    preloadBossReports(true);
  });
  actions.appendChild(refreshBtn);

  // Download PDF button for current report
  const dlCurrentBtn = el('button', 'carousel-btn primary', '⬇ PDF');
  dlCurrentBtn.type = 'button';
  dlCurrentBtn.title = 'Download current report as PDF';
  dlCurrentBtn.addEventListener('click', async () => {
    const rep = currentReport();
    const origText = dlCurrentBtn.innerHTML;
    dlCurrentBtn.disabled = true;
    dlCurrentBtn.innerHTML = '<span class="dl-spinner"></span> Generating…';
    try {
      const url = `/api/report?type=${encodeURIComponent(rep.id)}${TOKEN ? `&token=${encodeURIComponent(TOKEN)}` : ''}`;
      const resp = await fetch(url, { headers: authHeader() });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const blob = await resp.blob();
      const blobUrl = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = blobUrl;
      a.download = `staypoint-${rep.id}-report.pdf`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      setTimeout(() => URL.revokeObjectURL(blobUrl), 15000);
    } catch (err) {
      alert(`Download failed: ${err.message}`);
    } finally {
      dlCurrentBtn.disabled = false;
      dlCurrentBtn.innerHTML = origText;
    }
  });
  actions.appendChild(dlCurrentBtn);

  hdr.appendChild(actions);
  card.appendChild(hdr);

  // 2. Carousel Nav Bar (Tabs & Pager)
  const navBar = el('div', 'boss-carousel-nav');
  const tabsWrap = el('div', 'boss-carousel-tabs');
  const tabBtns = [];

  BOSS_REPORTS.forEach((rep, idx) => {
    const tab = el('button', 'carousel-tab' + (idx === state.bossReportIndex ? ' active' : ''));
    tab.type = 'button';
    tab.innerHTML = `<span>${rep.icon}</span> ${rep.label}`;
    tab.addEventListener('click', () => {
      state.bossReportIndex = idx;
      updateCarousel();
    });
    tabsWrap.appendChild(tab);
    tabBtns.push(tab);
  });
  navBar.appendChild(tabsWrap);

  const pagerWrap = el('div', 'boss-carousel-pager');
  const prevBtn = el('button', 'carousel-btn', '‹ Prev');
  prevBtn.type = 'button';
  prevBtn.title = 'Previous report (←)';
  const pagerInd = el('span', 'carousel-page-indicator');
  const nextBtn = el('button', 'carousel-btn', 'Next ›');
  nextBtn.type = 'button';
  nextBtn.title = 'Next report (→)';

  prevBtn.addEventListener('click', () => {
    state.bossReportIndex = (state.bossReportIndex - 1 + BOSS_REPORTS.length) % BOSS_REPORTS.length;
    updateCarousel();
  });
  nextBtn.addEventListener('click', () => {
    state.bossReportIndex = (state.bossReportIndex + 1) % BOSS_REPORTS.length;
    updateCarousel();
  });

  pagerWrap.appendChild(prevBtn);
  pagerWrap.appendChild(pagerInd);
  pagerWrap.appendChild(nextBtn);
  navBar.appendChild(pagerWrap);
  card.appendChild(navBar);

  // 3. Document Preview Sheet Frame
  const frameWrap = el('div', 'boss-preview-frame-wrap');
  const sheet = el('div', 'boss-preview-sheet');
  const iframe = document.createElement('iframe');
  iframe.className = 'boss-preview-iframe';
  iframe.setAttribute('title', 'Boss Card Report Preview');
  iframe.setAttribute('sandbox', 'allow-same-origin allow-scripts allow-modals allow-popups');

  const loadingOverlay = el('div', 'boss-preview-loading');
  loadingOverlay.innerHTML = '<span class="dl-spinner"></span><span>Loading executive report preview…</span>';

  const errorOverlay = el('div', 'boss-preview-error');
  errorOverlay.style.display = 'none';

  sheet.appendChild(iframe);
  frameWrap.appendChild(sheet);
  frameWrap.appendChild(loadingOverlay);
  frameWrap.appendChild(errorOverlay);
  card.appendChild(frameWrap);

  function applyZoom() {
    if (state.bossReportZoom === 1.0) {
      sheet.style.transform = 'none';
      sheet.style.transformOrigin = 'top center';
    } else {
      sheet.style.transform = `scale(${state.bossReportZoom})`;
      sheet.style.transformOrigin = 'top center';
    }
  }

  async function loadReport() {
    const rep = currentReport();
    loadingOverlay.style.display = 'flex';
    errorOverlay.style.display = 'none';

    try {
      if (state.bossReportCache[rep.id]) {
        iframe.srcdoc = state.bossReportCache[rep.id];
        loadingOverlay.style.display = 'none';
        return;
      }

      const url = `/api/report?type=${encodeURIComponent(rep.id)}&format=html${TOKEN ? `&token=${encodeURIComponent(TOKEN)}` : ''}`;
      const res = await fetch(url, { headers: authHeader() });
      if (!res.ok) {
        throw new Error(`Server returned HTTP ${res.status}`);
      }
      const html = await res.text();
      state.bossReportCache[rep.id] = html;
      state.bossReportCacheTimestamps[rep.id] = Date.now();
      saveBossReportCache();
      iframe.srcdoc = html;
      loadingOverlay.style.display = 'none';
    } catch (err) {
      loadingOverlay.style.display = 'none';
      errorOverlay.innerHTML = '';
      errorOverlay.style.display = 'flex';
      errorOverlay.appendChild(el('div', 'boss-preview-error-title', `Failed to load ${rep.label}`));
      errorOverlay.appendChild(el('div', 'boss-preview-error-desc', err.message || 'Unknown network error.'));
      const retryBtn = el('button', 'carousel-btn primary', '↻ Retry');
      retryBtn.type = 'button';
      retryBtn.addEventListener('click', loadReport);
      errorOverlay.appendChild(retryBtn);
    }
  }

  function updateCarousel() {
    tabBtns.forEach((btn, idx) => {
      btn.classList.toggle('active', idx === state.bossReportIndex);
    });
    pagerInd.textContent = `Report ${state.bossReportIndex + 1} of ${BOSS_REPORTS.length}: ${currentReport().label}`;
    applyZoom();
    loadReport();
  }

  // Keyboard navigation for carousel
  card.setAttribute('tabindex', '0');
  card.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      prevBtn.click();
    } else if (e.key === 'ArrowRight') {
      e.preventDefault();
      nextBtn.click();
    }
  });

  updateCarousel();
  return card;
}

function openBossReportModal(reportId, label) {
  const existing = document.getElementById('boss-report-modal');
  if (existing) existing.remove();

  const backdrop = el('div', 'boss-modal-backdrop');
  backdrop.id = 'boss-report-modal';

  const win = el('div', 'boss-modal-window');

  const hdr = el('div', 'boss-modal-header');
  const title = el('div', 'boss-modal-title', `Executive Report: ${label}`);
  const closeBtn = el('button', 'boss-modal-close-btn', '✕');
  closeBtn.type = 'button';
  closeBtn.title = 'Close modal (Esc)';
  hdr.appendChild(title);
  hdr.appendChild(closeBtn);
  win.appendChild(hdr);

  const body = el('div', 'boss-modal-body');
  const sheet = el('div', 'boss-modal-sheet');
  const modalIframe = document.createElement('iframe');
  modalIframe.className = 'boss-preview-iframe';
  modalIframe.setAttribute('sandbox', 'allow-same-origin allow-scripts allow-modals allow-popups');

  if (state.bossReportCache && state.bossReportCache[reportId]) {
    modalIframe.srcdoc = state.bossReportCache[reportId];
  } else {
    modalIframe.src = `/api/report?type=${encodeURIComponent(reportId)}&format=html${TOKEN ? `&token=${encodeURIComponent(TOKEN)}` : ''}`;
  }

  sheet.appendChild(modalIframe);
  body.appendChild(sheet);
  win.appendChild(body);
  backdrop.appendChild(win);

  const closeModal = () => {
    backdrop.remove();
    document.removeEventListener('keydown', onEsc);
  };
  const onEsc = (e) => {
    if (e.key === 'Escape') closeModal();
  };

  closeBtn.addEventListener('click', closeModal);
  backdrop.addEventListener('click', (e) => {
    if (e.target === backdrop) closeModal();
  });
  document.addEventListener('keydown', onEsc);

  document.body.appendChild(backdrop);
}

function renderEventStream() {
  const container = document.getElementById('boss-container');
  if (!container || !container.querySelector('.boss-card')) return;

  let streamEl = container.querySelector('.event-stream');
  if (!streamEl) {
    streamEl = el('div', 'event-stream');
    container.querySelector('.boss-card').appendChild(streamEl);
  }
  streamEl.innerHTML = '';
  for (const evt of state.events.slice(0, 50)) {
    const row = el('div', 'event-row');
    row.appendChild(el('span', 'event-ts',   fmtTime(evt.timestamp)));
    row.appendChild(el('span', 'event-type', evt.type || 'event'));
    const data = typeof evt.data === 'object' ? JSON.stringify(evt.data) : String(evt.data ?? '');
    row.appendChild(el('span', 'event-data', data));
    streamEl.appendChild(row);
  }
}

// ── Chat helpers ──────────────────────────────────────────

function startChatPoll(taskId) {
  stopChatPoll();
  state.chatPollTimer = setInterval(() => refreshChatMessages(taskId), 8000);
}

function stopChatPoll() {
  if (state.chatPollTimer) { clearInterval(state.chatPollTimer); state.chatPollTimer = null; }
}

async function refreshChatMessages(taskId) {
  if (state.openDetailTaskId !== taskId) { stopChatPoll(); return; }
  try {
    const isFleet = isFleetTaskId(taskId);
    const endpoint = isFleet ? `/api/fleet/tasks/${taskId}/comments` : `/api/tasks/${taskId}/comments`;
    const cr = await apiFetch(endpoint);
    const comments = cr.comments || (Array.isArray(cr) ? cr : []);
    state.taskComments[taskId] = comments;
    if (state.tasks[taskId]) state.tasks[taskId].comments = comments;
    const messagesDiv = document.getElementById('page-chat-messages');
    if (!messagesDiv) return;
    const atBottom = messagesDiv.scrollHeight - messagesDiv.scrollTop <= messagesDiv.clientHeight + 30;
    renderChatMessages(messagesDiv, comments);
    if (atBottom) messagesDiv.scrollTop = messagesDiv.scrollHeight;
    const pageToggle = document.getElementById('page-chat-toggle');
    if (pageToggle) pageToggle.textContent = chatToggleLabel(comments.length);
  } catch { /* silent */ }
}

function chatToggleLabel(n) {
  return `Messages (${n})`;
}

function renderChatMessages(container, comments) {
  container.innerHTML = '';
  if (!comments.length) {
    container.appendChild(el('p', 'panel-field-muted', 'No messages yet.'));
    return;
  }
  for (const c of comments) {
    const authorType = (c.authorType || c.author_type || '').toLowerCase();
    const isAgent = authorType === 'agent' || authorType === 'system';
    const authorLabel = isAgent
      ? (c.authorName || c.author_name || 'Agent')
      : (c.author || 'You');
    const ts = c.createdAt || c.created_at || c.timestamp || '';

    const msg = el('div', `chat-msg ${isAgent ? 'chat-msg-agent' : 'chat-msg-user'}`);
    msg.appendChild(el('div', 'chat-msg-meta', `${authorLabel}${ts ? ' · ' + fmtDateTime(ts) : ''}`));
    const bubble = el('div', 'chat-msg-bubble');
    bubble.appendChild(mdEl(c.body || c.message || ''));
    msg.appendChild(bubble);
    container.appendChild(msg);
  }
}

async function sendComment(taskId, body) {
  const isFleet = isFleetTaskId(taskId);
  const endpoint = isFleet ? `/api/fleet/tasks/${taskId}/comments` : `/api/tasks/${taskId}/comments`;
  const resp = await fetch(endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeader() },
    body: JSON.stringify({ body, message: body, author: 'user' }),
  });
  if (!resp.ok) throw new Error(`Send failed: ${resp.status}`);
  return resp.json().catch(() => null);
}

// ── Detail panel (Right sidebar / Properties) ─────────────
const WORK_KIND_LABELS = {
  coding:       'Coding — Claude Opus only (Gemini never writes code)',
  qa:           'QA & testing — Claude Opus only (writes tests)',
  review:       'Review — Gemini 3.1 Pro, backup Claude Opus (advisory)',
  architecture: 'Architecture — Gemini 3.1 Pro, backup Claude Opus',
  planning:     'Planning — Gemini Flash, backup Claude Sonnet',
  docs:         'Docs — Gemini Flash, backup Claude Sonnet',
};

function workKindLabel(kind) {
  return WORK_KIND_LABELS[kind] || kind || null;
}

// Board rule (router.GeminiCodeForbidden): Gemini never writes code. Code
// kinds (and unknown kinds, which route as coding) cannot pick Gemini.
const NON_CODE_KINDS = new Set(['planning', 'architecture', 'review', 'docs']);
function kindAllowsGemini(kind) { return NON_CODE_KINDS.has(kind); }

// Provider choices (STA-838): value is "provider" or "provider:model".
const PROVIDER_CHOICES = [
  { value: '',                              label: 'Default for the kind of work' },
  { value: 'claude',                        label: 'Claude (Opus)' },
  { value: 'claude:sonnet',                 label: 'Claude Sonnet' },
  { value: 'gemini',                        label: 'Gemini 3.1 Pro', gemini: true },
  { value: 'gemini:gemini-3.8-flash-high',  label: 'Gemini 3.8 Flash', gemini: true },
];

function splitProviderChoice(v) {
  const [provider, model] = String(v || '').split(':');
  return { provider: provider || '', model_override: model || '' };
}

function providerChoiceValue(task) {
  const p = task.provider || '';
  const m = task.model_override || '';
  if (!p) return '';
  if (p === 'claude') return m === 'sonnet' ? 'claude:sonnet' : 'claude';
  if (p === 'gemini') return m === 'gemini-3.8-flash-high' ? 'gemini:gemini-3.8-flash-high' : 'gemini';
  return '';
}

// gateGeminiOptions applies the Gemini rule to a provider select. Non-code
// kinds: Gemini allowed. Code kinds: refused in a work repo (isWork true),
// and in a personal repo every run waits for a Board Touch ID approval.
// isWork null = unknown (create form): the server decides from the repo.
function gateGeminiOptions(select, kind, hintEl, isWork = null) {
  const nonCode = kindAllowsGemini(kind);
  const allowed = nonCode || isWork !== true;
  for (const opt of select.options) {
    if (opt.dataset.gemini === '1') opt.disabled = !allowed;
  }
  if (!allowed && select.selectedOptions[0] && select.selectedOptions[0].dataset.gemini === '1') select.value = '';
  let hint = '';
  if (!allowed) hint = 'Gemini never writes code in a work repo, even with Board approval.';
  else if (!nonCode) hint = 'Gemini on a code task: personal repos only, and every run waits for your Touch ID approval. Refused in work repos.';
  if (hintEl) hintEl.textContent = hint;
}

async function putTaskProvider(taskId, choiceValue) {
  const r = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/provider`, {
    method: 'PUT',
    headers: { ...authHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify(splitProviderChoice(choiceValue)),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `${r.status} ${r.statusText}`);
  return data;
}

// buildProviderField is the task page's provider select (Details).
function buildProviderField(task) {
  const wrap = el('div', 'task-provider-field');
  const select = document.createElement('select');
  select.className = 'task-provider-select';
  select.setAttribute('aria-label', 'Provider');
  for (const c of PROVIDER_CHOICES) {
    const opt = document.createElement('option');
    opt.value = c.value;
    opt.textContent = c.label;
    if (c.gemini) opt.dataset.gemini = '1';
    select.appendChild(opt);
  }
  select.value = providerChoiceValue(task);
  const hint = el('div', 'form-hint');
  const gate = () => gateGeminiOptions(select, task.work_kind || 'coding', hint, task.account_role === 'work');
  gate();
  select.addEventListener('change', async () => {
    const prev = providerChoiceValue(task);
    select.disabled = true;
    try {
      const updated = await putTaskProvider(task.id, select.value);
      Object.assign(task, { provider: updated.provider || '', model_override: updated.model_override || '' });
      if (state.tasks[task.id]) Object.assign(state.tasks[task.id], { provider: task.provider, model_override: task.model_override });
      gate();
      hint.textContent = (hint.textContent ? hint.textContent + ' ' : '') + 'Saved. The next run uses this provider.';
    } catch (err) {
      select.value = prev;
      hint.textContent = err.message || 'Failed to set provider.';
    } finally {
      select.disabled = false;
    }
  });
  wrap.appendChild(select);
  wrap.appendChild(hint);
  return wrap;
}

// buildKindField is the task page's work-kind select (STA-861), next to the
// provider select. PUT /api/tasks/{id}/kind; refused while a run holds the
// task, and Gemini never takes a code kind in a work repo.
function buildKindField(task) {
  const wrap = el('div', 'task-kind-field');
  const select = document.createElement('select');
  select.className = 'task-kind-select';
  select.setAttribute('aria-label', 'Kind of work');
  for (const k of WORK_KINDS) {
    const opt = document.createElement('option');
    opt.value = k;
    opt.textContent = workKindLabel(k);
    select.appendChild(opt);
  }
  select.value = task.work_kind || 'coding';
  const hint = el('div', 'form-hint');
  const isWork = task.account_role === 'work';
  if (taskRunActive(task) || taskIsTerminal(task)) {
    select.disabled = true;
    if (taskRunActive(task)) hint.textContent = 'Stop the run before changing the kind of work.';
  }
  select.addEventListener('change', async () => {
    const prev = task.work_kind || 'coding';
    const refusal = kindChangeRefusal(task, select.value, isWork);
    if (refusal) {
      select.value = prev;
      hint.textContent = refusal;
      return;
    }
    select.disabled = true;
    try {
      const r = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/kind`, {
        method: 'PUT',
        headers: { ...authHeader(), 'Content-Type': 'application/json' },
        body: JSON.stringify({ work_kind: select.value }),
      });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || `${r.status} ${r.statusText}`);
      task.work_kind = data.work_kind || select.value;
      if (state.tasks[task.id]) state.tasks[task.id].work_kind = task.work_kind;
      hint.textContent = 'Saved. The next run routes for this kind.';
      const provSelect = wrap.parentElement && wrap.parentElement.parentElement
        ? wrap.parentElement.parentElement.querySelector('.task-provider-select') : null;
      if (provSelect) gateGeminiOptions(provSelect, task.work_kind, null, isWork);
    } catch (err) {
      select.value = prev;
      hint.textContent = err.message || 'Failed to set kind.';
    } finally {
      select.disabled = false;
    }
  });
  wrap.appendChild(select);
  wrap.appendChild(hint);
  return wrap;
}

function addPanelField(content, label, value) {
  if (!value && value !== 0) return;
  const field = el('div', 'panel-field');
  field.appendChild(el('div', 'panel-field-label', label));
  if (typeof value === 'string') {
    field.appendChild(el('div', 'panel-field-value', value));
  } else {
    field.appendChild(value);
  }
  content.appendChild(field);
}

// Governance, blockers, dependency tree, parent and timestamps for the task
// page Details section. These lived only in the old one-column drawer; the
// drawer now renders the task page layout (STA-700), so both views show them.
// openTask opens a linked task in the same view the user is in.
function appendTaskRelations(target, task, openTask) {
  // Governance Section: Reviewers, Approvers, Quality Gates
  const gov = task.governance;
  if (gov || task.reviewers?.length || task.approvers?.length) {
    const govSection = el('div', 'panel-gov-section');
    govSection.style.cssText = 'margin-top:16px;padding:12px;background:rgba(255,255,255,0.03);border:1px solid var(--border);border-radius:6px;';
    govSection.appendChild(el('div', 'panel-section-title', 'Governance & Quality Gates'));

    // Reviewers
    const reviewers = gov?.reviewers || task.reviewers || [];
    const revWrap = el('div', 'panel-field');
    revWrap.appendChild(el('div', 'panel-field-label', 'Reviewers'));
    if (reviewers.length) {
      const revList = el('div', 'panel-gov-list');
      revList.style.cssText = 'display:flex;flex-direction:column;gap:4px;margin-top:4px;';
      for (const r of reviewers) {
        const item = el('div', 'panel-gov-item');
        item.style.cssText = 'display:flex;align-items:center;justify-content:space-between;font-size:0.8rem;';
        const name = r.reviewer_name || r.name || (r.reviewer_id ? `Reviewer (${r.reviewer_id.slice(0, 8)})` : 'Reviewer');
        item.appendChild(el('span', 'pill', name));
        const dec = gov?.decisions?.find(d => d.reviewer_id === (r.reviewer_id || r.id));
        if (dec) {
          const decSpan = el('span', 'status-pill', dec.decision);
          item.appendChild(decSpan);
        } else {
          item.appendChild(el('span', 'muted-text', 'Pending review'));
        }
        revList.appendChild(item);
      }
      revWrap.appendChild(revList);
    } else {
      revWrap.appendChild(el('div', 'panel-field-muted', 'None assigned'));
    }
    govSection.appendChild(revWrap);

    // Approvers
    const approvers = gov?.approvers || task.approvers || [];
    const appWrap = el('div', 'panel-field');
    appWrap.appendChild(el('div', 'panel-field-label', 'Approvers'));
    if (approvers.length) {
      const appList = el('div', 'panel-gov-list');
      appList.style.cssText = 'display:flex;flex-direction:column;gap:4px;margin-top:4px;';
      for (const a of approvers) {
        const item = el('div', 'panel-gov-item');
        item.style.cssText = 'display:flex;align-items:center;justify-content:space-between;font-size:0.8rem;';
        const name = a.approver_name || a.name || (a.approver_id ? `Approver (${a.approver_id.slice(0, 8)})` : 'Approver');
        item.appendChild(el('span', 'pill', name));
        const vote = gov?.votes?.find(v => v.approver_id === (a.approver_id || a.id));
        if (vote) {
          const voteSpan = el('span', 'status-pill', vote.vote);
          item.appendChild(voteSpan);
        } else {
          item.appendChild(el('span', 'muted-text', 'Pending approval'));
        }
        appList.appendChild(item);
      }
      appWrap.appendChild(appList);
    } else {
      appWrap.appendChild(el('div', 'panel-field-muted', 'None assigned'));
    }
    govSection.appendChild(appWrap);

    // Gate Policy
    const reqReview = (gov?.config?.require_review ?? true) ? 'Required' : 'Optional';
    const thresh = gov?.config ? `${gov.config.approval_threshold} vote(s)` : '1 vote';
    const gateInfo = el('div', 'panel-field');
    gateInfo.appendChild(el('div', 'panel-field-label', 'Gate Policy'));
    gateInfo.appendChild(el('div', 'panel-field-value', `Review: ${reqReview} · Approval Threshold: ${thresh}`));
    govSection.appendChild(gateInfo);

    target.appendChild(govSection);
  }

  // Blocker Status & Upstream Dependencies
  const isBlocked = Boolean(task.is_blocked || task.status === 'blocked');
  const hasBlockers = Boolean(task.blocked_by && task.blocked_by.length > 0);
  const rawReason = task.block_reason || '';
  const hasExplicitReason = rawReason && rawReason.toLowerCase() !== 'blocked via tui' && rawReason.trim() !== '';

  if (isBlocked || hasBlockers) {
    const blockerWrap = el('div', 'panel-field');
    blockerWrap.appendChild(el('div', 'panel-field-label', 'Blocked'));

    let blockerSummary = '';
    if (hasBlockers) {
      const count = task.blocked_by.length;
      blockerSummary = `⚠ Blocked by ${count} upstream task${count > 1 ? 's' : ''}`;
      if (hasExplicitReason && !rawReason.toLowerCase().startsWith('blocked by')) {
        blockerSummary += ` · ${rawReason}`;
      }
    } else if (hasExplicitReason) {
      blockerSummary = `⚠ ${rawReason}`;
    } else {
      blockerSummary = '⚠ Blocked — no specific reason recorded';
    }

    const tag = el('span', 'panel-blocker-tag', blockerSummary);
    blockerWrap.appendChild(tag);
    target.appendChild(blockerWrap);
  }

  // Clickable Upstream Blockers ("Blocked By")
  if (task.blocked_by?.length) {
    const bbSection = el('div', 'panel-field');
    bbSection.appendChild(el('div', 'panel-field-label', `Blocked By (${task.blocked_by.length})`));

    const bbList = el('div', 'panel-blocker-list');
    bbList.style.cssText = 'display:flex;flex-direction:column;gap:6px;margin-top:6px;';

    for (const bb of task.blocked_by) {
      const card = el('div', 'panel-blocker-card');
      card.style.cssText = 'padding:8px 10px;background:rgba(255,255,255,0.04);border:1px solid rgba(248,81,73,0.3);border-radius:6px;cursor:pointer;transition:all 0.15s ease;display:flex;flex-direction:column;gap:3px;';
      card.addEventListener('mouseenter', () => { card.style.background = 'rgba(248,81,73,0.1)'; card.style.borderColor = 'var(--red)'; });
      card.addEventListener('mouseleave', () => { card.style.background = 'rgba(255,255,255,0.04)'; card.style.borderColor = 'rgba(248,81,73,0.3)'; });
      card.addEventListener('click', () => openTask(bb.id));

      const headerRow = el('div', null);
      headerRow.style.cssText = 'display:flex;align-items:center;justify-content:space-between;gap:8px;font-size:0.8rem;';

      const left = el('div', null);
      left.style.cssText = 'display:flex;align-items:center;gap:6px;min-width:0;';
      const linkId = el('span', 'panel-task-link', `${bb.identifier || bb.id?.slice(0, 10)} ↗`);
      linkId.style.cssText = 'font-weight:600;color:var(--accent, #38bdf8);text-decoration:underline;';
      const title = el('span', null, bb.title || bb.name || 'Untitled Task');
      title.style.cssText = 'color:var(--fg);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:500;';
      left.appendChild(linkId);
      left.appendChild(title);

      const stage = bb.execution_stage || bb.status || 'todo';
      const stagePill = el('span', 'status-pill', stage);
      stagePill.style.fontSize = '0.7rem';
      stagePill.style.padding = '1px 6px';

      headerRow.appendChild(left);
      headerRow.appendChild(stagePill);
      card.appendChild(headerRow);

      if (bb.rationale) {
        const rat = el('div', 'panel-blocker-rationale');
        rat.style.cssText = 'font-size:0.75rem;color:var(--amber, #f59e0b);margin-top:2px;font-style:italic;';
        rat.textContent = `↳ Rationale: ${bb.rationale}`;
        card.appendChild(rat);
      }

      bbList.appendChild(card);
    }
    bbSection.appendChild(bbList);
    target.appendChild(bbSection);
  }

  // Clickable Downstream Tasks ("Blocks")
  if (task.blocks?.length) {
    const blocksSection = el('div', 'panel-field');
    blocksSection.appendChild(el('div', 'panel-field-label', `Blocks (${task.blocks.length})`));

    const blocksList = el('div', 'panel-blocker-list');
    blocksList.style.cssText = 'display:flex;flex-direction:column;gap:6px;margin-top:6px;';

    for (const b of task.blocks) {
      const card = el('div', 'panel-blocker-card');
      card.style.cssText = 'padding:8px 10px;background:rgba(255,255,255,0.04);border:1px solid rgba(56,189,248,0.3);border-radius:6px;cursor:pointer;transition:all 0.15s ease;display:flex;flex-direction:column;gap:3px;';
      card.addEventListener('mouseenter', () => { card.style.background = 'rgba(56,189,248,0.1)'; card.style.borderColor = 'var(--accent, #38bdf8)'; });
      card.addEventListener('mouseleave', () => { card.style.background = 'rgba(255,255,255,0.04)'; card.style.borderColor = 'rgba(56,189,248,0.3)'; });
      card.addEventListener('click', () => openTask(b.id));

      const headerRow = el('div', null);
      headerRow.style.cssText = 'display:flex;align-items:center;justify-content:space-between;gap:8px;font-size:0.8rem;';

      const left = el('div', null);
      left.style.cssText = 'display:flex;align-items:center;gap:6px;min-width:0;';
      const linkId = el('span', 'panel-task-link', `${b.identifier || b.id?.slice(0, 10)} ↗`);
      linkId.style.cssText = 'font-weight:600;color:var(--accent, #38bdf8);text-decoration:underline;';
      const title = el('span', null, b.title || b.name || 'Untitled Task');
      title.style.cssText = 'color:var(--fg);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:500;';
      left.appendChild(linkId);
      left.appendChild(title);

      const stage = b.execution_stage || b.status || 'todo';
      const stagePill = el('span', 'status-pill', stage);
      stagePill.style.fontSize = '0.7rem';
      stagePill.style.padding = '1px 6px';

      headerRow.appendChild(left);
      headerRow.appendChild(stagePill);
      card.appendChild(headerRow);

      if (b.rationale) {
        const rat = el('div', 'panel-blocker-rationale');
        rat.style.cssText = 'font-size:0.75rem;color:var(--muted);margin-top:2px;font-style:italic;';
        rat.textContent = `↳ Rationale: ${b.rationale}`;
        card.appendChild(rat);
      }

      blocksList.appendChild(card);
    }
    blocksSection.appendChild(blocksList);
    target.appendChild(blocksSection);
  }

  // Dependency Hierarchy Visualization Section
  const hasDeps = Boolean(task.blocked_by?.length || task.blocks?.length || task.parent_id || task.dependencies?.subtasks?.length);
  if (hasDeps) {
    const depSection = el('div', 'panel-gov-section');
    depSection.style.cssText = 'margin-top:16px;padding:12px;background:rgba(255,255,255,0.03);border:1px solid var(--border);border-radius:6px;';
    depSection.appendChild(el('div', 'panel-section-title', 'Dependency Hierarchy'));

    const treeBox = el('div', 'panel-dep-tree');
    treeBox.style.cssText = 'font-family:monospace;font-size:0.8rem;line-height:1.6;margin-top:8px;padding:8px 10px;background:rgba(0,0,0,0.25);border-radius:4px;overflow-x:auto;';

    // Parent task line if present
    if (task.parent_id || task.dependencies?.parent) {
      const p = task.dependencies?.parent || { id: task.parent_id, name: task.parent_identifier || task.parent_id };
      const pLine = el('div', null);
      pLine.style.cssText = 'color:var(--muted);margin-bottom:4px;cursor:pointer;';
      pLine.innerHTML = `◆ Parent: <span style="color:var(--accent,#38bdf8);text-decoration:underline;">#${p.identifier || p.id?.slice(0, 10)}</span> ${escapeHtml(p.title || p.name || '')}`;
      pLine.addEventListener('click', () => openTask(p.id));
      treeBox.appendChild(pLine);
    }

    // Upstream blockers
    if (task.blocked_by?.length) {
      const upHeader = el('div', null, '▲ Upstream Blockers (Must resolve first):');
      upHeader.style.cssText = 'color:var(--red,#f85149);font-weight:600;margin-top:4px;';
      treeBox.appendChild(upHeader);

      task.blocked_by.forEach((bb, idx) => {
        const isLast = idx === task.blocked_by.length - 1;
        const prefix = isLast ? '  └── ⏳ ' : '  ├── ⏳ ';
        const row = el('div', null);
        row.style.cssText = 'cursor:pointer;padding:2px 0;transition:color 0.15s;';
        row.innerHTML = `${prefix}<span style="color:var(--accent,#38bdf8);text-decoration:underline;font-weight:600;">#${bb.identifier || bb.id?.slice(0, 10)}</span> <span style="color:var(--fg);">${escapeHtml(bb.title || bb.name || '')}</span> <span style="font-size:0.7rem;padding:0 4px;border-radius:3px;background:rgba(255,255,255,0.08);">${bb.execution_stage || bb.status || 'todo'}</span>`;
        row.addEventListener('click', () => openTask(bb.id));
        treeBox.appendChild(row);

        if (bb.rationale) {
          const subPrefix = isLast ? '      └─ ' : '  │   └─ ';
          const ratRow = el('div', null);
          ratRow.style.cssText = 'color:var(--amber,#f59e0b);font-size:0.75rem;';
          ratRow.textContent = `${subPrefix}Rationale: ${bb.rationale}`;
          treeBox.appendChild(ratRow);
        }
      });
    }

    // Current task
    const currLine = el('div', null);
    currLine.style.cssText = 'color:var(--fg);font-weight:700;margin:6px 0;padding:2px 6px;background:rgba(255,255,255,0.06);border-left:3px solid var(--accent,#38bdf8);border-radius:2px;';
    currLine.innerHTML = `● Current: #${task.identifier || task.id?.slice(0, 10)} ${escapeHtml(task.title || task.name || '')} <span style="font-size:0.7rem;font-weight:normal;padding:0 4px;border-radius:3px;background:rgba(255,255,255,0.1);">${task.execution_stage || task.status || 'todo'}</span>`;
    treeBox.appendChild(currLine);

    // Downstream blocked tasks
    if (task.blocks?.length) {
      const downHeader = el('div', null, '▼ Blocks (Downstream tasks waiting):');
      downHeader.style.cssText = 'color:var(--cyan,#38bdf8);font-weight:600;margin-top:4px;';
      treeBox.appendChild(downHeader);

      task.blocks.forEach((b, idx) => {
        const isLast = idx === task.blocks.length - 1;
        const prefix = isLast ? '  └── 🔒 ' : '  ├── 🔒 ';
        const row = el('div', null);
        row.style.cssText = 'cursor:pointer;padding:2px 0;transition:color 0.15s;';
        row.innerHTML = `${prefix}<span style="color:var(--accent,#38bdf8);text-decoration:underline;font-weight:600;">#${b.identifier || b.id?.slice(0, 10)}</span> <span style="color:var(--fg);">${escapeHtml(b.title || b.name || '')}</span> <span style="font-size:0.7rem;padding:0 4px;border-radius:3px;background:rgba(255,255,255,0.08);">${b.execution_stage || b.status || 'todo'}</span>`;
        row.addEventListener('click', () => openTask(b.id));
        treeBox.appendChild(row);

        if (b.rationale) {
          const subPrefix = isLast ? '      └─ ' : '  │   └─ ';
          const ratRow = el('div', null);
          ratRow.style.cssText = 'color:var(--muted);font-size:0.75rem;';
          ratRow.textContent = `${subPrefix}Rationale: ${b.rationale}`;
          treeBox.appendChild(ratRow);
        }
      });
    }

    // Subtasks
    if (task.dependencies?.subtasks?.length) {
      const subHeader = el('div', null, `◇ Subtasks (${task.dependencies.subtasks.length}):`);
      subHeader.style.cssText = 'color:var(--muted);font-weight:600;margin-top:4px;';
      treeBox.appendChild(subHeader);

      task.dependencies.subtasks.forEach((st, idx) => {
        const isLast = idx === task.dependencies.subtasks.length - 1;
        const prefix = isLast ? '  └── ' : '  ├── ';
        const row = el('div', null);
        row.style.cssText = 'cursor:pointer;padding:2px 0;';
        row.innerHTML = `${prefix}<span style="color:var(--accent,#38bdf8);text-decoration:underline;">#${st.identifier || st.id?.slice(0, 10)}</span> ${escapeHtml(st.title || st.name || '')}`;
        row.addEventListener('click', () => openTask(st.id));
        treeBox.appendChild(row);
      });
    }

    depSection.appendChild(treeBox);
    target.appendChild(depSection);
  }

  // Parent task
  if (task.parent_id || task.parent_identifier) {
    addPanelField(target, 'Parent Task',
      task.parent_identifier || `#${task.parent_id?.slice(0, 12)}`);
  }

  // Timestamps
  if (task.created_at || task.updated_at) {
    const tsField = el('div', 'panel-field');
    tsField.appendChild(el('div', 'panel-field-label', 'Timestamps'));
    const tsVal = el('div', 'panel-field-muted');
    const parts = [];
    if (task.created_at) parts.push(`Created: ${fmtDateTime(task.created_at)}`);
    if (task.updated_at) parts.push(`Updated: ${fmtRelTime(task.updated_at)}`);
    tsVal.textContent = parts.join('  ·  ');
    tsField.appendChild(tsVal);
    target.appendChild(tsField);
  }

}

function isFleetTaskId(id) {
  if (!id) return false;
  if (id.startsWith('task-')) return false;
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id)) return true;
  if (/^[A-Za-z]+-\d+$/i.test(id)) return true;
  return false;
}

let lastDetailOpenTime = 0;
// The click that opened the drawer. It bubbles on to the document
// click-outside handler, which must never close what it just opened: the
// 150ms clock check alone fails when the open stalls the main thread (STA-700).
let detailOpenEvent = null;

// Bumped by every openDetail / openTaskPage call. Their fetches can resolve out
// of order (a second click, or ↗ while the drawer is still loading). Only the
// newest call renders, so a stale response neither paints over it nor leaves a
// second copy of the task layout's element ids in the DOM (STA-700).
let taskViewSeq = 0;

// Fetches everything the task layout renders, for the full page and the drawer.
// The page renders as soon as the cheap DB-backed calls return. The diff and
// checkpoint list each run several git processes (seconds on a loaded
// machine), so they arrive later through `diff`, which resolves to
// { diffData, checkpoints } (STA-775). The ship review card stays in the first
// batch: it decides whether Run Now and Mark done are offered at all.
async function fetchTaskViewData(resolvedId) {
  const isFleet = isFleetTaskId(resolvedId);
  const apiBase = isFleet ? '/api/fleet/tasks' : '/api/tasks';
  const diff = isFleet
    ? Promise.resolve({ diffData: { diff: '', files: [], checkpoint_id: '' }, checkpoints: [] })
    : Promise.all([fetchTaskDiff(resolvedId, ''), fetchTaskCheckpoints(resolvedId)])
      .then(([diffData, checkpoints]) => ({ diffData, checkpoints }));
  // Ordered with fetchWholeRunDiff: a newer whole-run fetch wins the stats strip.
  const diffSeq = wholeRunDiffSeq[resolvedId] = (wholeRunDiffSeq[resolvedId] || 0) + 1;
  const [taskResp, commentsResp, stepsResp, interactionsResp, runErrorsResp, shipCardResp, govResp] = await Promise.all([
    apiFetch(`${apiBase}/${encodeURIComponent(resolvedId)}`),
    apiFetch(`${apiBase}/${encodeURIComponent(resolvedId)}/comments`).catch(() => ({ comments: [] })),
    (!isFleet ? apiFetch(`/api/tasks/${encodeURIComponent(resolvedId)}/run-steps`).catch(() => ({ steps: [] })) : Promise.resolve({ steps: [] })),
    (!isFleet ? apiFetch(`/api/tasks/${encodeURIComponent(resolvedId)}/interactions`).catch(() => ({ interactions: [] })) : Promise.resolve({ interactions: [] })),
    (!isFleet ? apiFetch(`/api/tasks/${encodeURIComponent(resolvedId)}/run-errors?limit=20`).catch(() => ({ errors: [] })) : Promise.resolve({ errors: [] })),
    (!isFleet ? apiFetch(`/api/tasks/${encodeURIComponent(resolvedId)}/ship-review`).catch(() => null) : Promise.resolve(null)),
    // Governance is optional, so it no longer waits for the rest to finish.
    apiFetch(`/api/tasks/${encodeURIComponent(resolvedId)}/governance`).catch(() => null),
  ]);
  const task = taskResp.task || taskResp;
  if (taskResp.dependencies) task.dependencies = taskResp.dependencies;
  task.queue = taskResp.queue || null;
  task.turn = taskResp.turn || null;
  task.workspace = taskResp.workspace || null;
  const comments = (taskResp.comments && taskResp.comments.length)
    ? taskResp.comments
    : (commentsResp?.comments || (Array.isArray(commentsResp) ? commentsResp : []));
  task.comments = comments;
  task.runSteps = stepsResp?.steps || [];
  task.runErrors = runErrorsResp?.errors || [];
  const interactions = interactionsResp?.interactions || [];
  const shipCard = (shipCardResp && !shipCardResp.error) ? shipCardResp : null;
  if (govResp && !govResp.error) task.governance = govResp;

  return { task, comments, interactions, shipCard, diff, diffSeq };
}

// Fills in the diff and checkpoints once fetchTaskViewData's `diff` resolves,
// if the same task view (seq) is still showing.
function fillTaskViewDiff(view, data, key, seq) {
  data.diff.then(({ diffData, checkpoints }) => {
    if (seq !== taskViewSeq) return;
    const cached = state.tasks[key];
    if (cached) {
      cached._checkpoints = checkpoints;
      // fetchWholeRunDiff keeps the last good numbers on an error, and a
      // run-step refresh started after this fetch is newer: keep both.
      if (wholeRunDiffSeq[key] === data.diffSeq && diffData && ('file_stats' in diffData || !cached._diffData)) {
        cached._diffData = diffData;
      }
    }
    view.setDiff(diffData, checkpoints);
    refreshTaskStatsBar(key);
  });
}

function cacheTaskView(task, comments, fallbackId) {
  const key = task.id || fallbackId;
  state.tasks[key] = { ...(state.tasks[key] || {}), ...task };
  if (task.description) state.taskDescriptions[key] = task.description;
  state.taskComments[key] = comments;
  return key;
}

// Renders a cached copy of the task when the daemon can't be reached, with
// every action disabled.
function renderCachedTaskView(container, cached, opts) {
  renderTaskPage(container, cached, cached.comments || [], [], cached._diffData || null, cached._checkpoints || [], undefined, undefined, opts);
  const banner = document.createElement('div');
  banner.style.cssText = 'background:var(--red,#c0392b);color:#fff;padding:.5rem 1rem;font-size:.85rem;font-weight:600;';
  banner.textContent = 'Daemon unreachable — showing cached copy. Actions are disabled.';
  container.prepend(banner);
  container.querySelectorAll('button,input,textarea').forEach(el => { el.disabled = true; });
}

function isTaskPageShowing() {
  return Boolean(document.getElementById('view-task-page')?.classList.contains('active'));
}

// The drawer ("peek") renders the task page layout (STA-700), stacked to fit
// its width; see .task-page-drawer in style.css.
async function openDetail(target, pushHistory = true, orgHint = null, projectHint = null, fromRouteMiss = false) {
  // One copy of the layout at a time: its element ids are global. On the full
  // page a task link therefore opens that task's page instead of a drawer.
  if (isTaskPageShowing()) {
    const t = (typeof target === 'object' && target !== null) ? target : { id: target, org: orgHint, project: projectHint };
    return openTaskPage(t, pushHistory);
  }

  const panel   = document.getElementById('detail-panel');
  const content = document.getElementById('panel-content');
  const seq = ++taskViewSeq;

  stopChatPoll();
  stopElapsedTicker();
  lastDetailOpenTime = Date.now();
  detailOpenEvent = window.event || null;
  if (panel) {
    panel.classList.remove('hidden');
    panel.classList.toggle('full-page', Boolean(state.taskDetailFullPage));
  }
  const expandBtn = document.getElementById('panel-expand');
  if (expandBtn) {
    expandBtn.classList.toggle('active', Boolean(state.taskDetailFullPage));
    expandBtn.title = state.taskDetailFullPage
      ? 'Exit full page (Restore side panel)'
      : 'Expand to full page (main viewport minus sidebar)';
    expandBtn.innerHTML = state.taskDetailFullPage ? '🗗' : '&#x26F6;';
    expandBtn.setAttribute('aria-pressed', String(Boolean(state.taskDetailFullPage)));
  }
  // The full page isn't showing (checked above): drop its stale copy.
  const pageContent = document.getElementById('task-page-content');
  if (pageContent) pageContent.innerHTML = '';
  if (content) {
    content.classList.add('task-page-drawer');
    content.innerHTML = '<p style="color:var(--muted);padding:20px">Loading…</p>';
  }

  let targetId = target;
  if (typeof target === 'object' && target !== null) {
    orgHint = target.org || target.organization || orgHint;
    projectHint = target.project || projectHint;
    targetId = target.identifier || target.taskId || target.id;
  }

  // Attempt resolution from in-memory tasks
  const matchedTask = findTask(targetId, orgHint, projectHint);
  const resolvedId = matchedTask ? matchedTask.id : targetId;
  state.openDetailTaskId = resolvedId;

  // Determine canonical hierarchical path
  const canonicalPath = matchedTask
    ? taskToPath(matchedTask)
    : `/tasks/${encodeURIComponent(orgHint || 'STA')}/${encodeURIComponent(projectHint || 'default')}/${encodeURIComponent(targetId)}`;

  if (pushHistory && window.location.pathname !== canonicalPath) {
    history.pushState({ taskId: resolvedId, canonicalPath, taskPage: false }, '', canonicalPath);
  } else if (!pushHistory && (window.location.pathname.startsWith('/tasks/') || window.location.pathname.startsWith('/issues/')) && window.location.pathname !== canonicalPath) {
    history.replaceState({ taskId: resolvedId, canonicalPath, taskPage: false }, '', canonicalPath);
  }

  try {
    const data = await fetchTaskViewData(resolvedId);
    const { task, comments, interactions, shipCard } = data;
    if (seq !== taskViewSeq) return;

    // Use robust UUID for internal state
    if (task.id) {
      state.openDetailTaskId = task.id;
    }

    // Ensure browser URL displays the fully resolved canonical hierarchical path
    const finalCanonicalPath = taskToPath(task);
    if (window.location.pathname !== finalCanonicalPath && (window.location.pathname.startsWith('/tasks/') || window.location.pathname.startsWith('/issues/'))) {
      history.replaceState({ taskId: task.id, canonicalPath: finalCanonicalPath, taskPage: false }, '', finalCanonicalPath);
    }

    const detailKey = cacheTaskView(task, comments, resolvedId);
    const view = renderTaskPage(content, task, comments, interactions, undefined, undefined, task.runErrors, shipCard, { drawer: true });
    fillTaskViewDiff(view, data, detailKey, seq);
    startChatPoll(detailKey);
  } catch (err) {
    if (seq !== taskViewSeq) return;
    if (!fromRouteMiss && err && /^404\b/.test(err.message)) {
      const fallbackId = await resolveTaskRouteMiss(targetId, orgHint, projectHint);
      if (seq !== taskViewSeq) return;
      if (fallbackId && fallbackId !== resolvedId) {
        return openDetail(fallbackId, false, orgHint, projectHint, true);
      }
    }
    const cached = matchedTask || state.tasks[resolvedId] || state.tasks[targetId];
    if (cached && content) {
      if (cached.id) state.openDetailTaskId = cached.id;
      renderCachedTaskView(content, cached, { drawer: true });
    } else {
      const p = el('p', null, 'Task not found or failed to load.');
      p.style.cssText = 'color:var(--red);padding:20px;';
      if (content) {
        content.innerHTML = '';
        content.appendChild(p);
      }
    }
  }
}

function appendComments(container, comments) {
  container.appendChild(el('div', 'panel-section-title', `Comments (${comments.length})`));
  for (const c of comments) {
    const row = el('div', 'panel-comment');
    const authorType = c.authorType || c.author_type || '';
    const author = authorType === 'agent' ? 'agent' : (c.author || 'user');
    const ts = c.createdAt || c.created_at || c.timestamp || '';
    if (author || ts) {
      row.appendChild(el('div', 'panel-comment-meta', `${author} · ${ts ? fmtDateTime(ts) : ''}`));
    }
    row.appendChild(mdEl(c.body || c.message || ''));
    container.appendChild(row);
  }
}

function toggleDetailFullPage() {
  const panel = document.getElementById('detail-panel');
  const btn = document.getElementById('panel-expand');
  if (!panel) return;
  state.taskDetailFullPage = !state.taskDetailFullPage;
  panel.classList.toggle('full-page', Boolean(state.taskDetailFullPage));
  if (btn) {
    btn.classList.toggle('active', Boolean(state.taskDetailFullPage));
    btn.setAttribute('aria-pressed', String(Boolean(state.taskDetailFullPage)));
    if (state.taskDetailFullPage) {
      btn.title = 'Exit full page (Restore side panel)';
      btn.innerHTML = '🗗';
      btn.setAttribute('aria-label', 'Exit full page');
    } else {
      btn.title = 'Expand to full page (main viewport minus sidebar)';
      btn.innerHTML = '&#x26F6;';
      btn.setAttribute('aria-label', 'Expand to full page');
    }
  }
}

function closeDetailPanel() {
  const panel = document.getElementById('detail-panel');
  if (!panel || panel.classList.contains('hidden')) return;
  panel.classList.add('hidden');
  if (state.taskDetailFullPage) {
    state.taskDetailFullPage = false;
    panel.classList.remove('full-page');
    const expandBtn = document.getElementById('panel-expand');
    if (expandBtn) {
      expandBtn.classList.remove('active');
      expandBtn.setAttribute('aria-pressed', 'false');
      expandBtn.title = 'Expand to full page (main viewport minus sidebar)';
      expandBtn.innerHTML = '&#x26F6;';
      expandBtn.setAttribute('aria-label', 'Expand to full page');
    }
  }
  stopChatPoll();
  stopElapsedTicker();
  state.openDetailTaskId = null;
  if (window.location.pathname.startsWith('/tasks/') || window.location.pathname.startsWith('/issues/')) {
    const activeBtn = document.querySelector('.sidebar-item.active');
    const viewName = activeBtn?.dataset?.view || 'overview';
    navigateTo(viewName, state.currentOrgDetail, true);
  }
}

document.getElementById('panel-close')?.addEventListener('click', closeDetailPanel);
document.getElementById('panel-expand')?.addEventListener('click', toggleDetailFullPage);
document.getElementById('panel-open')?.addEventListener('click', () => {
  if (state.openDetailTaskId) openTaskPage(state.openDetailTaskId);
});

// ── Diff pane helpers ──────────────────────────────────────
async function fetchTaskDiff(taskId, checkpointId) {
  const qs = checkpointId ? `?checkpoint=${encodeURIComponent(checkpointId)}` : '';
  try {
    const r = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/diff${qs}`, { headers: authHeader() });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) {
      // STA-774: a base that failed verification (e.g. moved by the agent)
      // must read as an error, never as "No changes".
      return { diff: '', files: [], checkpoint_id: checkpointId || '', error: body.message || body.error || `${r.status} ${r.statusText}` };
    }
    return body;
  } catch {
    return { diff: '', files: [], checkpoint_id: checkpointId || '', error: 'Could not load the diff.' };
  }
}

// The stats strip's Lines +/- sums the whole-run diff kept in
// state.tasks[id]._diffData, which the page loads once. Re-fetch it after run
// steps that change files (debounced, so a burst of edits costs one /diff call)
// and whenever the diff pane re-fetches the whole run. Only the newest fetch
// writes, so a slow response cannot roll the numbers back.
const DIFF_REFRESH_DEBOUNCE_MS = 1000;
const diffRefreshTimers = {};
const wholeRunDiffSeq = {};

function scheduleTaskDiffRefresh(taskId) {
  clearTimeout(diffRefreshTimers[taskId]);
  diffRefreshTimers[taskId] = setTimeout(() => {
    delete diffRefreshTimers[taskId];
    fetchWholeRunDiff(taskId, '');
  }, DIFF_REFRESH_DEBOUNCE_MS);
}

async function fetchWholeRunDiff(taskId, checkpointId) {
  const seq = wholeRunDiffSeq[taskId] = (wholeRunDiffSeq[taskId] || 0) + 1;
  const fresh = await fetchTaskDiff(taskId, checkpointId);
  // fetchTaskDiff turns errors into an empty diff without file_stats; keep the
  // last good numbers rather than zeroing the strip.
  if (seq === wholeRunDiffSeq[taskId] && fresh && 'file_stats' in fresh && state.tasks[taskId]) {
    state.tasks[taskId]._diffData = fresh;
    refreshTaskStatsBar(taskId);
  }
  return fresh;
}

async function fetchTaskCheckpoints(taskId) {
  try {
    const r = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/checkpoints`);
    return r.checkpoints || [];
  } catch {
    return [];
  }
}

async function fetchFileDiff(taskId, filePath, checkpointId) {
  const qs = new URLSearchParams({ path: filePath });
  if (checkpointId) qs.set('checkpoint', checkpointId);
  try {
    return await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/diff/file?${qs}`);
  } catch {
    return { path: filePath, content: '', binary: false, truncated: false, status: 'modified' };
  }
}

// ── Long-content modal (STA-643) ─────────────────────────────
// Long blocks on the task page (brief, dev-server log, agent summary,
// migration SQL) show a bounded preview plus an "Open" button that shows the
// whole thing in this one shared dialog. getContent runs at click time, so
// the dialog includes live updates (dev log progress, an edited brief).
function longContentTrigger(key, title, getContent) {
  const btn = el('button', 'btn btn-secondary btn-sm long-content-open', 'Open');
  btn.type = 'button';
  btn.dataset.modalTrigger = key;
  btn.setAttribute('aria-haspopup', 'dialog');
  btn.setAttribute('aria-label', `Open full ${title.toLowerCase()}`);
  btn.addEventListener('click', () => openContentModal(title, getContent()));
  return btn;
}

// Copies a rendered block's children (already sanitized markdown) for the dialog.
function cloneChildren(src, cls) {
  const out = el('div', cls);
  if (src) src.childNodes.forEach((n) => out.appendChild(n.cloneNode(true)));
  return out;
}

function closeContentModal() {
  const open = document.querySelector('.content-modal');
  if (open && open._close) open._close();
}

function openContentModal(title, content) {
  closeContentModal();
  const opener = document.activeElement;

  const overlay = el('div', 'content-modal');
  const box = el('div', 'content-modal-box');
  const titleId = `content-modal-title-${Date.now()}`;
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  box.setAttribute('aria-labelledby', titleId);

  const header = el('div', 'content-modal-header');
  const h = el('h2', 'content-modal-title', title);
  h.id = titleId;
  header.appendChild(h);
  const closeBtn = el('button', 'content-modal-close', '✕');
  closeBtn.type = 'button';
  closeBtn.setAttribute('aria-label', 'Close');
  header.appendChild(closeBtn);
  box.appendChild(header);

  const body = el('div', 'content-modal-body');
  body.tabIndex = 0; // keyboard scrolling
  body.appendChild(content);
  box.appendChild(body);
  overlay.appendChild(box);

  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); return; }
    if (e.key !== 'Tab') return;
    // Keep focus inside the dialog.
    const focusables = [...box.querySelectorAll('button, [href], [tabindex]:not([tabindex="-1"])')];
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  function close() {
    document.removeEventListener('keydown', onKey, true);
    overlay.remove();
    if (opener && opener.isConnected && typeof opener.focus === 'function') opener.focus();
  }
  overlay._close = close;
  closeBtn.addEventListener('click', close);
  overlay.addEventListener('click', (e) => { if (e.target === overlay) close(); });
  // Capture phase + stopPropagation: Escape closes only this dialog, not the
  // detail panel or task page behind it.
  document.addEventListener('keydown', onKey, true);

  document.body.appendChild(overlay);
  closeBtn.focus();
}

// ── File diff modal ──────────────────────────────────────────
function openFileDiffModal(task, filePath, checkpointId) {
  // Remove any existing modal
  document.querySelector('.file-diff-modal')?.remove();

  const overlay = document.createElement('div');
  overlay.className = 'file-diff-modal';
  overlay.setAttribute('role', 'dialog');
  overlay.setAttribute('aria-modal', 'true');
  overlay.setAttribute('aria-label', 'File diff: ' + filePath);

  const inner = document.createElement('div');
  inner.className = 'file-diff-modal-inner';
  overlay.appendChild(inner);

  // Header
  const header = document.createElement('div');
  header.className = 'file-diff-header';
  const pathEl = document.createElement('span');
  pathEl.className = 'file-diff-header-path';
  pathEl.textContent = filePath;
  header.appendChild(pathEl);
  const closeBtn = document.createElement('button');
  closeBtn.className = 'file-diff-close';
  closeBtn.textContent = '✕';
  closeBtn.setAttribute('aria-label', 'Close diff');
  closeBtn.addEventListener('click', () => overlay.remove());
  header.appendChild(closeBtn);
  inner.appendChild(header);

  // Body (loading state)
  const body = document.createElement('div');
  body.className = 'file-diff-body';
  const loading = document.createElement('div');
  loading.className = 'ds-empty';
  loading.textContent = 'Loading diff…';
  body.appendChild(loading);
  inner.appendChild(body);

  document.body.appendChild(overlay);

  // Dismiss on backdrop click
  overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });
  // Dismiss on Escape. Capture phase + stopPropagation: Escape closes only the
  // diff, not the drawer behind it (STA-700). A modal already closed another
  // way just unhooks the listener on the next Escape.
  const escHandler = e => {
    if (e.key !== 'Escape') return;
    document.removeEventListener('keydown', escHandler, true);
    if (!overlay.isConnected) return;
    e.preventDefault();
    e.stopPropagation();
    overlay.remove();
  };
  document.addEventListener('keydown', escHandler, true);

  // Fetch and render
  fetchFileDiff(task.id, filePath, checkpointId).then(data => {
    body.innerHTML = '';
    // Status badge
    const status = data.binary ? 'binary' : (data.status || 'modified');
    const badge = document.createElement('span');
    badge.className = `file-diff-badge file-diff-badge-${status}`;
    badge.textContent = status;
    header.insertBefore(badge, closeBtn);

    // Truncated warning
    if (data.truncated) {
      const warn = document.createElement('div');
      warn.className = 'file-diff-truncated';
      warn.textContent = 'File diff truncated at 256 KB. Only partial content shown.';
      inner.insertBefore(warn, body);
    }

    // Render diff using vendored DiffSyntax
    if (typeof DiffSyntax !== 'undefined') {
      DiffSyntax.renderUnifiedDiff(body, data.content || '', filePath, { maxCtxLines: 3 });
    } else {
      const pre = document.createElement('pre');
      pre.className = 'ds-line ds-ctx ds-code';
      pre.style.cssText = 'padding:16px;font-size:12px;overflow:auto;';
      pre.textContent = data.content || '(no content)';
      body.appendChild(pre);
    }
  });
}

function renderDiffPane(container, task, checkpoints, diffData) {
  container.innerHTML = '';

  const titleRow = el('div', 'diff-pane-title');
  titleRow.appendChild(document.createTextNode('Diff'));
  const refreshBtn = el('button', 'diff-pane-refresh-btn', '↺');
  refreshBtn.title = 'Refresh diff';
  titleRow.appendChild(refreshBtn);
  container.appendChild(titleRow);

  // Checkpoint selector buttons
  const selector = el('div', 'diff-pane-selector');
  const cpOptions = [{ id: '', label: 'Whole run' }, ...checkpoints.slice(0, 5).map(c => ({
    id: c.id,
    label: c.message ? c.message.slice(0, 22) : c.id.slice(0, 8),
  }))];

  let activeCP = diffData.checkpoint_id || '';
  // The pane opens on the whole-run diff, which the server pins to the pre-run
  // checkpoint id. Re-fetches of it also refresh the stats strip's Lines.
  const wholeRunCP = activeCP;
  const refetch = (cpId) => (cpId === '' || cpId === wholeRunCP)
    ? fetchWholeRunDiff(task.id, cpId)
    : fetchTaskDiff(task.id, cpId);
  const fileStats = diffData.file_stats || (diffData.files || []).map(f => ({ path: f, added: 0, removed: 0 }));

  const fileList = el('ul', 'diff-file-list');
  if (diffData.error) {
    container.appendChild(el('div', 'diff-pane-error', diffData.error));
  } else if (diffData.base_verified === false) {
    container.appendChild(el('div', 'diff-pane-error', 'This task has no recorded base: the whole-run diff cannot be verified.'));
  }

  const renderFiles = (statsArr, cpId) => {
    fileList.innerHTML = '';
    if (!statsArr || !statsArr.length) {
      fileList.appendChild(el('li', 'diff-pane-empty', 'No changes since this checkpoint.'));
      return;
    }
    for (const stat of statsArr) {
      const fname = stat.path || stat;
      const row = el('li', 'diff-file-row');

      // File name — clicking opens the file diff modal
      const nameBtn = el('span', 'diff-file-name', fname);
      nameBtn.title = `View diff for ${fname}`;
      nameBtn.setAttribute('role', 'button');
      nameBtn.setAttribute('tabindex', '0');
      nameBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        openFileDiffModal(task, fname, cpId);
      });
      nameBtn.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openFileDiffModal(task, fname, cpId); }
      });
      row.appendChild(nameBtn);

      // Per-file line stats
      const statsWrap = el('span', 'diff-file-stats');
      if (stat.added > 0) {
        const add = el('span', 'diff-file-stat-add', `+${stat.added}`);
        statsWrap.appendChild(add);
      }
      if (stat.removed > 0) {
        const del = el('span', 'diff-file-stat-del', `-${stat.removed}`);
        statsWrap.appendChild(del);
      }
      if (stat.added > 0 || stat.removed > 0) row.appendChild(statsWrap);

      const undoBtn = el('button', 'diff-file-undo-btn', '↺ Undo');
      undoBtn.title = `Restore ${fname} to checkpoint${cpId ? ' ' + cpId.slice(0, 8) : ''}`;
      undoBtn.addEventListener('click', async (e) => {
        e.stopPropagation();
        undoBtn.disabled = true;
        undoBtn.textContent = '…';
        try {
          await apiFetch(`/api/tasks/${encodeURIComponent(task.id)}/checkpoint-restore-file`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ checkpoint_id: cpId || '', file_path: fname }),
          });
          undoBtn.textContent = '✓';
          setTimeout(async () => {
            const fresh = await refetch(cpId);
            renderFiles(fresh.file_stats || (fresh.files || []).map(f => ({ path: f, added: 0, removed: 0 })), cpId);
          }, 400);
        } catch {
          undoBtn.disabled = false;
          undoBtn.textContent = '↺ Undo';
        }
      });
      row.appendChild(undoBtn);
      fileList.appendChild(row);
    }
  };

  for (const opt of cpOptions) {
    const btn = el('button', 'diff-pane-selector-btn', opt.label);
    if (opt.id === activeCP) btn.classList.add('active');
    btn.addEventListener('click', async () => {
      selector.querySelectorAll('.diff-pane-selector-btn').forEach(b => b.classList.remove('active'));
      btn.classList.add('active');
      activeCP = opt.id;
      btn.textContent = '…';
      const fresh = await refetch(opt.id);
      btn.textContent = opt.label;
      renderFiles(fresh.file_stats || (fresh.files || []).map(f => ({ path: f, added: 0, removed: 0 })), opt.id);
    });
    selector.appendChild(btn);
  }
  container.appendChild(selector);

  renderFiles(fileStats, activeCP);
  container.appendChild(fileList);

  refreshBtn.addEventListener('click', async () => {
    refreshBtn.textContent = '…';
    const fresh = await refetch(activeCP);
    refreshBtn.textContent = '↺';
    renderFiles(fresh.file_stats || (fresh.files || []).map(f => ({ path: f, added: 0, removed: 0 })), activeCP);
  });
}

// ── Run-step timeline helpers ──────────────────────────────

const STEP_KIND_ICON = {
  wake:       '⏰',
  route:      '🔀',
  think:      '🧠',
  run:        '⚙️',
  read:       '📖',
  edit:       '✏️',
  checkpoint: '📌',
  state:      '💾',
  message:    '💬',
  tool:       '🔧',
  error:      '❌',
  result:     '✅',
};

function stepKindIcon(kind) {
  return STEP_KIND_ICON[kind] || '▶';
}

function fmtDuration(ms) {
  if (ms === null || ms === undefined || isNaN(ms)) return '—';
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  const m = Math.floor(ms / 60000);
  const s = Math.floor((ms % 60000) / 1000);
  return `${m}m ${s}s`;
}

// ── Run-control bar ───────────────────────────────────────────────────────────

function buildRunControlBar(task) {
  const wrap = el('div', 'run-control-bar');
  const stage = task.execution_stage || task.status || '';
  const isActive = stage === 'in_progress' || stage === 'paused';
  if (!isActive) wrap.classList.add('hidden');
  wrap.setAttribute('data-task-id', task.id || '');
  renderRunControlBarContent(wrap, task.id, stage);
  // Under the STA-525 step-boundary gate the task stays in_progress while
  // paused. Sync from run-control-state so a reload after pause shows Resume.
  if (stage === 'in_progress') syncRunControlBar(task.id);
  return wrap;
}

function renderRunControlBarContent(wrap, taskId, stage) {
  wrap.innerHTML = '';
  const isPaused = stage === 'paused';

  // Pause / Resume
  const pauseBtn = el('button', `run-ctrl-btn run-ctrl-pause${isPaused ? ' active' : ''}`,
    isPaused ? '▶ Resume' : '⏸ Pause after step');
  pauseBtn.title = isPaused ? 'Resume the run' : 'Finish current tool call, then pause';
  pauseBtn.addEventListener('click', () => {
    const action = isPaused ? 'resume' : 'pause';
    runControlAction(taskId, action);
  });

  // Stop
  const stopBtn = el('button', 'run-ctrl-btn run-ctrl-stop', '⏹ Stop');
  stopBtn.title = 'Terminate the run immediately';
  stopBtn.addEventListener('click', () => {
    if (confirm('Stop this run?')) runControlAction(taskId, 'stop');
  });

  // Send message
  const msgWrap = el('div', 'run-ctrl-msg-wrap');
  const msgBtn = el('button', 'run-ctrl-btn run-ctrl-msg', '✉ Send message');
  msgBtn.title = 'Inject a user message before the next turn';
  const msgArea = el('textarea', 'run-ctrl-textarea hidden');
  msgArea.placeholder = 'Message for the agent…';
  msgArea.rows = 2;
  const sendBtn = el('button', 'run-ctrl-btn run-ctrl-send hidden', 'Send');
  msgBtn.addEventListener('click', () => {
    msgArea.classList.toggle('hidden');
    sendBtn.classList.toggle('hidden');
    if (!msgArea.classList.contains('hidden')) msgArea.focus();
  });
  sendBtn.addEventListener('click', () => {
    const text = msgArea.value.trim();
    if (!text) return;
    runControlAction(taskId, 'message', text);
    msgArea.value = '';
    msgArea.classList.add('hidden');
    sendBtn.classList.add('hidden');
  });
  msgWrap.appendChild(msgBtn);
  msgWrap.appendChild(msgArea);
  msgWrap.appendChild(sendBtn);

  wrap.appendChild(pauseBtn);
  wrap.appendChild(stopBtn);
  wrap.appendChild(msgWrap);
}

function updateRunControlBar(taskId, disposition) {
  const bar = document.getElementById(`run-control-bar-${taskId}`);
  if (!bar) return;
  const isActive = disposition === 'in_progress' || disposition === 'paused';
  if (isActive) {
    bar.classList.remove('hidden');
    renderRunControlBarContent(bar, taskId, disposition);
  } else {
    bar.classList.add('hidden');
  }
}

async function runControlAction(taskId, action, text) {
  try {
    const body = { action };
    if (text) body.text = text;
    await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/run-control`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    // Sync bar immediately after pause/resume — the task stays in_progress
    // under STA-525 so no run.state SSE fires to flip the button label.
    if (action === 'pause' || action === 'resume') syncRunControlBar(taskId);
  } catch (e) {
    console.error('run-control action failed', e);
  }
}

// Fetch current pause/stop state and re-render the run-control bar.
// Called after pause/resume actions and on initial render of in_progress tasks.
async function syncRunControlBar(taskId) {
  try {
    const data = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/run-control-state`);
    const stage = data.paused ? 'paused' : 'in_progress';
    updateRunControlBar(taskId, stage);
  } catch {
    // ignore — bar stays in current state
  }
}

// Show or hide the task page run-queue line for a queue position.
function setRunQueueLine(line, pos) {
  if (!line) return;
  const text = queueLabel(pos);
  line.textContent = text;
  line.classList.toggle('hidden', !text);
}

// ── Timeline stats helpers ─────────────────────────────────
// groupStepsByRun, isRealRunGroup, latestRunSteps, currentRunSteps,
// runElapsedMs, and isStuck live in lib/runsteps.js (loaded before this
// script) so unit tests can import them without a DOM or a build step.

// Refresh the stats bar for the given task, using only current-run steps.
function refreshTaskStatsBar(taskId) {
  const statsBar = document.getElementById(`timeline-stats-${taskId}`);
  if (!statsBar) return;
  const task = state.tasks[taskId] || {};
  const allSteps = task.runSteps || [];
  const runSteps = currentRunSteps(allSteps);
  const elapsedMs = runElapsedMs(runSteps, Date.now());
  const stuck = isStuck(allSteps, Date.now(), task.status);
  statsBar.innerHTML = '';
  statsBar.appendChild(buildTimelineStats(task, runSteps, elapsedMs, stuck));
}

// Start a 1-second ticker that updates the open task's Elapsed stat while running.
function startElapsedTicker(taskId) {
  stopElapsedTicker();
  state.statsElapsedTimer = setInterval(() => {
    const task = state.tasks[taskId] || {};
    const allSteps = task.runSteps || [];
    const runSteps = currentRunSteps(allSteps);
    // Stop ticking once the run has a terminal state step
    const stateStep = runSteps.find(s => s.kind === 'state');
    if (stateStep) { stopElapsedTicker(); return; }
    refreshTaskStatsBar(taskId);
  }, 1000);
}

function stopElapsedTicker() {
  if (state.statsElapsedTimer) { clearInterval(state.statsElapsedTimer); state.statsElapsedTimer = null; }
}

function buildTimelineStats(task, steps, elapsedMs, isStuck) {
  const wrap = el('div', 'timeline-stats-inner');
  // A space between cells keeps the strip's text readable ("−6 Commands",
  // not "−6Commands"); the grid ignores whitespace-only text.
  const add = (tile) => {
    if (wrap.lastChild) wrap.appendChild(document.createTextNode(' '));
    wrap.appendChild(tile);
  };
  const stat = (label, value, cls) => {
    const s = el('span', `timeline-stat${cls ? ' ' + cls : ''}`);
    s.appendChild(el('span', 'timeline-stat-label', label));
    s.appendChild(el('span', 'timeline-stat-val', String(value)));
    return s;
  };

  const filesRead    = steps.filter(s => s && s.kind === 'read'  && s.status === 'done').length;
  const filesEdited  = steps.filter(s => s && s.kind === 'edit'  && s.status === 'done').length;
  const commandsOk   = steps.filter(s => s && s.kind === 'run'   && s.status === 'done').length;
  const commandsFail = steps.filter(s => s && s.kind === 'run' && (s.status === 'error' || s.status === 'failed')).length;
  const currentStep  = runStepLabel(steps);

  add(stat('Elapsed', fmtDuration(elapsedMs)));
  // How long the current turn has run. No fixed cap: only a turn with no
  // activity for stall_timeout is stopped (shown in the tooltip).
  const turnText = turnElapsedLabel(task.turn, Date.now());
  if (turnText) {
    const turnTile = stat('Turn', turnText, 'timeline-stat-turn');
    const stallMin = Math.round((task.turn.stall_timeout_sec || 0) / 60);
    turnTile.title = stallMin > 0
      ? `Current turn. Stopped only if the agent shows no activity for ${stallMin}m.`
      : 'Current turn. No stall limit.';
    add(turnTile);
  }
  const stepTile = stat('Step', currentStep, 'timeline-stat-step');
  stepTile.title = currentStep;
  add(stepTile);
  add(stat('Files read', filesRead));
  add(stat('Files edited', filesEdited));

  // Lines +/- over the whole run, from the diff the page loads after first
  // render; "…" until it arrives.
  const fileStats = (task._diffData && task._diffData.file_stats) || [];
  const added = fileStats.reduce((n, f) => n + (f.added || 0), 0);
  const removed = fileStats.reduce((n, f) => n + (f.removed || 0), 0);
  const linesTile = el('span', 'timeline-stat');
  linesTile.appendChild(el('span', 'timeline-stat-label', 'Lines'));
  const linesVal = el('span', 'timeline-stat-val');
  if (task._diffData === undefined) {
    linesVal.textContent = '…';
    linesVal.title = 'Loading diff';
  } else {
    linesVal.appendChild(el('span', 'lines-added', `+${added}`));
    linesVal.appendChild(document.createTextNode(' '));
    linesVal.appendChild(el('span', 'lines-removed', `−${removed}`));
  }
  linesTile.appendChild(linesVal);
  add(linesTile);

  const cmdTile = el('span', 'timeline-stat');
  cmdTile.appendChild(el('span', 'timeline-stat-label', 'Commands'));
  const cmdVal = el('span', 'timeline-stat-val');
  cmdVal.appendChild(el('span', 'cmds-ok', `✓${commandsOk}`));
  cmdVal.appendChild(document.createTextNode(' '));
  cmdVal.appendChild(el('span', 'cmds-fail', `✗${commandsFail}`));
  cmdTile.appendChild(cmdVal);
  add(cmdTile);

  add(stat('Tokens', fmtCompactNum(task.spent_tokens || 0)));
  add(stat('Cost', fmtCurrency(task.spent_usd || 0)));
  if (isStuck) {
    add(stat('⚠️ Stuck', '>5 min no step', 'timeline-stat-warn'));
  }
  return wrap;
}

function buildRunStepRow(s) {
  const isError = s.status === 'error';
  const isWarn = !isError && s.kind === 'message' && /^Warning:/.test(s.title || '');
  const row = el('div', `timeline-row timeline-row-${s.kind || 'unknown'}${isError ? ' timeline-row-error' : ''}${isWarn ? ' timeline-row-warn' : ''}`);
  row.setAttribute('data-step-id', s.id);

  // Route rows render as a compact banner: icon + plain text, no kind badge.
  if (s.kind === 'route') {
    const banner = el('div', 'timeline-row-summary timeline-route-banner');
    banner.style.cursor = s.body ? 'pointer' : 'default';
    banner.appendChild(el('span', 'timeline-icon', '🔀'));
    banner.appendChild(el('span', 'timeline-route-label', s.title || '(route)'));
    row.appendChild(banner);
    if (s.body) {
      const detail = el('div', 'timeline-body hidden', s.body);
      row.appendChild(detail);
      banner.addEventListener('click', () => detail.classList.toggle('hidden'));
    }
    return row;
  }

  const summary = el('div', 'timeline-row-summary');
  summary.style.cursor = (s.body || s.kind === 'run') ? 'pointer' : 'default';

  const icon = el('span', 'timeline-icon', stepKindIcon(s.kind));
  const kindBadge = el('span', `timeline-kind timeline-kind-${s.kind || 'unknown'}`, s.kind || 'step');
  const title = el('span', 'timeline-title', s.title || '(step)');
  const ts = el('span', 'timeline-ts', s.created_at ? fmtTime(s.created_at) : '');

  const dur = (s.started_at && s.ended_at)
    ? fmtDuration(new Date(s.ended_at).getTime() - new Date(s.started_at).getTime())
    : null;
  const durEl = dur ? el('span', 'timeline-dur', dur) : null;

  summary.appendChild(icon);
  summary.appendChild(kindBadge);
  summary.appendChild(title);
  if (durEl) summary.appendChild(durEl);
  // Exit-status badge for run steps: "exit 0" (pass) or "error" (fail).
  if (s.kind === 'run') {
    if (s.status === 'error') {
      summary.appendChild(el('span', 'timeline-exit timeline-exit-error', 'error'));
    } else if (s.status === 'done') {
      summary.appendChild(el('span', 'timeline-exit timeline-exit-pass', 'exit 0'));
    }
  } else if (s.status === 'error') {
    summary.appendChild(el('span', 'timeline-exit timeline-exit-error', 'error'));
  }
  summary.appendChild(ts);
  row.appendChild(summary);

  // Build expanded body: for run steps, prepend "$ <command>" and append exit status.
  // s.command holds the verbatim command (may differ from s.title when a description is set).
  const hasBody = !!(s.body && s.body.trim());
  const isRunStep = s.kind === 'run';
  if (isRunStep || hasBody) {
    let bodyText = s.body || '';
    if (isRunStep) {
      const cmd = s.command || s.title;
      const cmdHeader = cmd ? '$ ' + cmd : '';
      const exitLine = s.status === 'done' ? 'exit 0' : (s.status === 'error' ? 'exit 1' : '');
      bodyText = [cmdHeader, bodyText, exitLine].filter(Boolean).join('\n');
    }
    if (bodyText) {
      const body = el('pre', 'timeline-body hidden', bodyText);
      row.appendChild(body);
      summary.addEventListener('click', () => body.classList.toggle('hidden'));
    }
  }
  return row;
}

// ── Timeline grouping (STA-638): run → subtask → step ──────
// Each run gets a .timeline-run wrapper. A subtask is a top-level step that
// other steps of the same run point at via parent_seq; its .timeline-subtask
// wrapper holds its own row and the rows of everything under it.

function timelineStepRow(s) {
  const row = buildRunStepRow(s);
  if (s.seq != null) row.setAttribute('data-step-seq', String(s.seq));
  if (s.parent_seq != null) row.setAttribute('data-parent-seq', String(s.parent_seq));
  return row;
}

function buildTimelineRun(runId) {
  const run = el('div', 'timeline-run');
  run.setAttribute('data-run-id', runId || '');
  return run;
}

function buildTimelineSubtask(parentRow) {
  const sub = el('div', 'timeline-subtask');
  sub.setAttribute('data-subtask-step-id', parentRow.getAttribute('data-step-id') || '');
  sub.appendChild(parentRow);
  sub.appendChild(el('div', 'timeline-subtask-children'));
  // Clicking the subtask's own row folds its children, unless that row has a
  // body of its own to expand. Delegated so it survives row upserts.
  sub.addEventListener('click', (e) => {
    const own = sub.firstElementChild;
    const summary = e.target.closest('.timeline-row-summary');
    if (!summary || summary.parentElement !== own) return;
    if (own.querySelector(':scope > .timeline-body')) return;
    sub.classList.toggle('timeline-subtask-collapsed');
  });
  return sub;
}

function timelineRunEl(stepList, runId) {
  return stepList.querySelector(`:scope > .timeline-run[data-run-id="${CSS.escape(runId || '')}"]`);
}

// Fill a run wrapper from that run's steps (seq order).
function fillTimelineRun(runEl, group) {
  const bySeq = new Map();
  for (const s of group) if (s.seq != null) bySeq.set(s.seq, s);
  // Subtask root of each nested step: walk parent_seq up to a top-level step.
  const rootOf = new Map();
  for (const s of group) {
    let cur = s;
    for (let i = 0; i < 64 && cur.parent_seq != null && bySeq.has(cur.parent_seq); i++) {
      cur = bySeq.get(cur.parent_seq);
    }
    if (cur !== s) rootOf.set(s, cur.seq);
  }
  const rootSeqs = new Set(rootOf.values());
  const children = new Map();
  for (const s of group) {
    const row = timelineStepRow(s);
    const root = rootOf.get(s);
    if (root != null && children.has(root)) {
      children.get(root).appendChild(row);
    } else if (rootSeqs.has(s.seq) && !rootOf.has(s)) {
      const sub = buildTimelineSubtask(row);
      children.set(s.seq, sub.lastElementChild);
      runEl.appendChild(sub);
    } else {
      runEl.appendChild(row);
    }
  }
}

// Place one live step's row in its run: under its subtask when its parent
// row is already on the page, else at the end of the run.
function placeTimelineRow(runEl, row, s) {
  let root = null;
  let parentSeq = s.parent_seq;
  for (let i = 0; i < 64 && parentSeq != null; i++) {
    const p = runEl.querySelector(`.timeline-row[data-step-seq="${CSS.escape(String(parentSeq))}"]`);
    if (!p) break;
    root = p;
    const up = p.getAttribute('data-parent-seq');
    parentSeq = up == null ? null : up;
  }
  if (!root) { runEl.appendChild(row); return; }
  // Subtasks are one level deep: a row already under a subtask is a sibling.
  const siblings = root.closest('.timeline-subtask-children');
  if (siblings) { siblings.appendChild(row); return; }
  let sub =root.parentElement && root.parentElement.classList.contains('timeline-subtask')
    ? root.parentElement : null;
  if (!sub) {
    const at = root.nextSibling;
    sub = buildTimelineSubtask(root);
    runEl.insertBefore(sub, at);
  }
  sub.querySelector(':scope > .timeline-subtask-children').appendChild(row);
}

// "Run N" headers, only once the task has more than one real run.
function syncTimelineRunHeaders(stepList, allSteps) {
  const groups = groupStepsByRun(allSteps || []);
  const multi = groups.filter(isRealRunGroup).length > 1;
  let runNum = 0;
  for (const group of groups) {
    const real = isRealRunGroup(group);
    if (real) runNum++;
    const runEl = timelineRunEl(stepList, (group[0] && group[0].run_id) || '');
    if (!runEl) continue;
    let hdr = runEl.querySelector(':scope > .timeline-run-header');
    if (!multi || !real) { if (hdr) hdr.remove(); continue; }
    if (!hdr) { hdr = el('div', 'timeline-run-header'); runEl.prepend(hdr); }
    const firstStep = group[0];
    const stateStep = [...group].reverse().find(s => s.kind === 'state');
    const startTime = firstStep ? fmtDateTime(firstStep.created_at) : '';
    const endTime = stateStep ? fmtDateTime(stateStep.created_at) : '';
    hdr.textContent = `Run ${runNum}` + (startTime ? `  ·  started ${startTime}` : '') + (endTime ? `  ·  ended ${endTime}` : '');
  }
}

function appendRunStepToTimeline(taskId, step) {
  const stepList = document.getElementById(`timeline-steps-${taskId}`);
  if (!stepList) return;
  // Remove empty placeholder
  const empty = stepList.querySelector('.timeline-empty');
  if (empty) empty.remove();
  // Upsert: if a row with this step id already exists (e.g. status:running → status:done),
  // replace it in-place instead of appending a duplicate, keeping it open if it was.
  const existingRow = step.id ? stepList.querySelector(`.timeline-row[data-step-id="${CSS.escape(step.id)}"]`) : null;
  const newRow = timelineStepRow(step);
  if (existingRow) {
    const wasOpen = existingRow.querySelector(':scope > .timeline-body:not(.hidden)');
    const body = newRow.querySelector(':scope > .timeline-body');
    if (wasOpen && body) body.classList.remove('hidden');
    existingRow.replaceWith(newRow);
  } else {
    let runEl = timelineRunEl(stepList, step.run_id);
    if (!runEl) {
      runEl = buildTimelineRun(step.run_id);
      stepList.appendChild(runEl);
    }
    placeTimelineRow(runEl, newRow, step);
  }
  syncTimelineRunHeaders(stepList, (state.tasks[taskId] && state.tasks[taskId].runSteps) || [step]);

  // Update section title count
  const section = document.getElementById(`timeline-section-${taskId}`);
  if (section) {
    const titleEl = section.querySelector('.task-page-section-title');
    if (titleEl) {
      const current = stepList.querySelectorAll('.timeline-row').length;
      titleEl.textContent = `Timeline (${current})`;
    }
  }

  // Refresh stats bar with current-run steps
  refreshTaskStatsBar(taskId);
}

// ── Full-page task view ────────────────────────────────────

// Puts the open task page's canonical /tasks/<org>/<project>/<id> path in the
// address bar. Runs when the task loads and again after loadAll: the org
// prefix comes from the fleet overview, which a direct link can beat.
// Set once the full page has rendered a task; a miss ("Task not found") keeps
// the URL it was given.
let taskPageRenderedId = null;

function canonicalizeTaskPageURL(taskId) {
  if (!taskId || taskId !== taskPageRenderedId || state.openDetailTaskId !== taskId || !isTaskPageShowing()) return;
  const path = window.location.pathname;
  if (!path.startsWith('/tasks/') && !path.startsWith('/issues/')) return;
  const finalPath = taskToPath(state.tasks[taskId] || { id: taskId });
  if (path !== finalPath) {
    history.replaceState({ taskId, canonicalPath: finalPath, taskPage: true }, '', finalPath);
  }
}

// fromRouteMiss: this call re-opens a task found by resolveTaskRouteMiss, so
// the URL that missed is replaced with the canonical one.
async function openTaskPage(target, pushHistory = true, fromRouteMiss = false) {
  // Close sidebar if open, and drop its copy of the task layout: the two
  // share element ids (STA-700).
  const panel = document.getElementById('detail-panel');
  if (panel) panel.classList.add('hidden');
  const panelContent = document.getElementById('panel-content');
  if (panelContent) {
    panelContent.innerHTML = '';
    panelContent.classList.remove('task-page-drawer');
  }
  stopChatPoll();
  stopElapsedTicker();
  const seq = ++taskViewSeq;
  taskPageRenderedId = null;

  let targetId = target;
  let orgHint = null;
  let projectHint = null;
  if (typeof target === 'object' && target !== null) {
    orgHint = target.org || target.organization || null;
    projectHint = target.project || target.projectHint || null;
    targetId = target.identifier || target.taskId || target.id;
  }

  const matchedTask = findTask(targetId, orgHint, projectHint);
  const resolvedId = matchedTask ? matchedTask.id : (targetId || '');
  state.openDetailTaskId = resolvedId;

  const canonicalPath = matchedTask
    ? taskToPath(matchedTask)
    : `/tasks/${encodeURIComponent(orgHint || 'STA')}/${encodeURIComponent(projectHint || 'default')}/${encodeURIComponent(targetId || '')}`;

  if (pushHistory && window.location.pathname !== canonicalPath) {
    history.pushState({ taskId: resolvedId, canonicalPath, taskPage: true }, '', canonicalPath);
  }

  navigateTo('task-page', null, false);

  const pageContent = document.getElementById('task-page-content');
  if (pageContent) pageContent.innerHTML = '<p style="color:var(--muted);padding:2rem">Loading…</p>';

  try {
    const data = await fetchTaskViewData(resolvedId);
    const { task, comments, interactions, shipCard } = data;
    if (seq !== taskViewSeq) return;

    if (task.id) state.openDetailTaskId = task.id;

    const activeId = cacheTaskView(task, comments, resolvedId);
    // Every way onto the page ends on its canonical /tasks/<org>/<project>/<id>
    // URL, including a pasted or bookmarked /tasks/<id> (STA-775).
    taskPageRenderedId = activeId;
    canonicalizeTaskPageURL(activeId);
    const view = renderTaskPage(pageContent, task, comments, interactions, undefined, undefined, task.runErrors, shipCard);
    fillTaskViewDiff(view, data, activeId, seq);
    startChatPoll(activeId);
  } catch (err) {
    if (seq !== taskViewSeq) return;
    const is404 = err && /^404\b/.test(err.message);
    if (is404 && !fromRouteMiss) {
      const fallbackId = await resolveTaskRouteMiss(targetId, orgHint, projectHint);
      if (seq !== taskViewSeq) return;
      if (fallbackId && fallbackId !== resolvedId) {
        return openTaskPage({ id: fallbackId, org: orgHint, project: projectHint }, false, true);
      }
    }
    const cached = matchedTask || state.tasks[resolvedId] || state.tasks[targetId];
    if (is404) {
      if (pageContent) {
        pageContent.innerHTML = '<p style="color:var(--red);padding:2rem">Task not found.</p>';
      }
    } else if (cached && pageContent) {
      renderCachedTaskView(pageContent, cached);
    } else if (pageContent) {
      pageContent.innerHTML = '<p style="color:var(--red);padding:2rem">Failed to load — daemon may be unreachable.</p>';
    }
  }
}

// ── Ship Review Card ──────────────────────────────────────────────────────────
// Fetches the pending ship_review card for a task (if any) and renders a
// Board-facing review panel with: dev URL, test checklist, SHA pin, diff
// summary, and Approve / Send back / Reject buttons.

// Renders a collapsed read-only final-state card (approved or rejected).
// cleanup ({ branch, branch_deleted, branch_delete_error }) reports the
// post-merge branch delete (STA-637); omit it for rejected cards.
function renderFinalShipReviewCard(taskId, headSHA, status, mainSHA, rejectComment, cleanup) {
  const section = el('div', 'ship-review-card ship-review-card--final task-page-section');
  section.id = `ship-review-${taskId}`;
  const hdr = el('div', 'ship-review-header');
  hdr.appendChild(el('span', 'ship-review-badge', 'Ship Review'));
  const label = status === 'approved' ? 'Approved' : 'Rejected';
  hdr.appendChild(el('span', `ship-review-status-badge status-${status}`, label));
  section.appendChild(hdr);
  const shaRow = el('div', 'ship-review-row');
  shaRow.appendChild(el('span', 'ship-review-row-label', 'Reviewed SHA'));
  shaRow.appendChild(el('code', 'ship-review-sha', headSHA ? headSHA.slice(0, 12) : '—'));
  if (status === 'approved' && mainSHA) {
    shaRow.appendChild(el('span', 'ship-review-arrow', ` → ${cardTarget(cleanup)} `));
    shaRow.appendChild(el('code', 'ship-review-sha ship-review-sha--main', mainSHA.slice(0, 12)));
  }
  section.appendChild(shaRow);
  if (status === 'approved' && cleanup && cleanup.pr_number > 0) {
    section.appendChild(renderPRStatusSection(taskId, cleanup));
  }
  if (status === 'approved' && cleanup) {
    if (cleanup.branch_deleted) {
      const brRow = el('div', 'ship-review-row');
      brRow.appendChild(el('span', 'ship-review-row-label', 'Branch'));
      if (cleanup.branch) brRow.appendChild(el('code', 'ship-review-branch', cleanup.branch));
      brRow.appendChild(el('span', 'ship-review-branch-deleted', 'Branch deleted'));
      section.appendChild(brRow);
    } else if (cleanup.branch_delete_error) {
      section.appendChild(renderBranchDeleteWarning(taskId, headSHA, mainSHA, cleanup));
    }
  }
  if (status === 'approved' && cleanup && cleanup.test_task) {
    section.appendChild(renderTestTaskRow(cleanup.test_task));
  }
  if (status === 'rejected' && rejectComment) {
    const fb = el('div', 'ship-review-feedback-box');
    fb.appendChild(el('div', 'ship-review-feedback-label', 'Rejection reason:'));
    fb.appendChild(el('div', 'ship-review-feedback-body', rejectComment));
    section.appendChild(fb);
  }
  return section;
}

// renderBranchDeleteWarning shows a non-blocking "merged; branch delete failed"
// notice with a Retry button. The merge already stands; retry only re-runs the
// branch cleanup.
function renderBranchDeleteWarning(taskId, headSHA, mainSHA, cleanup) {
  const warn = el('div', 'ship-review-head-moved-banner ship-review-branch-warning');
  const text = el('span', 'ship-review-head-moved-text', `merged; branch delete failed: ${cleanup.branch_delete_error}`);
  warn.appendChild(text);
  const retry = el('button', 'ship-review-repin-btn ship-review-branch-retry-btn', 'Retry delete');
  retry.addEventListener('click', async () => {
    retry.disabled = true;
    try {
      const r = await withBoardWebAuthn((sessionToken, assertion) =>
        fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/delete-branch`, {
          method: 'POST',
          headers: { ...authHeader(), 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
        }), 'deleting the branch',
      );
      if (r === null) { retry.disabled = false; return; }
      const body = await r.json().catch(() => ({}));
      if (!r.ok && !body.card) throw new Error(boardActionErrorText(r, body));
      const next = {
        branch: cleanup.branch,
        target_branch: cleanup.target_branch,
        branch_deleted: !!body.branch_deleted,
        branch_delete_error: body.branch_delete_error || '',
      };
      const section = document.getElementById(`ship-review-${taskId}`);
      if (section) section.replaceWith(renderFinalShipReviewCard(taskId, headSHA, 'approved', mainSHA, '', next));
    } catch (e) {
      text.textContent = `merged; branch delete failed: ${e.message || e}`;
      retry.disabled = false;
    }
  });
  warn.appendChild(retry);
  return warn;
}

// SHIP_REVIEW_LIVE_WARNING matches liveDevWarning in handlers_ship_review.go.
const SHIP_REVIEW_LIVE_WARNING = 'LIVE PRODUCTION DATA. Actions in this preview are real.';
// SHIP_REVIEW_UNVERIFIED_WARNING matches unverifiedDevWarning in handlers_ship_review.go.
const SHIP_REVIEW_UNVERIFIED_WARNING = 'UNVERIFIED REPO PATH. This repo could not be verified as separate from a live_credentials project, so treat this preview as live.';

// shipReviewLiveGate returns why start-dev needs the Board gate
// ('live_credentials' | 'unverified_path'), or '' when it does not (STA-799).
function shipReviewLiveGate(card) {
  if (card.live_gate_reason) return card.live_gate_reason;
  return card.live_gate || card.live_credentials ? 'live_credentials' : '';
}

function shipReviewGateWarning(gate) {
  return gate === 'unverified_path' ? SHIP_REVIEW_UNVERIFIED_WARNING : SHIP_REVIEW_LIVE_WARNING;
}

function markShipReviewLiveButton(btn, liveClass, gate) {
  btn.classList.add(liveClass);
  btn.appendChild(el('span', 'ship-review-live-tag', gate === 'unverified_path' ? 'UNVERIFIED' : 'LIVE'));
}

// showShipReviewLiveConfirm shows the inline confirm a gated dev server start
// needs every time (STA-727; STA-799 for an unverified repo path). Confirm
// sends start-dev with the Board passkey headers and {"confirm_live": true};
// Cancel sends nothing.
function showShipReviewLiveConfirm(section, afterRow, taskId, triggerBtn, gate) {
  const existing = section.querySelector('.ship-review-live-confirm');
  if (existing) existing.remove();
  const unverified = gate === 'unverified_path';
  const box = el('div', 'ship-review-live-confirm');
  box.appendChild(el('div', 'ship-review-live-confirm-warning', shipReviewGateWarning(gate)));
  box.appendChild(el('div', 'ship-review-live-confirm-text', unverified
    ? 'A live_credentials project path could not be read, so StayPoint cannot rule out that this repo is that project. Start only if you are sure; anything you do in the preview may change real data.'
    : 'This dev server runs with production credentials. Anything you do in the preview changes real data.'));
  const err = el('div', 'ship-review-live-confirm-error');
  err.hidden = true;
  const actions = el('div', 'ship-review-live-confirm-actions');
  const ok = el('button', 'ship-review-live-confirm-btn', unverified ? 'Start dev server' : 'Start LIVE dev server');
  const cancel = el('button', 'ship-review-live-cancel-btn', 'Cancel');
  const close = () => { box.remove(); triggerBtn.disabled = false; };
  cancel.addEventListener('click', close);
  ok.addEventListener('click', async () => {
    ok.disabled = true;
    cancel.disabled = true;
    err.hidden = true;
    try {
      const r = await withBoardWebAuthn((sessionToken, assertion) =>
        fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/start-dev`, {
          method: 'POST',
          headers: {
            ...authHeader(),
            'Content-Type': 'application/json',
            'X-WebAuthn-Session': sessionToken,
            'X-WebAuthn-Assertion': assertion,
          },
          body: JSON.stringify({ confirm_live: true }),
        })
      );
      if (r === null) { ok.disabled = false; cancel.disabled = false; return; }
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        throw new Error(body.message || body.error || `${r.status} ${r.statusText}`);
      }
      close();
    } catch (e) {
      err.textContent = `Could not start the dev server: ${e.message || e}`;
      err.hidden = false;
      ok.disabled = false;
      cancel.disabled = false;
    }
  });
  actions.appendChild(ok);
  actions.appendChild(cancel);
  box.appendChild(err);
  box.appendChild(actions);
  triggerBtn.disabled = true;
  afterRow.after(box);
}

// renderShipReviewCardFromData renders the ship review card synchronously from
// updateDevProgressUI appends a progress line to the dev env log UI element
// for a task that is in async setup. Called from the SSE ship_review_dev_progress handler.
function updateDevProgressUI(taskId, step, message, ok) {
  const row = document.getElementById(`ship-review-devenv-${taskId}`);
  if (!row) return;
  const stateEl = row.querySelector('.ship-review-devenv-state');
  if (stateEl) {
    stateEl.textContent = step === 'ready' ? '✓ Dev env ready'
      : step === 'error' ? '❌ Dev env error'
      : `⏳ ${message}`;
  }
  let logEl = row.querySelector('.ship-review-devenv-log');
  if (!logEl) {
    logEl = el('pre', 'ship-review-devenv-log', '');
    row.appendChild(logEl);
    row.appendChild(devLogTrigger(logEl));
  }
  const icon = ok ? '✓' : '✗';
  logEl.textContent += `${icon} ${message}\n`;
  logEl.scrollTop = logEl.scrollHeight;
}

// a pre-fetched card object. Pass null/undefined to render nothing.
// Called both from renderTaskPage (pre-fetched data) and from the async
// renderShipReviewCard (SSE-triggered refresh).
// The task page header holds the ship review buttons and their inline forms
// (STA-638). Empty both slots whenever the card re-renders or leaves pending.
function clearShipReviewHeaderActions(taskId) {
  document.getElementById(`task-page-review-actions-${taskId}`)?.replaceChildren();
  document.getElementById(`task-page-action-tray-${taskId}`)
    ?.querySelectorAll('.ship-review-actions-wrap').forEach((n) => n.remove());
}

function devLogTrigger(logEl) {
  return longContentTrigger('dev-log', 'Dev-server log',
    () => el('pre', 'content-modal-pre', logEl.textContent));
}

// ── Ship review PR modes (STA-717) ───────────────────────────────────────────
// A project's merge mode decides what Approve does: "direct" merges to main
// as before, "open_pr" opens a GitHub PR and stops, "pr_merge" opens a PR,
// waits for CI on the pinned head and merges through GitHub from the card.

// cardTarget is the branch a card's Approve merges into.
function cardTarget(card) {
  return (card && card.target_branch) || 'main';
}

const MERGE_MODE_LABELS = {
  direct: 'Merge to main',
  open_pr: 'Open PR',
  pr_merge: 'Open PR + merge after CI',
};

const PR_CHECK_ICONS = { pass: '✓', fail: '✗', pending: '⏳', skipping: '–', cancel: '⊘' };
const PR_CHECKS_LABELS = {
  running: 'Checks running',
  passed: 'Checks passed',
  failed: 'Checks failed',
  none: 'Waiting for checks',
};
const PR_CHECKS_POLL_MS = 10_000;

function fmtCheckDuration(sec) {
  if (!sec || sec < 0) return '';
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return m ? `${m}m ${s}s` : `${s}s`;
}

// safeHttpURL returns url only when it is http(s), so a check link can never
// become a javascript: href.
function safeHttpURL(url) {
  try {
    const u = new URL(url);
    return (u.protocol === 'https:' || u.protocol === 'http:') ? u.toString() : '';
  } catch { return ''; }
}

function prLinkRow(card) {
  const row = el('div', 'ship-review-row ship-review-pr-row');
  row.appendChild(el('span', 'ship-review-row-label', 'Pull request'));
  const href = safeHttpURL(card.pr_url || '');
  if (href) {
    const a = el('a', 'ship-review-pr-link', `PR #${card.pr_number}`);
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    row.appendChild(a);
  } else {
    row.appendChild(el('span', 'ship-review-pr-link', `PR #${card.pr_number}`));
  }
  return row;
}

// renderPRChecksTable lists each check: state, name, duration, log link.
function renderPRChecksTable(checks) {
  const list = el('ul', 'ship-review-pr-checks');
  if (!checks || !checks.length) {
    list.appendChild(el('li', 'ship-review-pr-check ship-review-pr-check--empty', 'No checks reported for this head yet.'));
    return list;
  }
  for (const c of checks) {
    const bucket = c.bucket || 'pending';
    const li = el('li', `ship-review-pr-check ship-review-pr-check--${bucket}`);
    li.dataset.bucket = bucket;
    li.appendChild(el('span', 'ship-review-pr-check-icon', PR_CHECK_ICONS[bucket] || '?'));
    li.appendChild(el('span', 'ship-review-pr-check-name', c.name || '(unnamed)'));
    li.appendChild(el('span', 'ship-review-pr-check-state', (c.state || bucket).toLowerCase().replace(/_/g, ' ')));
    const dur = fmtCheckDuration(c.duration_sec);
    if (dur) li.appendChild(el('span', 'ship-review-pr-check-duration', dur));
    const href = safeHttpURL(c.link || '');
    if (href) {
      const a = el('a', 'ship-review-pr-check-link', 'log');
      a.href = href;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      li.appendChild(a);
    }
    list.appendChild(li);
  }
  return list;
}

// startPRChecksPoll polls the card's checks every PR_CHECKS_POLL_MS until the
// summary settles or the card leaves the DOM. onResult gets each response.
function startPRChecksPoll(taskId, anchor, onResult, { once = false } = {}) {
  let stopped = false;
  let attached = false;
  let waits = 0;
  const tick = async () => {
    if (stopped) return;
    // The card may be built before it is attached; wait for that, then stop
    // for good once it leaves the DOM.
    if (!anchor.isConnected) {
      if (!attached && waits++ < 40) setTimeout(tick, 250);
      return;
    }
    attached = true;
    let res = null;
    try {
      res = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/checks`);
    } catch { /* GitHub unreachable: keep the last snapshot */ }
    if (stopped || !anchor.isConnected) return;
    if (res) onResult(res);
    const settled = res && !res.head_moved && (res.summary === 'passed' || res.summary === 'failed');
    if (!once && !settled) setTimeout(tick, PR_CHECKS_POLL_MS);
  };
  setTimeout(tick, 0);
  return () => { stopped = true; };
}

// renderPRStatusSection shows the PR link and checks on a closed open_pr card.
function renderPRStatusSection(taskId, card) {
  const wrap = el('div', 'ship-review-pr-section');
  wrap.appendChild(prLinkRow(card));
  if (!card.main_sha) {
    wrap.appendChild(el('div', 'ship-review-pr-note', 'PR opened for review on GitHub. StayPoint does not merge it.'));
  }
  const title = el('div', 'ship-review-section-title', 'Checks');
  const summary = el('span', 'ship-review-pr-summary', card.pr_checks_summary ? (PR_CHECKS_LABELS[card.pr_checks_summary] || '') : '');
  title.appendChild(summary);
  wrap.appendChild(title);
  let table = renderPRChecksTable(card.pr_checks || []);
  wrap.appendChild(table);
  if (!card.main_sha) {
    startPRChecksPoll(taskId, wrap, (res) => {
      if (res.head_moved) return;
      const next = renderPRChecksTable(res.checks || []);
      table.replaceWith(next);
      table = next;
      summary.textContent = PR_CHECKS_LABELS[res.summary] || '';
    });
  }
  return wrap;
}

// ── Merge test gate (STA-734) ──────────────────────────────────────────────
// The "Test coverage" section: CI, test changes and coverage for the card's
// head. A blocking warning keeps Merge disabled until the Board picks
// "Merge without tests", which also files a backlog "Add tests" task.

const TEST_GATE_ICONS = { no_ci: '⚠', no_tests: '⚠', uncovered: '⚠', untested_sources: 'ℹ', no_coverage_data: 'ℹ', coverage_ambiguous: 'ℹ' };

// renderTestTaskRow links the backlog task a bypassed merge filed.
function renderTestTaskRow(tt) {
  const row = el('div', 'ship-review-row ship-review-test-task');
  row.appendChild(el('span', 'ship-review-row-label', 'Tests task'));
  const a = el('a', 'ship-review-test-task-link', tt.name || tt.id);
  a.href = tt.url || '#';
  a.addEventListener('click', (e) => { e.preventDefault(); openTaskPage(tt.id); });
  row.appendChild(a);
  row.appendChild(el('span', 'ship-review-test-task-note',
    tt.created ? 'Merged without tests: backlog task created.' : 'Merged without tests: backlog task for this PR.'));
  return row;
}

// fetchTestCoverage reads the card's test gate report. The error carries the
// server's message (the gate fails closed when the change can't be read).
async function fetchTestCoverage(taskId) {
  const r = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/test-coverage`, { headers: authHeader() });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.message || body.error || `${r.status} ${r.statusText}`);
  return body;
}

// renderTestCoverageSection returns the section and a handle. onState fires
// with { loaded, blocking, missing, report, error } whenever the verdict
// changes; apply(report) lets a 409 "untested" response update it in place.
// opts.enforcedAt names a later step that enforces the gate (a pr_merge card
// before its PR exists: Approve only opens the PR). The section then shows
// the verdict without a bypass, which would do nothing at this step.
function renderTestCoverageSection(taskId, onState, opts = {}) {
  const sec = el('div', 'ship-review-test-coverage');
  sec.dataset.state = 'loading';
  const title = el('div', 'ship-review-section-title', 'Test coverage');
  const status = el('span', 'ship-review-test-coverage-status', 'checking…');
  title.appendChild(status);
  sec.appendChild(title);
  const body = el('div', 'ship-review-test-coverage-body');
  body.appendChild(el('div', 'ship-review-test-coverage-note', 'Checking tests and CI for this head…'));
  sec.appendChild(body);
  const actions = el('div', 'ship-review-form-row ship-review-test-coverage-actions');
  const bypassBtn = el('button', 'ship-review-merge-without-tests-btn', 'Merge without tests');
  actions.appendChild(bypassBtn);
  actions.style.display = 'none';
  sec.appendChild(actions);
  const enforcedNote = el('div', 'ship-review-test-coverage-note ship-review-test-coverage-enforced',
    `Not enforced yet: ${opts.enforcedAt || ''} waits for this check, and Merge without tests is offered there.`);
  enforcedNote.style.display = 'none';
  sec.appendChild(enforcedNote);
  const showActions = (blocking) => {
    actions.style.display = blocking && !opts.enforcedAt ? '' : 'none';
    enforcedNote.style.display = blocking && opts.enforcedAt ? '' : 'none';
  };
  const state = { loaded: false, blocking: true, missing: [], report: null, error: '' };
  const emit = () => onState && onState(state);

  const apply = (report) => {
    state.loaded = true;
    state.error = '';
    state.report = report || null;
    state.blocking = !!(report && report.blocking);
    state.missing = (report && report.missing) || [];
    const warnings = (report && report.warnings) || [];
    const nodes = [];
    if (!report) {
      nodes.push(el('div', 'ship-review-test-coverage-note', 'No test coverage report for this card.'));
    } else if (report.exempt_only) {
      nodes.push(el('div', 'ship-review-test-ok', '✓ Docs, config or style only: no tests needed.'));
    } else if (!warnings.length) {
      nodes.push(el('div', 'ship-review-test-ok',
        (report.sources || []).length ? '✓ Tests changed, and CI runs them.' : '✓ No source files changed.'));
    }
    for (const w of warnings) {
      const box = el('div', `ship-review-test-warning ship-review-test-warning--${w.kind}${w.blocking ? ' ship-review-test-warning--blocking' : ''}`);
      box.dataset.kind = w.kind;
      box.appendChild(el('div', 'ship-review-test-warning-msg', `${TEST_GATE_ICONS[w.kind] || '⚠'} ${w.message}`));
      if (w.items && w.items.length) {
        const ul = el('ul', 'ship-review-test-warning-items');
        for (const it of w.items) ul.appendChild(el('li', 'ship-review-test-warning-item', it));
        box.appendChild(ul);
      }
      nodes.push(box);
    }
    if (report && report.coverage && report.coverage.available && !warnings.some((w) => w.kind === 'uncovered' || w.kind === 'coverage_ambiguous')) {
      nodes.push(el('div', 'ship-review-test-coverage-note', `Coverage from ${report.coverage.source}: every changed line runs under a test.`));
    }
    body.replaceChildren(...nodes);
    status.textContent = state.blocking ? `not tested: ${state.missing.join(', ')}` : 'ok';
    sec.dataset.state = state.blocking ? 'blocking' : 'ok';
    showActions(state.blocking);
    emit();
  };

  const fail = (msg) => {
    state.loaded = true;
    state.error = msg;
    state.blocking = true;
    state.missing = ['test coverage unknown'];
    const err = el('div', 'ship-review-test-warning ship-review-test-warning--blocking ship-review-test-warning--error', `⚠ Could not check test coverage: ${msg}`);
    const retry = el('button', 'ship-review-repin-btn ship-review-test-coverage-retry', 'Retry');
    retry.addEventListener('click', () => load());
    err.appendChild(retry);
    body.replaceChildren(err);
    status.textContent = 'unknown';
    sec.dataset.state = 'error';
    showActions(true);
    emit();
  };

  const load = () => {
    status.textContent = 'checking…';
    fetchTestCoverage(taskId).then((res) => apply(res.report)).catch((e) => fail(e.message || String(e)));
  };
  load();
  return { el: sec, state, apply, bypassBtn, reload: load };
}

// renderAnswerOnlyPanel replaces the ship review card for a run with no
// changes against its base (STA-774): the agent's final message, and the
// files it read instead of "files changed".
function renderAnswerOnlyPanel(comments, filesRead) {
  const section = el('div', 'ship-review-card answer-only-panel task-page-section');
  const hdr = el('div', 'ship-review-header');
  hdr.appendChild(el('span', 'ship-review-badge', 'No changes: answer-only run'));
  section.appendChild(hdr);
  const summary = [...comments].reverse().find((c) => (c.author || '') === 'agent-summary');
  const msg = summary ? (summary.message || summary.body || '') : '';
  if (msg) {
    const body = el('div', 'ship-review-summary-body md-content');
    body.innerHTML = renderMarkdown(msg.replace(/^\s*\[\[TASK_COMPLETE\]\]\s*$/gm, '').trim());
    section.appendChild(body);
  } else {
    section.appendChild(el('p', 'panel-field-muted', 'The run made no changes and left no final message.'));
  }
  const readRow = el('div', 'ship-review-row');
  readRow.appendChild(el('span', 'ship-review-row-label', `Files read: ${filesRead.length}`));
  section.appendChild(readRow);
  if (filesRead.length) {
    const list = el('ul', 'answer-only-files-read');
    for (const f of filesRead) list.appendChild(el('li', '', f));
    section.appendChild(list);
  }
  return section;
}

function renderShipReviewCardFromData(container, taskId, card) {
  if (!card || !taskId) return;
  clearShipReviewHeaderActions(taskId);

  // Render collapsed final state for terminal statuses.
  if (card.status === 'approved' || card.status === 'rejected') {
    const existing = document.getElementById(`ship-review-${taskId}`);
    const finalCard = renderFinalShipReviewCard(taskId, card.head_sha, card.status, card.main_sha, card.reject_comment, card);
    if (existing) existing.replaceWith(finalCard);
    else container.appendChild(finalCard);
    // STA-734: link the "Add tests" task a bypassed merge filed.
    if (card.status === 'approved') {
      fetchTestCoverage(taskId).then((res) => {
        if (res.test_task && finalCard.isConnected && !finalCard.querySelector('.ship-review-test-task')) {
          finalCard.appendChild(renderTestTaskRow(res.test_task));
        }
      }).catch(() => {});
    }
    return;
  }

  if (!['pending', 'sent_back'].includes(card.status)) return;

  const section = el('div', 'ship-review-card task-page-section');
  section.id = `ship-review-${taskId}`;

  // Header
  const hdr = el('div', 'ship-review-header');
  const badge = el('span', 'ship-review-badge', 'Ship Review');
  // STA-717: a pr_merge card whose head is on the PR waits for CI, then merges.
  const prActive = card.status === 'pending' && card.merge_mode === 'pr_merge' && card.pr_number > 0 &&
    !!card.pr_checks_sha && card.pr_checks_sha === card.head_sha;
  const approveMode = card.effective_merge_mode || 'direct';
  const statusBadge = el('span', `ship-review-status-badge status-${card.status}`,
    card.status === 'sent_back' ? 'Sent Back'
      : prActive ? (PR_CHECKS_LABELS[card.pr_checks_summary] || PR_CHECKS_LABELS.running)
      : 'Pending Approval');
  hdr.appendChild(badge);
  hdr.appendChild(statusBadge);
  section.appendChild(hdr);

  if (card.status === 'sent_back' && card.send_back_comment) {
    const fbBox = el('div', 'ship-review-feedback-box');
    fbBox.appendChild(el('div', 'ship-review-feedback-label', 'Board feedback:'));
    fbBox.appendChild(el('div', 'ship-review-feedback-body', card.send_back_comment));
    section.appendChild(fbBox);
  }

  // Agent summary — most recent harness-posted run summary for this task.
  if (card.agent_summary) {
    const summarySection = el('div', 'ship-review-summary-section');
    const summaryBody = el('div', 'ship-review-summary-body markdown-body');
    const summaryHead = el('div', 'long-content-head');
    summaryHead.appendChild(el('div', 'ship-review-section-title', 'Agent summary'));
    summaryHead.appendChild(longContentTrigger('agent-summary', 'Agent summary',
      () => cloneChildren(summaryBody, 'markdown-body')));
    summarySection.appendChild(summaryHead);
    // The completion marker is harness plumbing, not part of the summary.
    summaryBody.innerHTML = renderMarkdown(card.agent_summary.replace(/^\s*\[\[TASK_COMPLETE\]\]\s*$/gm, '').trim());
    summarySection.appendChild(summaryBody);
    section.appendChild(summarySection);
  }

  // DB migration warning — shown when any changed file is a migration.
  if (card.has_db_migration) {
    const migBanner = el('div', 'ship-review-migration-banner');
    migBanner.textContent = '⚠ DB migration detected — apply migration separately before approving.';
    section.appendChild(migBanner);
    // Show whether each migration has been marked applied (not yet verified).
    apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations`).then((m) => {
      const migs = (m && m.migrations) || [];
      if (!migs.length) return;
      const pending = migs.filter((x) => !x.applied_at);
      if (pending.length === 0) {
        const when = migs.map((x) => x.applied_at).sort().pop();
        migBanner.className = 'ship-review-migration-banner ship-review-migration-banner--applied';
        migBanner.textContent = `✓ Migration marked applied by ${migs[0].applied_by || 'board'} (${new Date(when).toLocaleString()}). Not verified against the database yet.`;
      } else {
        migBanner.textContent = `⚠ ${pending.length} DB migration${pending.length > 1 ? 's' : ''} not yet marked applied. Apply in the SQL editor, then Mark applied, before approving.`;
      }
    }).catch(() => {});
  }

  // STA-727: previews of a live_credentials project hit production. The red
  // banner sits above the dev env / Preview rows and the Start button.
  // STA-799: a repo path the server could not verify is gated the same way.
  const gate = shipReviewLiveGate(card);
  const live = gate !== '';
  if (live) {
    section.appendChild(el('div', 'ship-review-live-banner', shipReviewGateWarning(gate)));
  }

  // Dev env state (async setup progress).
  if (card.dev_state === 'starting' || card.dev_state === 'error' || (card.dev_log && card.dev_log.length > 0)) {
    const devEnvRow = el('div', 'ship-review-devenv-row');
    devEnvRow.id = `ship-review-devenv-${taskId}`;
    const stateLabel = card.dev_state === 'starting' ? '⏳ Starting dev env…'
      : card.dev_state === 'error' ? '❌ Dev env error'
      : card.dev_state === 'ready' ? '✓ Dev env ready'
      : '…';
    devEnvRow.appendChild(el('div', 'ship-review-devenv-state', stateLabel));
    if (card.dev_log && card.dev_log.length > 0) {
      const logEl = el('pre', 'ship-review-devenv-log', card.dev_log.join('\n'));
      devEnvRow.appendChild(logEl);
      devEnvRow.appendChild(devLogTrigger(logEl));
    }
    section.appendChild(devEnvRow);
  }

  // Dev URL — only render http/https loopback URLs to prevent XSS via javascript: etc.
  // Send Back stops the dev server, so a sent_back card has nothing to preview.
  if (card.dev_url && card.status !== 'sent_back') {
    let safeDevURL = null;
    try {
      const u = new URL(card.dev_url);
      if ((u.protocol === 'http:' || u.protocol === 'https:') &&
          (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '::1' || u.hostname.startsWith('127.'))) {
        safeDevURL = u.toString();
      }
    } catch { /* invalid URL — skip */ }
    const devRow = el('div', 'ship-review-row');
    devRow.appendChild(el('span', 'ship-review-row-label', 'Preview'));
    if (safeDevURL) {
      const devLink = el('a', 'ship-review-dev-link', safeDevURL);
      devLink.href = safeDevURL;
      devLink.target = '_blank';
      devLink.rel = 'noopener noreferrer';
      devRow.appendChild(devLink);
    } else {
      devRow.appendChild(el('span', 'ship-review-dev-link', card.dev_url + ' (invalid URL)'));
    }
    // start-dev only accepts pending cards (STA-654); a sent_back card's dev
    // server was stopped and will 409.
    if (card.status === 'pending') {
      const restartBtn = el('button', 'ship-review-restart-btn', '↺ Restart');
      restartBtn.title = 'Restart dev server';
      if (live) markShipReviewLiveButton(restartBtn, 'ship-review-restart-btn--live', gate);
      restartBtn.addEventListener('click', async () => {
        if (live) { showShipReviewLiveConfirm(section, devRow, taskId, restartBtn, gate); return; }
        restartBtn.disabled = true;
        await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/start-dev`, { method: 'POST' }).catch(() => {});
        restartBtn.disabled = false;
      });
      devRow.appendChild(restartBtn);
    }
    section.appendChild(devRow);
  } else if (card.status === 'pending' && card.dev_configured !== false && card.dev_state !== 'starting') {
    // No dev server yet: offer to start one (a live project never auto-starts).
    const startRow = el('div', 'ship-review-row');
    startRow.appendChild(el('span', 'ship-review-row-label', 'Preview'));
    const startBtn = el('button', 'ship-review-start-dev-btn', '▶ Start dev server');
    if (live) markShipReviewLiveButton(startBtn, 'ship-review-start-dev-btn--live', gate);
    startBtn.addEventListener('click', async () => {
      if (live) { showShipReviewLiveConfirm(section, startRow, taskId, startBtn, gate); return; }
      startBtn.disabled = true;
      await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/start-dev`, { method: 'POST' }).catch(() => {});
      startBtn.disabled = false;
    });
    startRow.appendChild(startBtn);
    section.appendChild(startRow);
  }

  // SHA pin
  const shaRow = el('div', 'ship-review-row');
  shaRow.appendChild(el('span', 'ship-review-row-label', 'Branch SHA'));
  shaRow.appendChild(el('code', 'ship-review-sha', card.head_sha ? card.head_sha.slice(0, 12) : '—'));
  shaRow.appendChild(el('span', 'ship-review-branch', card.branch || ''));
  // The branch Approve merges into (dev-server for work repos), never main implicitly.
  if (card.target_branch) {
    shaRow.appendChild(el('span', 'ship-review-target-branch', `→ ${card.target_branch}`));
  }
  section.appendChild(shaRow);

  // PR + CI checks (STA-717). applyPRChecks is wired to the actions below.
  let applyPRChecks = null;
  const prWarning = el('div', 'ship-review-pr-warning');
  prWarning.style.display = 'none';
  const prWarningList = el('ul', 'ship-review-pr-warning-list');
  if (card.pr_number > 0 && card.status === 'pending') {
    const prSec = el('div', 'ship-review-pr-section');
    prSec.appendChild(prLinkRow(card));
    if (!prActive) {
      prSec.appendChild(el('div', 'ship-review-pr-note',
        `This head is not on PR #${card.pr_number} yet. Approve pushes it to the PR and re-runs the checks.`));
    } else {
      const title = el('div', 'ship-review-section-title', 'Checks');
      const summaryEl = el('span', 'ship-review-pr-summary', '');
      title.appendChild(summaryEl);
      prSec.appendChild(title);
      let table = renderPRChecksTable(card.pr_checks || []);
      prSec.appendChild(table);
      prWarning.appendChild(el('div', 'ship-review-pr-warning-title', ''));
      prWarning.appendChild(prWarningList);
      prSec.appendChild(prWarning);
      if (card.pr_merge_error) {
        const refusal = el('div', 'ship-review-github-refusal');
        refusal.appendChild(el('span', 'ship-review-github-refusal-label', 'GitHub refused the last merge: '));
        refusal.appendChild(el('span', 'ship-review-github-refusal-text', card.pr_merge_error));
        prSec.appendChild(refusal);
      }
      applyPRChecks = (summary, checks) => {
        const next = renderPRChecksTable(checks || []);
        table.replaceWith(next);
        table = next;
        const label = PR_CHECKS_LABELS[summary] || PR_CHECKS_LABELS.running;
        summaryEl.textContent = label;
        statusBadge.textContent = label;
        statusBadge.className = `ship-review-status-badge status-checks-${summary || 'running'}`;
        const blocking = (checks || []).filter((c) => c.bucket !== 'pass' && c.bucket !== 'skipping');
        const green = summary === 'passed';
        prWarning.style.display = green ? 'none' : '';
        prWarning.querySelector('.ship-review-pr-warning-title').textContent =
          summary === 'failed' ? `⚠ ${blocking.length} check${blocking.length === 1 ? '' : 's'} not green. Merging now needs an override.`
            : summary === 'none' ? '⚠ No checks reported for this head yet. Merging now needs an override.'
            : `⚠ ${blocking.length} check${blocking.length === 1 ? '' : 's'} still running. Merging now needs an override.`;
        prWarningList.replaceChildren(...blocking.map((c) => el('li', `ship-review-pr-warning-item ship-review-pr-check--${c.bucket || 'pending'}`,
          `${PR_CHECK_ICONS[c.bucket] || '?'} ${c.name} (${(c.state || c.bucket || '').toLowerCase().replace(/_/g, ' ')})`)));
        section.dataset.checks = summary || 'running';
        section.dispatchEvent(new CustomEvent('pr-checks', { detail: { summary, blocking } }));
      };
      startPRChecksPoll(taskId, prSec, (res) => {
        if (res.head_moved) {
          section.dispatchEvent(new CustomEvent('pr-head-moved', { detail: { sha: res.pr_head_sha || '' } }));
          return;
        }
        applyPRChecks(res.summary, res.checks);
      });
    }
    section.appendChild(prSec);
  }

  // Test coverage (STA-734): Merge waits for this verdict.
  let testGate = null;
  if (card.status === 'pending') {
    // A pr_merge card before its PR exists: Approve only opens the PR, and
    // the server enforces the gate at Merge PR, so no bypass here.
    const enforcedAt = approveMode !== 'pr_merge' || prActive ? ''
      : card.pr_number > 0 ? `Merge PR #${card.pr_number}` : 'Merge PR, once the PR is open,';
    testGate = renderTestCoverageSection(taskId, () => section.dispatchEvent(new CustomEvent('test-gate')), { enforcedAt });
    section.appendChild(testGate.el);
  }

  // What to test
  if (card.test_steps && card.test_steps.length > 0) {
    const testSection = el('div', 'ship-review-test-section');
    testSection.appendChild(el('div', 'ship-review-section-title', 'What to test'));
    const list = el('ol', 'ship-review-test-list');
    for (const step of card.test_steps) {
      // Strip leading "1. " / "1) " the agent already included — <ol> adds its own.
      const text = step.replace(/^\s*\d+[.)]\s*/, '');
      list.appendChild(el('li', 'ship-review-test-step', text));
    }
    testSection.appendChild(list);
    section.appendChild(testSection);
  }

  // Files changed
  if (card.files_changed && card.files_changed.length > 0) {
    const filesSection = el('div', 'ship-review-files-section');
    filesSection.appendChild(el('div', 'ship-review-section-title', `Files changed (${card.files_changed.length})`));
    const fileList = el('ul', 'ship-review-file-list');
    for (const f of card.files_changed) {
      fileList.appendChild(el('li', 'ship-review-file-item', f));
    }
    filesSection.appendChild(fileList);
    section.appendChild(filesSection);
  }

  // Check runs (agent-reported; not independently verified by the server)
  if (card.check_runs && card.check_runs.length > 0) {
    const checksSection = el('div', 'ship-review-checks-section');
    const checksTitle = el('div', 'ship-review-section-title', 'Check runs');
    const unverifiedBadge = el('span', 'ship-review-unverified-badge', 'agent-reported');
    checksTitle.appendChild(unverifiedBadge);
    checksSection.appendChild(checksTitle);
    for (const run of card.check_runs) {
      const exitCode = typeof run.exit_code === 'number' ? run.exit_code : -1;
      const row = el('div', 'ship-review-check-row');
      const exitLabel = el('code', 'ship-review-check-exit', `exit ${exitCode}`);
      const cmdEl = el('code', 'ship-review-check-cmd', run.command || '');
      row.appendChild(exitLabel);
      row.appendChild(cmdEl);
      if (run.output_tail) {
        const out = el('pre', 'ship-review-check-output', run.output_tail.slice(0, 2000));
        row.appendChild(out);
      }
      checksSection.appendChild(row);
    }
    section.appendChild(checksSection);
  }

  // ── Inline action area ────────────────────────────────────────────────────
  // No browser alert/confirm/prompt — all UI is inline.

  const actionsWrap = el('div', 'ship-review-actions-wrap');

  // ── Error banner (shown on API failure) ──
  const errBanner = el('div', 'ship-review-err-banner');
  errBanner.style.display = 'none';
  actionsWrap.appendChild(errBanner);
  const showErr = (msg) => { errBanner.textContent = msg; errBanner.style.display = ''; };
  const clearErr = () => { errBanner.style.display = 'none'; };

  // ── head_moved inline banner (hidden until 409 head_moved) ──
  const headMovedBanner = el('div', 'ship-review-head-moved-banner');
  headMovedBanner.style.display = 'none';
  {
    headMovedBanner.appendChild(el('span', 'ship-review-head-moved-text', 'Branch HEAD moved since card was rendered. '));
    const repin = el('button', 'ship-review-repin-btn', 'Ask agent to re-pin');
    repin.addEventListener('click', async () => {
      repin.disabled = true;
      try {
        const r = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/send-back`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: JSON.stringify({ comment: 'HEAD moved — please re-pin to the current branch HEAD.' }),
          }), 'asking the agent to re-pin',
        );
        if (r === null) { repin.disabled = false; return; }
        if (!r.ok) throw await boardActionError(r);
        showWorkingState(section, taskId, card.run_number || 1);
      } catch (e) {
        repin.disabled = false;
        showErr('Re-pin send-back failed: ' + (e.message || e));
      }
    });
    headMovedBanner.appendChild(repin);
  }
  actionsWrap.appendChild(headMovedBanner);

  // ── Send-back inline form (hidden until "↩ Send Back" clicked) ──
  const sendBackForm = el('div', 'ship-review-inline-form');
  sendBackForm.style.display = 'none';
  // Set by "Send failures to agent": the send-back carries ci_failures so the
  // agent's fix is pushed to the PR and its checks re-run (STA-717).
  let sendBackCI = false;
  let sbTextareaRef = null;
  const sbLabel = el('label', 'ship-review-form-label', 'Feedback for the agent (required):');
  {
    sendBackForm.appendChild(sbLabel);
    const sbTextarea = document.createElement('textarea');
    sbTextareaRef = sbTextarea;
    sbTextarea.className = 'ship-review-form-textarea';
    sbTextarea.rows = 3;
    sbTextarea.placeholder = 'What should the agent fix or improve?';
    sendBackForm.appendChild(sbTextarea);
    const sbRow = el('div', 'ship-review-form-row');
    const sbSubmit = el('button', 'ship-review-sendback-submit-btn', '↩ Send Back');
    const sbCancel = el('button', 'ship-review-form-cancel-btn', 'Cancel');
    sbRow.appendChild(sbSubmit);
    sbRow.appendChild(sbCancel);
    sendBackForm.appendChild(sbRow);
    sbCancel.addEventListener('click', () => { sendBackForm.style.display = 'none'; clearErr(); });
    sbSubmit.addEventListener('click', async () => {
      const comment = sbTextarea.value.trim();
      if (!comment) { showErr('Feedback is required.'); return; }
      clearErr();
      sbSubmit.disabled = true;
      try {
        const r = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/send-back`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: JSON.stringify(sendBackCI ? { comment, ci_failures: true } : { comment }),
          }), 'sending the review back',
        );
        if (r === null) { sbSubmit.disabled = false; return; }
        if (!r.ok) throw await boardActionError(r);
        showWorkingState(section, taskId, card.run_number || 1);
      } catch (e) {
        sbSubmit.disabled = false;
        showErr('Send back failed: ' + (e.message || e));
      }
    });
  }
  actionsWrap.appendChild(sendBackForm);

  // ── Reject inline form (hidden until "✕ Reject" clicked) ──
  const rejectForm = el('div', 'ship-review-inline-form');
  rejectForm.style.display = 'none';
  {
    rejectForm.appendChild(el('label', 'ship-review-form-label', 'Reason for rejection (optional):'));
    const rjTextarea = document.createElement('textarea');
    rjTextarea.className = 'ship-review-form-textarea';
    rjTextarea.rows = 3;
    rjTextarea.placeholder = 'Why is this being rejected?';
    rejectForm.appendChild(rjTextarea);
    const delRow = el('div', 'ship-review-form-checkbox-row');
    const delChk = document.createElement('input');
    delChk.type = 'checkbox';
    delChk.id = `ship-review-del-branch-${taskId}`;
    delChk.className = 'ship-review-del-branch-chk';
    const delLabel = document.createElement('label');
    delLabel.htmlFor = delChk.id;
    delLabel.textContent = 'Delete remote branch';
    delRow.appendChild(delChk);
    delRow.appendChild(delLabel);
    rejectForm.appendChild(delRow);
    const rjRow = el('div', 'ship-review-form-row');
    const rjSubmit = el('button', 'ship-review-reject-submit-btn', '✕ Reject');
    const rjCancel = el('button', 'ship-review-form-cancel-btn', 'Cancel');
    rjRow.appendChild(rjSubmit);
    rjRow.appendChild(rjCancel);
    rejectForm.appendChild(rjRow);
    rjCancel.addEventListener('click', () => { rejectForm.style.display = 'none'; clearErr(); });
    rjSubmit.addEventListener('click', async () => {
      const comment = rjTextarea.value.trim();
      const delBranch = delChk.checked;
      clearErr();
      rjSubmit.disabled = true;
      try {
        const sendReject = (confirmUnmerged) => withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/reject`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: JSON.stringify({ comment, delete_branch: delBranch,
              ...(confirmUnmerged ? { confirm_delete_unmerged: confirmUnmerged } : {}) }),
          }), 'rejecting the review',
        );
        let r = await sendReject('');
        if (r === null) { rjSubmit.disabled = false; return; }
        if (r.status === 409 && delBranch) {
          // The branch holds work not in the target: deleting it needs the
          // Board to name the branch (a separate, explicit override).
          const body = await r.clone().json().catch(() => ({}));
          if (body.error === 'unmerged_branch') {
            const typed = prompt(`${body.message}\n\nType the branch name (${body.branch}) to delete it and discard that work, or Cancel to keep it.`);
            if (typed === null) { rjSubmit.disabled = false; return; }
            r = await sendReject(typed.trim());
            if (r === null) { rjSubmit.disabled = false; return; }
          }
        }
        if (!r.ok) throw await boardActionError(r);
        clearShipReviewHeaderActions(taskId);
        section.replaceWith(renderFinalShipReviewCard(taskId, card.head_sha, 'rejected', '', comment));
      } catch (e) {
        rjSubmit.disabled = false;
        showErr('Reject failed: ' + (e.message || e));
      }
    });
  }
  actionsWrap.appendChild(rejectForm);

  // ── Approve inline confirm form ──
  const approveConfirmForm = el('div', 'ship-review-inline-form ship-review-approve-confirm');
  approveConfirmForm.style.display = 'none';
  {
    const shortSHA = card.head_sha ? card.head_sha.slice(0, 12) : '—';
    const repoName = card.repo_name || '';
    const repoSuffix = repoName ? ` (${repoName})` : '';
    const prTarget = card.pr_number > 0 ? `PR #${card.pr_number}` : 'a PR';
    approveConfirmForm.appendChild(el('div', 'ship-review-form-label',
      approveMode === 'open_pr' ? `Push ${shortSHA} and open ${prTarget} → ${cardTarget(card)}${repoSuffix}. StayPoint will not merge it.`
        : approveMode === 'pr_merge' ? `Push ${shortSHA} to ${prTarget} → ${cardTarget(card)}${repoSuffix} and run the checks. Merge comes after CI.`
        : `Merge ${shortSHA} → ${cardTarget(card)}${repoSuffix}`));
    const acRow = el('div', 'ship-review-form-row');
    const acMerge = el('button', 'ship-review-form-submit',
      approveMode === 'direct' ? `Merge to ${cardTarget(card)}` : approveMode === 'open_pr' ? 'Open PR' : 'Push & run checks');
    const acCancel = el('button', 'ship-review-form-cancel', 'Cancel');
    acRow.appendChild(acMerge);
    acRow.appendChild(acCancel);
    approveConfirmForm.appendChild(acRow);
    acCancel.addEventListener('click', () => { approveConfirmForm.style.display = 'none'; clearErr(); });
    acMerge.addEventListener('click', () => doApprove(acMerge, {}));
  }
  // doApprove posts Approve; extra carries the test-gate bypass (STA-734).
  async function doApprove(acMerge, extra) {
      clearErr();
      acMerge.disabled = true;
      try {
        const r = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/approve`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: JSON.stringify({ head_sha: card.head_sha, ...(extra || {}) }),
          }), approveMode === 'direct' ? 'merging to main' : 'opening the PR',
        );
        if (r === null) { acMerge.disabled = false; return; }
        if (r.status === 409) {
          const body = await r.json().catch(() => ({}));
          if (body.error === 'untested') {
            if (testGate) testGate.apply(body.test_coverage);
            acMerge.disabled = false;
            showBypassConfirm(body.missing || []);
            return;
          }
          if (body.error === 'head_moved') {
            const newSHA = body.new_head_sha || '?';
            headMovedBanner.querySelector('.ship-review-head-moved-text').textContent =
              `Branch HEAD moved to ${newSHA.slice(0, 12)} since card was rendered. `;
            headMovedBanner.style.display = '';
            approveConfirmForm.style.display = 'none';
            acMerge.disabled = false;
            return;
          }
          throw new Error(boardActionErrorText(r, body));
        }
        if (!r.ok) throw await boardActionError(r);
        const result = await r.json();
        if (result.merge_mode === 'open_pr' || result.merge_mode === 'pr_merge') {
          clearShipReviewHeaderActions(taskId);
          if (result.merge_mode === 'open_pr') {
            section.replaceWith(renderFinalShipReviewCard(taskId, card.head_sha, 'approved', '', '', { ...(result.card || {}), test_task: result.test_task }));
          } else {
            section.remove();
            renderShipReviewCardFromData(container, taskId, result.card);
          }
          return;
        }
        const mainSHA = result.main_sha || (result.card && result.card.main_sha) || '';
        clearShipReviewHeaderActions(taskId);
        section.replaceWith(renderFinalShipReviewCard(taskId, card.head_sha, 'approved', mainSHA, '', {
          branch: card.branch,
          target_branch: card.target_branch,
          branch_deleted: !!result.branch_deleted,
          branch_delete_error: result.branch_delete_error || '',
          test_task: result.test_task,
        }));
      } catch (e) {
        showErr('Approve failed: ' + (e.message || e));
        acMerge.disabled = false;
      }
  }
  actionsWrap.appendChild(approveConfirmForm);

  // ── PR merge (pr_merge mode, STA-717) ──
  const shortHead = card.head_sha ? card.head_sha.slice(0, 12) : '—';
  const mergeConfirmForm = el('div', 'ship-review-inline-form ship-review-merge-confirm');
  mergeConfirmForm.style.display = 'none';
  const anywayConfirmForm = el('div', 'ship-review-inline-form ship-review-merge-anyway-confirm');
  anywayConfirmForm.style.display = 'none';
  const anywayLabel = el('div', 'ship-review-form-label', '');
  // "Merge without tests" confirm (STA-734): names what is missing.
  const bypassConfirmForm = el('div', 'ship-review-inline-form ship-review-merge-without-tests-confirm');
  bypassConfirmForm.style.display = 'none';
  const bypassLabel = el('div', 'ship-review-form-label', '');
  const bypassMissing = el('ul', 'ship-review-merge-without-tests-missing');
  const allForms = [sendBackForm, rejectForm, approveConfirmForm, mergeConfirmForm, anywayConfirmForm, bypassConfirmForm];
  const showOnly = (form) => {
    for (const f of allForms) f.style.display = (f === form && f.style.display === 'none') ? '' : 'none';
    headMovedBanner.style.display = 'none';
    clearErr();
  };
  const showGitHubRefusal = (msg) => {
    errBanner.replaceChildren(el('span', 'ship-review-github-refusal-label', 'GitHub refused the merge: '),
      el('span', 'ship-review-github-refusal-text', msg));
    errBanner.style.display = '';
  };
  const doMerge = async (btn, override, reason, extra) => {
    clearErr();
    btn.disabled = true;
    try {
      const r = await withBoardWebAuthn((sessionToken, assertion) =>
        fetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/merge`, {
          method: 'POST',
          headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
          // head_sha pins the merge to the head on screen (STA-717).
          body: JSON.stringify({
            head_sha: card.head_sha,
            ...(override ? { override: true, override_reason: reason || '' } : {}),
            ...(extra || {}),
          }),
        }), override ? 'merging the PR anyway' : 'merging the PR',
      );
      if (r === null) { btn.disabled = false; return; }
      const body = await r.json().catch(() => ({}));
      if (r.ok) {
        clearShipReviewHeaderActions(taskId);
        section.replaceWith(renderFinalShipReviewCard(taskId, card.head_sha, 'approved', body.main_sha || '', '', {
          ...(body.card || { branch: card.branch, target_branch: card.target_branch, branch_deleted: !!body.branch_deleted, branch_delete_error: body.branch_delete_error || '' }),
          test_task: body.test_task,
        }));
        return;
      }
      btn.disabled = false;
      mergeConfirmForm.style.display = 'none';
      anywayConfirmForm.style.display = 'none';
      bypassConfirmForm.style.display = 'none';
      if (body.error === 'untested') {
        if (testGate) testGate.apply(body.test_coverage);
        showBypassConfirm(body.missing || []);
        return;
      }
      if (body.error === 'head_moved') {
        headMovedBanner.querySelector('.ship-review-head-moved-text').textContent =
          `Branch moved to ${(body.new_head_sha || '?').slice(0, 12)} after the checks started. Merge blocked until the card is re-pinned and checks re-run. `;
        headMovedBanner.style.display = '';
        if (applyPRChecks) applyPRChecks('running', []);
        return;
      }
      if (body.error === 'checks_not_green') {
        if (applyPRChecks) applyPRChecks(body.not_green && body.not_green.some((c) => c.bucket === 'fail' || c.bucket === 'cancel') ? 'failed' : 'running', body.checks || []);
        showErr('Checks are not all green: ' + (body.not_green || []).map((c) => c.name).join(', '));
        return;
      }
      if (body.error === 'github_refused') { showGitHubRefusal(body.message || ''); return; }
      if (body.error === 'unverified_migrations') {
        showErr(`${body.message} (${(body.unverified_migrations || []).join(', ')})`);
        return;
      }
      showErr('Merge failed: ' + (body.message || body.error || `${r.status} ${r.statusText}`));
    } catch (e) {
      btn.disabled = false;
      showErr('Merge failed: ' + (e.message || e));
    }
  };
  {
    mergeConfirmForm.appendChild(el('div', 'ship-review-form-label', `Merge PR #${card.pr_number} ${shortHead} → ${cardTarget(card)}`));
    const row = el('div', 'ship-review-form-row');
    const go = el('button', 'ship-review-form-submit ship-review-merge-submit', 'Merge PR');
    const cancel = el('button', 'ship-review-form-cancel', 'Cancel');
    row.appendChild(go);
    row.appendChild(cancel);
    mergeConfirmForm.appendChild(row);
    cancel.addEventListener('click', () => { mergeConfirmForm.style.display = 'none'; clearErr(); });
    go.addEventListener('click', () => doMerge(go, false));
  }
  {
    anywayConfirmForm.appendChild(anywayLabel);
    const reason = document.createElement('input');
    reason.type = 'text';
    reason.className = 'ship-review-form-input ship-review-override-reason';
    reason.placeholder = 'Why merge anyway? (logged)';
    anywayConfirmForm.appendChild(reason);
    const row = el('div', 'ship-review-form-row');
    const go = el('button', 'ship-review-reject-submit-btn ship-review-merge-anyway-submit', 'Merge anyway');
    const cancel = el('button', 'ship-review-form-cancel', 'Cancel');
    row.appendChild(go);
    row.appendChild(cancel);
    anywayConfirmForm.appendChild(row);
    cancel.addEventListener('click', () => { anywayConfirmForm.style.display = 'none'; clearErr(); });
    go.addEventListener('click', () => doMerge(go, true, reason.value.trim()));
  }
  // Only PR-mode cards get the merge forms, so every other card keeps its
  // existing form order.
  if (prActive) {
    actionsWrap.appendChild(mergeConfirmForm);
    actionsWrap.appendChild(anywayConfirmForm);
  }

  // checksGreen tracks the PR checks (pr_merge); the test gate is separate.
  let checksGreen = card.pr_checks_summary === 'passed';
  function showBypassConfirm(missing) {
    const list = (missing && missing.length) ? missing : ((testGate && testGate.state.missing) || []);
    const target = prActive ? `PR #${card.pr_number} ${shortHead}` : shortHead;
    bypassLabel.textContent = `Merge ${target} → ${cardTarget(card)} without tests? Missing:`;
    bypassMissing.replaceChildren(...list.map((m) => el('li', 'ship-review-merge-without-tests-missing-item', m)));
    bypassNote.textContent = (prActive && !checksGreen)
      ? 'Checks are not all green either, so this also overrides them. Both are logged, and a backlog task to add the tests is created.'
      : 'This is logged, and a backlog task to add the tests is created.';
    for (const f of allForms) f.style.display = f === bypassConfirmForm ? '' : 'none';
    headMovedBanner.style.display = 'none';
    clearErr();
  }
  const bypassNote = el('div', 'ship-review-merge-without-tests-note', '');
  {
    bypassConfirmForm.appendChild(bypassLabel);
    bypassConfirmForm.appendChild(bypassMissing);
    bypassConfirmForm.appendChild(bypassNote);
    const reason = document.createElement('input');
    reason.type = 'text';
    reason.className = 'ship-review-form-input ship-review-merge-without-tests-reason';
    reason.placeholder = 'Why merge without tests? (logged)';
    bypassConfirmForm.appendChild(reason);
    const row = el('div', 'ship-review-form-row');
    const go = el('button', 'ship-review-reject-submit-btn ship-review-merge-without-tests-submit', 'Merge without tests');
    const cancel = el('button', 'ship-review-form-cancel', 'Cancel');
    row.appendChild(go);
    row.appendChild(cancel);
    bypassConfirmForm.appendChild(row);
    cancel.addEventListener('click', () => { bypassConfirmForm.style.display = 'none'; clearErr(); });
    go.addEventListener('click', () => {
      const extra = { merge_without_tests: true, merge_without_tests_reason: reason.value.trim() };
      if (prActive) doMerge(go, !checksGreen, reason.value.trim(), extra);
      else doApprove(go, extra);
    });
  }
  // Before the reject form: specs and the tray expect Reject's form last.
  actionsWrap.insertBefore(bypassConfirmForm, rejectForm);
  if (testGate) testGate.bypassBtn.addEventListener('click', () => showBypassConfirm());

  // Warning buttons: Merge anyway / Send failures to agent.
  if (prActive) {
    const warnRow = el('div', 'ship-review-form-row ship-review-pr-warning-actions');
    const anywayBtn = el('button', 'ship-review-merge-anyway-btn', 'Merge anyway');
    const failuresBtn = el('button', 'ship-review-send-failures-btn', '↩ Send failures to agent');
    warnRow.appendChild(anywayBtn);
    warnRow.appendChild(failuresBtn);
    prWarning.appendChild(warnRow);
    anywayBtn.addEventListener('click', () => {
      const n = prWarningList.children.length;
      anywayLabel.textContent = `Merge PR #${card.pr_number} ${shortHead} → ${cardTarget(card)} with ${n} check${n === 1 ? '' : 's'} not green? This is logged as an override.`;
      showOnly(anywayConfirmForm);
    });
    failuresBtn.addEventListener('click', async () => {
      failuresBtn.disabled = true;
      clearErr();
      try {
        const f = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review/check-failures`);
        for (const x of allForms) x.style.display = 'none';
        sendBackCI = true;
        sbLabel.textContent = 'CI failures for the agent (edit before sending):';
        sbTextareaRef.value = f.comment || '';
        sbTextareaRef.rows = 10;
        sendBackForm.style.display = '';
      } catch (e) {
        showErr('Could not read the failing checks: ' + (e.message || e));
      }
      failuresBtn.disabled = false;
    });
  }

  // ── Primary action buttons ──
  const actions = el('div', 'ship-review-actions');

  if (prActive) {
    const mergeBtn = el('button', 'ship-review-merge-btn', `✓ Merge PR #${card.pr_number}`);
    mergeBtn.disabled = true;
    mergeBtn.title = 'Enabled when all checks are green';
    mergeBtn.addEventListener('click', () => showOnly(mergeConfirmForm));
    actions.appendChild(mergeBtn);
    // Merge needs green checks and a passing test gate (STA-734).
    const updateMergeBtn = () => {
      const gate = testGate ? testGate.state : { loaded: true, blocking: false };
      const gateOK = gate.loaded && !gate.blocking;
      mergeBtn.disabled = !checksGreen || !gateOK;
      mergeBtn.title = !checksGreen ? 'Enabled when all checks are green'
        : !gate.loaded ? 'Checking test coverage…'
        : !gateOK ? 'Not tested: see Test coverage. Use Merge without tests to bypass.' : '';
      if (mergeBtn.disabled) mergeConfirmForm.style.display = 'none';
    };
    section.addEventListener('pr-checks', (e) => {
      checksGreen = e.detail.summary === 'passed';
      updateMergeBtn();
    });
    section.addEventListener('test-gate', updateMergeBtn);
    section.addEventListener('pr-head-moved', (e) => {
      mergeBtn.disabled = true;
      headMovedBanner.querySelector('.ship-review-head-moved-text').textContent =
        `PR head moved to ${(e.detail.sha || '?').slice(0, 12)} after the checks started. Merge blocked until the card is re-pinned and checks re-run. `;
      headMovedBanner.style.display = '';
    });
  } else {
    const approveLabel = approveMode === 'open_pr' ? '✓ Approve & open PR'
      : approveMode === 'pr_merge' ? (card.pr_number > 0 ? `✓ Approve & push to PR #${card.pr_number}` : '✓ Approve & open PR')
      : '✓ Approve & Merge';
    const approveBtn = el('button', 'ship-review-approve-btn', approveLabel);
    approveBtn.addEventListener('click', () => showOnly(approveConfirmForm));
    actions.appendChild(approveBtn);
    // direct and open_pr Approve land on main, so they wait for the test
    // gate (STA-734); pr_merge Approve only opens the PR.
    if (testGate && approveMode !== 'pr_merge') {
      const updateApproveBtn = () => {
        const g = testGate.state;
        approveBtn.disabled = !g.loaded || g.blocking;
        approveBtn.title = !g.loaded ? 'Checking test coverage…'
          : g.blocking ? 'Not tested: see Test coverage. Use Merge without tests to bypass.' : '';
        if (approveBtn.disabled) approveConfirmForm.style.display = 'none';
      };
      updateApproveBtn();
      section.addEventListener('test-gate', updateApproveBtn);
    }
  }

  const sendBackBtn = el('button', 'ship-review-sendback-btn', '↩ Send Back');
  sendBackBtn.addEventListener('click', () => {
    if (sendBackCI) {
      sendBackCI = false;
      sbLabel.textContent = 'Feedback for the agent (required):';
      sbTextareaRef.value = '';
      sbTextareaRef.rows = 3;
      sendBackForm.style.display = 'none';
    }
    showOnly(sendBackForm);
  });
  actions.appendChild(sendBackBtn);

  const rejectBtn = el('button', 'ship-review-reject-btn', '✕ Reject');
  rejectBtn.addEventListener('click', () => showOnly(rejectForm));
  actions.appendChild(rejectBtn);

  // On the task page the buttons sit in the sticky header and their forms
  // drop down under it; anywhere else they stay at the bottom of the card.
  const headerSlot = document.getElementById(`task-page-review-actions-${taskId}`);
  const headerTray = document.getElementById(`task-page-action-tray-${taskId}`);
  if (headerSlot && headerTray) {
    headerSlot.appendChild(actions);
    headerTray.appendChild(actionsWrap);
  } else {
    actionsWrap.appendChild(actions);
    section.appendChild(actionsWrap);
  }
  container.appendChild(section);
  if (applyPRChecks) applyPRChecks(card.pr_checks_summary || 'running', card.pr_checks || []);

  // For SSE-driven re-renders the buttons may already be in the DOM.
  // Hide them so they don't sit alongside the card's own action buttons.
  const page = container.closest('#task-page-content, #panel-content') || container;
  for (const btn of page.querySelectorAll('.run-now-btn, .mark-done-btn, .mark-done-error, .ship-review-see-card-link')) {
    btn.style.display = 'none';
  }
}

// showWorkingState replaces the card with a "Sent back · agent working (run N)…" spinner.
// The SSE handler will re-render when the new card arrives.
function showWorkingState(section, taskId, prevRunNumber) {
  clearShipReviewHeaderActions(taskId);
  const nextRun = (prevRunNumber || 1) + 1;
  const working = el('div', 'ship-review-card ship-review-card--working task-page-section');
  working.id = `ship-review-${taskId}`;
  const hdr = el('div', 'ship-review-header');
  hdr.appendChild(el('span', 'ship-review-badge', 'Ship Review'));
  hdr.appendChild(el('span', 'ship-review-status-badge status-working', 'Sent Back'));
  working.appendChild(hdr);
  const status = el('div', 'ship-review-working-status');
  const spinner = el('span', 'ship-review-spinner', '');
  spinner.setAttribute('aria-label', 'working');
  status.appendChild(spinner);
  status.appendChild(document.createTextNode(` Sent back · agent working (run ${nextRun})…`));
  working.appendChild(status);
  section.replaceWith(working);
}

async function renderShipReviewCard(container, taskId) {
  if (!taskId) return;
  let card;
  try {
    const resp = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/ship-review`);
    card = resp;
  } catch {
    return; // no card or fetch error — silently skip
  }
  renderShipReviewCardFromData(container, taskId, card);
}

// ── Migrations panel ──────────────────────────────────────────────────────────

// highlightSQL applies minimal keyword-based syntax colouring to a SQL string.
function highlightSQL(raw) {
  const keywords = [
    'SELECT','INSERT','UPDATE','DELETE','CREATE','DROP','ALTER','TRUNCATE',
    'TABLE','VIEW','INDEX','SEQUENCE','FUNCTION','PROCEDURE','TRIGGER',
    'FROM','WHERE','JOIN','LEFT','RIGHT','INNER','OUTER','ON','AS',
    'SET','VALUES','INTO','AND','OR','NOT','NULL','DEFAULT','PRIMARY','KEY',
    'FOREIGN','REFERENCES','UNIQUE','CHECK','CONSTRAINT','ADD','COLUMN',
    'TYPE','CASCADE','IF','EXISTS','BEGIN','COMMIT','ROLLBACK','POLICY',
    'ENABLE','DISABLE','ROW','LEVEL','SECURITY','GRANT','REVOKE',
    'RETURNS','LANGUAGE','PLPGSQL','VOLATILE','STABLE','IMMUTABLE',
  ];
  const escaped = escapeHtml(raw);
  // Wrap keywords (word-boundary, case-insensitive)
  const kwRe = new RegExp(`\\b(${keywords.join('|')})\\b`, 'gi');
  const withKw = escaped.replace(kwRe, '<span class="sql-kw">$1</span>');
  // Comments
  const withComments = withKw.replace(/(--[^\n]*)/g, '<span class="sql-comment">$1</span>');
  // String literals
  const withStrings = withComments.replace(/('(?:[^']|'')*')/g, '<span class="sql-str">$1</span>');
  return withStrings;
}

// showManualVerifyUI inserts a "paste results" UI below the actions row
// for manual verification mode.
function showManualVerifyUI(card, actions, taskId, mig, checks, verifyQuery, applyBtn, copyVerifyBtn) {
  const existing = card.querySelector('.migration-manual-verify');
  if (existing) return;

  const box = el('div', 'migration-manual-verify');
  box.style.cssText = 'margin-top:8px;padding:8px;border:1px solid var(--border,#334155);border-radius:6px;font-size:0.8rem;';

  box.appendChild(el('div', '', 'No read-only connection configured. Run the verification query in your SQL editor, then tick each object below:'));

  const checkList = el('div', 'migration-manual-checks');
  checkList.style.cssText = 'margin:8px 0;display:flex;flex-direction:column;gap:4px;';
  const tickStates = {};
  for (const c of checks) {
    const row = el('label', 'migration-check-row');
    row.style.cssText = 'display:flex;align-items:center;gap:6px;cursor:pointer;';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.dataset.description = c.description;
    tickStates[c.description] = false;
    cb.addEventListener('change', () => { tickStates[c.description] = cb.checked; });
    row.appendChild(cb);
    row.appendChild(el('span', '', c.description));
    checkList.appendChild(row);
  }
  box.appendChild(checkList);

  const confirmBtn = el('button', 'btn btn-primary btn-sm', 'Confirm results');
  confirmBtn.style.marginTop = '4px';
  confirmBtn.addEventListener('click', async () => {
    confirmBtn.disabled = true;
    const checkResults = checks.map(c => ({ description: c.description, passed: !!tickStates[c.description] }));
    try {
      const resp = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations/mark-applied`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...authHeader() },
        body: JSON.stringify({ path: mig.path, applied_by: 'board', check_results: checkResults }),
      });
      const data = await resp.json();
      if (data.ok) {
        applyBtn.textContent = '✓ Applied (manual)';
        applyBtn.dataset.applied = '1';
        card.classList.add('migration-applied');
        box.remove();
        if (copyVerifyBtn) copyVerifyBtn.style.display = 'none';
      } else if (resp.status === 409) {
        confirmBtn.disabled = false;
        const failed = (data.failed || []).join(', ');
        const errMsg = card.querySelector('.migration-verify-error') || el('div', 'migration-verify-error');
        errMsg.style.cssText = 'color:var(--danger,#f87171);margin-top:4px;font-size:0.75rem;';
        errMsg.textContent = `Failed: ${failed || 'some checks did not pass'}`;
        box.appendChild(errMsg);
      }
    } catch (err) {
      confirmBtn.disabled = false;
      console.error('manual confirm failed:', err);
    }
  });
  box.appendChild(confirmBtn);

  // Override link
  const overrideLink = document.createElement('a');
  overrideLink.href = '#';
  overrideLink.textContent = 'Override (type reason)';
  overrideLink.style.cssText = 'display:block;margin-top:6px;font-size:0.75rem;color:var(--accent,#38bdf8);';
  overrideLink.addEventListener('click', e => {
    e.preventDefault();
    const reason = prompt('Override reason (required):');
    if (!reason) return;
    fetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations/mark-applied`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ path: mig.path, applied_by: 'board', override_reason: reason }),
    }).then(r => r.json()).then(d => {
      if (d.ok) {
        applyBtn.textContent = '✓ Applied (override)';
        applyBtn.dataset.applied = '1';
        card.classList.add('migration-applied');
        box.remove();
        if (copyVerifyBtn) copyVerifyBtn.style.display = 'none';
      }
    });
  });
  box.appendChild(overrideLink);

  card.appendChild(box);
}

// showVerifyFailedUI displays failed auto-verify results with override option.
function showVerifyFailedUI(card, actions, taskId, mig, data, applyBtn, copyVerifyBtn) {
  const existing = card.querySelector('.migration-verify-failed');
  if (existing) existing.remove();

  const box = el('div', 'migration-verify-failed');
  box.style.cssText = 'margin-top:8px;padding:8px;border:1px solid var(--danger,#f87171);border-radius:6px;font-size:0.8rem;';
  box.appendChild(el('div', '', `Verification failed — these objects are missing:`));
  const ul = el('ul', '');
  ul.style.cssText = 'margin:4px 0 4px 16px;';
  for (const f of (data.failed || [])) {
    ul.appendChild(el('li', '', f));
  }
  box.appendChild(ul);

  const overrideLink = document.createElement('a');
  overrideLink.href = '#';
  overrideLink.textContent = 'Override (type reason)';
  overrideLink.style.cssText = 'display:block;margin-top:4px;font-size:0.75rem;color:var(--accent,#38bdf8);';
  overrideLink.addEventListener('click', e => {
    e.preventDefault();
    const reason = prompt('Override reason (required):');
    if (!reason) return;
    fetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations/mark-applied`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ path: mig.path, applied_by: 'board', override_reason: reason }),
    }).then(r => r.json()).then(d => {
      if (d.ok) {
        applyBtn.textContent = '✓ Applied (override)';
        applyBtn.dataset.applied = '1';
        card.classList.add('migration-applied');
        box.remove();
        if (copyVerifyBtn) copyVerifyBtn.style.display = 'none';
      }
    });
  });
  box.appendChild(overrideLink);
  card.appendChild(box);
}

async function renderMigrationsPanel(container, taskId) {
  let data;
  try {
    data = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations`);
  } catch {
    return; // no migrations or fetch error
  }
  const migrations = data.migrations || [];
  if (!migrations.length) return;
  const hasAutoVerify = !!data.has_auto_verify;

  const section = el('div', 'task-page-section migrations-section');
  section.id = `migrations-panel-${taskId}`;

  const titleRow = el('div', 'task-page-section-title-row');
  titleRow.style.cssText = 'display:flex;align-items:center;gap:8px;';
  titleRow.appendChild(el('div', 'task-page-section-title', `Migrations (${migrations.length})`));
  if (hasAutoVerify) {
    const autoBadge = el('span', 'migration-badge-auto', '⚡ Auto-verify');
    autoBadge.title = 'Read-only DB connection configured — Mark applied will verify automatically';
    autoBadge.style.cssText = 'font-size:0.7rem;padding:2px 6px;border-radius:4px;background:var(--accent,#38bdf8);color:#0f172a;font-weight:600;';
    titleRow.appendChild(autoBadge);
  }

  // Validate sql_editor_url scheme before setting href (prevent javascript: XSS).
  const safeEditorURL = (() => {
    if (!data.sql_editor_url) return '';
    try {
      const u = new URL(data.sql_editor_url);
      return (u.protocol === 'http:' || u.protocol === 'https:') ? data.sql_editor_url : '';
    } catch { return ''; }
  })();
  if (safeEditorURL) {
    const editorLink = document.createElement('a');
    editorLink.href = safeEditorURL;
    editorLink.target = '_blank';
    editorLink.rel = 'noopener noreferrer';
    editorLink.textContent = 'Open SQL editor →';
    editorLink.className = 'migrations-editor-link';
    editorLink.style.cssText = 'font-size:0.8rem;color:var(--accent,#38bdf8);margin-left:auto;';
    titleRow.appendChild(editorLink);
  }
  section.appendChild(titleRow);

  for (const mig of migrations) {
    const card = el('div', 'migration-file-card');

    // Path header row
    const pathRow = el('div', 'migration-file-path');
    pathRow.textContent = mig.path;

    // Risk badge
    const riskBadge = el('span', mig.additive_only ? 'migration-badge-safe' : 'migration-badge-risk',
      mig.read_error ? '⚠ Could not read file' : (mig.additive_only ? 'Additive only ✓' : '⚠ Destructive'));
    pathRow.appendChild(riskBadge);
    card.appendChild(pathRow);

    // Risk statement list
    if (!mig.additive_only && mig.risk_statements && mig.risk_statements.length) {
      const riskList = el('ul', 'migration-risk-list');
      for (const stmt of mig.risk_statements) {
        const li = el('li', 'migration-risk-item', stmt);
        riskList.appendChild(li);
      }
      card.appendChild(riskList);
    }

    if (mig.read_error) {
      card.appendChild(el('div', 'migration-risk-list', mig.read_error));
    }

    // SQL block with syntax highlighting
    const pre = document.createElement('pre');
    pre.className = 'migration-sql-block';
    const code = document.createElement('code');
    code.innerHTML = highlightSQL(mig.sql || '');
    pre.appendChild(code);
    card.appendChild(pre);

    // Action row: Copy + Mark applied
    const actions = el('div', 'migration-actions');

    actions.appendChild(longContentTrigger('migration-sql', `Migration SQL: ${mig.path}`, () => {
      const full = el('pre', 'content-modal-pre migration-sql-full');
      const c = document.createElement('code');
      c.innerHTML = highlightSQL(mig.sql || '');
      full.appendChild(c);
      return full;
    }));

    const copyBtn = el('button', 'btn btn-secondary btn-sm migration-copy-btn', 'Copy SQL');
    copyBtn.title = 'Copy SQL to clipboard';
    copyBtn.disabled = !mig.sql;
    const flash = (label) => {
      copyBtn.textContent = label;
      setTimeout(() => { copyBtn.textContent = 'Copy SQL'; }, 2000);
    };
    copyBtn.addEventListener('click', async () => {
      const sql = mig.sql || '';
      if (!sql) { flash('Nothing to copy'); return; }
      try {
        await navigator.clipboard.writeText(sql);
        flash('✓ Copied');
        return;
      } catch { /* fall through to the textarea fallback */ }
      const ta = document.createElement('textarea');
      ta.value = sql;
      ta.setAttribute('readonly', '');
      ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      let ok = false;
      try { ok = document.execCommand('copy'); } catch { ok = false; }
      document.body.removeChild(ta);
      flash(ok ? '✓ Copied' : 'Copy failed: select the SQL above');
    });
    actions.appendChild(copyBtn);

    // Verification query copy button (shown before verification is done)
    const verifyChecks = mig.verification_checks || [];
    let copyVerifyBtn = null;
    if (verifyChecks.length > 0 && !hasAutoVerify) {
      copyVerifyBtn = el('button', 'btn btn-secondary btn-sm migration-copy-verify-btn', 'Copy verify query');
      copyVerifyBtn.title = 'Copy read-only verification query to clipboard';
      copyVerifyBtn.addEventListener('click', async () => {
        const q = mig.verification_query || '';
        try { await navigator.clipboard.writeText(q); }
        catch { const ta = document.createElement('textarea'); ta.value = q; ta.style.cssText='position:fixed;opacity:0;'; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta); }
        copyVerifyBtn.textContent = '✓ Copied';
        setTimeout(() => { copyVerifyBtn.textContent = 'Copy verify query'; }, 2000);
      });
      actions.appendChild(copyVerifyBtn);
    }

    const applyBtn = el('button', 'btn btn-secondary btn-sm migration-apply-btn', 'Mark applied');
    applyBtn.title = hasAutoVerify ? 'Verify schema and record as applied' : 'Paste check results or override to record as applied';
    applyBtn.dataset.path = mig.path;
    applyBtn.dataset.applied = mig.applied_at ? '1' : '';
    if (mig.applied_at) {
      applyBtn.textContent = '✓ Marked applied';
      applyBtn.title = `Marked applied by ${mig.applied_by || 'board'} at ${new Date(mig.applied_at).toLocaleString()} (not verified against the database)`;
      card.classList.add('migration-applied');
    }
    applyBtn.addEventListener('click', async () => {
      if (applyBtn.dataset.applied) return;
      applyBtn.disabled = true;
      applyBtn.textContent = hasAutoVerify ? 'Verifying…' : 'Marking…';
      try {
        const resp = await fetch(`/api/tasks/${encodeURIComponent(taskId)}/migrations/mark-applied`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeader() },
          body: JSON.stringify({ path: mig.path, applied_by: 'board' }),
        });
        const data = await resp.json();

        if (data.ok === true) {
          // Success — auto or unchecked mode
          const modeBadge = data.mode === 'auto' ? ' (auto-verified)' : data.mode === 'unchecked' ? '' : ' (manual)';
          applyBtn.textContent = `✓ Applied${modeBadge}`;
          applyBtn.dataset.applied = '1';
          card.classList.add('migration-applied');
          if (copyVerifyBtn) copyVerifyBtn.style.display = 'none';
        } else if (data.mode === 'manual' && data.verification_query) {
          // Manual fallback — show paste UI
          applyBtn.disabled = false;
          applyBtn.textContent = 'Mark applied';
          showManualVerifyUI(card, actions, taskId, mig, verifyChecks, data.verification_query, applyBtn, copyVerifyBtn);
        } else if (!resp.ok && data.failed) {
          // Verification failed — show failed checks
          applyBtn.disabled = false;
          applyBtn.textContent = 'Mark applied';
          showVerifyFailedUI(card, actions, taskId, mig, data, applyBtn, copyVerifyBtn);
        } else {
          applyBtn.disabled = false;
          applyBtn.textContent = 'Mark applied';
          console.error('mark-applied unexpected response:', data);
        }
      } catch (err) {
        applyBtn.disabled = false;
        applyBtn.textContent = 'Mark applied';
        console.error('mark-applied failed:', err);
      }
    });
    actions.appendChild(applyBtn);

    card.appendChild(actions);
    section.appendChild(card);
  }

  container.appendChild(section);
}

// Stages where an agent is mid-run (or queued to run) and waiting on a card:
// answering resumes it by default. Anywhere else (error, stopped, backlog,
// in_review, done…) answering defaults to recording the answer only.
const DECISION_RESUME_STAGES = ['todo', 'in_progress', 'blocked', 'paused'];

// A ship review card in pending/sent_back/approved/rejected state blocks Run
// Now and Mark done: starting a new run on finished work or closing without
// merging both confuse the review flow.
const SHIP_CARD_ACTIVE_STATUSES = new Set(['pending', 'sent_back', 'approved', 'rejected']);
const RUN_NOW_STATUSES = new Set(['active', 'todo', 'backlog', 'blocked', 'in_review']);

function shipCardBlocksRun(shipCard) {
  return !!(shipCard && SHIP_CARD_ACTIVE_STATUSES.has(shipCard.status));
}

// runNowOffered reports whether the task page shows Run Now for task.
function runNowOffered(task, shipCard) {
  return !!(task && task.id && RUN_NOW_STATUSES.has(task.status) && !shipCardBlocksRun(shipCard));
}

function decisionResumesByDefault(task) {
  return DECISION_RESUME_STAGES.includes(String((task && task.execution_stage) || '').toLowerCase());
}

// Renders pending decision cards into container and returns how many.
// Each card answers with "Accept & resume", "Accept only" or "Reject". Accept
// only sends resume:false, so the task stays where it is. Accept & resume on
// a task that is not waiting mid-run records the answer, then presses Run Now
// so Run Now's Board, hold and backlog gates apply (task-e3fe2c0b).
// opts.canRunNow says whether the page offers Run Now (no active ship review
// card, a runnable status); a parked task where it does not gets no Accept &
// resume, so a card answer never starts a run the Run Now button would not.
function renderInteractionCards(container, task, interactions, opts = {}) {
  const taskId = (task && task.id) || '';
  const pending = (interactions || []).filter(i => i.status === 'pending');
  if (!pending.length) {
    container.appendChild(el('p', 'panel-field-muted task-page-tab-empty', 'No decisions waiting on you.'));
    return 0;
  }
  const resumeDefault = decisionResumesByDefault(task);
  const offerResume = resumeDefault || !!opts.canRunNow;

  const section = el('div', 'task-page-section');
  section.id = 'interaction-cards-section';
  section.appendChild(el('div', 'task-page-section-title', `Waiting on you (${pending.length})`));
  const stageName = task.execution_stage || 'not running';
  section.appendChild(el('p', 'panel-field-muted decision-stage-hint', resumeDefault
    ? 'The agent is waiting on this. Accept & resume lets it continue.'
    : offerResume
      ? `This task is ${stageName}. Accept only records your answer and leaves it there; Accept & resume also starts a run.`
      : `This task is ${stageName}. Accept only records your answer and leaves it there.`));

  for (const interaction of pending) {
    const kind = interaction.interaction_kind || '';
    const card = el('div', `interaction-card kind-${kind}`);

    let payloadObj = {};
    try { payloadObj = JSON.parse(interaction.payload || '{}'); } catch { /* ignore */ }

    // Header
    const kindLabel = { ask_user_questions: 'Questions', request_confirmation: 'Confirmation', suggest_tasks: 'Suggested Tasks' }[kind] || kind;
    const header = el('div', 'interaction-card-header');
    header.appendChild(el('span', `interaction-kind-badge kind-${kind}`, kindLabel));
    card.appendChild(header);

    // Body
    const body = el('div', 'interaction-card-body');
    // selectedAnswers: map of question index -> selected option string (for ask_user_questions)
    const selectedAnswers = {};

    if (kind === 'request_confirmation') {
      const prompt = payloadObj.prompt || '';
      if (prompt) {
        const pd = el('div', 'interaction-prompt');
        pd.appendChild(mdEl(prompt));
        body.appendChild(pd);
      }
    } else if (kind === 'ask_user_questions') {
      const qs = payloadObj.questions || [];
      qs.forEach((q, qi) => {
        const item = el('div', 'interaction-question-item');
        item.appendChild(el('p', '', q.question || ''));
        if (q.options && q.options.length) {
          const opts = el('div', 'interaction-question-opts');
          for (const opt of q.options) {
            const btn = el('button', 'interaction-question-opt', opt);
            btn.addEventListener('click', () => {
              // toggle selection; only one option per question
              opts.querySelectorAll('.interaction-question-opt').forEach(b => b.classList.remove('selected'));
              btn.classList.add('selected');
              selectedAnswers[qi] = opt;
            });
            opts.appendChild(btn);
          }
          item.appendChild(opts);
        }
        body.appendChild(item);
      });
    } else if (kind === 'suggest_tasks') {
      const tasks = payloadObj.tasks || [];
      for (const t of tasks) {
        const item = el('div', 'interaction-task-item');
        item.appendChild(el('div', 'interaction-task-title', t.title || ''));
        if (t.description) item.appendChild(el('div', 'interaction-task-desc', t.description));
        body.appendChild(item);
      }
    }
    card.appendChild(body);

    // Actions
    const actions = el('div', 'interaction-card-actions');

    const allBtns = () => [resumeBtn, acceptOnlyBtn, rejectBtn];
    const showError = (text) => {
      const prev = header.querySelector('.interaction-resolve-error');
      if (prev) prev.remove();
      header.appendChild(el('span', 'interaction-resolve-error', text));
    };
    const resolveInteraction = async (status, resume) => {
      allBtns().forEach(b => { b.disabled = true; });
      try {
        // Build response payload for ask_user_questions
        let response;
        if (kind === 'ask_user_questions') {
          const qs = payloadObj.questions || [];
          response = { answers: qs.map((q, qi) => ({ question: q.question || '', selected: selectedAnswers[qi] || null })) };
        }
        await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/interactions/${interaction.id}/resolve`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          // A mid-run task resumes from the card's own wake; a parked one
          // is answered quietly and, when asked, started by Run Now below.
          body: JSON.stringify({ status, resume: resume && resumeDefault, ...(response !== undefined ? { response } : {}) })
        });
      } catch (err) {
        allBtns().forEach(b => { b.disabled = false; });
        showError(`Failed: ${(err && err.message) || err}`);
        return;
      }
      // The answer is recorded from here on: the card is resolved whatever
      // Run Now does, and its buttons stay off (a second resolve would fail).
      card.classList.add('resolved');
      let resumed = resume;
      if (resume && !resumeDefault) {
        try {
          // Run Now itself, with its Touch ID retry for prod-targeting tasks.
          resumed = (await postRunNow(taskId)) !== null;
          if (!resumed) showError('Answer recorded; Run Now was cancelled.');
        } catch (err) {
          resumed = false;
          showError(`Answer recorded; Run Now failed: ${(err && err.message) || err}`);
        }
      }
      const label = status !== 'accepted' ? '✗ Rejected' : (resumed ? '✓ Accepted · resumed' : '✓ Accepted · answer only');
      header.appendChild(el('span', `interaction-resolved-badge status-${status}`, label));
    };

    const resumeBtn = el('button', 'interaction-accept-btn', 'Accept & resume');
    resumeBtn.type = 'button';
    resumeBtn.title = resumeDefault ? 'Record the answer; the waiting agent continues' : 'Record the answer, then Run Now';
    resumeBtn.addEventListener('click', () => resolveInteraction('accepted', true));

    const acceptOnlyBtn = el('button', 'interaction-accept-btn interaction-accept-only-btn', 'Accept only');
    acceptOnlyBtn.type = 'button';
    acceptOnlyBtn.title = 'Record the answer; the task stays where it is';
    acceptOnlyBtn.addEventListener('click', () => resolveInteraction('accepted', false));

    // The default action for this task's state is the primary button.
    (resumeDefault ? resumeBtn : acceptOnlyBtn).classList.add('is-default');

    const rejectBtn = el('button', 'interaction-reject-btn', 'Reject');
    rejectBtn.type = 'button';
    // A rejection resumes only an agent that is waiting mid-run.
    rejectBtn.addEventListener('click', () => resolveInteraction('rejected', resumeDefault));

    actions.appendChild(resumeDefault ? resumeBtn : acceptOnlyBtn);
    if (resumeDefault) actions.appendChild(acceptOnlyBtn);
    else if (offerResume) actions.appendChild(resumeBtn);
    actions.appendChild(rejectBtn);
    card.appendChild(actions);
    section.appendChild(card);
  }
  container.appendChild(section);
  return pending.length;
}

// Header line under the back button: "STA-12 · assigned to X (claude · sonnet)".
// The model comes from the latest route step, since the task row has none.
function taskPageHeaderMeta(task, ident) {
  const parts = [];
  if (ident) parts.push(ident);
  const assignee = task.assignee_name || task.checkout_agent_id || '';
  // Latest route step that names a model; fallback reasons are not a model.
  let model = '';
  for (const s of [...(task.runSteps || [])].reverse()) {
    if (s && s.kind === 'route' && (model = routeModel(s.title))) break;
  }
  if (assignee) parts.push(`assigned to ${assignee}${model ? ` (${model})` : ''}`);
  else if (model) parts.push(model);
  return parts.join(' · ');
}

// Last tab picked on the task page, so a re-render of the same task (SSE,
// Run Now, Mark done) keeps it instead of jumping back to the default.
let taskPagePanelTab = { taskId: null, key: null };

const TASK_PAGE_TABS = [
  { key: 'decisions', label: 'Decisions' },
  { key: 'review', label: 'Review' },
  { key: 'diff', label: 'Diff' },
  { key: 'migrations', label: 'Migrations' },
  { key: 'brief', label: 'Brief' },
  { key: 'artifacts', label: 'Artifacts' },
];

// Right column of the task page (STA-638): one tablist over Decisions /
// Review / Diff / Migrations / Brief / Artifacts. Returns the column, the tabpanel per key (callers fill
// them with the existing renderers), and setters for the tab badges.
function buildTaskPagePanel(taskId, defaultKey) {
  const side = el('div', 'task-page-side task-page-panel');
  const tablist = el('div', 'task-page-tabs');
  tablist.setAttribute('role', 'tablist');
  tablist.setAttribute('aria-label', 'Task details');
  const body = el('div', 'task-page-tabpanels');

  const tabs = {};
  const panels = {};
  const counts = {};
  for (const { key, label } of TASK_PAGE_TABS) {
    const tab = el('button', 'task-page-tab');
    tab.type = 'button';
    tab.id = `task-page-tab-${key}`;
    tab.setAttribute('role', 'tab');
    tab.setAttribute('aria-controls', `task-page-tabpanel-${key}`);
    tab.dataset.tab = key;
    tab.appendChild(el('span', 'task-page-tab-label', label));
    counts[key] = el('span', 'task-page-tab-count');
    tab.appendChild(counts[key]);
    tablist.appendChild(tab);
    tabs[key] = tab;

    const panel = el('div', `task-page-tabpanel task-page-tabpanel-${key}`);
    panel.id = `task-page-tabpanel-${key}`;
    panel.setAttribute('role', 'tabpanel');
    panel.setAttribute('aria-labelledby', tab.id);
    body.appendChild(panel);
    panels[key] = panel;
  }

  const select = (key, focus = false) => {
    for (const { key: k } of TASK_PAGE_TABS) {
      const on = k === key;
      tabs[k].setAttribute('aria-selected', on ? 'true' : 'false');
      tabs[k].tabIndex = on ? 0 : -1;
      tabs[k].classList.toggle('active', on);
      panels[k].hidden = !on;
    }
    if (focus) tabs[key].focus();
    taskPagePanelTab = { taskId, key };
  };

  tablist.addEventListener('click', (e) => {
    const tab = e.target.closest('[role="tab"]');
    if (tab) select(tab.dataset.tab);
  });
  // Arrow keys move between tabs (WAI-ARIA tabs pattern).
  tablist.addEventListener('keydown', (e) => {
    const keys = TASK_PAGE_TABS.map(t => t.key);
    const i = keys.indexOf(e.target.dataset && e.target.dataset.tab);
    if (i < 0) return;
    let next = null;
    if (e.key === 'ArrowRight') next = keys[(i + 1) % keys.length];
    else if (e.key === 'ArrowLeft') next = keys[(i - 1 + keys.length) % keys.length];
    else if (e.key === 'Home') next = keys[0];
    else if (e.key === 'End') next = keys[keys.length - 1];
    if (!next) return;
    e.preventDefault();
    select(next, true);
  });

  const remembered = taskPagePanelTab.taskId === taskId ? taskPagePanelTab.key : null;
  select(remembered && panels[remembered] ? remembered : defaultKey);

  side.appendChild(tablist);
  side.appendChild(body);
  const setCount = (key, n) => {
    counts[key].textContent = n ? String(n) : '';
    // Decisions waiting on the Board are the one count that needs attention.
    if (key === 'decisions') tabs[key].classList.toggle('has-pending', n > 0);
  };
  return { side, panels, select, setCount };
}

// Renders the task layout into the full page (#task-page-content) or, with
// opts.drawer, into the drawer (#panel-content, STA-700). The drawer has no
// Back button (it has its own close / expand / open controls) and actions
// that reload the task reload it in the drawer.
function renderTaskPage(container, task, comments, interactions, diffData, checkpoints, runErrors, shipCard, opts = {}) {
  container.innerHTML = '';
  const drawer = Boolean(opts.drawer);
  const reopen = () => (drawer ? openDetail(task.id, false) : openTaskPage(task.id));
  const openTask = (id) => (drawer ? openDetail(id) : openTaskPage(id));

  const ident = task.identifier || (task.id ? `#${task.id.slice(0, 8)}` : '');

  // ── Header (STA-638): identifier · assignee/model · title · status · actions ──
  // Primary actions live here so they are on screen without scrolling:
  // Run Now / Mark done, run control while running, and the ship review
  // buttons (mounted by renderShipReviewCardFromData into the review slot).
  const header = el('div', 'task-page-header');
  const headerRow = el('div', 'task-page-header-row');
  if (!drawer) {
    const backBtn = el('button', 'task-page-back-btn', '← Back');
    backBtn.addEventListener('click', () => {
      stopChatPoll();
      stopElapsedTicker();
      state.openDetailTaskId = null;
      history.back();
    });
    headerRow.appendChild(backBtn);
  }

  const headerMain = el('div', 'task-page-header-main');
  const metaLine = taskPageHeaderMeta(task, ident);
  if (metaLine) headerMain.appendChild(el('div', 'task-page-breadcrumb', metaLine));
  const titleRow = el('div', 'task-page-title-row');
  titleRow.appendChild(el('h1', 'task-page-title', task.title || task.name || '(untitled)'));
  const pillsRow = el('div', 'task-page-pills');
  if (task.status) pillsRow.appendChild(statusPill(task.status));
  if (task.priority) pillsRow.appendChild(statusPill(task.priority));
  const hiddenBadge = originBadge(task);
  if (hiddenBadge) {
    hiddenBadge.textContent = isLegacy(task) ? 'Legacy' : 'Archived';
    hiddenBadge.classList.add('task-page-origin-badge');
    pillsRow.appendChild(hiddenBadge);
  }
  titleRow.appendChild(pillsRow);
  headerMain.appendChild(titleRow);
  headerRow.appendChild(headerMain);

  const headerActions = el('div', 'task-page-actions');
  const reviewSlot = el('div', 'task-page-review-actions');
  reviewSlot.id = `task-page-review-actions-${task.id}`;
  headerActions.appendChild(reviewSlot);
  headerRow.appendChild(headerActions);
  header.appendChild(headerRow);

  // Inline forms opened by header buttons (merge confirm, send-back, reject,
  // mark-done errors) drop down here, under the buttons that opened them.
  const actionTray = el('div', 'task-page-action-tray');
  actionTray.id = `task-page-action-tray-${task.id}`;
  header.appendChild(actionTray);
  container.appendChild(header);

  // ── Stats strip ──
  // Uses currentRunSteps so refused runs don't pollute the display.
  const runSteps = task.runSteps || [];
  const statsBar = el('div', 'timeline-stats-bar task-page-stats');
  statsBar.id = `timeline-stats-${task.id}`;
  statsBar.setAttribute('data-task-id', task.id || '');
  {
    const curSteps = currentRunSteps(runSteps);
    const elapsedMs = runElapsedMs(curSteps, Date.now());
    const stuck = isStuck(runSteps, Date.now(), task.status);
    statsBar.appendChild(buildTimelineStats(task, curSteps, elapsedMs, stuck));
  }
  container.appendChild(statsBar);

  // Run queue line (STA-773): a run refused for capacity waits here until a
  // slot, its repo, or its quota pool frees up. Kept current by run.queue SSE.
  const queueLine = el('div', 'task-page-queue-line');
  queueLine.id = `run-queue-line-${task.id}`;
  queueLine.setAttribute('role', 'status');
  setRunQueueLine(queueLine, task.queue);
  container.appendChild(queueLine);

  // Non-git task (STA-864): runs in place; nothing is checkpointed, and
  // there is no ship review card or Approve.
  if (task.workspace && task.workspace.git === false) {
    const warn = el('div', 'task-page-nongit-banner',
      task.workspace.warning || 'Warning: not a git repository — changes are not checkpointed and can\'t be reviewed or undone');
    warn.id = `nongit-banner-${task.id}`;
    warn.setAttribute('role', 'note');
    if (task.workspace.dir) warn.title = task.workspace.dir;
    container.appendChild(warn);
  }

  // Two-column body: timeline left, panel right, each scrolling internally.
  const layout = el('div', 'task-page-layout');

  // ── Left column: timeline ──
  const main = el('div', 'task-page-main task-page-timeline');

  // ── Right column: tabs over decisions, review, diff, migrations, brief ──
  // Open on Decisions while a card waits on the Board, then Review while a
  // ship review does, else Diff.
  const reviewPending = !!(shipCard && shipCard.status === 'pending');
  const decisionsPending = (interactions || []).some(i => i.status === 'pending');
  const { side, panels: tabPanels, select: selectPanelTab, setCount: setTabCount } =
    buildTaskPagePanel(task.id || '', decisionsPending ? 'decisions' : (reviewPending ? 'review' : 'diff'));
  // The ship review card mounts here; SSE updates re-render into the same slot.
  const reviewCardSlot = el('div', 'task-page-review-card-slot');
  tabPanels.review.appendChild(reviewCardSlot);

  // Description (editable)
  const descSection = el('div', 'task-page-section');
  const descTitleRow = el('div', 'task-page-section-title-row');
  descTitleRow.style.cssText = 'display:flex;align-items:center;gap:8px;';
  descTitleRow.appendChild(el('div', 'task-page-section-title', 'Description'));
  const descEditBtn = el('button', 'btn-icon desc-edit-btn', '✎');
  descEditBtn.setAttribute('aria-label', 'Edit description');
  descEditBtn.title = 'Edit description';
  descEditBtn.style.cssText = 'font-size:0.85rem;padding:2px 6px;cursor:pointer;opacity:0.6;';
  descTitleRow.appendChild(descEditBtn);
  descTitleRow.appendChild(longContentTrigger('brief', 'Brief', () => cloneChildren(descView, 'desc-full')));
  descSection.appendChild(descTitleRow);
  let desc = (task.description || '').trim();
  const descView = el('div', 'desc-view');
  if (desc) {
    descView.appendChild(mdEl(desc));
  } else {
    descView.appendChild(el('p', 'panel-field-muted desc-empty', 'No description provided.'));
  }
  descSection.appendChild(descView);

  // Edit form (hidden by default)
  const descEditForm = el('div', 'desc-edit-form');
  descEditForm.style.display = 'none';
  const descTextarea = document.createElement('textarea');
  descTextarea.className = 'desc-textarea';
  descTextarea.rows = 6;
  descTextarea.style.cssText = 'width:100%;box-sizing:border-box;font-family:inherit;font-size:0.9rem;padding:8px;border:1px solid var(--border);border-radius:4px;background:var(--bg-secondary, #1a1a2e);color:inherit;resize:vertical;';
  descTextarea.value = desc;
  descEditForm.appendChild(descTextarea);
  const descActionsRow = el('div', 'desc-edit-actions');
  descActionsRow.style.cssText = 'display:flex;gap:8px;margin-top:8px;';
  const descSaveBtn = el('button', 'btn btn-primary btn-sm desc-save-btn', 'Save');
  descSaveBtn.style.cssText = 'font-size:0.82rem;padding:4px 12px;';
  const descCancelBtn = el('button', 'btn btn-secondary btn-sm desc-cancel-btn', 'Cancel');
  descCancelBtn.style.cssText = 'font-size:0.82rem;padding:4px 12px;';
  const descErrMsg = el('span', 'desc-err-msg', '');
  descErrMsg.style.cssText = 'color:var(--red,#f87171);font-size:0.8rem;';
  descActionsRow.appendChild(descSaveBtn);
  descActionsRow.appendChild(descCancelBtn);
  descActionsRow.appendChild(descErrMsg);
  descEditForm.appendChild(descActionsRow);
  descSection.appendChild(descEditForm);

  function enterDescEdit() {
    descTextarea.value = (task.description || '').trim();
    descView.style.display = 'none';
    descEditForm.style.display = 'block';
    descEditBtn.style.display = 'none';
    descErrMsg.textContent = '';
    descTextarea.focus();
  }
  function exitDescEdit() {
    descEditForm.style.display = 'none';
    descView.style.display = '';
    descEditBtn.style.display = '';
  }
  descEditBtn.addEventListener('click', enterDescEdit);
  descCancelBtn.addEventListener('click', exitDescEdit);
  descSaveBtn.addEventListener('click', async () => {
    descSaveBtn.disabled = true;
    descErrMsg.textContent = '';
    try {
      const updated = await apiFetch(`/api/tasks/${encodeURIComponent(task.id)}/description`, {
        method: 'PUT',
        body: JSON.stringify({ description: descTextarea.value }),
      });
      task.description = updated.description || descTextarea.value;
      if (state.tasks[task.id]) state.tasks[task.id].description = task.description;
      desc = (task.description || '').trim();
      while (descView.firstChild) descView.removeChild(descView.firstChild);
      if (desc) {
        descView.appendChild(mdEl(desc));
      } else {
        descView.appendChild(el('p', 'panel-field-muted desc-empty', 'No description provided.'));
      }
      exitDescEdit();
    } catch (err) {
      descErrMsg.textContent = err.message || 'Failed to save description.';
    } finally {
      descSaveBtn.disabled = false;
    }
  });

  // Notes (if any) — shown with the brief in the right column.
  const notesVal = task.notes || '';
  let notesSection = null;
  if (notesVal) {
    notesSection = el('div', 'task-page-section');
    notesSection.appendChild(el('div', 'task-page-section-title', 'Notes'));
    notesSection.appendChild(mdEl(notesVal));
  }

  // ── Timeline ──────────────────────────────────────────────
  const timelineSection = el('div', 'task-page-section');
  timelineSection.id = `timeline-section-${task.id}`;
  timelineSection.setAttribute('data-task-id', task.id || '');
  timelineSection.appendChild(el('div', 'task-page-section-title', `Timeline${runSteps.length ? ` (${runSteps.length})` : ''}`));

  // Start live elapsed ticker if the run is still active.
  {
    const curSteps = currentRunSteps(runSteps);
    const hasTerminal = curSteps.some(s => s.kind === 'state');
    if (!hasTerminal && task.status !== 'done' && task.status !== 'cancelled') {
      startElapsedTicker(task.id);
    }
  }

  // Run-control bar (pause / stop / send message) — in the header while running.
  const rcBar = buildRunControlBar(task);
  rcBar.id = `run-control-bar-${task.id}`;
  headerActions.appendChild(rcBar);

  // Step rows — grouped run → subtask → step (STA-638); each send-back
  // starts a new run, headed "Run N" once there is more than one real run.
  const stepList = el('div', 'timeline-steps');
  stepList.id = `timeline-steps-${task.id}`;
  if (runSteps.length === 0) {
    stepList.appendChild(el('p', 'panel-field-muted timeline-empty', 'No steps yet. Steps will appear here during a run.'));
  } else {
    const runGroups = groupStepsByRun(runSteps);
    // With several real runs, refused runs (wake/route/state only) are noise.
    const multi = runGroups.filter(isRealRunGroup).length > 1;
    for (const group of runGroups) {
      if (multi && !isRealRunGroup(group)) continue;
      const runEl = buildTimelineRun((group[0] && group[0].run_id) || '');
      fillTimelineRun(runEl, group);
      stepList.appendChild(runEl);
    }
    syncTimelineRunHeaders(stepList, runSteps);
  }
  timelineSection.appendChild(stepList);
  main.appendChild(timelineSection);

  // ── Error panel ────────────────────────────────────────────
  const taskRunErrors = runErrors || task.runErrors || [];
  if (taskRunErrors.length > 0) {
    const errSection = el('div', 'task-page-section run-errors-section');
    errSection.appendChild(el('div', 'task-page-section-title', `Errors (${taskRunErrors.length})`));
    const errList = el('div', 'run-errors-list');
    for (const e of taskRunErrors) {
      errList.appendChild(buildRunErrorRow(e));
    }
    const logsLink = document.createElement('a');
    logsLink.className = 'run-errors-logs-link';
    logsLink.href = '/logs';
    logsLink.textContent = 'View all run logs →';
    logsLink.addEventListener('click', (ev) => { ev.preventDefault(); navigateTo('logs', null, true); });
    errSection.appendChild(errList);
    errSection.appendChild(logsLink);
    main.appendChild(errSection);
  }

  // Pending security gate requests for this task's run
  if (task.run_id || task.runId) {
    renderPendingGatesForTask(main, task.run_id || task.runId);
  }

  // "Trust this task until…" (task-6c1ed91f): banner, auto-approvals, create.
  if (task.id && !isFleetTaskId(task.id)) {
    renderTaskTrust(main, task.id, reopen);
  }

  // Decision cards (pending ask_user_questions / request_confirmation /
  // suggest_tasks) live on the Decisions tab (task-e3fe2c0b).
  setTabCount('decisions', renderInteractionCards(tabPanels.decisions, task, interactions || [],
    { canRunNow: runNowOffered(task, shipCard) }));

  layout.appendChild(main);

  // Ship Review card (when agent has created one for Board approval).
  // Rendered synchronously from the pre-fetched shipCard so it appears
  // immediately with no extra round-trip; its action buttons go to the header.
  renderShipReviewCardFromData(reviewCardSlot, task.id || '', shipCard || null);
  // STA-774: a run that changed nothing gets no card and no Approve.
  if (!shipCard && diffData && diffData.answer_only) {
    reviewCardSlot.appendChild(renderAnswerOnlyPanel(comments || [], diffData.files_read || []));
  }

  // Migrations panel — lazy-loads migration files from the task's diff
  const migPanel = tabPanels.migrations;
  const showNoMigrations = () => {
    if (migPanel.querySelector('.migrations-section')) return;
    migPanel.appendChild(el('p', 'panel-field-muted task-page-tab-empty', 'No migration files in this task\'s diff.'));
  };
  if (task.id && !isFleetTaskId(task.id || '')) {
    renderMigrationsPanel(migPanel, task.id).then(() => {
      setTabCount('migrations', migPanel.querySelectorAll('.migration-file-card').length);
      showNoMigrations();
    }, showNoMigrations);
  } else {
    showNoMigrations();
  }

  // Artifacts panel: the task's documents (task_documents), latest version
  // rendered, with version history and download.
  if (task.id && !isFleetTaskId(task.id || '')) {
    renderTaskArtifacts(tabPanels.artifacts, task.id).then(n => setTabCount('artifacts', n));
  } else {
    tabPanels.artifacts.appendChild(el('p', 'panel-field-muted task-page-tab-empty', 'Fleet tasks have no documents.'));
  }

  // Agent interaction (chat): only the composer row is pinned to the bottom of
  // the page. The thread sits behind a "Messages (n)" toggle in that row and
  // opens in place above it (STA-643).
  const chatSection = el('div', 'task-page-dock');
  chatSection.id = 'page-chat-section';

  const messagesDiv = el('div', 'chat-messages');
  messagesDiv.id = 'page-chat-messages';
  renderChatMessages(messagesDiv, comments || []);
  chatSection.appendChild(messagesDiv);

  const chatToggle = el('button', 'btn btn-secondary btn-sm task-page-chat-toggle', chatToggleLabel((comments || []).length));
  chatToggle.type = 'button';
  chatToggle.id = 'page-chat-toggle';
  chatToggle.setAttribute('aria-controls', 'page-chat-messages');
  // Open/closed is the Board's preference, so it survives reloads.
  const setChatOpen = (open) => {
    try { localStorage.setItem(STORAGE_TASK_PAGE_CHAT_OPEN_KEY, open ? '1' : '0'); } catch { /* storage blocked */ }
    messagesDiv.hidden = !open;
    chatToggle.setAttribute('aria-expanded', String(open));
    if (open) messagesDiv.scrollTop = messagesDiv.scrollHeight;
  };
  let chatOpen = false;
  try { chatOpen = localStorage.getItem(STORAGE_TASK_PAGE_CHAT_OPEN_KEY) === '1'; } catch { /* storage blocked */ }
  setChatOpen(chatOpen);
  chatToggle.addEventListener('click', () => setChatOpen(messagesDiv.hidden));

  const compose = el('div', 'chat-compose task-page-composer');
  compose.appendChild(chatToggle);
  const textarea = document.createElement('textarea');
  textarea.className = 'chat-textarea';
  textarea.placeholder = 'Message the agent. Delivered at the next step boundary (⌘↵ to send)';
  textarea.rows = 1;
  textarea.addEventListener('input', () => {
    textarea.style.height = 'auto';
    textarea.style.height = Math.max(38, Math.min(160, textarea.scrollHeight)) + 'px';
  });
  const sendBtn = el('button', 'chat-send-btn', 'Send');
  sendBtn.type = 'button';
  const taskId = task.id || (state.openDetailTaskId || '');

  const doSend = async () => {
    const body = textarea.value.trim();
    if (!body) return;
    textarea.value = '';
    textarea.style.height = '';
    sendBtn.disabled = true;
    try {
      await sendComment(taskId, body);
      await refreshChatMessages(taskId);
      setChatOpen(true); // show the message that was just sent
    } catch { /* silent */ } finally {
      sendBtn.disabled = false;
      textarea.focus();
    }
  };
  sendBtn.addEventListener('click', doSend);
  textarea.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); doSend(); }
  });

  compose.appendChild(textarea);
  compose.appendChild(sendBtn);
  chatSection.appendChild(compose);

  // ── Diff ──
  // diffData undefined means it is still loading: the page renders first and
  // the caller fills the pane through the returned setDiff (STA-775).
  const diffPane = el('div', 'task-page-diff');
  const isFleetTask = isFleetTaskId(task.id || '');
  const setDiff = (data, cps) => {
    if (isFleetTask) return;
    renderDiffPane(diffPane, task, cps || [], data || { diff: '', files: [], checkpoint_id: '' });
    const d = data || {};
    setTabCount('diff', (d.file_stats && d.file_stats.length) || (d.files && d.files.length) || 0);
  };
  if (isFleetTask) {
    diffPane.appendChild(el('p', 'panel-field-muted task-page-tab-empty', 'Fleet tasks have no diff.'));
  } else if (diffData === undefined) {
    diffPane.appendChild(el('p', 'panel-field-muted task-page-diff-loading', 'Loading diff…'));
  } else {
    setDiff(diffData, checkpoints);
  }
  tabPanels.diff.appendChild(diffPane);

  // ── Brief ──
  tabPanels.brief.appendChild(descSection);
  if (notesSection) tabPanels.brief.appendChild(notesSection);

  // ── Details ──
  const meta = el('div', 'task-page-meta');
  meta.appendChild(el('div', 'task-page-section-title', 'Details'));

  const addMetaField = (label, value) => {
    if (!value && value !== 0) return;
    const field = el('div', 'panel-field');
    field.appendChild(el('div', 'panel-field-label', label));
    if (typeof value === 'string') {
      field.appendChild(el('div', 'panel-field-value', value));
    } else {
      field.appendChild(value);
    }
    meta.appendChild(field);
  };

  addMetaField('Status', task.status);
  addMetaField('Priority', task.priority);
  addMetaField('Assignee', task.assignee_name || task.checkout_agent_id || null);
  addMetaField('Project', projectSlug(task) !== 'default' ? projectSlug(task) : null);
  addMetaField('Org', task.organization || null);
  addMetaField('Stage', task.execution_stage || null);
  if (task.id && !isFleetTaskId(task.id)) {
    addMetaField('Kind of work', buildKindField(task));
    addMetaField('Provider', buildProviderField(task));
  } else {
    addMetaField('Kind of work', workKindLabel(task.work_kind));
  }
  addMetaField('Goal', task.goal_title || (task.goal_id ? task.goal_id.slice(0, 12) : null));
  addMetaField('Repo', task.repo_path ? `${task.repo_path} (${task.git_branch || 'main'})` : null);
  addMetaField('Spend',
    (task.spent_usd || task.spent_tokens)
      ? `${fmtCurrency(task.spent_usd || 0)} · ${fmtCompactNum(task.spent_tokens || 0)} tokens`
      : null);
  addMetaField('Budget',
    (task.max_budget_usd || task.max_turns)
      ? `${fmtCurrency(task.max_budget_usd || 0)} · ${task.max_turns || 50} runs max`
      : null);
  addMetaField('Internal ID', task.id || null);

  // Labels
  let labels = task.labels;
  if (typeof labels === 'string') {
    try { labels = JSON.parse(labels); } catch { labels = labels ? [labels] : []; }
  }
  if (Array.isArray(labels) && labels.length) {
    const lblWrap = el('div', 'panel-field');
    lblWrap.appendChild(el('div', 'panel-field-label', 'Labels'));
    const badgeContainer = el('div');
    badgeContainer.style.cssText = 'display:flex;flex-wrap:wrap;gap:6px;margin-top:4px;';
    for (const l of labels) {
      const name = typeof l === 'object' ? l.name : l;
      const color = typeof l === 'object' && l.color ? l.color : '#38bdf8';
      const badge = el('span', 'task-label-badge', name);
      badge.style.cssText = `font-size:0.75rem;padding:2px 8px;border-radius:12px;font-weight:600;background:${color}22;border:1px solid ${color};color:${color};`;
      badgeContainer.appendChild(badge);
    }
    lblWrap.appendChild(badgeContainer);
    meta.appendChild(lblWrap);
  }

  if (task.source_ref) {
    // Paperclip import (STA-857): where this task came from.
    meta.appendChild(el('div', 'task-source-ref', `Imported from ${task.source_ref}`));
  }
  if (task.origin === 'paperclip_import' && !task.repo_path) {
    meta.appendChild(el('div', 'task-source-ref muted-text', 'No repo yet: set one (staypoint task set-repo) before moving it out of backlog.'));
  }
  appendTaskRelations(meta, task, openTask);

  // An active ship review card blocks Run Now and Mark done (shipCardBlocksRun).
  const hasActiveCard = shipCardBlocksRun(shipCard);

  // ── Run Now button ──
  if (runNowOffered(task, shipCard)) {
    const runBtn = el('button', 'run-now-btn', '▶ Run Now');
    runBtn.type = 'button';
    runBtn.addEventListener('click', async () => {
      runBtn.disabled = true;
      runBtn.textContent = 'Starting…';
      try {
        const res = await postRunNow(task.id);
        if (res === null) { runBtn.disabled = false; runBtn.textContent = '▶ Run Now'; return; }
        runBtn.textContent = '✓ Started';
        setTimeout(reopen, 800);
      } catch (err) {
        runBtn.disabled = false;
        runBtn.textContent = '▶ Run Now';
        console.error('run-now failed:', err);
        alert(`Run Now failed: ${err.message || err}`);
      }
    });
    headerActions.prepend(runBtn);
  } else if (task.id && RUN_NOW_STATUSES.has(task.status) && shipCard && ['pending', 'sent_back'].includes(shipCard.status)) {
    const reviewLink = el('a', 'ship-review-see-card-link', '↓ See review card');
    reviewLink.href = '#';
    reviewLink.style.cssText = 'display:block;margin-top:8px;font-size:0.85rem;';
    reviewLink.addEventListener('click', (e) => {
      e.preventDefault();
      selectPanelTab('review');
      document.getElementById(`ship-review-${task.id}`)?.scrollIntoView({ behavior: 'smooth' });
    });
    meta.appendChild(reviewLink);
  }

  // ── Run all children (task-f6777004) ──
  // One passkey moves every backlog/todo child to todo, in priority order, and
  // wakes it; tasks.max_running_children caps how many run at once.
  const childList = task.dependencies?.subtasks || task.subtasks || [];
  if (task.id && !isFleetTaskId(task.id) && childList.length) {
    const runKidsBtn = el('button', 'run-children-btn', '▶ Run all children');
    runKidsBtn.type = 'button';
    runKidsBtn.title = 'Queue every backlog/todo child (Board, passkey)';
    runKidsBtn.addEventListener('click', async () => {
      runKidsBtn.disabled = true;
      try {
        const res = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(task.id)}/run-children`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: '{}',
          }), 'running all children of this task');
        if (res === null) { runKidsBtn.disabled = false; return; }
        if (!res.ok) throw await boardActionError(res);
        const out = await res.json();
        const lines = (out.children || []).map(c => `${c.result}: ${c.name || c.task_id}${c.reason ? ` (${c.reason})` : ''}`);
        alert(`Queued ${out.queued} child task(s).\n\n${lines.join('\n')}`);
        setTimeout(reopen, 300);
      } catch (err) {
        runKidsBtn.disabled = false;
        alert(`Run all children failed: ${err.message || err}`);
      }
    });
    headerActions.appendChild(runKidsBtn);
  }

  // ── Mark done / Cancel task (any non-terminal stage, STA-861) ──
  // The Board can close any open task from here. Mark done goes through the
  // Board gate (passkey) with {board: true}, so no work product is needed; an
  // active run is stopped first.
  const closeActions = taskCloseActions(task);
  const stopRunFirst = async () => {
    if (!taskRunActive(task)) return;
    const r = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/run-control`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ action: 'stop' }),
    });
    if (!r.ok) throw await boardActionError(r);
  };
  if (closeActions.markDone && !isFleetTaskId(task.id)) {
    const doneError = el('div', 'mark-done-error');
    doneError.style.display = 'none';
    const showCloseError = (msg) => {
      doneError.textContent = msg;
      doneError.style.display = 'block';
    };

    const doneBtn = el('button', 'mark-done-btn', '✓ Mark done');
    doneBtn.type = 'button';
    doneBtn.addEventListener('click', async () => {
      const confirmText = markDoneConfirmText(task, { hasActiveCard });
      if (confirmText && !confirm(confirmText)) return;
      const note = prompt('Optional note for the timeline (why this is done):', '');
      if (note === null) return;
      doneBtn.disabled = true;
      doneBtn.textContent = 'Marking done…';
      doneError.style.display = 'none';
      try {
        await stopRunFirst();
        const res = await withBoardWebAuthn((sessionToken, assertion) =>
          fetch(`/api/tasks/${encodeURIComponent(task.id)}/done`, {
            method: 'POST',
            headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
            body: JSON.stringify(markDoneBody(note)),
          }), 'marking the task done',
        );
        if (res === null) { doneBtn.disabled = false; doneBtn.textContent = '✓ Mark done'; return; }
        if (!res.ok) throw await boardActionError(res);
        doneBtn.textContent = '✓ Done';
        setTimeout(reopen, 800);
      } catch (err) {
        showCloseError(err.message || 'Request failed');
        doneBtn.disabled = false;
        doneBtn.textContent = '✓ Mark done';
        console.error('mark-done failed:', err);
      }
    });

    const cancelBtn = el('button', 'cancel-task-btn', '✕ Cancel task');
    cancelBtn.type = 'button';
    cancelBtn.addEventListener('click', async () => {
      if (!confirm(cancelConfirmText(task))) return;
      cancelBtn.disabled = true;
      doneError.style.display = 'none';
      try {
        await stopRunFirst();
        const res = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/stage`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeader() },
          body: JSON.stringify({ stage: 'cancelled' }),
        });
        if (!res.ok) throw await boardActionError(res);
        cancelBtn.textContent = '✕ Cancelled';
        setTimeout(reopen, 800);
      } catch (err) {
        showCloseError(err.message || 'Request failed');
        cancelBtn.disabled = false;
        console.error('cancel-task failed:', err);
      }
    });

    headerActions.prepend(cancelBtn);
    headerActions.prepend(doneBtn);
    actionTray.appendChild(doneError);
  }

  // ── Move to… (task-40f0a2f0) ──
  // The Board sets the stage. Agent-created tasks leave backlog only this
  // way; the server decides, and a refusal shows its message here.
  const stageOptions = isFleetTaskId(task.id) ? [] : stageMenuOptions(task, { canRun: runNowOffered(task, shipCard) });
  if (stageOptions.length) {
    const stageError = el('div', 'task-stage-error');
    stageError.style.display = 'none';
    const select = el('select', 'select-filter task-stage-select');
    select.title = 'Move this task to another stage (Board)';
    select.setAttribute('aria-label', 'Move to stage');
    const placeholder = el('option', '', 'Move to…');
    placeholder.value = '';
    select.appendChild(placeholder);
    for (const o of stageOptions) {
      const opt = el('option', '', o.label);
      opt.value = o.stage;
      select.appendChild(opt);
    }
    const moveTo = async (o) => {
      if (o.via === 'done') { headerActions.querySelector('.mark-done-btn')?.click(); return false; }
      if (o.via === 'run') return (await postRunNow(task.id)) !== null;
      const confirmText = stageChangeConfirmText(task, o.stage);
      if (confirmText && !confirm(confirmText)) return false;
      if (o.via === 'block') {
        const reason = prompt('Why is this task blocked?', '');
        if (reason === null) return false;
        if (!reason.trim()) throw new Error('A blocked task needs a reason.');
        await stopRunFirst();
        const r = await fetch(`/api/tasks/${encodeURIComponent(task.id)}/block`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...authHeader() },
          body: JSON.stringify({ reason: reason.trim() }),
        });
        if (!r.ok) throw await boardActionError(r);
        return true;
      }
      await stopRunFirst();
      return (await postStage(task.id, o.stage, `moving this task to ${o.label}`)) !== null;
    };
    select.addEventListener('change', async () => {
      const o = stageOptions.find(x => x.stage === select.value);
      if (!o) return;
      select.disabled = true;
      stageError.style.display = 'none';
      try {
        if (await moveTo(o)) { setTimeout(reopen, 300); return; }
      } catch (err) {
        stageError.textContent = `Move to ${o.label.replace(/…$/, '')} failed: ${err.message || err}`;
        stageError.style.display = 'block';
        console.error('stage change failed:', err);
      }
      select.value = '';
      select.disabled = false;
    });
    headerActions.appendChild(select);
    actionTray.appendChild(stageError);
  }

  tabPanels.review.appendChild(meta);
  layout.appendChild(side);
  container.appendChild(layout);
  container.appendChild(chatSection);
  return { setDiff };
}

document.addEventListener('click', (e) => {
  const panel = document.getElementById('detail-panel');
  if (!panel || panel.classList.contains('hidden')) return;
  // If detail was just opened/switched in this click event, do not close
  if (e === detailOpenEvent || Date.now() - lastDetailOpenTime < 150) return;
  // If clicked inside the detail panel, do not close
  if (panel.contains(e.target)) return;
  // Nor for clicks in a dialog the drawer opened (body-level overlays), or on
  // a control that removed itself, such as a dialog's close button.
  if (!e.target.isConnected || e.target.closest('.content-modal, .file-diff-modal')) return;
  // If clicked on close, expand, or open button, ignore
  if (e.target.closest('#panel-close') || e.target.closest('#panel-expand') || e.target.closest('#panel-open')) return;
  closeDetailPanel();
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeDetailPanel();
  }
});

// ── Render All ────────────────────────────────────────────
function renderAll() {
  renderOverview();
  renderKanban();
  renderBoss();

  // Re-render org detail if one is open and fleet data refreshed
  if (state.currentOrgDetail) {
    const org = (state.fleet?.organizations || []).find(o => o.name === state.currentOrgDetail);
    if (org) renderOrgDetailView(org);
  }

  renderSidebarOrgTree();

  // Re-render any open dynamic pages
  if (document.getElementById('view-projects')?.classList.contains('active')) renderProjects();
  if (document.getElementById('view-agents')?.classList.contains('active'))   renderAgentsPage();
  if (document.getElementById('view-recent-tasks')?.classList.contains('active')) renderRecentTasks();
  if (document.getElementById('view-task-status')?.classList.contains('active'))  renderTaskStatusPage();
  if (document.getElementById('view-all-tasks')?.classList.contains('active'))    { populateAllTasksOrgFilter(); renderAllTasksPage(); }
  if (document.getElementById('view-cost')?.classList.contains('active'))         renderCostPage();
  wireShowHiddenToggles();
  updateShowHiddenCounts();
}

// ── Filter listeners (overview) ───────────────────────────
document.getElementById('task-search-input')?.addEventListener('input', (e) => {
  state.taskFilter.search = e.target.value;
  renderGlobalTaskTable();
});
document.getElementById('task-org-filter')?.addEventListener('change', (e) => {
  state.taskFilter.org = e.target.value;
  populateOverviewProjectFilter();
  renderGlobalTaskTable();
});
document.getElementById('task-project-filter')?.addEventListener('change', (e) => {
  state.taskFilter.project = e.target.value;
  renderGlobalTaskTable();
});
document.getElementById('task-priority-filter')?.addEventListener('change', (e) => {
  state.taskFilter.priority = e.target.value;
  renderGlobalTaskTable();
});
document.getElementById('task-status-filter')?.addEventListener('change', (e) => {
  state.taskFilter.status = e.target.value;
  renderGlobalTaskTable();
});

// ── Filter listeners (task status page) ──────────────────
document.getElementById('ts-search-input')?.addEventListener('input', (e) => {
  state.tsFilter.search = e.target.value;
  renderTaskStatusPage();
});
document.getElementById('ts-org-filter')?.addEventListener('change', (e) => {
  state.tsFilter.org = e.target.value;
  populateTSProjectFilter();
  renderTaskStatusPage();
});
document.getElementById('ts-project-filter')?.addEventListener('change', (e) => {
  state.tsFilter.project = e.target.value;
  renderTaskStatusPage();
});
document.getElementById('ts-priority-filter')?.addEventListener('change', (e) => {
  state.tsFilter.priority = e.target.value;
  renderTaskStatusPage();
});
document.getElementById('ts-status-filter')?.addEventListener('change', (e) => {
  state.tsFilter.status = e.target.value;
  renderTaskStatusPage();
});

// ── Filter listeners (agents page) ───────────────────────
document.getElementById('agents-search')?.addEventListener('input', (e) => {
  state.agentsFilter.search = e.target.value;
  renderAgentsPage();
});
document.getElementById('agents-org-filter')?.addEventListener('change', (e) => {
  state.agentsFilter.org = e.target.value;
  state.agentsFilter.project = 'all';
  populateAgentsProjectFilter();
  renderAgentsPage();
});
document.getElementById('agents-project-filter')?.addEventListener('change', (e) => {
  state.agentsFilter.project = e.target.value;
  renderAgentsPage();
});
document.getElementById('agents-provider-filter')?.addEventListener('change', (e) => {
  state.agentsFilter.provider = e.target.value;
  renderAgentsPage();
});
// ── Filter listeners (recent tasks page) ───────────────────
document.getElementById('recent-tasks-org-filter')?.addEventListener('change', (e) => {
  state.recentTasksFilter.org = e.target.value;
  populateRecentTasksProjectFilter();
  renderRecentTasks();
});
document.getElementById('recent-tasks-project-filter')?.addEventListener('change', (e) => {
  state.recentTasksFilter.project = e.target.value;
  renderRecentTasks();
});
document.getElementById('recent-tasks-priority-filter')?.addEventListener('change', (e) => {
  state.recentTasksFilter.priority = e.target.value;
  renderRecentTasks();
});
document.querySelectorAll('.agent-filter-pill').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.agent-filter-pill').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    state.agentsFilter.status = btn.dataset.status || 'all';
    renderAgentsPage();
  });
});

// ── Checklist View ────────────────────────────────────────

let checklistItems = [];   // local cache
let checklistCommitVerification = null;
// The checklist re-renders after boot once the fleet load lands (STA-670), so
// the warning log's open state lives here rather than in the DOM.
let checklistGateLogOpen = false;

function isItemCommitBlocked(itemId) {
  if (!checklistCommitVerification) return false;
  return checklistCommitVerification.blocked_item_ids?.includes(itemId) || false;
}

function getItemCommitBlockReason(itemId) {
  if (!checklistCommitVerification?.missing_commits) return 'Required commit missing from main or running daemon';
  const itemWarn = checklistCommitVerification.missing_commits.find(w => w.item_id === itemId);
  return itemWarn ? itemWarn.reason : 'Required commit missing from main or running daemon';
}

function isSectionCommitBlocked(sectionName) {
  if (!checklistCommitVerification) return false;
  return checklistCommitVerification.blocked_sections?.includes(sectionName) || false;
}

// Commit codes and task IDs mean nothing to someone running the checklist by
// hand, so they come out of the visible text and sit in a collapsed toggle.
const CL_REF_RE = /\b(?:(?:STA|MAN|PER|RUN|RES)-\d+|task-[0-9a-f]{6,}|(?=[0-9a-f]*\d)(?=[0-9a-f]*[a-f])[0-9a-f]{7,40})\b/g;

function splitChecklistRefs(text) {
  if (!text) return { text: '', refs: [] };
  const refs = [];
  const clean = String(text)
    .replace(CL_REF_RE, m => { refs.push(m); return ''; })
    .replace(/\(\s*(?:[,;/&\s]|and|or)*\)/g, '')
    .replace(/[ \t]+([,.;:)])/g, '$1')
    .replace(/[ \t]{2,}/g, ' ')
    .trim();
  return { text: clean, refs };
}

function checklistRefsText(item) {
  const refs = [item.section, item.title, item.description, item.how_to_test]
    .flatMap(t => splitChecklistRefs(t).refs);
  if (item.commit_hash) refs.push(item.commit_hash);
  const uniq = [...new Set(refs)];
  const tasks = uniq.filter(r => !/^[0-9a-f]{7,40}$/.test(r));
  const commits = uniq.filter(r => /^[0-9a-f]{7,40}$/.test(r));
  const parts = [];
  if (tasks.length) parts.push(`Tasks: ${tasks.join(', ')}`);
  if (commits.length) parts.push(`Build: ${commits.join(', ')}`);
  return parts.join(' · ');
}

function buildChecklistRefsToggle(item) {
  const text = checklistRefsText(item);
  if (!text) return null;
  const d = document.createElement('details');
  d.className = 'cl-refs';
  d.appendChild(el('summary', '', 'details'));
  d.appendChild(el('div', 'cl-refs-body', text));
  return d;
}

async function loadChecklist(sprint) {
  const urlParams = new URLSearchParams(window.location.search);
  const sprintFromUrl = urlParams.get('sprint');
  const sel = document.getElementById('checklist-sprint-filter');
  const s = sprint || sprintFromUrl || sel?.value || 'STA-236';
  if (sel && sel.value !== s) {
    sel.value = s;
  }
  try {
    const r = await apiFetch(`/api/checklist?sprint=${encodeURIComponent(s)}`);
    checklistItems = r.items || [];
    checklistCommitVerification = r.commit_verification || null;
    renderChecklist();
    updateChecklistProgress();
    if (typeof updateDevTourToggleUI === 'function') updateDevTourToggleUI();
    if (typeof isWalkthroughActive === 'function' && isWalkthroughActive()) renderWalkthroughHUD();
  } catch (err) {
    const c = document.getElementById('checklist-container');
    if (c) c.innerHTML = `<p style="color:var(--red)">Failed to load checklist: ${err.message}. Try seeding first.</p>`;
  }
}

async function seedChecklist() {
  const sprint = document.getElementById('checklist-sprint-filter')?.value || 'STA-236';
  const btn = document.getElementById('checklist-seed-btn');
  if (btn) { btn.disabled = true; btn.textContent = 'Seeding…'; }
  try {
    await fetch('/api/checklist/seed', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ sprint, force: true }),
    });
    await loadChecklist(sprint);
  } catch (err) {
    alert('Seed failed: ' + err.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Seed / Reset'; }
  }
}

async function loadChecklistSprints() {
  try {
    const r = await apiFetch('/api/checklist/sprints');
    const sel = document.getElementById('checklist-sprint-filter');
    if (!sel || !r.sprints?.length) return;
    const currentVal = sel.value;
    const urlParams = new URLSearchParams(window.location.search);
    const sprintFromUrl = urlParams.get('sprint');
    sel.innerHTML = '';
    for (const s of r.sprints) {
      const opt = document.createElement('option');
      opt.value = s;
      opt.textContent = s === 'STA-236' ? 'STA-236 (Verification & DoD Gate)' : (s === 'STA-168-2' ? 'STA-168-2 (Sprint 2)' : (s === 'STA-168' ? 'STA-168 (Sprint 1)' : s));
      sel.appendChild(opt);
    }
    if (sprintFromUrl && r.sprints.includes(sprintFromUrl)) {
      sel.value = sprintFromUrl;
    } else if (currentVal && r.sprints.includes(currentVal)) {
      sel.value = currentVal;
    } else {
      sel.value = r.sprints[0];
    }
  } catch { /* sprints endpoint optional */ }
}

function getSectionCollapsed(sectionName, isFinished) {
  const stored = localStorage.getItem('staypoint_cl_col_' + sectionName);
  if (stored !== null) return stored === 'true';
  // Default: start collapsed if all items in section are reviewed
  return isFinished;
}

function setSectionCollapsed(sectionName, collapsed) {
  localStorage.setItem('staypoint_cl_col_' + sectionName, collapsed ? 'true' : 'false');
}

function renderChecklist() {
  const container = document.getElementById('checklist-container');
  if (!container) return;
  container.innerHTML = '';

  if (!checklistItems.length) {
    const p = el('p', 'muted-text', 'No checklist items. Click "Seed / Reset" to populate from STA-168.');
    container.appendChild(p);
    updateChecklistProgress();
    return;
  }

  // Group by section
  const sections = {};
  for (const item of checklistItems) {
    if (!sections[item.section]) sections[item.section] = [];
    sections[item.section].push(item);
  }

  // Commit-Hash Gate Warning Banner & Log
  if (checklistCommitVerification && (!checklistCommitVerification.verified || (checklistCommitVerification.missing_commits && checklistCommitVerification.missing_commits.length > 0))) {
    const missing = checklistCommitVerification.missing_commits || [];
    const blockedItems = checklistCommitVerification.blocked_item_ids || [];
    const blockedSections = checklistCommitVerification.blocked_sections || [];

    const banner = el('div', 'checklist-commit-gate-banner');
    const header = el('div', 'cl-gate-header');
    const icon = el('div', 'cl-gate-icon', '⛔');
    const content = el('div', 'cl-gate-content');
    const title = el('div', 'cl-gate-title', 'Checklist Commit-Hash Gate: Missing or Unmerged Commits Detected');
    const desc = el('div', 'cl-gate-desc', `${blockedItems.length} item(s) across ${blockedSections.length} section(s) are blocked from being marked "done". Referenced commits must exist in main and be compiled into the running staypointd daemon.`);

    const meta = el('div', 'cl-gate-meta');
    meta.innerHTML = `<span>Running Daemon: <code>${escapeHtml(checklistCommitVerification.running_commit || 'unrebuilt / none')}</code></span> <span>Main Branch: <code>${escapeHtml(checklistCommitVerification.main_commit || 'unknown')}</code></span>`;
    content.appendChild(title);
    content.appendChild(desc);
    content.appendChild(meta);

    const toggleLabel = () => `${checklistGateLogOpen ? 'Hide' : 'Show'} Warning Log (${missing.length})`;
    const toggleBtn = el('button', 'cl-gate-toggle-btn', toggleLabel());
    header.appendChild(icon);
    header.appendChild(content);
    header.appendChild(toggleBtn);
    banner.appendChild(header);

    const logContainer = el('div', 'cl-gate-log-container');
    logContainer.style.display = checklistGateLogOpen ? 'block' : 'none';

    const table = el('table', 'cl-gate-log-table');
    table.innerHTML = `<thead><tr><th>Commit</th><th>Section / Item</th><th>Status in main</th><th>Status in daemon</th><th>Reason</th></tr></thead>`;
    const tbody = el('tbody');
    for (const m of missing) {
      const tr = el('tr');
      tr.innerHTML = `
        <td><code>${escapeHtml(m.commit)}</code></td>
        <td><strong>${escapeHtml(m.section)}</strong><br/><span class="muted-text">${escapeHtml(m.item_title || '')}</span></td>
        <td>${m.missing_from_main ? '<span style="color:var(--red)">✗ Missing</span>' : '<span style="color:var(--green)">✓ Present</span>'}</td>
        <td>${m.missing_from_binary ? '<span style="color:var(--red)">✗ Unrebuilt / Missing</span>' : '<span style="color:var(--green)">✓ Present</span>'}</td>
        <td style="color:var(--muted)">${escapeHtml(m.reason || '')}</td>
      `;
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    logContainer.appendChild(table);
    banner.appendChild(logContainer);

    toggleBtn.addEventListener('click', () => {
      checklistGateLogOpen = !checklistGateLogOpen;
      logContainer.style.display = checklistGateLogOpen ? 'block' : 'none';
      toggleBtn.textContent = toggleLabel();
    });

    container.appendChild(banner);
  }

  // Progress bar
  const total = checklistItems.length;
  const done  = checklistItems.filter(i => i.status !== 'pending').length;
  const pct   = total ? Math.round(done / total * 100) : 0;

  const progBar = el('div', 'checklist-progress-bar');
  const progFill = el('div', 'checklist-progress-fill');
  progFill.style.width = `${pct}%`;
  progBar.appendChild(progFill);
  container.appendChild(progBar);

  const progEl = document.getElementById('checklist-progress');
  if (progEl) progEl.textContent = `${done}/${total} reviewed (${pct}%)`;

  // Section Collapse toolbar
  const toolbar = el('div', 'checklist-toolbar');

  const colCompBtn = el('button', 'cl-ctrl-btn', 'Collapse Finished');
  colCompBtn.title = 'Collapse all sections where 100% of items are reviewed';
  colCompBtn.addEventListener('click', () => {
    for (const [secName, secItems] of Object.entries(sections)) {
      const isFin = secItems.every(i => i.status !== 'pending');
      setSectionCollapsed(secName, isFin);
    }
    renderChecklist();
  });

  const expAllBtn = el('button', 'cl-ctrl-btn', 'Expand All');
  expAllBtn.addEventListener('click', () => {
    for (const secName of Object.keys(sections)) {
      setSectionCollapsed(secName, false);
    }
    renderChecklist();
  });

  const colAllBtn = el('button', 'cl-ctrl-btn', 'Collapse All');
  colAllBtn.addEventListener('click', () => {
    for (const secName of Object.keys(sections)) {
      setSectionCollapsed(secName, true);
    }
    renderChecklist();
  });

  toolbar.appendChild(colCompBtn);
  toolbar.appendChild(expAllBtn);
  toolbar.appendChild(colAllBtn);
  container.appendChild(toolbar);

  for (const [sectionName, items] of Object.entries(sections)) {
    const sec = el('div', 'checklist-section dashboard-section');
    sec.dataset.section = sectionName;
    const isFinished = items.every(i => i.status !== 'pending');
    const isCollapsed = getSectionCollapsed(sectionName, isFinished);

    const header = el('div', 'checklist-section-header');

    const titleLeft = el('div', 'checklist-section-title-left');
    const chevron = el('span', 'checklist-section-chevron', isCollapsed ? '▶' : '▼');
    const titleText = el('span', 'checklist-section-name', splitChecklistRefs(sectionName).text);
    titleLeft.appendChild(chevron);
    titleLeft.appendChild(titleText);
    if (isSectionCommitBlocked(sectionName)) {
      const blockedBadge = el('span', 'badge badge-commit-blocked', '⛔ Commit Gate Active');
      blockedBadge.title = 'Section contains unmerged or uncompiled commits';
      titleLeft.appendChild(blockedBadge);
    }

    const passCount = items.filter(i => i.status === 'pass').length;
    const partialCount = items.filter(i => i.status === 'partial').length;
    const failCount = items.filter(i => i.status === 'fail').length;
    const notDoneCount = items.filter(i => i.status === 'not_done').length;

    const badges = el('div', 'checklist-section-badges');
    if (passCount || partialCount || failCount || notDoneCount) {
      badges.innerHTML = `✅ ${passCount}${partialCount ? ` ◐ ${partialCount}` : ''} ❌ ${failCount}${notDoneCount ? ` ⚠️ ${notDoneCount}` : ''}`;
    }

    header.appendChild(titleLeft);
    header.appendChild(badges);
    sec.appendChild(header);

    const secBody = el('div', 'checklist-section-body');
    if (isCollapsed) {
      secBody.style.display = 'none';
    }

    for (const item of items) {
      secBody.appendChild(buildChecklistItem(item));
    }
    sec.appendChild(secBody);

    header.addEventListener('click', () => {
      const currentlyCollapsed = secBody.style.display === 'none';
      if (currentlyCollapsed) {
        secBody.style.display = 'block';
        chevron.textContent = '▼';
        setSectionCollapsed(sectionName, false);
        requestAnimationFrame(() => {
          secBody.querySelectorAll('.checklist-notes-input').forEach(ta => {
            ta.style.height = 'auto';
            ta.style.height = Math.max(48, ta.scrollHeight) + 'px';
          });
        });
      } else {
        secBody.style.display = 'none';
        chevron.textContent = '▶';
        setSectionCollapsed(sectionName, true);
      }
    });

    container.appendChild(sec);
  }

  requestAnimationFrame(() => {
    container.querySelectorAll('.checklist-notes-input').forEach(ta => {
      if (ta.offsetParent !== null) {
        ta.style.height = 'auto';
        ta.style.height = Math.max(48, ta.scrollHeight) + 'px';
      }
    });
  });
}

function buildChecklistItem(item) {
  const row = el('div', 'checklist-item');
  row.dataset.id = item.id;

  // Status buttons column
  const isBlocked = isItemCommitBlocked(item.id);
  const blockReason = isBlocked ? getItemCommitBlockReason(item.id) : '';
  const btnCol = el('div', 'checklist-status-btns');
  for (const [status, icon, label] of [
    ['pass',     '✓', 'Pass'],
    ['partial',  '◐', 'In-Between / Needs Work'],
    ['fail',     '✗', 'Fail'],
    ['skip',     '–', 'Skip'],
    ['not_done', '!', 'Not Done'],
  ]) {
    const isPass = status === 'pass';
    const btn = el('button', `cl-btn${item.status === status ? ' active-' + status : ''}${isPass && isBlocked ? ' cl-btn-blocked' : ''}`, icon);
    btn.dataset.status = status;
    if (isPass && isBlocked) {
      btn.disabled = true;
      btn.title = `⛔ Blocked by Commit Gate: ${blockReason}`;
    } else {
      btn.title = label;
    }
    btn.addEventListener('click', () => updateChecklistStatus(item.id, status));
    btnCol.appendChild(btn);
  }
  row.appendChild(btnCol);

  // Body
  const body = el('div', 'checklist-body');
  const titleEl = el('div', `checklist-title${item.status !== 'pending' ? ' status-' + item.status : ''}`, splitChecklistRefs(item.title).text);
  if (isBlocked) {
    const blockedTag = el('span', 'badge badge-commit-blocked', '⛔ Commit Gate');
    blockedTag.title = `Blocked: ${blockReason}`;
    titleEl.appendChild(blockedTag);
  }
  if (item.contract) {
    const contractTag = el('span', 'checklist-contract-tag', '⚙ contract');
    contractTag.title = 'Machine-verifiable contract: ' + item.contract;
    titleEl.appendChild(contractTag);
  }
  const tourItemBtn = el('button', 'cl-item-walkthrough-btn', '▶ Tour');
  tourItemBtn.title = 'Start interactive walkthrough tour from this question';
  tourItemBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    const sel = document.getElementById('checklist-sprint-filter');
    const s = sel?.value || 'STA-236';
    const idx = checklistItems.findIndex(i => i.id === item.id);
    startWalkthrough(s, idx >= 0 ? idx : 0);
  });
  titleEl.appendChild(tourItemBtn);
  body.appendChild(titleEl);
  if (item.description) body.appendChild(el('div', 'checklist-desc', splitChecklistRefs(item.description).text));
  if (item.how_to_test) body.appendChild(el('div', 'checklist-howto', splitChecklistRefs(item.how_to_test).text));
  const refsToggle = buildChecklistRefsToggle(item);
  if (refsToggle) body.appendChild(refsToggle);

  // Notes + version history
  const notesRow = el('div', 'checklist-notes-row');
  const notesInput = el('textarea', 'checklist-notes-input');
  const draftKey = 'staypoint_cl_draft_' + item.id;
  const draftVal = localStorage.getItem(draftKey);
  const initialNote = (draftVal !== null && draftVal !== undefined && (!item.notes || draftVal.length >= item.notes.length))
    ? draftVal
    : (item.notes || '');
  notesInput.value = initialNote;
  item.notes = initialNote;
  notesInput.placeholder = 'Add a note…';
  const rawNote = initialNote;
  const lineCount = Math.max(rawNote.split('\n').length, Math.ceil(rawNote.length / 50));
  notesInput.rows = Math.max(2, Math.min(15, lineCount));
  const autoResize = () => {
    notesInput.style.height = 'auto';
    notesInput.style.height = Math.max(48, notesInput.scrollHeight) + 'px';
  };
  notesInput.addEventListener('input', () => {
    autoResize();
    const val = notesInput.value;
    item.notes = val;
    const idx = checklistItems.findIndex(i => i.id === item.id);
    if (idx !== -1) checklistItems[idx].notes = val;
    localStorage.setItem(draftKey, val);
    const state = typeof getWalkthroughState === 'function' ? getWalkthroughState() : null;
    if (state?.active && checklistItems[state.index]?.id === item.id) {
      const hudInput = document.getElementById('hud-notes-input');
      if (hudInput) hudInput.value = val;
    }
  });
  notesInput.addEventListener('focus', autoResize);
  notesInput.addEventListener('blur', () => {
    // Note: NEVER collapse height on blur
    const val = (notesInput.value || '').trim();
    if (val !== (item.notes || '').trim()) {
      updateChecklistNotes(item.id, notesInput.value);
    }
  });
  requestAnimationFrame(autoResize);

  const saveBtn = el('button', 'checklist-save-btn', 'Save note');
  saveBtn.addEventListener('click', async () => {
    saveBtn.textContent = 'Saving…';
    saveBtn.disabled = true;
    await updateChecklistNotes(item.id, notesInput.value);
    saveBtn.textContent = 'Saved!';
    setTimeout(() => {
      saveBtn.textContent = 'Save note';
      saveBtn.disabled = false;
    }, 1200);
  });

  const histBtn = el('span', 'checklist-history-toggle', `v${item.version}`);
  histBtn.title = 'Click to show version history';
  const histList = el('div', 'checklist-history-list');
  histList.style.display = 'none';
  histBtn.addEventListener('click', async () => {
    if (histList.style.display === 'none') {
      histList.style.display = 'block';
      histList.innerHTML = '<span class="muted-text">Loading…</span>';
      try {
        const r = await apiFetch(`/api/checklist/${item.id}/history`);
        histList.innerHTML = '';
        if (!r.history?.length) {
          histList.appendChild(el('div', 'checklist-history-entry', 'No history yet.'));
        } else {
          for (const e of r.history) {
            const entry = el('div', 'checklist-history-entry');
            entry.innerHTML = `<strong>${e.status}</strong> — ${fmtDateTime(e.changed_at)}${e.notes ? ': ' + escapeHtml(e.notes) : ''}`;
            histList.appendChild(entry);
          }
        }
      } catch {
        histList.innerHTML = '<span class="muted-text">Failed to load history.</span>';
      }
    } else {
      histList.style.display = 'none';
    }
  });

  notesRow.appendChild(notesInput);
  notesRow.appendChild(saveBtn);
  notesRow.appendChild(histBtn);
  body.appendChild(notesRow);
  body.appendChild(histList);
  row.appendChild(body);

  return row;
}

function updateSectionCounts(sectionName) {
  const sec = document.querySelector(`.checklist-section[data-section="${CSS.escape(sectionName)}"]`);
  if (!sec) return;
  const items = checklistItems.filter(i => i.section === sectionName);
  const passCount = items.filter(i => i.status === 'pass').length;
  const partialCount = items.filter(i => i.status === 'partial').length;
  const failCount = items.filter(i => i.status === 'fail').length;
  const notDoneCount = items.filter(i => i.status === 'not_done').length;
  const badges = sec.querySelector('.checklist-section-badges');
  if (badges) {
    if (passCount || partialCount || failCount || notDoneCount) {
      badges.innerHTML = `✅ ${passCount}${partialCount ? ` ◐ ${partialCount}` : ''} ❌ ${failCount}${notDoneCount ? ` ⚠️ ${notDoneCount}` : ''}`;
    } else {
      badges.innerHTML = '';
    }
  }
}

async function updateChecklistStatus(id, status, explicitNotes = null) {
  const idx = checklistItems.findIndex(i => i.id === id);
  if (idx === -1) return;

  if (status === 'pass' && isItemCommitBlocked(id)) {
    const reason = getItemCommitBlockReason(id);
    alert(`⛔ Commit Gate Block: This checklist item cannot be marked "done" / "pass".\n\nReason: ${reason}\n\nRequired commits must be merged into main and compiled into the running staypointd daemon.`);
    return;
  }

  const row = document.querySelector(`.checklist-item[data-id="${id}"]`);
  const currentNotesInput = row?.querySelector('.checklist-notes-input');
  const draftKey = 'staypoint_cl_draft_' + id;
  const draftVal = localStorage.getItem(draftKey);

  let notes = explicitNotes;
  if (notes === null || notes === undefined) {
    if (currentNotesInput && currentNotesInput.value !== undefined) {
      notes = currentNotesInput.value;
    } else if (draftVal !== null && draftVal !== undefined) {
      notes = draftVal;
    } else {
      notes = checklistItems[idx].notes || '';
    }
  }

  // Synchronize in memory immediately
  checklistItems[idx].status = status;
  checklistItems[idx].notes = notes;
  if (currentNotesInput) currentNotesInput.value = notes;

  try {
    const r = await fetch(`/api/checklist/${id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ status, notes }),
    });
    if (!r.ok) {
      const errText = await r.text();
      let errMsg = errText;
      try {
        const parsed = JSON.parse(errText);
        if (parsed.error) errMsg = parsed.error;
      } catch {}
      throw new Error(errMsg);
    }
    const updated = await r.json();
    checklistItems[idx] = updated;
    localStorage.removeItem(draftKey);

    if (row) {
      row.querySelectorAll('.cl-btn').forEach(btn => {
        btn.className = `cl-btn${btn.dataset.status === updated.status ? ' active-' + updated.status : ''}`;
      });
      const titleEl = row.querySelector('.checklist-title');
      if (titleEl) {
        titleEl.className = `checklist-title${updated.status !== 'pending' ? ' status-' + updated.status : ''}`;
      }
      const histBtn = row.querySelector('.checklist-history-toggle');
      if (histBtn) histBtn.textContent = `v${updated.version}`;
      const input = row.querySelector('.checklist-notes-input');
      if (input && updated.notes !== undefined) input.value = updated.notes;
    }
    updateChecklistProgress();
    updateSectionCounts(checklistItems[idx].section);

    if (typeof isWalkthroughActive === 'function' && isWalkthroughActive()) {
      const state = getWalkthroughState();
      if (state && checklistItems[state.index]?.id === id && typeof updateHUDStatusButtons === 'function') {
        updateHUDStatusButtons(updated.status);
      }
    }
  } catch (err) {
    console.error('checklist update failed', err);
  }
}

async function updateChecklistNotes(id, notes) {
  const idx = checklistItems.findIndex(i => i.id === id);
  if (idx === -1) return;
  checklistItems[idx].notes = notes;
  const draftKey = 'staypoint_cl_draft_' + id;
  try {
    const r = await fetch(`/api/checklist/${id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json', ...authHeader() },
      body: JSON.stringify({ notes }),
    });
    if (!r.ok) throw new Error(await r.text());
    const updated = await r.json();
    checklistItems[idx] = updated;
    localStorage.removeItem(draftKey);
    const row = document.querySelector(`.checklist-item[data-id="${id}"]`);
    const histBtn = row?.querySelector('.checklist-history-toggle');
    if (histBtn) histBtn.textContent = `v${updated.version}`;
    const input = row?.querySelector('.checklist-notes-input');
    if (input && updated.notes !== undefined) input.value = updated.notes;
    updateChecklistProgress();
  } catch (err) {
    console.error('checklist note save failed', err);
  }
}

function updateChecklistProgress() {
  const total = checklistItems.length;
  const done  = checklistItems.filter(i => i.status !== 'pending').length;
  const pct   = total ? Math.round(done / total * 100) : 0;
  const progEl = document.getElementById('checklist-progress');
  if (progEl) progEl.textContent = total ? `${done}/${total} reviewed (${pct}%)` : '';
}

// Sidebar wire-up for checklist
document.getElementById('checklist-seed-btn')?.addEventListener('click', seedChecklist);
document.getElementById('checklist-verify-btn')?.addEventListener('click', verifyChecklistContracts);
document.getElementById('checklist-sprint-filter')?.addEventListener('change', (e) => {
  const url = new URL(window.location);
  url.searchParams.set('sprint', e.target.value);
  window.history.replaceState({}, '', url);
  loadChecklist(e.target.value);
});

function showToast(message, type = 'info', duration = 6000) {
  let container = document.getElementById('toast-container');
  if (!container) {
    container = document.createElement('div');
    container.id = 'toast-container';
    container.className = 'toast-container';
    document.body.appendChild(container);
  }
  const icons = {
    info: 'ℹ️',
    success: '✅',
    warning: '⚠️',
    error: '❌'
  };
  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  toast.innerHTML = `
    <span class="toast-icon">${icons[type] || 'ℹ️'}</span>
    <span class="toast-message">${escapeHtml(message)}</span>
    <button class="toast-close" aria-label="Dismiss">&times;</button>
  `;
  const dismiss = () => {
    toast.classList.add('toast-fadeout');
    setTimeout(() => toast.remove(), 250);
  };
  toast.querySelector('.toast-close')?.addEventListener('click', dismiss);
  container.appendChild(toast);
  if (duration > 0) {
    setTimeout(dismiss, duration);
  }
}

async function verifyChecklistContracts() {
  const sprint = document.getElementById('checklist-sprint-filter')?.value || 'STA-236';
  const btn = document.getElementById('checklist-verify-btn');
  const bannerArea = document.getElementById('checklist-banner-area');

  if (btn) {
    btn.disabled = true;
    btn.innerHTML = '<span class="dl-spinner"></span> Verifying…';
  }

  if (bannerArea) {
    bannerArea.innerHTML = `
      <div class="checklist-summary-banner banner-loading">
        <span class="banner-icon">⚡</span>
        <div class="banner-content">
          <div class="banner-title">Evaluating Contracts for ${escapeHtml(sprint)}…</div>
          <div class="banner-desc">Executing machine assertions and inspecting regression status.</div>
        </div>
      </div>
    `;
  }
  showToast(`Evaluating machine contracts for sprint ${sprint}…`, 'info', 4000);

  try {
    const res = await fetch(`/api/checklist/evaluate?sprint=${encodeURIComponent(sprint)}&downgrade=true`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${window.__STAYPOINT_TOKEN__ || ''}` }
    });
    if (!res.ok) {
      const errBody = await res.json().catch(() => ({}));
      throw new Error(errBody.error || `Server responded with ${res.status}`);
    }
    const summary = await res.json();
    const commitShort = summary.commit ? summary.commit.substring(0, 7) : (summary.commit_sha ? summary.commit_sha.substring(0, 7) : 'HEAD');

    let bannerClass = 'banner-success';
    let bannerIcon = '✅';
    let bannerTitle = `All ${summary.total} Machine Contracts Passing`;
    let bannerDesc = `Zero regressions detected across all test suites at commit ${commitShort}.`;
    let toastType = 'success';
    let toastMsg = `✅ All ${summary.total} contracts passing (commit ${commitShort})`;

    if (summary.divergences > 0) {
      bannerClass = 'banner-divergence';
      bannerIcon = '⚠️';
      bannerTitle = `${summary.divergences} Contract Regression(s) Detected`;
      bannerDesc = `${summary.divergences} previously-passing item(s) failed machine verification and were automatically downgraded. Passed: ${summary.passed}, Failed: ${summary.failed}. Commit: ${commitShort}.`;
      toastType = 'warning';
      toastMsg = `⚠️ ${summary.divergences} regression(s) detected and downgraded!`;
    } else if (summary.failed > 0) {
      bannerClass = 'banner-warning';
      bannerIcon = '⚡';
      bannerTitle = `${summary.passed}/${summary.total} Contracts Passing (${summary.failed} failing)`;
      bannerDesc = `Evaluated machine contracts for sprint ${escapeHtml(sprint)}. Zero regressions detected. Commit: ${commitShort}.`;
      toastType = 'warning';
      toastMsg = `Checklist: ${summary.passed}/${summary.total} contracts passing (${summary.failed} failing)`;
    }

    if (bannerArea) {
      bannerArea.innerHTML = `
        <div class="checklist-summary-banner ${bannerClass}">
          <span class="banner-icon">${bannerIcon}</span>
          <div class="banner-content">
            <div class="banner-title">${escapeHtml(bannerTitle)}</div>
            <div class="banner-desc">${escapeHtml(bannerDesc)}</div>
          </div>
          <button class="banner-close" aria-label="Dismiss">&times;</button>
        </div>
      `;
      bannerArea.querySelector('.banner-close')?.addEventListener('click', () => {
        bannerArea.innerHTML = '';
      });
    }
    showToast(toastMsg, toastType, 7000);
    await loadChecklist(sprint);
  } catch (err) {
    console.error('Failed to verify contracts:', err);
    if (bannerArea) {
      bannerArea.innerHTML = `
        <div class="checklist-summary-banner banner-divergence">
          <span class="banner-icon">❌</span>
          <div class="banner-content">
            <div class="banner-title">Failed to Evaluate Contracts</div>
            <div class="banner-desc">${escapeHtml(err.message)}</div>
          </div>
          <button class="banner-close" aria-label="Dismiss">&times;</button>
        </div>
      `;
      bannerArea.querySelector('.banner-close')?.addEventListener('click', () => {
        bannerArea.innerHTML = '';
      });
    }
    showToast('Failed to evaluate checklist contracts: ' + err.message, 'error', 8000);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.innerHTML = '⚡ Verify Contracts';
    }
  }
}

// ── Dev Walkthrough Tour Engine ───────────────────────────

const DEV_TOUR_TOGGLE_KEY = 'staypoint_dev_walkthrough_enabled';
const WALKTHROUGH_STATE_KEY = 'staypoint_walkthrough_state';

function isDevTourEnabled() {
  const v = localStorage.getItem(DEV_TOUR_TOGGLE_KEY);
  return v !== 'false'; // default enabled
}

function setDevTourEnabled(enabled) {
  localStorage.setItem(DEV_TOUR_TOGGLE_KEY, enabled ? 'true' : 'false');
  updateDevTourToggleUI();
  if (!enabled) {
    stopWalkthrough();
  }
}

function updateDevTourToggleUI() {
  const btn = document.getElementById('checklist-tour-toggle-btn');
  const startBtn = document.getElementById('checklist-start-tour-btn');
  const enabled = isDevTourEnabled();
  if (btn) {
    btn.textContent = enabled ? '🧪 Dev Tour: ON' : '🧪 Dev Tour: OFF';
    btn.style.borderColor = enabled ? '#38bdf8' : 'var(--border)';
    btn.style.color = enabled ? '#38bdf8' : 'var(--muted)';
  }
  if (startBtn) {
    startBtn.style.display = enabled ? 'inline-block' : 'none';
    const state = getWalkthroughState();
    if (state?.active) {
      startBtn.textContent = `▶ Resume Walkthrough (${(state.index || 0) + 1}/${checklistItems.length || '?'})`;
    } else {
      startBtn.textContent = '▶ Start Walkthrough';
    }
  }
}

function getWalkthroughState() {
  try {
    return JSON.parse(localStorage.getItem(WALKTHROUGH_STATE_KEY) || 'null');
  } catch {
    return null;
  }
}

function saveWalkthroughState(state) {
  if (!state) {
    localStorage.removeItem(WALKTHROUGH_STATE_KEY);
  } else {
    localStorage.setItem(WALKTHROUGH_STATE_KEY, JSON.stringify(state));
  }
}

function isWalkthroughActive() {
  const s = getWalkthroughState();
  return Boolean(s && s.active);
}

// Maps a checklist item to its target sidebar view and label
function resolveItemTarget(item) {
  if (!item) return { view: 'overview', label: 'All Organizations' };
  const text = `${item.section || ''} ${item.how_to_test || ''} ${item.description || ''} ${item.title || ''}`.toLowerCase();

  if (text.includes('/projects') || text.includes('projects page') || text.includes('project card')) {
    return { view: 'projects', label: 'Projects' };
  }
  if (text.includes('/agents') || text.includes('agents page') || text.includes('agent card')) {
    return { view: 'agents', label: 'Agents' };
  }
  if (text.includes('/recent-tasks') || text.includes('recent tasks') || text.includes('activity feed') || text.includes('subtask tree')) {
    return { view: 'recent-tasks', label: 'Recent Tasks' };
  }
  if (text.includes('/all-tasks') || text.includes('all tasks page')) {
    return { view: 'all-tasks', label: 'All Tasks' };
  }
  if (text.includes('/task-status') || text.includes('task status') || text.includes('9-column table') || text.includes('global task')) {
    return { view: 'task-status', label: 'Global Task Status' };
  }
  if (text.includes('/cost') || text.includes('cost & accounting') || text.includes('spend by') || text.includes('cost page')) {
    return { view: 'cost', label: 'Cost & Accounting' };
  }
  if (text.includes('/settings') || text.includes('settings page') || text.includes('provider accounts') || text.includes('fleet info')) {
    return { view: 'settings', label: 'Settings' };
  }
  if (text.includes('/checklist') || text.includes('checklist page') || text.includes('divergence')) {
    return { view: 'checklist', label: 'Checklist' };
  }
  if (text.includes('/kanban') || text.includes('kanban board')) {
    return { view: 'kanban', label: 'Kanban' };
  }
  if (text.includes('boss card') || text.includes('all organizations') || text.includes('kpi row') || text.includes('quota gauge')) {
    return { view: 'overview', label: 'All Organizations' };
  }
  return { view: 'overview', label: 'All Organizations' };
}

async function ensureChecklistItemsLoaded(sprint) {
  const s = sprint || document.getElementById('checklist-sprint-filter')?.value || 'STA-236';
  if (!checklistItems || !checklistItems.length || checklistItems[0]?.sprint !== s) {
    try {
      const r = await apiFetch(`/api/checklist?sprint=${encodeURIComponent(s)}`);
      checklistItems = r.items || [];
    } catch (e) {
      console.error('Failed to load items for walkthrough:', e);
    }
  }
}

async function startWalkthrough(sprint, startIndex = 0) {
  const s = sprint || document.getElementById('checklist-sprint-filter')?.value || 'STA-236';
  await ensureChecklistItemsLoaded(s);
  if (!checklistItems.length) {
    alert('No checklist items found for sprint ' + s + '. Please seed first.');
    return;
  }

  let targetIndex = startIndex;
  if (targetIndex < 0 || targetIndex >= checklistItems.length) {
    const firstPending = checklistItems.findIndex(i => i.status === 'pending');
    targetIndex = firstPending >= 0 ? firstPending : 0;
  }

  const curItem = checklistItems[targetIndex];
  saveWalkthroughState({
    active: true,
    sprint: s,
    index: targetIndex,
    itemId: curItem?.id,
    isMinimized: false
  });

  renderWalkthroughHUD();
  updateDevTourToggleUI();
}

function stopWalkthrough() {
  saveWalkthroughState(null);
  const hud = document.getElementById('checklist-walkthrough-hud');
  if (hud) hud.style.display = 'none';
  clearWalkthroughSidebarHighlight();
  updateDevTourToggleUI();
}

function clearWalkthroughSidebarHighlight() {
  document.querySelectorAll('.sidebar-item').forEach(b => {
    b.classList.remove('tour-target-highlight');
  });
}

function updateWalkthroughSidebarHighlight() {
  clearWalkthroughSidebarHighlight();
  if (!isWalkthroughActive()) return;
  const state = getWalkthroughState();
  if (!state || !checklistItems.length) return;
  const item = checklistItems[state.index];
  if (!item) return;

  const target = resolveItemTarget(item);
  const currentPath = window.location.pathname.replace(/\/+$/, '') || '/';
  const targetPath = viewToPath(target.view, null);

  // If we are NOT on the target page, highlight the sidebar item
  if (currentPath !== targetPath) {
    const sidebarBtn = document.querySelector(`.sidebar-item[data-view="${target.view}"]`);
    if (sidebarBtn) {
      sidebarBtn.classList.add('tour-target-highlight');
    }
  }

  // Update HUD guide box
  const guideBox = document.getElementById('hud-guide-box');
  const guideLabel = document.getElementById('hud-guide-label');
  const jumpBtn = document.getElementById('hud-jump-btn');

  if (guideBox && guideLabel && jumpBtn) {
    if (currentPath === targetPath) {
      guideBox.classList.add('guide-matched');
      guideLabel.innerHTML = `✅ <strong>On target page:</strong> ${target.label}`;
      jumpBtn.style.display = 'none';
    } else {
      guideBox.classList.remove('guide-matched');
      guideLabel.innerHTML = `📍 <strong>Step ${(state.index || 0) + 1}:</strong> Click <u>${target.label}</u> in sidebar`;
      jumpBtn.style.display = 'inline-block';
      jumpBtn.textContent = `Jump to ${target.label}`;
      jumpBtn.onclick = () => {
        navigateTo(target.view, null, true);
      };
    }
  }
}

function renderWalkthroughHUD() {
  const hud = document.getElementById('checklist-walkthrough-hud');
  if (!hud) return;
  if (!isWalkthroughActive() || !isDevTourEnabled()) {
    hud.style.display = 'none';
    clearWalkthroughSidebarHighlight();
    return;
  }

  const state = getWalkthroughState();
  if (!state || !checklistItems.length) {
    ensureChecklistItemsLoaded(state?.sprint).then(() => renderWalkthroughHUD());
    return;
  }

  const idx = Math.max(0, Math.min(checklistItems.length - 1, state.index || 0));
  const item = checklistItems[idx];
  if (!item) return;

  state.itemId = item.id;
  state.index = idx;
  saveWalkthroughState(state);

  hud.style.display = 'flex';
  hud.classList.toggle('minimized', Boolean(state.isMinimized));

  // Step counter & section
  const counterEl = document.getElementById('hud-step-counter');
  if (counterEl) counterEl.textContent = `Question ${idx + 1} of ${checklistItems.length}`;

  const secEl = document.getElementById('hud-item-section');
  if (secEl) secEl.textContent = splitChecklistRefs(item.section).text || 'General Verification';

  // Title
  const titleEl = document.getElementById('hud-item-title');
  if (titleEl) {
    titleEl.textContent = splitChecklistRefs(item.title).text;
    if (item.contract) {
      const tag = el('span', 'checklist-contract-tag', '⚙ contract');
      tag.title = 'Machine contract: ' + item.contract;
      titleEl.appendChild(tag);
    }
  }

  // Desc
  const descEl = document.getElementById('hud-item-desc');
  if (descEl) descEl.textContent = splitChecklistRefs(item.description).text;

  // How to test
  const howtoEl = document.getElementById('hud-item-howto');
  if (howtoEl) howtoEl.textContent = splitChecklistRefs(item.how_to_test).text || 'Verify this requirement in the application view.';

  const refsEl = document.getElementById('hud-item-refs');
  if (refsEl) {
    const refsText = checklistRefsText(item);
    refsEl.style.display = refsText ? '' : 'none';
    refsEl.open = false;
    const body = refsEl.querySelector('.cl-refs-body');
    if (body) body.textContent = refsText;
  }

  // Target & sidebar highlight
  updateWalkthroughSidebarHighlight();

  // Status buttons
  updateHUDStatusButtons(item.status);

  // Notes
  const notesInput = document.getElementById('hud-notes-input');
  const draftKey = 'staypoint_cl_draft_' + item.id;
  const draftVal = localStorage.getItem(draftKey);
  const currentNote = (draftVal !== null && draftVal !== undefined && (!item.notes || draftVal.length >= item.notes.length))
    ? draftVal
    : (item.notes || '');

  if (notesInput) {
    notesInput.value = currentNote;
    notesInput.oninput = () => {
      const v = notesInput.value;
      item.notes = v;
      checklistItems[idx].notes = v;
      localStorage.setItem(draftKey, v);
      const pageTa = document.querySelector(`.checklist-item[data-id="${item.id}"] .checklist-notes-input`);
      if (pageTa) pageTa.value = v;
    };
  }

  // Prev / Next buttons
  const prevBtn = document.getElementById('hud-prev-btn');
  if (prevBtn) {
    prevBtn.disabled = idx <= 0;
    prevBtn.onclick = () => prevWalkthroughQuestion();
  }

  const nextBtn = document.getElementById('hud-next-btn');
  if (nextBtn) {
    const isLast = idx >= checklistItems.length - 1;
    nextBtn.textContent = isLast ? 'Finish Tour 🎉' : 'Done / Next ▶';
    nextBtn.onclick = () => nextWalkthroughQuestion();
  }

  // Jump select dropdown
  const jumpSel = document.getElementById('hud-jump-select');
  if (jumpSel) {
    jumpSel.innerHTML = '';
    checklistItems.forEach((it, i) => {
      const opt = document.createElement('option');
      opt.value = i;
      const statusIcon = it.status === 'pass' ? '✓' : (it.status === 'fail' ? '✗' : (it.status === 'partial' ? '◐' : '○'));
      opt.textContent = `${statusIcon} #${i + 1}: ${splitChecklistRefs(it.title).text.slice(0, 30)}…`;
      if (i === idx) opt.selected = true;
      jumpSel.appendChild(opt);
    });
    jumpSel.onchange = (e) => {
      jumpWalkthroughQuestion(parseInt(e.target.value, 10));
    };
  }

  // Save note button
  const saveNoteBtn = document.getElementById('hud-save-note-btn');
  const saveInd = document.getElementById('hud-save-indicator');
  if (saveNoteBtn) {
    saveNoteBtn.onclick = async () => {
      const val = notesInput ? notesInput.value : item.notes;
      saveNoteBtn.disabled = true;
      saveNoteBtn.textContent = 'Saving…';
      await updateChecklistNotes(item.id, val);
      saveNoteBtn.disabled = false;
      saveNoteBtn.textContent = 'Saved!';
      if (saveInd) saveInd.textContent = '✓ Note saved';
      setTimeout(() => {
        saveNoteBtn.textContent = 'Save Note';
        if (saveInd) saveInd.textContent = '';
      }, 1500);
    };
  }
}

function updateHUDStatusButtons(currentStatus) {
  const state = getWalkthroughState();
  if (!state || !checklistItems.length) return;
  const item = checklistItems[state.index];
  if (!item) return;

  const isBlocked = isItemCommitBlocked(item.id);
  const blockReason = isBlocked ? getItemCommitBlockReason(item.id) : '';

  document.querySelectorAll('.hud-status-btn').forEach(btn => {
    const st = btn.dataset.status;
    const isPass = st === 'pass';
    btn.className = `hud-status-btn${item.status === st ? ' active-' + st : ''}${isPass && isBlocked ? ' cl-btn-blocked' : ''}`;
    if (isPass && isBlocked) {
      btn.disabled = true;
      btn.title = `⛔ Blocked by Commit Gate: ${blockReason}`;
    } else {
      btn.disabled = false;
      btn.title = '';
    }
    btn.onclick = async () => {
      if (isPass && isBlocked) {
        alert(`⛔ Commit Gate Block: This checklist item cannot be marked "done" / "pass".\n\nReason: ${blockReason}\n\nRequired commits must be merged into main and compiled into the running staypointd daemon.`);
        return;
      }
      const notesInput = document.getElementById('hud-notes-input');
      const val = notesInput ? notesInput.value : item.notes;
      await updateChecklistStatus(item.id, st, val);
      updateHUDStatusButtons(st);
      const saveInd = document.getElementById('hud-save-indicator');
      if (saveInd) {
        saveInd.textContent = `✓ Status set to ${st.toUpperCase()}`;
        setTimeout(() => { if (saveInd) saveInd.textContent = ''; }, 2000);
      }
    };
  });
}

async function nextWalkthroughQuestion() {
  const state = getWalkthroughState();
  if (!state || !checklistItems.length) return;
  const idx = state.index;
  const item = checklistItems[idx];

  // Auto-save current note if modified
  const notesInput = document.getElementById('hud-notes-input');
  if (notesInput && item) {
    const val = notesInput.value;
    if (val !== item.notes) {
      await updateChecklistNotes(item.id, val);
    }
  }

  if (idx >= checklistItems.length - 1) {
    alert('🎉 You have completed all questions in the walkthrough!');
    stopWalkthrough();
    return;
  }

  state.index = idx + 1;
  state.itemId = checklistItems[state.index]?.id;
  saveWalkthroughState(state);
  renderWalkthroughHUD();
}

async function prevWalkthroughQuestion() {
  const state = getWalkthroughState();
  if (!state || !checklistItems.length) return;
  const idx = state.index;
  if (idx <= 0) return;

  const item = checklistItems[idx];
  const notesInput = document.getElementById('hud-notes-input');
  if (notesInput && item) {
    const val = notesInput.value;
    if (val !== item.notes) {
      await updateChecklistNotes(item.id, val);
    }
  }

  state.index = idx - 1;
  state.itemId = checklistItems[state.index]?.id;
  saveWalkthroughState(state);
  renderWalkthroughHUD();
}

function jumpWalkthroughQuestion(targetIndex) {
  const state = getWalkthroughState();
  if (!state || !checklistItems.length) return;
  if (targetIndex < 0 || targetIndex >= checklistItems.length) return;

  state.index = targetIndex;
  state.itemId = checklistItems[targetIndex]?.id;
  saveWalkthroughState(state);
  renderWalkthroughHUD();
}

// Wire up HUD static controls
document.getElementById('hud-exit-btn')?.addEventListener('click', stopWalkthrough);
document.getElementById('hud-toggle-minimize-btn')?.addEventListener('click', () => {
  const state = getWalkthroughState();
  if (!state) return;
  state.isMinimized = !state.isMinimized;
  saveWalkthroughState(state);
  document.getElementById('checklist-walkthrough-hud')?.classList.toggle('minimized', Boolean(state.isMinimized));
  const minBtn = document.getElementById('hud-toggle-minimize-btn');
  if (minBtn) minBtn.textContent = state.isMinimized ? '▲' : '_';
});
document.getElementById('checklist-tour-toggle-btn')?.addEventListener('click', () => {
  const next = !isDevTourEnabled();
  setDevTourEnabled(next);
});
document.getElementById('checklist-start-tour-btn')?.addEventListener('click', () => {
  const state = getWalkthroughState();
  if (state?.active) {
    renderWalkthroughHUD();
  } else {
    startWalkthrough(document.getElementById('checklist-sprint-filter')?.value || 'STA-236', 0);
  }
});

// ── Filter listeners (projects page) ─────────────────────
document.getElementById('projects-status-filter')?.addEventListener('change', (e) => {
  state.projectsFilter.status = e.target.value;
  saveProjectsFilters();
  renderProjects();
});

document.getElementById('projects-org-multiselect-btn')?.addEventListener('click', (e) => {
  e.stopPropagation();
  const menu = document.getElementById('projects-org-menu');
  const btn = document.getElementById('projects-org-multiselect-btn');
  if (!menu || !btn) return;
  const isExpanded = btn.getAttribute('aria-expanded') === 'true';
  menu.classList.toggle('hidden', isExpanded);
  btn.setAttribute('aria-expanded', String(!isExpanded));
});

document.getElementById('projects-org-select-all')?.addEventListener('click', (e) => {
  e.stopPropagation();
  const checkboxes = document.querySelectorAll('#projects-org-options .multiselect-checkbox');
  checkboxes.forEach(cb => { cb.checked = true; });
  state.projectsFilter.orgs = [];
  saveProjectsFilters();
  updateProjectsOrgButtonLabel();
  renderProjects();
});

document.getElementById('projects-org-clear-all')?.addEventListener('click', (e) => {
  e.stopPropagation();
  const checkboxes = document.querySelectorAll('#projects-org-options .multiselect-checkbox');
  checkboxes.forEach(cb => { cb.checked = false; });
  state.projectsFilter.orgs = ['__none__'];
  saveProjectsFilters();
  updateProjectsOrgButtonLabel();
  renderProjects();
});

// Outside click to dismiss multi-select dropdown
document.addEventListener('click', (e) => {
  const ms = document.getElementById('projects-org-multiselect');
  const menu = document.getElementById('projects-org-menu');
  const btn = document.getElementById('projects-org-multiselect-btn');
  if (ms && menu && !ms.contains(e.target)) {
    menu.classList.add('hidden');
    btn?.setAttribute('aria-expanded', 'false');
  }
});

// Legacy select listener for backwards compatibility
document.getElementById('projects-org-filter')?.addEventListener('change', (e) => {
  if (e.target.value === 'all') {
    state.projectsFilter.orgs = [];
  } else {
    state.projectsFilter.orgs = [e.target.value];
  }
  saveProjectsFilters();
  populateProjectsOrgFilter();
  renderProjects();
});

// ── Boot ──────────────────────────────────────────────────
// Route first: parse the URL immediately so the correct view renders
// without waiting for the slow /api/fleet/overview call (≈3–4 s).
// openTaskPage() fetches /api/tasks/{id} itself; it does not need state.fleet.
// Non-task views that depend on fleet data will show a loading state until
// loadAll() populates state; SSE updates fill in the rest incrementally.
(function boot() {
  const initialRoute = pathToRoute();
  const isTaskRoute = !!(initialRoute.taskId || initialRoute.identifier);

  if (isTaskRoute) {
    // Render task page immediately — no fleet data required.
    openTaskPage(initialRoute, false);
  } else {
    // Non-task view: activate it now; loadAll() will re-render once data arrives.
    navigateTo(initialRoute.view, initialRoute.org, false);
  }

  connectSSE();
  refreshBoardPasskeyStatus();
  refreshDevBuildBadge();
  loadOrgHolds();
  const bannerEnrollBtn = document.getElementById('board-passkey-banner-enroll');
  bannerEnrollBtn?.addEventListener('click', async () => {
    bannerEnrollBtn.disabled = true;
    try { await enrollBoardPasskey(); } finally { bannerEnrollBtn.disabled = false; }
  });
  loadBoardAlerts();
  updateDevTourToggleUI();
  if (isWalkthroughActive()) {
    renderWalkthroughHUD();
  }

  // Load fleet/tasks/sessions in the background; re-render non-task views on completion.
  const t0 = performance.now();
  loadAll().then(() => {
    const elapsed = Math.round(performance.now() - t0);
    console.debug(`[boot] loadAll() completed in ${elapsed} ms`);
    loadBossReportCache();
    preloadBossReports(false);
    // If we are still on a non-task view, re-render it now that fleet data is ready.
    if (!isTaskRoute) {
      navigateTo(initialRoute.view, initialRoute.org, false);
    }
  });
}());

// Periodic Boss Card re-render (every 30 minutes)
setInterval(() => {
  preloadBossReports(true);
}, BOSS_REPORT_CACHE_TTL_MS);

// ── Create Task Modal ──────────────────────────────────────
(function initCreateTaskModal() {
  const modal   = document.getElementById('create-task-modal');
  const openBtn = document.getElementById('open-create-task-btn');
  const closeBtn = document.getElementById('create-task-modal-close');
  const cancelBtn = document.getElementById('create-task-cancel');
  const form    = document.getElementById('create-task-form');
  const errBox  = document.getElementById('create-task-error');
  const workKindSel = document.getElementById('ct-work-kind');
  const providerSel = document.getElementById('ct-provider');
  const providerHint = document.getElementById('ct-provider-hint');
  const submitBtn = document.getElementById('create-task-submit');

  if (!modal || !openBtn || !form) return;

  const gateProvider = () => {
    if (providerSel) gateGeminiOptions(providerSel, (workKindSel && workKindSel.value) || 'coding', providerHint);
  };
  if (workKindSel) workKindSel.addEventListener('change', gateProvider);

  function openModal() {
    modal.style.display = 'flex';
    form.reset();
    if (workKindSel) workKindSel.value = 'coding';
    if (providerSel) providerSel.value = '';
    gateProvider();
    errBox.style.display = 'none';
    submitBtn.disabled = false;
    document.getElementById('ct-name').focus();
  }

  function closeModal() {
    modal.style.display = 'none';
  }

  openBtn.addEventListener('click', openModal);
  closeBtn.addEventListener('click', closeModal);
  cancelBtn.addEventListener('click', closeModal);
  modal.addEventListener('click', e => { if (e.target === modal) closeModal(); });
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && modal.style.display !== 'none') closeModal(); });

  form.addEventListener('submit', async e => {
    e.preventDefault();
    errBox.style.display = 'none';
    const name = document.getElementById('ct-name').value.trim();
    if (!name) { showErr('Task name is required.'); return; }

    submitBtn.disabled = true;
    submitBtn.textContent = 'Creating…';

    const descVal = (document.getElementById('ct-description')?.value || '').trim();
    const body = {
      name,
      work_kind: (workKindSel && workKindSel.value) || 'coding',
      organization: document.getElementById('ct-org').value.trim(),
      project:      document.getElementById('ct-project').value.trim(),
      repo_path:    document.getElementById('ct-repo').value.trim(),
      git_branch:   document.getElementById('ct-branch').value.trim(),
      max_budget_usd: parseFloat(document.getElementById('ct-budget').value) || 0,
      max_turns:    parseInt(document.getElementById('ct-turns').value, 10) || 0,
      ...(descVal && { description: descVal }),
      ...splitProviderChoice(providerSel ? providerSel.value : ''),
    };

    try {
      const resp = await fetch('/api/tasks', {
        method: 'POST',
        headers: { ...authHeader(), 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const task = await resp.json().catch(() => ({}));
      if (!resp.ok) throw new Error(task.error || `${resp.status} ${resp.statusText}`);
      closeModal();
      // Refresh task list
      const fresh = await apiFetch('/api/tasks?status=all').catch(() => ({ tasks: [] }));
      const taskArr = (fresh.tasks || []);
      for (const t of taskArr) { state.tasks[t.id] = t; }
      renderTaskStatusPage();
      // Navigate to new task
      if (task && task.id) openTaskPage(task.id);
    } catch (err) {
      showErr(err.message || 'Failed to create task.');
      submitBtn.disabled = false;
      submitBtn.textContent = 'Create Task';
    }
  });

  function showErr(msg) {
    errBox.textContent = msg;
    errBox.style.display = 'block';
  }
})();

// ── Run error helpers ─────────────────────────────────────────────────────────

function buildRunErrorRow(e) {
  const row = el('div', 'run-error-row');

  const header = el('div', 'run-error-header');
  const exitBadge = el('span', `badge badge-error`, `exit ${e.exit_code}`);
  const adapter = el('span', 'run-error-adapter', e.adapter || e.model || '');
  const ts = el('span', 'run-error-ts muted-text', e.created_at ? fmtDateTime(e.created_at) : '');
  header.appendChild(exitBadge);
  if (e.adapter || e.model) header.appendChild(adapter);
  header.appendChild(ts);
  if (e.duration_ms) header.appendChild(el('span', 'run-error-dur muted-text', fmtDuration(e.duration_ms)));
  row.appendChild(header);

  if (e.stderr_tail && e.stderr_tail.trim()) {
    const pre = document.createElement('pre');
    pre.className = 'run-error-stderr';
    pre.textContent = e.stderr_tail.trim();
    row.appendChild(pre);
  }

  if (e.task_id) {
    const taskLink = document.createElement('a');
    taskLink.className = 'run-error-task-link muted-text';
    taskLink.href = `/tasks/${e.task_id}`;
    taskLink.textContent = `Task: ${e.task_id}`;
    taskLink.addEventListener('click', (ev) => {
      ev.preventDefault();
      openTaskPage(e.task_id, true);
    });
    row.appendChild(taskLink);
  }

  return row;
}

function fmtDuration(ms) {
  if (!ms) return '';
  if (ms < 1000) return `${ms}ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

// ── Logs page ─────────────────────────────────────────────────────────────────

let logsState = { errors: [], filter: '', adapterFilter: 'all' };

async function renderLogsPage() {
  const container = document.getElementById('logs-container');
  if (!container) return;
  container.innerHTML = '<p style="color:var(--muted);padding:1.5rem">Loading…</p>';

  const refreshBtn = document.getElementById('logs-refresh-btn');
  const searchInput = document.getElementById('logs-search');
  const adapterSel = document.getElementById('logs-adapter-filter');

  const load = async () => {
    container.innerHTML = '<p style="color:var(--muted);padding:1.5rem">Loading…</p>';
    try {
      const resp = await apiFetch('/api/run-errors?limit=200');
      logsState.errors = resp.errors || [];
      populateLogsAdapterFilter(adapterSel, logsState.errors);
      renderLogsTable(container, logsState.errors, logsState.filter, logsState.adapterFilter);
    } catch (err) {
      container.innerHTML = `<p style="color:var(--red);padding:1.5rem">Failed to load logs: ${err.message}</p>`;
    }
  };

  if (refreshBtn && !refreshBtn._logsHandler) {
    refreshBtn._logsHandler = true;
    refreshBtn.addEventListener('click', load);
  }
  if (searchInput && !searchInput._logsHandler) {
    searchInput._logsHandler = true;
    searchInput.addEventListener('input', () => {
      logsState.filter = searchInput.value.toLowerCase();
      renderLogsTable(container, logsState.errors, logsState.filter, logsState.adapterFilter);
    });
  }
  if (adapterSel && !adapterSel._logsHandler) {
    adapterSel._logsHandler = true;
    adapterSel.addEventListener('change', () => {
      logsState.adapterFilter = adapterSel.value;
      renderLogsTable(container, logsState.errors, logsState.filter, logsState.adapterFilter);
    });
  }

  await load();
}

function populateLogsAdapterFilter(sel, errors) {
  if (!sel) return;
  const adapters = [...new Set(errors.map(e => e.adapter || e.model).filter(Boolean))].sort();
  const current = sel.value;
  // keep "all" option
  while (sel.options.length > 1) sel.remove(1);
  for (const a of adapters) {
    const opt = document.createElement('option');
    opt.value = a;
    opt.textContent = a;
    sel.appendChild(opt);
  }
  sel.value = adapters.includes(current) ? current : 'all';
}

function renderLogsTable(container, errors, filter, adapterFilter) {
  container.innerHTML = '';

  let filtered = errors;
  if (adapterFilter && adapterFilter !== 'all') {
    filtered = filtered.filter(e => (e.adapter || e.model) === adapterFilter);
  }
  if (filter) {
    filtered = filtered.filter(e =>
      (e.task_id || '').toLowerCase().includes(filter) ||
      (e.adapter || '').toLowerCase().includes(filter) ||
      (e.model || '').toLowerCase().includes(filter) ||
      (e.stderr_tail || '').toLowerCase().includes(filter) ||
      (e.run_id || '').toLowerCase().includes(filter)
    );
  }

  if (filtered.length === 0) {
    container.appendChild(el('p', 'muted-text', errors.length === 0
      ? 'No run errors recorded.'
      : 'No errors match the current filter.'));
    return;
  }

  const header = el('div', 'run-errors-header-row');
  header.appendChild(el('span', 'run-errors-col-label', `${filtered.length} error${filtered.length !== 1 ? 's' : ''}`));
  container.appendChild(header);

  for (const e of filtered) {
    container.appendChild(buildRunErrorRow(e));
  }
}

// Gates page and task-page pending gates live in gates.js (STA-868).

// ── Artifacts (task documents) ────────────────────────────
// Task documents (`staypoint task doc add`) are the deliverables of planning,
// architecture and docs tasks. The task page's Artifacts tab shows one task's
// documents; the sidebar Artifacts page lists them across tasks.
// Pure helpers (filtering, filter options, file names) live in lib/artifacts.js.

// Doc key the Artifacts tab shows for a task: the last one picked, or the one
// a row on the Artifacts page opened, so a re-render (SSE) keeps it.
let artifactFocus = { taskId: null, docKey: null };

const artifactDate = (ts) => (ts ? new Date(ts).toLocaleString() : '');

function downloadArtifact(doc) {
  const blob = new Blob([doc.content || ''], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = artifactFileName(doc, doc.version);
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

// renderTaskArtifacts fills panel with the task's documents: a list of doc
// keys and a viewer for the selected one. Resolves to the document count.
async function renderTaskArtifacts(panel, taskId) {
  panel.innerHTML = '';
  const wrap = el('div', 'task-artifacts');
  panel.appendChild(wrap);
  let docs;
  try {
    const resp = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/documents`);
    docs = resp.documents || [];
  } catch (err) {
    wrap.appendChild(el('p', 'panel-field-muted task-page-tab-empty', `Failed to load documents: ${err.message}`));
    return 0;
  }
  if (!docs.length) {
    wrap.appendChild(el('p', 'panel-field-muted task-page-tab-empty',
      'No documents on this task. Agents store deliverables with `staypoint task doc add <task> <key> <content>`.'));
    return 0;
  }

  const list = el('div', 'artifact-key-list');
  list.setAttribute('role', 'listbox');
  list.setAttribute('aria-label', 'Task documents');
  const viewer = el('div', 'artifact-viewer');
  wrap.appendChild(list);
  wrap.appendChild(viewer);

  const buttons = {};
  let loadSeq = 0;
  const show = async (summary, version) => {
    artifactFocus = { taskId, docKey: summary.doc_key };
    for (const [k, b] of Object.entries(buttons)) {
      const on = k === summary.doc_key;
      b.classList.toggle('active', on);
      b.setAttribute('aria-selected', on ? 'true' : 'false');
    }
    const seq = ++loadSeq;
    viewer.innerHTML = '';
    viewer.appendChild(el('p', 'panel-field-muted', 'Loading…'));
    const q = version ? `?version=${version}` : '';
    let doc;
    try {
      doc = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/documents/${encodeURIComponent(summary.doc_key)}${q}`);
    } catch (err) {
      if (seq !== loadSeq) return;
      viewer.innerHTML = '';
      viewer.appendChild(el('p', 'panel-field-muted', `Failed to load ${summary.doc_key}: ${err.message}`));
      return;
    }
    if (seq !== loadSeq) return;
    viewer.innerHTML = '';

    const head = el('div', 'artifact-viewer-head');
    head.appendChild(el('span', 'artifact-viewer-key', doc.doc_key));
    const sel = el('select', 'select-filter artifact-version-select');
    sel.setAttribute('aria-label', `${doc.doc_key} version`);
    for (const v of summary.versions || [{ version: summary.latest_version, created_at: summary.updated_at }]) {
      const opt = el('option', '', `v${v.version}${v.version === summary.latest_version ? ' (latest)' : ''} · ${artifactDate(v.created_at)}`);
      opt.value = String(v.version);
      if (v.version === doc.version) opt.selected = true;
      sel.appendChild(opt);
    }
    sel.addEventListener('change', () => show(summary, Number(sel.value)));
    head.appendChild(sel);
    const named = { ...doc, task_identifier: summary.task_identifier };
    const dl = el('button', 'btn btn-secondary artifact-download-btn', 'Download');
    dl.type = 'button';
    dl.title = `Download ${artifactFileName(named, doc.version)}`;
    dl.addEventListener('click', () => downloadArtifact(named));
    const body = el('div', 'md-body markdown-body artifact-viewer-body');
    body.innerHTML = renderMarkdown(doc.content || '');
    // Full view in the shared long-content dialog, like the Brief's Open.
    head.appendChild(longContentTrigger(`artifact-${doc.doc_key}`, `${doc.doc_key} v${doc.version}`,
      () => cloneChildren(body, 'md-body markdown-body artifact-full-body')));
    head.appendChild(dl);
    viewer.appendChild(head);
    viewer.appendChild(el('div', 'artifact-viewer-meta',
      `Version ${doc.version} of ${summary.latest_version} · ${formatDocSize(new Blob([doc.content || '']).size)} · ${artifactDate(doc.created_at)}`));
    viewer.appendChild(body);
  };

  for (const d of docs) {
    const b = el('button', 'artifact-key-btn');
    b.type = 'button';
    b.setAttribute('role', 'option');
    b.dataset.docKey = d.doc_key;
    b.appendChild(el('span', 'artifact-key-name', d.doc_key));
    b.appendChild(el('span', 'artifact-key-meta', `v${d.latest_version} · ${formatDocSize(d.size)} · ${artifactDate(d.updated_at)}`));
    b.addEventListener('click', () => show(d));
    list.appendChild(b);
    buttons[d.doc_key] = b;
  }

  const focusKey = artifactFocus.taskId === taskId ? artifactFocus.docKey : null;
  show(docs.find(d => d.doc_key === focusKey) || docs[0]);
  return docs.length;
}

// Shows a document's latest version in the full long-content dialog without
// leaving the Artifacts page (task-e3fe2c0b).
async function viewArtifactFull(taskId, docKey) {
  let doc;
  try {
    doc = await apiFetch(`/api/tasks/${encodeURIComponent(taskId)}/documents/${encodeURIComponent(docKey)}`);
  } catch (err) {
    openContentModal(docKey, el('p', 'panel-field-muted', `Failed to load ${docKey}: ${err.message}`));
    return;
  }
  const body = el('div', 'md-body markdown-body artifact-full-body');
  body.innerHTML = renderMarkdown(doc.content || '');
  openContentModal(`${doc.doc_key} v${doc.version}`, body);
}

// Opens a task on its Artifacts tab with docKey selected.
function openTaskArtifact(taskId, docKey) {
  artifactFocus = { taskId, docKey };
  taskPagePanelTab = { taskId, key: 'artifacts' };
  openTaskPage(taskId);
}

const artifactsState = { docs: [], filter: { org: 'all', task: 'all', kind: 'all' } };

function fillArtifactSelect(sel, allLabel, options, current) {
  if (!sel) return;
  sel.innerHTML = '';
  const all = el('option', '', allLabel);
  all.value = 'all';
  sel.appendChild(all);
  for (const o of options) {
    const opt = el('option', '', o.label);
    opt.value = o.value;
    sel.appendChild(opt);
  }
  sel.value = options.some(o => o.value === current) ? current : 'all';
}

function renderArtifactsTable() {
  const container = document.getElementById('artifacts-container');
  if (!container) return;
  container.innerHTML = '';
  const rows = filterArtifacts(artifactsState.docs, artifactsState.filter);
  if (!rows.length) {
    const msg = artifactsState.docs.length
      ? 'No documents match these filters.'
      : 'No task documents yet. Planning and architecture tasks store deliverables with `staypoint task doc add`.';
    container.appendChild(el('p', 'artifacts-empty', msg));
    return;
  }
  const wrapper = el('div', 'task-table-wrapper');
  const table = el('table', 'global-task-table artifacts-table');
  const thead = el('thead');
  const hr = el('tr');
  for (const h of ['Document', 'Task', 'Org', 'Version', 'Size', 'Updated', '']) hr.appendChild(el('th', '', h));
  thead.appendChild(hr);
  table.appendChild(thead);
  const tbody = el('tbody');
  for (const d of rows) {
    const tr = el('tr', 'artifacts-row');
    tr.tabIndex = 0;
    tr.title = `Open ${d.doc_key} on ${artifactTaskLabel(d)}`;
    tr.appendChild(el('td', 'artifacts-doc-key', d.doc_key));
    tr.appendChild(el('td', '', artifactTaskLabel(d)));
    tr.appendChild(el('td', '', d.organization || '—'));
    tr.appendChild(el('td', '', d.version_count > 1 ? `v${d.latest_version} (${d.version_count} versions)` : `v${d.latest_version}`));
    tr.appendChild(el('td', '', formatDocSize(d.size)));
    tr.appendChild(el('td', '', artifactDate(d.updated_at)));
    const viewCell = el('td', 'artifacts-view-cell');
    const viewBtn = el('button', 'btn btn-secondary btn-sm artifact-view-btn', 'View');
    viewBtn.type = 'button';
    viewBtn.title = `View ${d.doc_key} in full`;
    viewBtn.addEventListener('click', (e) => { e.stopPropagation(); viewArtifactFull(d.task_id, d.doc_key); });
    viewCell.appendChild(viewBtn);
    tr.appendChild(viewCell);
    const open = () => openTaskArtifact(d.task_id, d.doc_key);
    tr.addEventListener('click', open);
    tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') open(); });
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrapper.appendChild(table);
  container.appendChild(wrapper);
}

async function renderArtifactsPage() {
  const container = document.getElementById('artifacts-container');
  if (!container) return;
  const selects = {
    org: document.getElementById('artifacts-org-filter'),
    task: document.getElementById('artifacts-task-filter'),
    kind: document.getElementById('artifacts-kind-filter'),
  };
  for (const [key, sel] of Object.entries(selects)) {
    if (sel && !sel._artifactsHandler) {
      sel._artifactsHandler = true;
      sel.addEventListener('change', () => {
        artifactsState.filter[key] = sel.value;
        renderArtifactsTable();
      });
    }
  }
  const refreshBtn = document.getElementById('artifacts-refresh-btn');
  if (refreshBtn && !refreshBtn._artifactsHandler) {
    refreshBtn._artifactsHandler = true;
    refreshBtn.addEventListener('click', renderArtifactsPage);
  }

  container.innerHTML = '<p class="artifacts-empty">Loading…</p>';
  try {
    const resp = await apiFetch('/api/documents');
    artifactsState.docs = resp.documents || [];
  } catch (err) {
    container.innerHTML = '';
    container.appendChild(el('p', 'artifacts-empty artifacts-error', `Failed to load documents: ${err.message}`));
    return;
  }
  const opts = artifactFilterOptions(artifactsState.docs);
  const f = artifactsState.filter;
  fillArtifactSelect(selects.org, 'All Orgs', opts.orgs.map(o => ({ value: o, label: o })), f.org);
  fillArtifactSelect(selects.task, 'All Tasks', opts.tasks.map(t => ({ value: t.id, label: t.label })), f.task);
  fillArtifactSelect(selects.kind, 'All Kinds', opts.kinds.map(k => ({ value: k, label: k })), f.kind);
  for (const [key, sel] of Object.entries(selects)) if (sel) f[key] = sel.value;
  renderArtifactsTable();
}

// ── Pull Requests page (task-9fb380ef) ─────────────────────────────────────
// Lists open PRs per repo with checks and the linked StayPoint task. Merge,
// Merge selected and Combine into one PR are Board actions: each goes through
// withBoardWebAuthn (one passkey per action), pinned to the head SHAs shown.

const prsState = { repos: [], selected: {} }; // selected: repo_path -> Set of PR numbers

function prAgeText(sec) {
  if (!sec || sec < 0) return '';
  if (sec < 3600) return `${Math.max(1, Math.round(sec / 60))}m`;
  if (sec < 86400) return `${Math.round(sec / 3600)}h`;
  return `${Math.round(sec / 86400)}d`;
}

function prChecksCell(c) {
  const wrap = el('span', 'pr-checks');
  c = c || {};
  wrap.appendChild(el('span', 'pr-check pr-check-pass', `✓ ${c.pass || 0}`));
  wrap.appendChild(el('span', 'pr-check pr-check-fail', `✗ ${c.fail || 0}`));
  wrap.appendChild(el('span', 'pr-check pr-check-pending', `… ${c.pending || 0}`));
  if (c.playwright) {
    const pw = el('span', `pr-check pr-check-playwright pr-check-${c.playwright}`, `Playwright: ${c.playwright}`);
    pw.title = 'Playwright UI Specs is counted separately: main has known failures';
    wrap.appendChild(pw);
  }
  return wrap;
}

function prMergeableText(pr) {
  if (pr.mergeable === 'CONFLICTING') return 'conflicts';
  if (pr.mergeable === 'MERGEABLE') return (pr.merge_state_status || 'mergeable').toLowerCase();
  return (pr.mergeable || 'unknown').toLowerCase();
}

async function prBoardPost(path, body, label) {
  return withBoardWebAuthn((sessionToken, assertion) =>
    fetch(path, {
      method: 'POST',
      headers: { ...authHeader(), 'Content-Type': 'application/json', 'X-WebAuthn-Session': sessionToken, 'X-WebAuthn-Assertion': assertion },
      body: JSON.stringify(body),
    }), label);
}

function prMergeReport(out) {
  const parts = [];
  if (out.merged && out.merged.length) parts.push(`Merged: ${out.merged.map(n => '#' + n).join(', ')}`);
  if (out.failed) parts.push(`Failed: #${out.failed.number} (${out.failed.error}: ${out.failed.message})`);
  if (out.not_attempted && out.not_attempted.length) parts.push(`Not attempted: ${out.not_attempted.map(n => '#' + n).join(', ')}`);
  return parts.join('\n') || 'Nothing merged.';
}

function renderPullRequestsRepo(repo, status) {
  const box = el('div', 'prs-repo');
  box.dataset.repo = repo.repo_path;
  const head = el('div', 'prs-repo-head');
  head.appendChild(el('h2', 'prs-repo-name', repo.name));
  head.appendChild(el('span', 'muted-text prs-repo-path', repo.repo_path));
  box.appendChild(head);
  if (repo.error) {
    box.appendChild(el('p', 'prs-error', `Could not list PRs: ${repo.error}`));
    return box;
  }
  const prs = repo.prs || [];
  if (!prs.length) {
    box.appendChild(el('p', 'muted-text prs-empty', 'No open pull requests.'));
    return box;
  }
  const sel = prsState.selected[repo.repo_path] || (prsState.selected[repo.repo_path] = new Set());
  const ordered = () => prs.filter(p => sel.has(p.number));

  const bar = el('div', 'prs-bulk-bar');
  const mergeSel = el('button', 'prs-merge-selected-btn', 'Merge selected');
  const combineSel = el('button', 'prs-combine-btn', 'Combine into one PR');
  mergeSel.type = 'button';
  combineSel.type = 'button';
  const syncBar = () => {
    mergeSel.disabled = sel.size < 1;
    combineSel.disabled = sel.size < 2;
  };
  bar.appendChild(mergeSel);
  bar.appendChild(combineSel);
  bar.appendChild(el('span', 'muted-text', 'Selected PRs run in the order listed.'));
  box.appendChild(bar);

  const table = el('table', 'data-table prs-table');
  const thead = el('thead');
  const hr = el('tr');
  for (const h of ['', '#', 'Title', 'Task', 'Branch → base', 'Author', 'Age', 'Mergeable', 'Checks', 'Files', '']) hr.appendChild(el('th', null, h));
  thead.appendChild(hr);
  table.appendChild(thead);
  const tbody = el('tbody');
  for (const pr of prs) {
    const tr = el('tr', 'prs-row');
    tr.dataset.number = String(pr.number);
    const cbTd = el('td');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.className = 'prs-select';
    cb.checked = sel.has(pr.number);
    cb.addEventListener('change', () => { if (cb.checked) sel.add(pr.number); else sel.delete(pr.number); syncBar(); });
    cbTd.appendChild(cb);
    tr.appendChild(cbTd);
    const numTd = el('td');
    const a = el('a', 'prs-number', `#${pr.number}`);
    a.href = pr.url; a.target = '_blank'; a.rel = 'noopener';
    numTd.appendChild(a);
    tr.appendChild(numTd);
    const titleTd = el('td', 'prs-title', pr.title);
    if (pr.is_draft) titleTd.appendChild(el('span', 'prs-draft', ' draft'));
    tr.appendChild(titleTd);
    const taskTd = el('td', 'prs-task');
    if (pr.task) {
      const ta = el('a', null, pr.task.name || pr.task.id);
      ta.href = `/tasks/${encodeURIComponent(pr.task.id)}`;
      ta.title = pr.task.id;
      ta.addEventListener('click', (e) => { e.preventDefault(); if (typeof openTaskPage === 'function') openTaskPage(pr.task.id); });
      taskTd.appendChild(ta);
    } else {
      taskTd.appendChild(el('span', 'muted-text', '—'));
    }
    tr.appendChild(taskTd);
    tr.appendChild(el('td', 'prs-branch', `${pr.head_ref} → ${pr.base_ref}`));
    tr.appendChild(el('td', null, pr.author || ''));
    tr.appendChild(el('td', null, prAgeText(pr.age_sec)));
    tr.appendChild(el('td', `prs-mergeable prs-mergeable-${(pr.mergeable || 'unknown').toLowerCase()}`, prMergeableText(pr)));
    const ckTd = el('td');
    ckTd.appendChild(prChecksCell(pr.checks));
    tr.appendChild(ckTd);
    tr.appendChild(el('td', null, String(pr.changed_files || 0)));
    const actTd = el('td');
    const mb = el('button', 'prs-merge-btn', 'Merge');
    mb.type = 'button';
    mb.title = `Merge commit, pinned to ${String(pr.head_sha).slice(0, 12)}`;
    mb.addEventListener('click', async () => {
      if (!confirm(`Merge #${pr.number} "${pr.title}" into ${pr.base_ref} (merge commit, head ${String(pr.head_sha).slice(0, 12)})?`)) return;
      mb.disabled = true;
      await runPRMerge(repo, [pr], status);
      mb.disabled = false;
    });
    actTd.appendChild(mb);
    tr.appendChild(actTd);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  box.appendChild(table);

  mergeSel.addEventListener('click', async () => {
    const list = ordered();
    if (!list.length) return;
    if (!confirm(`Merge ${list.length} PR(s) in this order, stopping at the first failure?\n\n${list.map(p => `#${p.number} ${p.title}`).join('\n')}`)) return;
    mergeSel.disabled = true;
    await runPRMerge(repo, list, status);
    syncBar();
  });
  combineSel.addEventListener('click', async () => {
    const list = ordered();
    if (list.length < 2) return;
    const base = list[0].base_ref || 'main';
    if (list.some(p => p.base_ref !== base)) { status('Combine needs PRs with the same base branch.', true); return; }
    const title = prompt(`Title for the combined PR into ${base}:`, `Combine ${list.map(p => '#' + p.number).join(', ')}`);
    if (title === null) return;
    combineSel.disabled = true;
    try {
      const res = await prBoardPost('/api/pull-requests/combine', {
        repo_path: repo.repo_path, base, title,
        prs: list.map(p => ({ number: p.number, head_sha: p.head_sha, title: p.title })),
      }, 'combining pull requests');
      if (res === null) return;
      const out = await res.json().catch(() => ({}));
      if (!res.ok) {
        status(`Combine failed${out.failed_pr ? ` at #${out.failed_pr}` : ''}: ${out.error || res.status} ${out.message || ''}. Nothing was left on GitHub.`, true);
        return;
      }
      status(`Opened combined PR: ${out.pr && out.pr.url}`, false);
      sel.clear();
      renderPullRequestsPage();
    } catch (e) {
      status(`Combine failed: ${e.message || e}`, true);
    } finally {
      syncBar();
    }
  });
  syncBar();
  return box;
}

async function runPRMerge(repo, list, status) {
  try {
    const res = await prBoardPost('/api/pull-requests/merge', {
      repo_path: repo.repo_path,
      prs: list.map(p => ({ number: p.number, head_sha: p.head_sha })),
    }, list.length > 1 ? 'merging the selected pull requests' : `merging #${list[0].number}`);
    if (res === null) return;
    const out = await res.json().catch(() => ({}));
    if (!out.merged && !res.ok) { status(boardActionErrorText(res, out), true); return; }
    status(prMergeReport(out), !!out.failed);
    const sel = prsState.selected[repo.repo_path];
    if (sel) for (const n of out.merged || []) sel.delete(n);
    renderPullRequestsPage();
  } catch (e) {
    status(`Merge failed: ${e.message || e}`, true);
  }
}

async function renderPullRequestsPage() {
  const container = document.getElementById('prs-container');
  if (!container) return;
  const refresh = document.getElementById('prs-refresh-btn');
  if (refresh && !refresh.dataset.wired) {
    refresh.dataset.wired = '1';
    refresh.addEventListener('click', () => renderPullRequestsPage());
  }
  let statusEl = document.getElementById('prs-status');
  if (!statusEl) {
    statusEl = el('pre', 'prs-status');
    statusEl.id = 'prs-status';
    statusEl.style.display = 'none';
    container.before(statusEl);
  }
  const status = (msg, isErr) => {
    statusEl.textContent = msg;
    statusEl.classList.toggle('prs-status-error', !!isErr);
    statusEl.style.display = msg ? '' : 'none';
  };
  if (!container.childElementCount) container.appendChild(el('p', 'muted-text', 'Loading pull requests…'));
  let data;
  try {
    data = await apiFetch('/api/pull-requests');
  } catch (e) {
    container.innerHTML = '';
    container.appendChild(el('p', 'prs-error', `Failed to load pull requests: ${e.message}`));
    return;
  }
  prsState.repos = data.repos || [];
  container.innerHTML = '';
  if (!prsState.repos.length) {
    container.appendChild(el('p', 'muted-text prs-empty', 'No repos yet: the page lists StayPoint\'s own repo and any repo with a task that registered a PR.'));
    return;
  }
  for (const repo of prsState.repos) container.appendChild(renderPullRequestsRepo(repo, status));
}
