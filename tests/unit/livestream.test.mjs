// Unit tests for the shared live stream (task-53fcbcff): many tabs, one
// EventSource. Run with: node --test tests/unit/livestream.test.mjs
//
// Tabs are simulated in one process: Node's real BroadcastChannel carries the
// relay, a fake Web Locks implements queueing and steal, and a fake
// EventSource records every connection opened.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { createLiveStream } = require('../../internal/server/webui/lib/livestream.js');

const settle = () => new Promise((r) => setTimeout(r, 20));

class FakeLocks {
  constructor() { this.holder = null; this.queue = []; }
  request(name, opts, cb) {
    return new Promise((resolve, reject) => {
      const req = { cb, resolve, reject };
      if (opts.steal) {
        if (this.holder) {
          const old = this.holder;
          this.holder = null;
          old.reject(Object.assign(new Error('stolen'), { name: 'AbortError' }));
        }
        this.grant(req);
        return;
      }
      opts.signal?.addEventListener('abort', () => {
        const i = this.queue.indexOf(req);
        if (i >= 0) { this.queue.splice(i, 1); reject(Object.assign(new Error('aborted'), { name: 'AbortError' })); }
      });
      if (this.holder) this.queue.push(req);
      else this.grant(req);
    });
  }
  grant(req) {
    this.holder = req;
    Promise.resolve().then(() => req.cb()).then((v) => {
      if (this.holder === req) { this.holder = null; this.next(); }
      req.resolve(v);
    });
  }
  next() {
    const r = this.queue.shift();
    if (r) this.grant(r);
  }
}

function fakeEventSourceClass(opened) {
  return class FakeEventSource {
    constructor(url) {
      this.url = url;
      this.closed = false;
      opened.push(this);
    }
    close() { this.closed = true; }
    open() { this.onopen?.(); }
    emit(evt) { this.onmessage?.({ data: JSON.stringify(evt) }); }
    fail() { this.onerror?.(); }
  };
}

// browser builds the shared env for one simulated browser (one origin).
function browser({ locks = true } = {}) {
  const opened = [];
  const timers = [];
  const env = {
    EventSource: fakeEventSourceClass(opened),
    BroadcastChannel,
    AbortController,
    locks: locks ? new FakeLocks() : undefined,
    setTimeout: (fn) => { const t = { fn, done: false }; timers.push(t); return t; },
    clearTimeout: (t) => { if (t) t.done = true; },
    setInterval: () => 1,
    clearInterval: () => {},
  };
  const tabs = [];
  return {
    locks: env.locks,
    opened,
    live: () => opened.filter((es) => !es.closed),
    runTimers() {
      for (const t of timers.splice(0)) if (!t.done) { t.done = true; t.fn(); }
    },
    openTab({ visible = true } = {}) {
      const tab = { events: [], statuses: [] };
      tab.document = { visibilityState: visible ? 'visible' : 'hidden', addEventListener() {} };
      tab.stream = createLiveStream({
        baseUrl: '/api/events',
        token: 'tok',
        onEvent: (e) => tab.events.push(e),
        onStatus: (s) => tab.statuses.push(s),
        env: { ...env, document: tab.document },
      });
      tabs.push(tab);
      return tab;
    },
    closeAll() { for (const t of tabs) t.stream.close(); },
  };
}

test('eight tabs share one EventSource', async () => {
  const b = browser();
  const tabs = Array.from({ length: 8 }, () => b.openTab());
  await settle();
  assert.equal(b.opened.length, 1, 'exactly one stream opened');
  assert.equal(tabs.filter((t) => t.stream.role() === 'leader').length, 1);
  assert.match(b.opened[0].url, /^\/api\/events\?token=tok$/);
  b.closeAll();
});

