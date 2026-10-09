import { test as base, expect, type APIRequestContext, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import * as fs from 'node:fs';
import * as path from 'node:path';

// Everything here talks to the throwaway daemon started by scripts/ui-e2e.sh.
// Fixtures are created over HTTP; the browser is only used for what a person
// would do in the UI.

const TOKEN = process.env.STAYPOINT_API_TOKEN || '';
if (TOKEN.length < 16) {
  throw new Error('STAYPOINT_API_TOKEN is not set: run the suite via scripts/ui-e2e.sh');
}

export const BOARD_TOKEN = process.env.STAYPOINT_BOARD_TOKEN || '';

export type Task = {
  id: string;
  name: string;
  status: string;
  execution_stage: string;
  organization?: string;
  project?: string;
};

export type Interaction = {
  id: number;
  task_id: string;
  interaction_kind: string;
  payload: string;
  status: string;
  response?: unknown;
};

export class StayPointAPI {
  constructor(private readonly request: APIRequestContext) {}

  private headers(asBoard = false) {
    const h: Record<string, string> = { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' };
    // Tasks created or started without a Board session are agent tasks
    // (backlog, Board-only to start); seeding acts as the Board.
    if (asBoard && BOARD_TOKEN) h.Cookie = `staypoint_board=${BOARD_TOKEN}`;
    return h;
  }

  private async json<T>(method: string, path: string, data?: unknown, asBoard = false): Promise<T> {
    const res = await this.request.fetch(path, { method, headers: this.headers(asBoard), data });
    const text = await res.text();
    if (!res.ok()) {
      throw new Error(`${method} ${path} -> ${res.status()}: ${text}`);
    }
    return (text ? JSON.parse(text) : {}) as T;
  }

  /** Creates a local task with a unique name so specs never collide. */
  async createTask(label: string, extra: Record<string, unknown> = {}): Promise<Task> {
    const name = `${label} ${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
    const body = { name, organization: 'STA', project: 'ui-e2e', ...extra };
    const created = await this.json<Task>('POST', '/api/tasks', body, true);
    // The create response omits organization/project; keep what was sent so
    // taskPagePath() builds the same URL the UI does.
    return { ...created, organization: body.organization, project: body.project };
  }

  /** Creates a task the way an agent does (token only): backlog, origin agent. */
  async createAgentTask(label: string, extra: Record<string, unknown> = {}): Promise<Task> {
    const name = `${label} ${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
    const body = { name, organization: 'STA', project: 'ui-e2e', ...extra };
    const created = await this.json<Task>('POST', '/api/tasks', body, false);
    return { ...created, organization: body.organization, project: body.project };
  }

  /** POSTs /stage with the agent token only; returns the raw status and body. */
  async setStageAsAgent(id: string, stage: string): Promise<{ status: number; body: { error?: string } }> {
    const res = await this.request.fetch(`/api/tasks/${encodeURIComponent(id)}/stage`, {
      method: 'POST', headers: this.headers(false), data: { stage },
    });
    return { status: res.status(), body: await res.json().catch(() => ({})) };
  }

  async getTask(id: string): Promise<Task> {
    const r = await this.json<{ task: Task }>('GET', `/api/tasks/${encodeURIComponent(id)}`);
    return r.task;
  }

  async addComment(id: string, body: string) {
    return this.json('POST', `/api/tasks/${encodeURIComponent(id)}/comments`, { body, author: 'ui-e2e' });
  }

  async listComments(id: string): Promise<Array<{ message?: string; body?: string }>> {
    const r = await this.json<{ comments: Array<{ message?: string; body?: string }> | null }>(
      'GET',
      `/api/tasks/${encodeURIComponent(id)}/comments`,
    );
    return r.comments || [];
  }

  /** payload is an object here; the API stores it as a JSON string. */
  async createInteraction(id: string, kind: string, payload: unknown): Promise<Interaction> {
    return this.json<Interaction>('POST', `/api/tasks/${encodeURIComponent(id)}/interactions`, {
      kind,
      payload: JSON.stringify(payload),
    });
  }

  async listInteractions(id: string): Promise<Interaction[]> {
    const r = await this.json<{ interactions: Interaction[] | null }>(
      'GET',
      `/api/tasks/${encodeURIComponent(id)}/interactions`,
    );
    return r.interactions || [];
  }

  async seedChecklist(sprint: string) {
    return this.json('POST', '/api/checklist/seed', { sprint });
  }

  async setStage(id: string, stage: string) {
    return this.json('POST', `/api/tasks/${encodeURIComponent(id)}/stage`, { stage }, true);
  }

  async runControl(id: string, action: string) {
    return this.json('POST', `/api/tasks/${encodeURIComponent(id)}/run-control`, { action });
  }

  async getRunControlState(id: string): Promise<{ paused: boolean; stop_requested: boolean }> {
    return this.json('GET', `/api/tasks/${encodeURIComponent(id)}/run-control-state`);
  }

  async getMigrations(id: string): Promise<{
    migrations: Array<{
      path: string; sql: string; risk_statements: string[]; additive_only: boolean;
      verification_checks: Array<{ kind: string; description: string; sql: string }>;
      verification_query: string;
    }>;
    sql_editor_url: string;
    has_auto_verify: boolean;
  }> {
    return this.json('GET', `/api/tasks/${encodeURIComponent(id)}/migrations`);
  }

  async markMigrationApplied(
    id: string, path: string, appliedBy = 'board',
    extra: Record<string, unknown> = {},
  ): Promise<{ ok: boolean; path: string; applied_by: string; mode?: string; verification_query?: string; checks?: unknown[] }> {
    const res = await this.request.fetch(`/api/tasks/${encodeURIComponent(id)}/migrations/mark-applied`, {
      method: 'POST',
      headers: this.headers(),
      data: { path, applied_by: appliedBy, ...extra },
    });
    const text = await res.text();
    if (!res.ok()) {
      throw new Error(`POST /api/tasks/${encodeURIComponent(id)}/migrations/mark-applied -> ${res.status()}: ${text}`);
    }
    return (text ? JSON.parse(text) : {}) as { ok: boolean; path: string; applied_by: string; mode?: string; verification_query?: string; checks?: unknown[] };
  }

  /** Seeds a ship review card directly into the DB (test-only endpoint). */
  async upsertShipReview(id: string, status = 'pending'): Promise<{ id: string; status: string; head_sha: string }> {
    return this.json('PUT', `/api/tasks/${encodeURIComponent(id)}/ship-review/seed`, {
      status,
      test_steps: ['Open the preview URL and verify the feature.'],
    });
  }

  async createGateRequest(cmdline: string, reasons: string[] = [], runId = '', taskId = ''): Promise<{ id: string; cmdline: string; status: string; decided_by?: string }> {
    return this.json('POST', '/api/security/gate-requests', { cmdline, reasons, run_id: runId, task_id: taskId });
  }

  async listGateRules(): Promise<{ rules: Array<{ id: number; pattern: string; scope: string; scope_value: string; hit_count: number }> }> {
    return this.json('GET', '/api/security/gate-rules');
  }

  async listGateRequests(status = 'pending'): Promise<{ gate_requests: Array<{ id: string; cmdline: string; status: string; reasons: string[]; run_id: string; created_at: string }> }> {
    return this.json('GET', `/api/security/gate-requests?status=${status}`);
  }

  async listGateAuditLog(gateId: string): Promise<{ audit_log: Array<{ id: string; gate_id: string; actor_id: string; event_type: string; from_status?: string; to_status?: string; created_at: string }> }> {
    return this.json('GET', `/api/security/gate-requests/${gateId}/audit-log`);
  }
}

/** URL of the full task page, matching taskToPath() in app.js for local tasks. */
export function taskPagePath(task: Task): string {
  return `/tasks/${encodeURIComponent(task.organization || 'STA')}/${encodeURIComponent(
    task.project || 'default',
  )}/${encodeURIComponent(task.id)}`;
}

/**
 * Opens the full task page and waits until it has rendered. Under CPU
 * contention the first render can take several seconds, so this waits longer
 * than the default expect timeout before specs assert anything on the page.
 */
export async function gotoTaskPage(page: Page, task: Task) {
  await page.goto(taskPagePath(task));
  await expect(page.locator('#task-page-content .task-page-title')).toHaveText(task.name, { timeout: 20_000 });
}

/**
 * Selects a tab of the task page's right panel (STA-641). Only one tab shows
 * at a time: Review (ship review card, Details), Diff, Migrations, Brief
 * (description, notes), Artifacts (task documents). It opens on Review while a ship review is pending,
 * else on Diff. Pass scope '#panel-content' for the drawer (STA-700).
 */
export async function openTaskPanelTab(
  page: Page,
  name: 'Review' | 'Diff' | 'Migrations' | 'Brief' | 'Artifacts',
  scope = '#task-page-content',
) {
  const tab = page.locator(`${scope} .task-page-panel`).getByRole('tab', { name: new RegExp(`^\\W*${name}\\b`) });
  await tab.click();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
}

/**
 * Runs tests/ui/stepsim against the throwaway DB: the real StepRecorder
 * writing a short run for the task. Returns how many steps it persisted.
 */
export function simulateRunSteps(taskId: string): number {
  const bin = process.env.STAYPOINT_UI_STEPSIM;
  const db = process.env.STAYPOINT_UI_DB;
  if (!bin || !db) throw new Error('STAYPOINT_UI_STEPSIM / STAYPOINT_UI_DB not set: run via scripts/ui-e2e.sh');
  const out = execFileSync(bin, ['--db', db, '--task', taskId], { encoding: 'utf8' });
  const m = out.match(/^STEPS (\d+)$/m);
  return m ? Number(m[1]) : 0;
}

/**
 * Like simulateRunSteps but emits a wake + tool steps WITHOUT a terminal
 * state step, leaving the task visually mid-run so the elapsed ticker keeps
 * ticking. Returns how many steps were persisted.
 */
export function simulatePartialRun(taskId: string): number {
  const bin = process.env.STAYPOINT_UI_STEPSIM;
  const db = process.env.STAYPOINT_UI_DB;
  if (!bin || !db) throw new Error('STAYPOINT_UI_STEPSIM / STAYPOINT_UI_DB not set: run via scripts/ui-e2e.sh');
  const out = execFileSync(bin, ['--db', db, '--task', taskId, '--partial'], { encoding: 'utf8' });
  const m = out.match(/^STEPS (\d+)$/m);
  return m ? Number(m[1]) : 0;
}

/**
 * Saves a full-viewport screenshot as <artifacts>/<name> (the dir
 * scripts/ui-e2e.sh exports as STAYPOINT_UI_ARTIFACTS) and attaches it to the
 * test report. Used for side-by-side checks against design mocks, so it is
 * kept on success, unlike Playwright's only-on-failure screenshots.
 */
export async function saveArtifactScreenshot(page: Page, name: string): Promise<string> {
  const dir = path.resolve(process.env.STAYPOINT_UI_ARTIFACTS || 'artifacts');
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, name);
  await page.screenshot({ path: file });
  await base.info().attach(name, { path: file, contentType: 'image/png' });
  return file;
}

/**
 * Product bugs the suite already knows about. A spec that hits one is marked
 * as expected-to-fail, so the suite stays green while the bug is open and
 * turns red the moment the bug is fixed (an "unexpected pass"), which is the
 * cue to delete the entry. STAYPOINT_UI_STRICT=1 ignores this list.
 */
export const KNOWN_BUGS = {
  'STA-378': 'StepRecorder inserts run_steps.status and expects an integer id, but the run_steps table has no status column and a TEXT id: every step insert fails',
} as const;

export type KnownBug = keyof typeof KNOWN_BUGS;

export function knownBug(id: KnownBug) {
  if (process.env.STAYPOINT_UI_STRICT === '1') return;
  base.info().annotations.push({ type: 'known-bug', description: `${id}: ${KNOWN_BUGS[id]}` });
  base.fail(true, `${id}: ${KNOWN_BUGS[id]}`);
}

type Fixtures = { api: StayPointAPI; page: Page; boardPage: Page; boardSessionPage: Page };

/**
 * Gives the page a Board session cookie (fresh nonce, then the ?board_nonce=
 * bootstrap) and a CDP virtual authenticator (transport: internal, user
 * verification on) that answers navigator.credentials.create/get() so tests
 * never need Touch ID. Enrolls nothing.
 */
async function openBoardSession(page: Page, baseURL: string, request: APIRequestContext) {
  const bt = BOARD_TOKEN;
  if (!bt) throw new Error('STAYPOINT_BOARD_TOKEN is not set: run via scripts/ui-e2e.sh');

  const nonceRes = await request.post(`${baseURL}/api/board/fresh-nonce`, {
    headers: { 'Authorization': `Bearer ${TOKEN}`, 'X-Board-Token': bt },
  });
  if (!nonceRes.ok()) {
    throw new Error(`POST /api/board/fresh-nonce -> ${nonceRes.status()}: ${await nonceRes.text()}`);
  }
  const { nonce } = await nonceRes.json();

  await page.goto(`${baseURL}/?token=${encodeURIComponent(TOKEN)}&board_nonce=${encodeURIComponent(nonce)}`);
  await expect(page).toHaveURL(`${baseURL}/`);

  // Backup eligible and backed up (BE=1, BS=1) like an iCloud Keychain passkey:
  // CDP defaults both to 0, which hid STA-716 (stored BE lost, every Board
  // action 403).
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable', { enableUI: false });
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: false,
      hasUserVerification: true,
      isUserVerified: true,
      defaultBackupEligibility: true,
      defaultBackupState: true,
    },
  });
}

