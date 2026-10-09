/* One live event stream per browser, shared by every StayPoint tab.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 *
 * Each EventSource holds an HTTP/1.1 connection open for good, and browsers
 * allow six per host. Five tabs with their own stream left one connection for
 * every other fetch, so the UI froze on "connecting" and "Loading..."
 * (task-53fcbcff). Here one tab, the leader, holds the only EventSource and
 * relays each event to the other tabs over a BroadcastChannel.
 *
 * The leader is whichever tab holds a Web Lock. When it closes, the browser
 * hands the lock to the next waiting tab, which opens the stream from the last
 * event id it relayed, so the server replays anything missed. A leader that
 * stops answering (a frozen or sleeping tab) is replaced: a visible follower
 * pings it, and if nothing answers it steals the lock. Browsers without Web
 * Locks or BroadcastChannel fall back to one stream per tab.
 */
(function (root) {
  'use strict';

  const LOCK_NAME = 'staypoint-live-sse';
  const CHANNEL_NAME = 'staypoint-live';
  const RETRY_MS = 3000;
  const PING_EVERY_MS = 30000;
  const PING_TIMEOUT_MS = 5000;

  // createLiveStream starts the shared stream for this tab.
  //   baseUrl   '/api/events'
  //   token     appended as ?token= when set
  //   onEvent   called with each parsed event, in every tab
  //   onStatus  called with 'connecting' | 'live' | 'reconnecting' on change
  //   env       browser APIs, injectable for tests: EventSource,
  //             BroadcastChannel, locks, document, setTimeout, clearTimeout,
  //             setInterval
  // Returns { status(), role(), close() }.
  function createLiveStream(opts) {
    const env = Object.assign({
      EventSource: root.EventSource,
      BroadcastChannel: root.BroadcastChannel,
      locks: root.navigator && root.navigator.locks,
      AbortController: root.AbortController,
      document: root.document,
      setTimeout: root.setTimeout && root.setTimeout.bind(root),
      clearTimeout: root.clearTimeout && root.clearTimeout.bind(root),
      setInterval: root.setInterval && root.setInterval.bind(root),
      clearInterval: root.clearInterval && root.clearInterval.bind(root),
    }, opts.env || {});
    const onEvent = opts.onEvent || (() => {});
    const onStatus = opts.onStatus || (() => {});

    let status = 'connecting';
    let role = 'follower';
    let cursor = null;
    let source = null;
    let retryTimer = null;
    let pingTimer = null;
    let pingInterval = null;
    let releaseLock = null;
    let queued = null; // AbortController of this tab's waiting lock request
    let closed = false;
    let channel = null;

    function setStatus(s) {
      if (s === status) return;
      status = s;
      onStatus(s);
    }

    function deliver(evt) {
      if (evt && evt.id != null) cursor = evt.id;
      onEvent(evt);
    }

    function streamURL() {
      let url = cursor != null ? `${opts.baseUrl}?cursor=${cursor}` : opts.baseUrl;
      if (opts.token) url += `${url.includes('?') ? '&' : '?'}token=${encodeURIComponent(opts.token)}`;
      return url;
    }

    function post(msg) {
      try { channel && channel.postMessage(msg); } catch { /* channel closed */ }
    }

    function openSource() {
      if (closed || (channel && role !== 'leader')) return;
      if (source) { source.close(); source = null; }
      const es = new env.EventSource(streamURL());
      source = es;
      es.onopen = () => {
        if (source !== es) return;
        if (retryTimer) { env.clearTimeout(retryTimer); retryTimer = null; }
        setStatus('live');
        post({ type: 'status', status: 'live' });
      };
      es.onerror = () => {
        if (source !== es) return;
        es.close();
        source = null;
        setStatus('reconnecting');
        post({ type: 'status', status: 'reconnecting' });
        retryTimer = env.setTimeout(() => { retryTimer = null; openSource(); }, RETRY_MS);
      };
      es.onmessage = (ev) => {
        if (source !== es) return;
        let evt;
        try { evt = JSON.parse(ev.data); } catch { return; }
        deliver(evt);
        post({ type: 'event', evt });
      };
    }

    function stopSource() {
      if (retryTimer) { env.clearTimeout(retryTimer); retryTimer = null; }
      if (source) { source.close(); source = null; }
    }

    // Fallback: no lock or channel API, so every tab streams for itself.
    if (!env.locks || !env.BroadcastChannel || !env.AbortController) {
      role = 'leader';
      openSource();
      return {
        status: () => status,
        role: () => role,
        close() { closed = true; stopSource(); },
      };
    }

    channel = new env.BroadcastChannel(CHANNEL_NAME);
    channel.onmessage = (ev) => {
      const msg = ev.data || {};
      if (role === 'leader') {
        if (msg.type === 'ping' || msg.type === 'hello') post({ type: 'status', status });
        return;
      }
      if (msg.type === 'event') {
        deliver(msg.evt);
        gotLeaderMessage();
      } else if (msg.type === 'status') {
        setStatus(msg.status);
        gotLeaderMessage();
      }
    };

    function gotLeaderMessage() {
      if (pingTimer) { env.clearTimeout(pingTimer); pingTimer = null; }
    }

    // A visible follower checks that a leader is answering. Background
    // followers skip it: nobody is looking, and a visible tab will check.
    function checkLeader() {
      if (closed || role !== 'follower' || pingTimer) return;
      if (env.document && env.document.visibilityState === 'hidden') return;
      post({ type: 'ping' });
      pingTimer = env.setTimeout(() => {
        pingTimer = null;
        if (!closed && role === 'follower') acquire(true);
      }, PING_TIMEOUT_MS);
    }

    function becomeLeader(release) {
      gotLeaderMessage();
      releaseLock = release;
      role = 'leader';
      openSource();
    }

    function becomeFollower() {
      stopSource();
      role = 'follower';
      releaseLock = null;
    }

    // acquire waits in line for the lock (or takes it, when steal is set) and
    // leads while it holds it. The lock is held until close() or until another
    // tab steals it, which rejects this request. A tab keeps at most one
    // request in line: stealing cancels the queued one.
    function acquire(steal) {
      if (closed) return;
      if (queued) { queued.abort(); queued = null; }
      const ctl = steal ? null : new env.AbortController();
      queued = ctl;
      const lockOpts = steal ? { steal: true } : { signal: ctl.signal };
      env.locks.request(LOCK_NAME, lockOpts, () => {
        if (queued === ctl) queued = null;
        return new Promise((resolve) => {
          if (closed) { resolve(); return; }
          becomeLeader(resolve);
        });
      }).then(() => {
        if (role === 'leader') becomeFollower();
      }, () => {
        if (ctl && ctl.signal.aborted) return; // this tab cancelled it
        // Stolen by a tab that thought this one was unresponsive. Wait in line
        // again; the new leader's status messages keep this tab live.
        if (role === 'leader') becomeFollower();
        if (!closed) acquire(false);
      });
    }

    acquire(false);
    // Ask a current leader for its status so this tab's badge is right at once.
    post({ type: 'hello' });
    checkLeader();
    pingInterval = env.setInterval(checkLeader, PING_EVERY_MS);
    if (env.document && env.document.addEventListener) {
      env.document.addEventListener('visibilitychange', checkLeader);
    }

    return {
      status: () => status,
      role: () => role,
      close() {
        closed = true;
        stopSource();
        gotLeaderMessage();
        if (pingInterval) env.clearInterval(pingInterval);
        if (queued) { queued.abort(); queued = null; }
        if (releaseLock) releaseLock();
        try { channel.close(); } catch { /* already closed */ }
      },
    };
  }

  const api = { createLiveStream };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  Object.assign(root, api);
})(typeof globalThis !== 'undefined' ? globalThis : this);
