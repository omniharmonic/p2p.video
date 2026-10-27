// p2p.video mark, wordmark and AGPL source offer [SL-REL-1].
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const B = await load('../src/lib/brandmark.ts');

test('the source offer points at the public repository and names the build', () => {
  assert.equal(B.SOURCE_URL, 'https://github.com/omniharmonic/p2p.video');
  const html = B.sourceLinkHTML();
  assert.match(html, /href="https:\/\/github\.com\/omniharmonic\/p2p\.video"/);
  assert.match(html, /data-source-link/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.match(B.sourceTitle('20261027T101500Z-ABC'), /AGPL v3.*build 20261027T101500Z-ABC\./);
  assert.doesNotMatch(B.sourceTitle('<script>'), /script/, 'odd build IDs are not echoed');
  assert.doesNotMatch(B.sourceTitle(null), /build/);
});

test('the wordmark is the lowercase domain and the static pages carry the same mark', async () => {
  assert.equal(B.WORDMARK_HTML.replace(/<[^>]+>/g, ''), 'p2p.video');
  const path = /<path class="brand-lens" d="([^"]+)"/.exec(B.BRAND_MARK_SVG)[1];
  for (const page of ['../index.html', '../record.html']) {
    const html = await readFile(new URL(page, import.meta.url), 'utf8');
    assert.ok(html.includes(path), `${page} has the two-ring mark`);
    assert.match(html, /<span class="wordmark-tld">\.video<\/span>/, `${page} has the wordmark`);
    assert.match(html, /data-source-link href="https:\/\/github\.com\/omniharmonic\/p2p\.video"/, `${page} offers the source`);
    assert.doesNotMatch(html, /StudioLink/, `${page} still says StudioLink`);
  }
});