test('leader relays status and events to every tab', async () => {
  const b = browser();
  const tabs = Array.from({ length: 8 }, () => b.openTab());
  await settle();
  const es = b.live()[0];
  es.open();
  es.emit({ id: 7, type: 'task.updated', data: { id: 't1' } });
  await settle();
  for (const t of tabs) {
    assert.equal(t.stream.status(), 'live');
    assert.deepEqual(t.events.map((e) => e.id), [7]);
  }
  b.closeAll();
});

test('a tab opened after the stream is live learns the status at once', async () => {
  const b = browser();
  b.openTab();
  await settle();
  b.live()[0].open();
  const late = b.openTab();
  await settle();
  assert.equal(late.stream.role(), 'follower');
  assert.equal(late.stream.status(), 'live');
  assert.equal(b.opened.length, 1);
  b.closeAll();
});

test('closing the leader hands the stream to another tab, resuming from the cursor', async () => {
  const b = browser();
  const tabs = Array.from({ length: 3 }, () => b.openTab());
  await settle();
  const leader = tabs.find((t) => t.stream.role() === 'leader');
  const es = b.live()[0];
  es.open();
  es.emit({ id: 41, type: 'x' });
  await settle();
  leader.stream.close();
  await settle();
  assert.equal(b.live().length, 1, 'one stream after hand-off');
  assert.match(b.live()[0].url, /cursor=41/);
  assert.equal(tabs.filter((t) => t !== leader && t.stream.role() === 'leader').length, 1);
  b.closeAll();
});

test('stream error shows reconnecting in every tab and retries on the leader only', async () => {
  const b = browser();
  const tabs = Array.from({ length: 3 }, () => b.openTab());
  await settle();
  b.live()[0].open();
  b.live()[0].fail();
  await settle();
  for (const t of tabs) assert.equal(t.stream.status(), 'reconnecting');
  assert.equal(b.live().length, 0);
  b.runTimers();
  assert.equal(b.live().length, 1, 'leader reopened one stream');
  b.live()[0].open();
  await settle();
  for (const t of tabs) assert.equal(t.stream.status(), 'live');
  b.closeAll();
});

test('a visible tab replaces a leader that does not answer (frozen tab)', async () => {
  const b = browser();
  // A frozen leader: holds the lock, never answers on the channel.
  let unfreeze;
  b.locks.request('staypoint-live-sse', {}, () => new Promise((r) => { unfreeze = r; })).catch(() => {}); // rejected when stolen
  await settle();
  const tab = b.openTab();
  await settle();
  assert.equal(tab.stream.role(), 'follower', 'waits behind the frozen holder');
  assert.equal(b.opened.length, 0);
  b.runTimers(); // ping timed out
  await settle();
  assert.equal(tab.stream.role(), 'leader', 'stole the lock');
  assert.equal(b.live().length, 1);
  b.closeAll();
  unfreeze();
});

test('a leader whose lock is stolen closes its stream and waits in line again', async () => {
  const b = browser();
  const a = b.openTab();
  await settle();
  assert.equal(a.stream.role(), 'leader');
  const first = b.live()[0];
  let release;
  b.locks.request('staypoint-live-sse', { steal: true }, () => new Promise((r) => { release = r; }));
  await settle();
  assert.equal(a.stream.role(), 'follower');
  assert.ok(first.closed, 'old leader closed its stream');
  release();
  await settle();
  assert.equal(a.stream.role(), 'leader', 'took the lock back when it was free');
  assert.equal(b.live().length, 1);
  b.closeAll();
});

test('hidden tabs do not ping or steal', async () => {
  const b = browser();
  b.openTab();
  await settle();
  const hidden = b.openTab({ visible: false });
  await settle();
  b.runTimers();
  await settle();
  assert.equal(hidden.stream.role(), 'follower');
  assert.equal(b.opened.length, 1);
  b.closeAll();
});

test('without Web Locks each tab streams for itself', async () => {
  const b = browser({ locks: false });
  const tabs = [b.openTab(), b.openTab()];
  await settle();
  assert.equal(b.opened.length, 2);
  for (const t of tabs) assert.equal(t.stream.role(), 'leader');
  b.closeAll();
});
