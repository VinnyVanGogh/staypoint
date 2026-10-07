/* Pure, DOM-free quota gauge helpers shared by app.js and Node unit tests.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 *
 * The API marks a gauge (or one window of it) measured=false when nothing
 * reported it. Those numbers are placeholders, so they must render as
 * "No data", never as "0.0% used". Older payloads without the flags keep the
 * previous behaviour.
 */
(function (root) {
  'use strict';

  const clamp = (v) => Math.max(0, Math.min(100, v));

  // quotaWindowMeasured reports whether window ('five_hour' | 'weekly') of q
  // carries a real reading.
  function quotaWindowMeasured(q, win) {
    if (!q) return false;
    if (q.measured === false) return false;
    if (q[`${win}_measured`] === false) return false;
    return true;
  }

  // quotaWindowView normalises one window for display.
  // Returns { measured, used, remaining, text }. used/remaining are null when
  // unmeasured.
  function quotaWindowView(q, win) {
    if (!quotaWindowMeasured(q, win)) {
      return { measured: false, used: null, remaining: null, text: 'No data' };
    }
    let remaining = q[`${win}_remaining_pct`];
    let used = q[`${win}_used_pct`];
    if (remaining == null && used != null) remaining = Math.max(0, 100 - used);
    if (used == null && remaining != null) used = Math.max(0, 100 - remaining);
    if (remaining == null && used == null) { remaining = 100; used = 0; }
    if (remaining === 100 && used > 0) remaining = Math.max(0, 100 - used);
    remaining = clamp(remaining);
    used = clamp(used);
    return {
      measured: true,
      used,
      remaining,
      text: `${used.toFixed(1)}% used · ${remaining.toFixed(1)}% left`,
    };
  }

  // quotaStatusPill picks the header pill for a gauge.
  function quotaStatusPill(q) {
    if (q && (q.is_locked || q.projection_status === 'locked_out')) {
      return { cls: 'pill-red', text: '✖ Locked Out' };
    }
    if (!q || q.measured === false || q.projection_status === 'unknown') {
      return { cls: 'pill-stopped', text: 'No data' };
    }
    if (q.projection_status === 'overpaced') {
      return { cls: 'pill-amber', text: '⚠ Overpaced' };
    }
    return { cls: 'pill-green', text: '✔ On Track' };
  }

  const api = { quotaWindowMeasured, quotaWindowView, quotaStatusPill };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  Object.assign(root, api);
})(typeof globalThis !== 'undefined' ? globalThis : this);
