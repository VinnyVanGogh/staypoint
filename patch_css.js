const fs = require('fs');
const file = 'internal/server/webui/style.css';
let content = fs.readFileSync(file, 'utf8');

content = content.replace(
  /\.timeline-run-header \{([\s\S]*?)margin-top: 4px;\s*\}/,
  `.timeline-run-header {$1margin-top: 4px;\n  cursor: pointer;\n  user-select: none;\n}`
);

fs.writeFileSync(file, content, 'utf8');
