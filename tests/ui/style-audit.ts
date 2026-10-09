import type { Page } from '@playwright/test';
import * as fs from 'node:fs';
import * as path from 'node:path';

// Finds form controls the app's stylesheet never reached: they still have the
// browser's own look. The reference values come from a control of the same
// tag inside a shadow root, which document styles cannot touch, so this works
// for whatever browser defaults Chromium ships.

export type UnstyledControl = {
  /** Short CSS-ish path to the element, e.g. `button.run-children-btn`. */
  selector: string;
  tag: string;
  classes: string;
  text: string;
  /** Which default leaked through: 'background+border' and/or 'font'. */
  reasons: string[];
  rect: { x: number; y: number; width: number; height: number };
};

const MARK = 'data-style-audit-id';

/**
 * Returns every visible button/input/select/textarea on the page whose
 * computed style is still the browser default, and tags each one with a
 * data-style-audit-id so callers can screenshot it.
 */
export async function findUnstyledControls(page: Page): Promise<UnstyledControl[]> {
  return page.evaluate((mark) => {
    const skipTypes = new Set(['checkbox', 'radio', 'hidden', 'range', 'file', 'color', 'image']);
    const host = document.createElement('div');
    host.style.cssText = 'position:absolute;left:-9999px;top:0;';
    document.body.appendChild(host);
    const root = host.attachShadow({ mode: 'closed' });
    const refs: Record<string, CSSStyleDeclaration> = {};
    for (const tag of ['button', 'input', 'select', 'textarea']) {
      const r = document.createElement(tag);
      root.appendChild(r);
      refs[tag] = getComputedStyle(r);
    }
    const snapshot = (cs: CSSStyleDeclaration) => ({
      bg: cs.backgroundColor,
      bs: cs.borderTopStyle,
      bw: cs.borderTopWidth,
      fs: cs.fontSize,
      ff: cs.fontFamily,
    });
    const ref: Record<string, ReturnType<typeof snapshot>> = {};
    for (const [tag, cs] of Object.entries(refs)) ref[tag] = snapshot(cs);
    host.remove();

    const out: Array<Record<string, unknown>> = [];
    let n = 0;
    for (const elem of Array.from(document.querySelectorAll('button, input, select, textarea'))) {
      const e = elem as HTMLElement;
      const tag = e.tagName.toLowerCase();
      if (tag === 'input' && skipTypes.has(((e as HTMLInputElement).type || '').toLowerCase())) continue;
      const rect = e.getBoundingClientRect();
      const cs = getComputedStyle(e);
      if (rect.width === 0 || rect.height === 0 || cs.visibility === 'hidden' || cs.display === 'none') continue;
      const s = snapshot(cs);
      const r = ref[tag];
      const reasons: string[] = [];
      if (s.bg === r.bg && s.bs === r.bs && s.bw === r.bw) reasons.push('background+border');
      if (s.fs === r.fs && s.ff === r.ff) reasons.push('font');
      if (!reasons.length) continue;
      const id = String(n++);
      e.setAttribute(mark, id);
      const cls = Array.from(e.classList).join('.');
      out.push({
        selector: `${tag}${e.id ? '#' + e.id : ''}${cls ? '.' + cls : ''}`,
        tag,
        classes: e.className || '',
        text: (e.innerText || (e as HTMLInputElement).placeholder || (e as HTMLInputElement).value || '').trim().slice(0, 60),
        reasons,
        rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
        id,
      });
    }
    return out as unknown as UnstyledControl[];
  }, MARK);
}

/**
 * Screenshots each finding (with some surrounding context) into dir and
 * appends one JSON line per finding to dir/findings.jsonl. Used to build
 * docs/ui-style-audit.md; the regression check only needs the list.
 */
export async function recordFindings(page: Page, where: string, found: UnstyledControl[], dir: string) {
  fs.mkdirSync(dir, { recursive: true });
  const slug = where.replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '').toLowerCase();
  for (const [i, f] of found.entries()) {
    const file = path.join(dir, `${slug}-${i}.png`);
    const loc = page.locator(`[${MARK}="${(f as UnstyledControl & { id: string }).id}"]`);
    await loc.scrollIntoViewIfNeeded().catch(() => {});
    const box = (await loc.boundingBox().catch(() => null)) || f.rect;
    const pad = 60;
    const vp = page.viewportSize() || { width: 1512, height: 900 };
    const x = Math.max(0, box.x - pad * 3);
    const y = Math.max(0, box.y - pad);
    const clip = {
      x, y,
      width: Math.min(vp.width - x, box.width + pad * 6),
      height: Math.min(vp.height - y, box.height + pad * 2),
    };
    try {
      await page.screenshot({ path: file, clip: clip.width > 0 && clip.height > 0 ? clip : undefined });
    } catch {
      await page.screenshot({ path: file });
    }
    fs.appendFileSync(path.join(dir, 'findings.jsonl'), JSON.stringify({ where, ...f, screenshot: path.basename(file) }) + '\n');
  }
}

export function describeFindings(found: UnstyledControl[]): string {
  return found.map(f => `${f.selector} "${f.text}" [${f.reasons.join(', ')}]`).join('\n');
}
