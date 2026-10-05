/* Simple markdown renderer shared by app.js and Node unit tests.
 *
 * Escape-first: the input is HTML-escaped before any markdown rule runs, so
 * every tag in the output comes from a rule below, never from the input.
 *
 * Works as both a classic browser script (exposes globals on globalThis) and a
 * Node CommonJS import (module.exports = api).
 */
(function (root) {
  'use strict';

  function escapeHtml(s) {
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function renderMarkdown(text) {
    if (!text) return '';
    // NUL delimits code placeholders below, so it must not survive from the input.
    let s = escapeHtml(text).replace(/\u0000/g, '');
    // Code is rendered first and parked behind placeholders so the inline and
    // block rules below never touch its contents.
    const code = [];
    const park = html => `\u0000${code.push(html) - 1}\u0000`;
    s = s.replace(/```[\w]*\n?([\s\S]*?)```/g, (_, c) =>
      park(`<pre><code>${c.trimEnd()}</code></pre>`));
    s = s.replace(/`([^`\n]+)`/g, (_, c) => park(`<code>${c}</code>`));
    s = s.replace(/^### (.+)$/gm, '<h3>$1</h3>');
    s = s.replace(/^## (.+)$/gm, '<h2>$1</h2>');
    s = s.replace(/^# (.+)$/gm, '<h1>$1</h1>');
    s = s.replace(/\*\*\*([^*]+)\*\*\*/g, '<strong><em>$1</em></strong>');
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/\*(?=\S)([^*\n]*?\S)\*/g, '<em>$1</em>');
    // _x_ only at word boundaries, so snake_case names stay literal.
    s = s.replace(/(^|\W)_(?=\S)([^\n]*?\S)_(?!\w)/gm, '$1<em>$2</em>');
    s = s.replace(/^&gt; (.+)$/gm, '<blockquote>$1</blockquote>');
    s = s.replace(/^[-*] (.+)$/gm, '<li>$1</li>');
    s = s.replace(/(<li>[\s\S]*?<\/li>)(\n<li>[\s\S]*?<\/li>)*/g, m => `<ul>${m}</ul>`);
    s = s.replace(/^\d+\. (.+)$/gm, '<li>$1</li>');
    s = s.replace(/^---+$/gm, '<hr>');
    s = s.replace(/\u0000(\d+)\u0000/g, (_, i) => code[i]);
    const lines = s.split(/\n\n+/);
    const wrapped = lines.map(chunk => {
      chunk = chunk.trim();
      if (!chunk) return '';
      if (/^<(h[1-6]|ul|ol|pre|blockquote|hr)/.test(chunk)) return chunk;
      return `<p>${chunk.replace(/\n/g, '<br>')}</p>`;
    });
    return wrapped.join('\n');
  }

  const api = { escapeHtml, renderMarkdown };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  } else {
    Object.assign(root, api);
  }
})(globalThis);
