// Reflect view: computed facts for a period, then the cited LLM summary.
// All data is rendered with textContent; links only to same-origin paths.
(function () {
  const PERIODS = [['30d', 'Month'], ['90d', '3 months'], ['180d', '6 months'], ['365d', 'Year']];
  const SECTIONS = [
    ['themes', 'Themes'],
    ['frustrations', 'Frustrations & corrections'],
    ['breakages', 'What kept breaking'],
    ['decisions', 'Decisions'],
    ['suggestions', 'Suggestions'],
  ];
  const state = { since: localStorage.getItem('staypoint_reflect_since') || '30d', poll: null };

  function node(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function fmt(n) {
    if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return String(n || 0);
  }

  function refLink(r) {
    const label = r.kind + ':' + (r.label || r.id);
    if (r.url && r.url.startsWith('/')) {
      const a = node('a', 'reflect-ref', label);
      a.href = r.url;
      a.title = r.kind + ' ' + r.id;
      return a;
    }
    const s = node('span', 'reflect-ref', label);
    s.title = r.kind + ' ' + r.id;
    return s;
  }

  function refs(list) {
    const wrap = node('span', 'reflect-refs');
    (list || []).forEach((r, i) => {
      if (i) wrap.appendChild(document.createTextNode(' '));
      wrap.appendChild(refLink(r));
    });
    return wrap;
  }

  function kpi(label, value, sub) {
    const c = node('div', 'kpi-card');
    c.appendChild(node('div', 'kpi-label', label));
    c.appendChild(node('div', 'kpi-value', value));
    if (sub) c.appendChild(node('div', 'kpi-sub', sub));
    return c;
  }

  function countTable(title, rows) {
    const box = node('div', 'reflect-box');
    box.appendChild(node('h3', 'reflect-h', title));
    if (!rows || !rows.length) {
      box.appendChild(node('p', 'muted-text', 'Nothing in this period.'));
      return box;
    }
    const t = node('table', 'reflect-table');
    rows.forEach((r) => {
      const tr = node('tr');
      tr.appendChild(node('td', 'reflect-n', String(r.count)));
      const k = node('td', 'reflect-key', r.key);
      tr.appendChild(k);
      const rc = node('td');
      rc.appendChild(refs((r.refs || []).slice(0, 3)));
      tr.appendChild(rc);
      t.appendChild(tr);
    });
    box.appendChild(t);
    return box;
  }

  function usageTable(usage) {
    const box = node('div', 'reflect-box');
    box.appendChild(node('h3', 'reflect-h', 'Tokens & cost by seat and model'));
    if (!usage || !usage.length) {
      box.appendChild(node('p', 'muted-text', 'No usage recorded in this period.'));
      return box;
    }
    const t = node('table', 'reflect-table');
    usage.forEach((u) => {
      const tr = node('tr');
      [u.seat, u.model, fmt(u.tokens) + ' tok', '$' + u.cost_usd.toFixed(2), u.requests + ' req'].forEach((v) => tr.appendChild(node('td', '', v)));
      t.appendChild(tr);
    });
    box.appendChild(t);
    return box;
  }

  function summaryBlock(data, container) {
    const box = node('div', 'reflect-box reflect-summary');
    const head = node('div', 'reflect-summary-head');
    head.appendChild(node('h3', 'reflect-h', 'Summary'));
    const btn = node('button', 'sidebar-filter-btn', data.summary ? 'Regenerate' : 'Generate summary');
    btn.type = 'button';
    const job = data.summary_job;
    if (job && job.running) {
      btn.disabled = true;
      btn.textContent = 'Summarising on the personal Claude seat…';
    }
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      btn.textContent = 'Starting…';
      try {
        await apiFetch('/api/reflect/summary?since=' + encodeURIComponent(state.since), { method: 'POST' });
      } catch (e) {
        btn.textContent = 'Failed: ' + e.message;
        return;
      }
      render();
    });
    head.appendChild(btn);
    box.appendChild(head);
    if (job && job.error) box.appendChild(node('p', 'prs-error', 'Last attempt failed: ' + job.error));
    const s = data.summary;
    if (!s) {
      box.appendChild(node('p', 'muted-text', 'No summary for this period yet. It runs on the personal Claude seat (no API key), leaves work data out, and cites every claim.'));
      container.appendChild(box);
      return;
    }
    box.appendChild(node('p', 'muted-text', `${s.model} on the ${s.seat} seat · ${s.corpus_items} evidence items · generated ${new Date(s.generated_at).toLocaleString()}` +
      (s.includes_work ? ' · includes work data' : '') + (s.dropped_uncited_claims ? ` · ${s.dropped_uncited_claims} uncited claims dropped` : '')));
    SECTIONS.forEach(([key, title]) => {
      const claims = s[key] || [];
      if (!claims.length) return;
      box.appendChild(node('h4', 'reflect-sub', title));
      const ul = node('ul', 'reflect-claims');
      claims.forEach((c) => {
        const li = node('li');
        li.appendChild(node('span', '', c.text + ' '));
        li.appendChild(refs(c.sources));
        ul.appendChild(li);
      });
      box.appendChild(ul);
    });
    container.appendChild(box);
  }

  async function render() {
    const container = document.getElementById('reflect-container');
    if (!container) return;
    const bar = document.getElementById('reflect-periods');
    if (bar && !bar.dataset.wired) {
      bar.dataset.wired = '1';
      PERIODS.forEach(([p, label]) => {
        const b = node('button', 'sidebar-filter-btn', label);
        b.type = 'button';
        b.dataset.since = p;
        b.addEventListener('click', () => {
          state.since = p;
          localStorage.setItem('staypoint_reflect_since', p);
          render();
        });
        bar.appendChild(b);
      });
    }
    if (bar) bar.querySelectorAll('button').forEach((b) => b.classList.toggle('active', b.dataset.since === state.since));
    if (!container.childElementCount) container.appendChild(node('p', 'muted-text', 'Loading…'));
    let data;
    try {
      data = await apiFetch('/api/reflect?since=' + encodeURIComponent(state.since));
    } catch (e) {
      container.textContent = '';
      container.appendChild(node('p', 'prs-error', 'Failed to load reflect: ' + e.message));
      return;
    }
    const f = data.facts;
    container.textContent = '';
    const kpis = node('div', 'overview-kpis');
    kpis.appendChild(kpi('Tasks shipped', String(f.tasks_shipped), `${f.tasks_created} created`));
    kpis.appendChild(kpi('Merged', String(f.merged_ship_reviews), `${f.pull_requests_recorded} PRs recorded`));
    kpis.appendChild(kpi('Run hours', f.run_hours.toFixed(1), `${f.runs} runs`));
    kpis.appendChild(kpi('Tokens', fmt(f.total_tokens), '$' + f.total_cost_usd.toFixed(2)));
    if (data.archive) {
      kpis.appendChild(kpi('Archive', `${data.archive.transcripts} transcripts`,
        `${data.archive.ratio.toFixed(1)}x · ~${data.archive.projected_gb_per_year.toFixed(2)} GB/yr`));
    }
    container.appendChild(kpis);

    summaryBlock(data, container);

    const grid = node('div', 'reflect-grid');
    grid.appendChild(countTable('Shipped by org', f.tasks_shipped_by_org));
    grid.appendChild(countTable('Merged by org', f.merged_ship_reviews_by_org));
    grid.appendChild(usageTable(f.usage_by_seat_model));
    grid.appendChild(countTable('Gate requests', f.gate_requests_by_status));
    grid.appendChild(countTable('Board decisions', f.board_decisions));
    grid.appendChild(countTable('Board actions', f.board_audit_events));
    grid.appendChild(countTable('Run failures', f.run_failures));
    grid.appendChild(countTable('Blocked reasons', f.blocked_reasons));
    grid.appendChild(countTable('Most-touched repos', f.most_touched_repos));
    grid.appendChild(countTable('Archived sessions by profile', f.archived_sessions_by_profile));
    container.appendChild(grid);
    (f.warnings || []).forEach((w) => container.appendChild(node('p', 'muted-text', 'Note: ' + w)));

    clearTimeout(state.poll);
    if (data.summary_job && data.summary_job.running) {
      state.poll = setTimeout(() => {
        if (document.getElementById('view-reflect')?.classList.contains('active')) render();
      }, 5000);
    }
  }

  window.renderReflectPage = render;
})();
