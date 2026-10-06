const fs = require('fs');
const file = 'internal/server/webui/app.js';
let content = fs.readFileSync(file, 'utf8');

content = content.replace(
  /function buildTimelineRun\(runId\) \{\s*const run = el\('div', 'timeline-run'\);\s*run\.setAttribute\('data-run-id', runId \|\| ''\);\s*return run;\s*\}/,
  `function buildTimelineRun(runId) {\n  const run = el('details', 'timeline-run');\n  run.open = true;\n  run.setAttribute('data-run-id', runId || '');\n  return run;\n}`
);

content = content.replace(
  /function syncTimelineRunHeaders\(stepList, allSteps\) \{([\s\S]*?)hdr\.textContent = `Run \$\{runNum\}` \+ \(startTime \? `  ·  started \$\{startTime\}` : ''\) \+ \(endTime \? `  ·  ended \$\{endTime\}` : ''\);\s*\}/,
  `function syncTimelineRunHeaders(stepList, allSteps) {
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
    if (!hdr) { hdr = el('summary', 'timeline-run-header'); runEl.prepend(hdr); }
    const firstStep = group[0];
    const stateStep = [...group].reverse().find(s => s.kind === 'state');
    const startTime = firstStep ? fmtDateTime(firstStep.created_at) : '';
    const endTime = stateStep ? fmtDateTime(stateStep.created_at) : '';
    hdr.textContent = \`Run \$\{runNum\} (\$\{group.length\} actions)\` + (startTime ? \`  ·  started \$\{startTime\}\` : '') + (endTime ? \`  ·  ended \$\{endTime\}\` : '');
  }
}`
);

fs.writeFileSync(file, content, 'utf8');
