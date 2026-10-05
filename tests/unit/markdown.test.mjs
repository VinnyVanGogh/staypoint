// Unit tests for renderMarkdown italics (STA-696): `_x_` and `*x*` render as
// <em>, snake_case identifiers and code spans stay literal, and escaping still
// runs first.
// Run with: node --test tests/unit/markdown.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { renderMarkdown } = require('../../internal/server/webui/lib/markdown.js');

test('_underscore_ italic renders as <em>', () => {
  assert.equal(
    renderMarkdown('_Backfilled by the Board: summary_'),
    '<p><em>Backfilled by the Board: summary</em></p>',
  );
  assert.equal(renderMarkdown('a _word_ here'), '<p>a <em>word</em> here</p>');
});

test('*asterisk* italic renders as <em>', () => {
  assert.equal(renderMarkdown('a *word* here'), '<p>a <em>word</em> here</p>');
});

test('bold still wins over italic', () => {
  assert.equal(renderMarkdown('**bold** and _it_'), '<p><strong>bold</strong> and <em>it</em></p>');
});

test('snake_case identifiers inside words are not italicised', () => {
  assert.equal(renderMarkdown('set about_values_title now'), '<p>set about_values_title now</p>');
  assert.equal(renderMarkdown('a_b_c and x_y_'), '<p>a_b_c and x_y_</p>');
  assert.equal(
    renderMarkdown('see https://example.com/a_b_c/d'),
    '<p>see https://example.com/a_b_c/d</p>',
  );
});

test('italic may contain a snake_case word', () => {
  assert.equal(renderMarkdown('_uses about_values_title_'), '<p><em>uses about_values_title</em></p>');
});

test('code spans are never italicised or bolded', () => {
  assert.equal(renderMarkdown('run `_x_` now'), '<p>run <code>_x_</code> now</p>');
  assert.equal(renderMarkdown('`a *b* c`'), '<p><code>a *b* c</code></p>');
  assert.equal(renderMarkdown('`**not bold**`'), '<p><code>**not bold**</code></p>');
});

test('fenced code blocks are never italicised', () => {
  assert.equal(
    renderMarkdown('```\nfoo _bar_ *baz*\n```'),
    '<pre><code>foo _bar_ *baz*</code></pre>',
  );
});

test('lone or spaced markers are left alone', () => {
  assert.equal(renderMarkdown('a * b * c'), '<p>a * b * c</p>');
  assert.equal(renderMarkdown('a _ b _ c'), '<p>a _ b _ c</p>');
  assert.equal(renderMarkdown('_unclosed'), '<p>_unclosed</p>');
});

test('italic content is still HTML-escaped (escape-first)', () => {
  assert.equal(
    renderMarkdown('_<img src=x onerror=alert(1)>_'),
    '<p><em>&lt;img src=x onerror=alert(1)&gt;</em></p>',
  );
  assert.equal(
    renderMarkdown('*<script>x</script>*'),
    '<p><em>&lt;script&gt;x&lt;/script&gt;</em></p>',
  );
});

test('NUL characters in the input cannot forge a code placeholder', () => {
  const out = renderMarkdown('`a` \u00000\u0000 _b_');
  assert.ok(!out.includes('\u0000'), out);
  assert.equal((out.match(/<code>/g) || []).length, 1, out);
  assert.ok(out.includes('<em>b</em>'), out);
});

test('bullet lists with * markers still render', () => {
  assert.equal(renderMarkdown('* one\n* two'), '<ul><li>one</li>\n<li>two</li></ul>');
});