export const test = base.extend<Fixtures>({
  api: async ({ request }, use) => {
    await use(new StayPointAPI(request));
  },
  // Log in the way a person does: the first visit carries ?token=, the daemon
  // swaps it for a session cookie and redirects to the clean URL.
  page: async ({ page, baseURL }, use) => {
    await page.goto(`${baseURL}/?token=${encodeURIComponent(TOKEN)}`);
    await expect(page).toHaveURL(`${baseURL}/`);
    await use(page);
  },
  // boardPage is a page with a Board session cookie set AND a CDP virtual
  // authenticator enrolled. Requires STAYPOINT_BOARD_TOKEN (exported by
  // scripts/ui-e2e.sh) and a TestMode server (staypoint-apitest-server) so
  // that GET /api/board/webauthn/test/last-pairing-code is available.
  boardPage: async ({ page, baseURL, request }, use) => {
    await openBoardSession(page, baseURL || '', request);

    // Enroll a passkey from the browser context (has session + board cookies).
    //    Uses the TestMode-only endpoint to retrieve the pairing code without a
    //    macOS notification (the server is started with TestMode: true for e2e).
    await page.evaluate(async (token: string) => {
      function b64uToAb(b: string): ArrayBuffer {
        const pad = b.replace(/-/g, '+').replace(/_/g, '/');
        const bin = atob(pad);
        const buf = new Uint8Array(bin.length);
        for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
        return buf.buffer;
      }
      function abToB64u(buf: ArrayBuffer): string {
        const bytes = new Uint8Array(buf);
        let bin = '';
        for (const b of bytes) bin += String.fromCharCode(b);
        return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      }

      const headers: Record<string, string> = { 'Authorization': `Bearer ${token}` };

      // Clear any credentials left from a previous test so this test gets a clean
      // virtual authenticator state. The endpoint is TestMode-only.
      await fetch('/api/board/webauthn/test/clear-credentials', { method: 'DELETE', headers });

      // begin registration
      const br = await fetch('/api/board/webauthn/register/begin', {
        method: 'POST', headers, body: JSON.stringify({}),
      });
      if (!br.ok) throw new Error(`register/begin failed: ${br.status} ${await br.text()}`);
      const regSession = br.headers.get('X-WebAuthn-Session');
      if (!regSession) throw new Error('register/begin: missing X-WebAuthn-Session header');
      const regOpts = await br.json() as { publicKey: Record<string, unknown> };

      const pk = regOpts.publicKey as {
        challenge: string;
        user: { id: string; [k: string]: unknown };
        excludeCredentials?: Array<{ id: string; type: string }>;
        [k: string]: unknown;
      };
      pk.challenge = b64uToAb(pk.challenge) as unknown as string;
      pk.user.id = b64uToAb(pk.user.id as string) as unknown as string;
      if (pk.excludeCredentials) {
        pk.excludeCredentials = pk.excludeCredentials.map((c) => ({
          ...c, id: b64uToAb(c.id) as unknown as string,
        }));
      }

      // CDP handles navigator.credentials.create()
      const newCred = await navigator.credentials.create({ publicKey: pk as unknown as PublicKeyCredentialCreationOptions }) as PublicKeyCredential;
      const regResp = newCred.response as AuthenticatorAttestationResponse;

      // Retrieve pairing code from TestMode-only endpoint (board cookie auto-included).
      const codeRes = await fetch('/api/board/webauthn/test/last-pairing-code', { headers });
      if (!codeRes.ok) throw new Error(`last-pairing-code failed: ${codeRes.status}`);
      const { code } = await codeRes.json() as { code: string };
      if (!code) throw new Error('last-pairing-code: no code available after register/begin');

      const credential = {
        id: newCred.id,
        rawId: abToB64u(newCred.rawId),
        type: newCred.type,
        response: {
          clientDataJSON: abToB64u(regResp.clientDataJSON),
          attestationObject: abToB64u(regResp.attestationObject),
        },
      };

      const fr = await fetch('/api/board/webauthn/register/finish', {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json', 'X-WebAuthn-Session': regSession },
        body: JSON.stringify({ code, credential }),
      });
      if (!fr.ok) {
        const e = await fr.json().catch(() => ({})) as { message?: string };
        throw new Error(`register/finish failed: ${fr.status}: ${e.message || JSON.stringify(e)}`);
      }
    }, TOKEN);

    await use(page);
  },
  // boardSessionPage has the Board session cookie and a CDP virtual
  // authenticator, but no passkey is enrolled: every stored credential is
  // wiped through the TestMode-only clear-credentials endpoint (STA-694).
  boardSessionPage: async ({ page, baseURL, request }, use) => {
    await openBoardSession(page, baseURL || '', request);
    const clear = await page.request.delete('/api/board/webauthn/test/clear-credentials', {
      headers: { Authorization: `Bearer ${TOKEN}` },
    });
    if (!clear.ok()) {
      throw new Error(`clear-credentials -> ${clear.status()}: ${await clear.text()}`);
    }
    await use(page);
  },
});

export { expect };
