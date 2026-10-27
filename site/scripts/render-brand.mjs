// Renders site/og.png (1200x630), site/apple-touch-icon.png and
// site/favicon-32.png from brand.html with the site's own fonts.
// Usage: PLAYWRIGHT_MODULE=.../playwright/index.mjs node site/scripts/render-brand.mjs
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const here = path.dirname(fileURLToPath(import.meta.url));
const site = path.resolve(here, '..');
const browser = await chromium.launch({headless: true});
try {
  const page = await browser.newPage({viewport: {width: 1300, height: 1000}, colorScheme: 'light'});
  await page.goto(pathToFileURL(path.join(here, 'brand.html')).href);
  await page.evaluate(() => document.fonts.ready);
  await page.locator('#og').screenshot({path: path.join(site, 'og.png')});
  await page.locator('#icon').screenshot({path: path.join(site, 'apple-touch-icon.png')});
  await page.locator('#fav').screenshot({path: path.join(site, 'favicon-32.png'), omitBackground: true});
} finally {
  await browser.close();
}
