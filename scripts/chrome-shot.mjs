// Drives Chrome headless through the DevTools protocol (Node >= 22, no dependencies) and saves PNG screenshots of the page.
//   node scripts/chrome-shot.mjs <url> <out.png> [seconds-before-shot=20] [extra-shots-every-seconds=0]
// Used with `N64_CHROME=60 N64_CHROME_URLFILE=/tmp/url go test -run TestChromeServe` (the test prints the page URL into the file).
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
const [url, out, secs = '20', every = '0'] = process.argv.slice(2);
const port = 9333 + Math.floor(Math.random() * 100); // never 8420
const dir = mkdtempSync(join(tmpdir(), 'chrome-shot-'));
const chrome = spawn(process.env.CHROME || 'google-chrome-stable', ['--headless=new', '--disable-gpu', '--no-first-run', '--mute-audio',
  `--user-data-dir=${dir}`, `--remote-debugging-port=${port}`, `--window-size=${process.env.CHROME_W || 640},${process.env.CHROME_H || 480}`, '--autoplay-policy=no-user-gesture-required', 'about:blank'], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws;
try {
  let targets;
  for (let i = 0; i < 50; i++) { try { targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json(); if (targets.length) break; } catch {} await sleep(200); }
  const page = targets.find((t) => t.type === 'page');
  ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((r) => (ws.onopen = r));
  let id = 0; const pending = new Map();
  ws.onmessage = (m) => { const d = JSON.parse(m.data); if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); } };
  const call = (method, params = {}) => new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
  await call('Page.enable');
  await call('Emulation.setDeviceMetricsOverride', { width: Number(process.env.CHROME_W || 640), height: Number(process.env.CHROME_H || 480), deviceScaleFactor: 1, mobile: false });
  await call('Page.navigate', { url });
  const shot = async (file) => { const r = await call('Page.captureScreenshot', { format: 'png' }); (await import('node:fs')).writeFileSync(file, Buffer.from(r.result.data, 'base64')); };
  const st = async () => (await call('Runtime.evaluate', { expression: 'JSON.stringify(window.st)', returnByValue: true })).result.result.value;
  const t0 = Date.now(); let n = 0;
  if (Number(every) > 0) {
    while ((Date.now() - t0) / 1000 < Number(secs)) { await sleep(Number(every) * 1000); await shot(out.replace(/\.png$/, `-${++n}.png`)); }
  } else await sleep(Number(secs) * 1000);
  console.log('page state:', await st());
  await shot(out);
  console.log('saved', out);
} finally { try { ws?.close(); } catch {} chrome.kill(); await sleep(300); rmSync(dir, { recursive: true, force: true }); }
