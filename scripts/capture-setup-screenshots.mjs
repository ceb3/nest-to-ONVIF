#!/usr/bin/env node
/**
 * Capture real setup wizard screenshots for docs/images/setup/.
 * Uses scripts/setup-screenshot-draft.yaml (generate with scripts/obfuscate-local-configs.sh).
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';
import yaml from 'yaml';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(__dirname, '..');
const outDir = path.join(root, 'docs', 'images', 'setup');
const baseURL = 'http://127.0.0.1:8190';
const draftFixture = path.join(__dirname, 'setup-screenshot-draft.yaml');
const draftPath = path.join(root, 'setup-draft.yaml');

function loadDraft() {
  const raw = fs.readFileSync(draftFixture, 'utf8');
  return yaml.parse(raw);
}

function googleFromDraft() {
  const draft = loadDraft();
  const g = draft.google || {};
  const e = draft.events || {};
  return {
    project_id: g.project_id || '',
    client_id: g.client_id || '',
    client_secret: g.client_secret || '',
    pubsub_subscription: e.pubsub_subscription || g.pubsub_subscription || '',
  };
}

function camerasFromDraft() {
  const draft = loadDraft();
  return (draft.cameras || []).map((c) => ({
    device_id: c.device_id,
    name: c.name,
    selected: c.selected !== false,
    audio: c.audio !== false,
    events_onvif: !!c.events_onvif,
    linger: '',
    ip: c.ip || '',
    mac: c.mac || '',
    type: c.name?.toLowerCase().includes('doorbell') ? 'DOORBELL' : 'CAMERA',
    protocols: 'WEB_RTC',
  }));
}

async function waitForServer(timeoutMs = 15000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetch(`${baseURL}/api/status`);
      if (res.ok) return;
    } catch (_) {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`setup server not reachable at ${baseURL}`);
}

function spawnSetup() {
  const bin = path.join(root, 'bin', 'nest-bridge');
  const child = spawn(bin, ['setup'], {
    cwd: root,
    env: { ...process.env, NEST_BRIDGE_ROOT: root },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return child;
}

async function screenshotStep(page, name, setupFn) {
  await page.goto(baseURL, { waitUntil: 'networkidle' });
  await setupFn(page);
  await page.waitForTimeout(400);
  const out = path.join(outDir, name);
  await page.screenshot({ path: out, fullPage: true });
  console.log('wrote', out);
}

async function main() {
  const spawnServer = process.argv.includes('--spawn');
  let server = null;
  let draftBackup = null;

  if (fs.existsSync(draftPath)) {
    draftBackup = fs.readFileSync(draftPath);
  }
  fs.copyFileSync(draftFixture, draftPath);

  if (spawnServer) {
    server = spawnSetup();
    server.stdout?.on('data', (d) => process.stderr.write(d));
    server.stderr?.on('data', (d) => process.stderr.write(d));
    await waitForServer();
  } else {
    await waitForServer();
  }

  fs.mkdirSync(outDir, { recursive: true });

  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 920, height: 900 } });

  await screenshotStep(page, 'setup-01-system.png', async (p) => {
    await p.evaluate(() => {
      discovery = { ready: true };
      go(0);
    });
    await p.waitForSelector('h2:text("System discovery")');
  });

  await screenshotStep(page, 'setup-02-google.png', async (p) => {
    const google = googleFromDraft();
    await p.evaluate((g) => {
      discovery = { ready: true };
      const realLoadConfig = loadConfig;
      loadConfig = async () => {
        const cfg = await realLoadConfig();
        return {
          ...cfg,
          google: { ...cfg.google, ...g },
          events: { ...cfg.events, pubsub_subscription: g.pubsub_subscription },
        };
      };
      go(1);
    }, google);
    await p.waitForSelector('h2:text("Google Cloud")');
    await p.waitForSelector('#project_id');
    await p.waitForFunction(
      (g) => document.getElementById('project_id')?.value === g.project_id,
      google,
    );
  });

  await screenshotStep(page, 'setup-03-authorize.png', async (p) => {
    await p.evaluate(() => {
      discovery = { ready: true };
      go(2);
    });
    await p.waitForSelector('h2:text("Authorize with Google")');
  });

  await screenshotStep(page, 'setup-04-cameras.png', async (p) => {
    await p.evaluate((cams) => {
      discovery = { ready: true };
      status = { ...status, authorized: true };
      cameras = cams;
      pubsubReady = false;
      step = 3;
      renderSteps();
      renderCameras();
    }, camerasFromDraft());
    await p.waitForSelector('h2:text("Choose cameras")');
  });

  await screenshotStep(page, 'setup-05-network.png', async (p) => {
    await p.evaluate(() => {
      discovery = { ready: true };
      go(4);
    });
    await p.waitForSelector('h2:text("Network")');
  });

  await screenshotStep(page, 'setup-06-deploy.png', async (p) => {
    const cams = camerasFromDraft();
    await p.evaluate((cameras) => {
      discovery = { ready: true };
      status = { ...status, deployed: true, docker: true, bridge_built: true };
      const realLoadConfig = loadConfig;
      loadConfig = async () => {
        const cfg = await realLoadConfig();
        return { ...cfg, cameras };
      };
      go(5);
    }, cams);
    await p.waitForSelector('h2:text("Review")');
    await p.waitForFunction(() => !document.body.innerText.includes('Chicken Run'));
  });

  await browser.close();

  if (server) {
    server.kill('SIGTERM');
    await new Promise((r) => server.on('close', r));
  }

  if (draftBackup) {
    fs.writeFileSync(draftPath, draftBackup);
  } else if (fs.existsSync(draftPath)) {
    fs.unlinkSync(draftPath);
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
