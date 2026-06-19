// Headless capture of the Meridian dashboard for the README hero GIF.
//
// Drives a real `meridian serve` + `meridian simulate` instance (started by
// capture-dashboard.sh) through Chromium, recording a video of: a PromQL query
// plotted on the signature strip-chart, the cursor crosshair sweeping its live
// readout, then a scroll down through the streaming monitors and the live
// anomaly strip. The video is converted to an optimized looping GIF by the
// shell wrapper - this script only produces the .webm.
//
// Env: BASE_URL, OUT_DIR, WIDTH, HEIGHT (all optional; sane defaults below).

import { chromium } from 'playwright';

const BASE_URL = process.env.BASE_URL || 'http://localhost:8080';
const OUT_DIR = process.env.OUT_DIR || '/tmp/meridian-demo/dash-video';
const WIDTH = parseInt(process.env.WIDTH || '1280', 10);
const HEIGHT = parseInt(process.env.HEIGHT || '800', 10);
const QUERY = process.env.QUERY || 'avg by (host) (cpu_usage_percent)';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Smoothly scroll the window to an absolute Y over `ms`, so the GIF pans rather
// than jumping. Runs in page context via requestAnimationFrame.
async function smoothScrollTo(page, y, ms) {
  await page.evaluate(
    ([targetY, dur]) =>
      new Promise((resolve) => {
        const startY = window.scrollY;
        const dy = targetY - startY;
        const t0 = performance.now();
        const ease = (p) => (p < 0.5 ? 2 * p * p : 1 - Math.pow(-2 * p + 2, 2) / 2);
        function step(now) {
          const p = Math.min(1, (now - t0) / dur);
          window.scrollTo(0, startY + dy * ease(p));
          if (p < 1) requestAnimationFrame(step);
          else resolve();
        }
        requestAnimationFrame(step);
      }),
    [y, ms],
  );
}

// Smoothly bring a panel (located by its title text) to the vertical center of
// the viewport - robust to exact layout heights.
async function smoothScrollPanelToCenter(page, title, ms) {
  const y = await page.evaluate((t) => {
    const el = [...document.querySelectorAll('*')].find(
      (n) => n.children.length === 0 && n.textContent?.trim() === t,
    );
    const panel = el?.closest('section, article, div[class*="rounded"]') || el;
    if (!panel) return window.scrollY;
    const r = panel.getBoundingClientRect();
    return Math.max(0, window.scrollY + r.top - (window.innerHeight - r.height) / 2);
  }, title);
  await smoothScrollTo(page, Math.round(y), ms);
}

// Sweep the cursor across the chart canvas left→right to animate the crosshair
// readout - the dashboard's signature interaction.
async function sweepCrosshair(page, box, steps, totalMs) {
  const y = box.y + box.height * 0.5;
  const x0 = box.x + box.width * 0.06;
  const x1 = box.x + box.width * 0.94;
  for (let i = 0; i <= steps; i++) {
    const x = x0 + ((x1 - x0) * i) / steps;
    await page.mouse.move(x, y, { steps: 3 });
    await sleep(totalMs / steps);
  }
}

const main = async () => {
  const browser = await chromium.launch({ args: ['--force-color-profile=srgb'] });
  const context = await browser.newContext({
    viewport: { width: WIDTH, height: HEIGHT },
    deviceScaleFactor: 1,
    colorScheme: 'dark',
    reducedMotion: 'no-preference',
    recordVideo: { dir: OUT_DIR, size: { width: WIDTH, height: HEIGHT } },
  });
  const page = await context.newPage();

  await page.goto(BASE_URL, { waitUntil: 'networkidle' });

  // Wait for the live WebSocket to connect (header lamp flips to "Live").
  await page.getByText('Live', { exact: true }).waitFor({ timeout: 20000 }).catch(() => {});
  await sleep(800);

  // Run a PromQL query on the signature panel.
  const input = page.getByRole('combobox', { name: 'PromQL query' });
  await input.click();
  await input.fill(QUERY);
  await page.keyboard.press('Escape'); // close the suggestions listbox
  await input.press('Enter');

  // Wait for the result chart to paint (result meta shows "N series").
  await page.getByText(/\d+ series/).first().waitFor({ timeout: 15000 }).catch(() => {});
  await sleep(1200);

  // Sweep the crosshair across the strip-chart.
  const canvas = page.locator('canvas').first();
  const box = await canvas.boundingBox();
  if (box) {
    await sweepCrosshair(page, box, 26, 6000);
    await sleep(400);
    // A quick second pass to settle the readout mid-chart.
    await page.mouse.move(box.x + box.width * 0.5, box.y + box.height * 0.4, { steps: 12 });
    await sleep(700);
  }

  // Pan down to the live monitors, then center the anomaly strip (the firing
  // "N active" indicator + rows) and the live stream while they stream.
  await smoothScrollTo(page, Math.round(HEIGHT * 0.5), 1300);
  await sleep(1600); // ingestion monitor + cluster topology updating
  await smoothScrollPanelToCenter(page, 'Anomalies', 1300);
  await sleep(4200); // anomaly strip: active count + firing rows
  await smoothScrollPanelToCenter(page, 'Live Stream', 1300);
  await sleep(3200); // live metric ticker
  await smoothScrollTo(page, 0, 1300);
  await sleep(800);

  await context.close(); // flushes the video file
  await browser.close();

  console.log('dashboard video written to', OUT_DIR);
};

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
