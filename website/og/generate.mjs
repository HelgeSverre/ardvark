// Renders og/template.html to ../og.png (1200x630) with Playwright.
import { chromium } from "playwright-core";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const template = path.join(here, "template.html");
const out = path.join(here, "..", "og.png");

// channel: "chrome" reuses the installed Chrome — no browser download needed.
const browser = await chromium.launch({ channel: "chrome" });
const page = await browser.newPage({
  viewport: { width: 1200, height: 630 },
  deviceScaleFactor: 2, // crisp on retina link previews
});
await page.goto(`file://${template}`);
await page.screenshot({ path: out });
await browser.close();

console.log(`wrote ${path.relative(process.cwd(), out)} (2400x1260 @2x)`);
