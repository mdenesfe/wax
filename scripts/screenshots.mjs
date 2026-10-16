// Takes the documentation screenshots of the admin interface, using the
// demo server (made-up data) and a headless Chrome driven over the
// DevTools protocol. No dependencies beyond Node 22+ and Chrome.
//
//   go run ./cmd/wax-demo &
//   node scripts/screenshots.mjs            # writes docs/images/*.png
//
// Environment: CHROME (path to the Chrome binary), DEMO (demo URL).

import { spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const chrome = process.env.CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const demo = process.env.DEMO || "http://127.0.0.1:8090";
const out = new URL("../docs/images/", import.meta.url).pathname;
const port = 9333;
const sleep = (ms) => new Promise((ok) => setTimeout(ok, ms));

// --- a minimal DevTools protocol client ---------------------------------------------

async function connect() {
  for (let i = 0; i < 50; i++) {
    try {
      const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
      const ws = new WebSocket(target.webSocketDebuggerUrl);
      await new Promise((ok, fail) => { ws.onopen = ok; ws.onerror = fail; });
      let id = 0;
      const waiting = new Map();
      ws.onmessage = (e) => {
        const msg = JSON.parse(e.data);
        const w = waiting.get(msg.id);
        if (!w) return;
        waiting.delete(msg.id);
        msg.error ? w.fail(new Error(msg.error.message)) : w.ok(msg.result);
      };
      return (method, params = {}) => new Promise((ok, fail) => {
        waiting.set(++id, { ok, fail });
        ws.send(JSON.stringify({ id, method, params }));
      });
    } catch {
      await sleep(200);
    }
  }
  throw new Error("Chrome did not start");
}

// --- scenes ---------------------------------------------------------------------------

// Each scene runs in the page: it sets up the interface and resolves when
// it is ready to be captured. Helpers: click(text, selector), wait(ms).
const helpers = `
  const wait = (ms) => new Promise((ok) => setTimeout(ok, ms));
  const click = async (text, sel = "button, a") => {
    const el = [...document.querySelectorAll(sel)].find((e) => e.textContent.includes(text) || e.title === text);
    if (!el) throw new Error("not found: " + text);
    el.click();
    await wait(700);
  };
  const go = async (hash) => { location.hash = hash; await wait(1500); };
`;

const scenes = [
  { name: "login", hash: "#/login", key: null, run: "" },
  { name: "chat", run: `await click("Emma Wilson", ".wa-chat");` },
  { name: "group", run: `await click("Product Team", ".wa-chat"); await click("Info", ".conv-who"); await wait(800);` },
  { name: "contact", run: `await click("Acme Bakery", ".wa-chat"); await click("Info", ".conv-who"); await wait(800);` },
  { name: "voice", run: `await click("Sofia Rossi", ".wa-chat");` },
  { name: "status", run: `await click("Status", ".rail-btn"); await click("Sofia Rossi", ".wa-chat"); await wait(1500);` },
  { name: "communities", run: `await click("Communities", ".rail-btn"); await wait(800);` },
  { name: "settings", run: `await click("Settings", ".rail-btn");` },
  { name: "pairing", hash: "#/instances/sales", run: "" },
  { name: "archived", run: `await click("Archived", ".wa-chat"); await click("Old Project", ".wa-chat");` },
  { name: "tenants", hash: "#/tenants", key: "admin", run: "" },
];

// --- capture ---------------------------------------------------------------------------

const profile = mkdtempSync(join(tmpdir(), "wax-shots-"));
const browser = spawn(chrome, [
  "--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
  "--hide-scrollbars", "--no-first-run", "--no-default-browser-check", "--force-color-profile=srgb",
], { stdio: "ignore" });

try {
  mkdirSync(out, { recursive: true });
  const send = await connect();
  await send("Page.enable");
  await send("Runtime.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 2, mobile: false });
  for (const theme of ["light", "dark"]) {
    await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: theme }] });
    for (const s of scenes) {
      const key = s.key === undefined ? "demo" : s.key;
      const hash = s.hash || "#/instances/support";
      await send("Page.navigate", { url: demo + "/ui/" });
      await sleep(800);
      // Sign in (or out) and open the scene's page, with the sidebar shown.
      const setup = key
        ? `sessionStorage.setItem("wax.key", ${JSON.stringify(key)}); sessionStorage.setItem("wax.role", ${JSON.stringify(key === "admin" ? "admin" : "tenant")});`
        : `sessionStorage.clear();`;
      await send("Runtime.evaluate", { expression: `${setup} localStorage.setItem("wax.sidebar", "open"); location.hash = ${JSON.stringify(hash)}; location.reload();` });
      await sleep(2500);
      const res = await send("Runtime.evaluate", { expression: `(async () => { ${helpers} ${s.run} await wait(600); return "ok"; })()`, awaitPromise: true });
      if (res.exceptionDetails) throw new Error(`${s.name}: ${res.exceptionDetails.exception?.description || res.exceptionDetails.text}`);
      const shot = await send("Page.captureScreenshot", { format: "png" });
      const file = join(out, `${s.name}-${theme}.png`);
      writeFileSync(file, Buffer.from(shot.data, "base64"));
      console.log("wrote", file);
    }
  }
} finally {
  browser.kill();
  await sleep(300);
  rmSync(profile, { recursive: true, force: true });
}
