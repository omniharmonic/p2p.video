// Renders the illustrated stand-in camera feeds in portraits.html to PNGs.
// Usage: node render.mjs <out-dir>   (PLAYWRIGHT_MODULE points at playwright)
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const out = process.argv[2];
if (!out) throw new Error('usage: node render.mjs <out-dir>');
const here = path.dirname(fileURLToPath(import.meta.url));
const browser = await chromium.launch({headless: true});
try {
  const page = await browser.newPage({viewport: {width: 1280, height: 720}});
  await page.goto(pathToFileURL(path.join(here, 'portraits.html')).href);
  for (const id of ['p0', 'p1', 'p2']) {
    await page.locator('#' + id).screenshot({path: path.join(out, id + '.png')});
  }
} finally {
  await browser.close();
}
