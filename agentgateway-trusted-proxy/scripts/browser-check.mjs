// Signs in through the identity provider like a person would, opens an agent,
// asks for a long answer, and checks the turn finishes and the UI shows no error.
// Drives the Chrome that is already installed (no browser download).
//
//   npm install --prefix scripts
//   node scripts/browser-check.mjs
//
// Settings, all optional: BASE, LOGIN, PASSWORD, AGENT, PROMPT, MIN_SECONDS.
import { chromium } from 'playwright-core';

const base = process.env.BASE ?? 'https://kagent.localtest.me:8443';
const login = process.env.LOGIN ?? 'alice@example.com';
const password = process.env.PASSWORD ?? 'alice-pass';
const agent = process.env.AGENT;
const minSeconds = Number(process.env.MIN_SECONDS ?? 15);
const prompt = process.env.PROMPT ??
  'Write a detailed, 2000 word explanation of how DNS resolution works, from the stub resolver to the authoritative server. Do not use any tools.';
if (!agent) { console.error('FAIL: set AGENT to the name of an agent in the kagent namespace'); process.exit(2); }

const fail = (msg) => { console.error(`FAIL: ${msg}`); process.exit(1); };
const browser = await chromium.launch({ channel: 'chrome', headless: true });
// The example's certificate is self-signed.
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1200, height: 800 } });
const page = await context.newPage();

const apiErrors = [];
page.on('response', (r) => {
  const u = new URL(r.url());
  if (u.host === new URL(base).host && u.pathname.startsWith('/api/') && r.status() >= 400) apiErrors.push(`${r.status()} ${u.pathname}`);
});

// 1. An unauthenticated visitor is sent to the identity provider.
await page.goto(`${base}/oauth2/start?rd=/`, { waitUntil: 'load' });
if (!/dex\./.test(page.url())) fail(`expected the identity provider, got ${page.url()}`);
console.log('ok   sign-in redirects to the identity provider');

// 2. Sign in, and come back to kagent.
await page.fill('input[name=login]', login);
await page.fill('input[name=password]', password);
await page.click('button[type=submit]');
await page.waitForURL((u) => u.host === new URL(base).host, { timeout: 20000 });
const cont = page.getByRole('button', { name: /continue/i }).first();
if (await cont.count()) await cont.click();
await page.waitForTimeout(2500);
console.log(`ok   signed in as ${login}`);

// 3. Chat with the agent.
await page.goto(`${base}/agents/kagent/${agent}/new`, { waitUntil: 'load' });
await page.waitForTimeout(3000);
const box = page.getByPlaceholder('Ask the agent something');
await box.click();
await box.fill(prompt);
const start = Date.now();
await page.keyboard.press('Enter');
await page.waitForFunction(() => document.body.innerText.includes('Answering'), null, { timeout: 30000 }).catch(() => {});
let finished = true;
await page.waitForFunction(() => !document.body.innerText.includes('Answering'), null, { timeout: 280000 }).catch(() => { finished = false; });
const seconds = Math.round((Date.now() - start) / 1000);
const text = await page.locator('body').innerText();
console.log(`turn took ${seconds}s`);
if (!finished) fail('the turn did not finish');
const uiError = text.match(/(?:could not finish this turn|network error)[^\n]*\n([^\n]*)/i);
if (uiError || /could not finish this turn|network error/i.test(text)) {
  fail(`the UI reports the turn failed${uiError ? `: "${uiError[1].trim()}"` : ''}. ` +
    'Sign-in worked, so look at the agent: try it with the same prompt on a cluster in insecure mode, and check the agent runtime (see Notes in the README)');
}
if (apiErrors.length) fail(`API errors: ${apiErrors.join(', ')}`);
if (seconds < minSeconds) fail(`the turn took ${seconds}s, so it did not test a long turn. Set PROMPT to ask for more`);
console.log(`ok   a ${seconds}s turn finished with no error`);
await browser.close();
