// A small client for the wax REST API, used only on the server: the keys
// never reach the browser.

import { mkdir, readFile, writeFile } from "node:fs/promises";

const base = process.env.WAX_URL ?? "http://wax:8080";
const keyFile = "/data/tenant-key";
const tenant = "app";

/** The instance (WhatsApp account) this application uses. */
export const instance = "main";

/** An error answer of the wax API. */
export class WaxError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

async function call(key: string, method: string, path: string, body?: unknown): Promise<Response> {
  const res = await fetch(base + path, {
    method,
    headers: {
      Authorization: `Bearer ${key}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
  });
  if (!res.ok) {
    const text = await res.text();
    let msg = text;
    try {
      msg = JSON.parse(text).error ?? text;
    } catch {}
    throw new WaxError(res.status, `${method} ${path}: ${res.status} ${msg}`);
  }
  return res;
}

let tenantKey: Promise<string> | undefined;

// key returns the tenant key: WAX_TENANT_KEY when given, otherwise the one
// kept in the data volume, otherwise a new one issued with the admin key
// (the admin key manages tenants only; it cannot use instances).
function key(): Promise<string> {
  tenantKey ??= (async () => {
    if (process.env.WAX_TENANT_KEY) return process.env.WAX_TENANT_KEY;
    try {
      return (await readFile(keyFile, "utf8")).trim();
    } catch {}
    const admin = process.env.WAX_API_KEY!;
    try {
      await call(admin, "POST", "/v1/tenants", { id: tenant, name: "Next.js example" });
    } catch (e) {
      if (!(e instanceof WaxError && e.status === 409)) throw e;
    }
    const issued = await (await call(admin, "POST", `/v1/tenants/${tenant}/keys`)).json();
    await mkdir("/data", { recursive: true });
    await writeFile(keyFile, issued.key, { mode: 0o600 });
    return issued.key as string;
  })().catch((e) => {
    tenantKey = undefined; // try again on the next request
    throw e;
  });
  return tenantKey;
}

/** wax calls the instance routes with the tenant key. */
export async function wax(method: string, path: string, body?: unknown): Promise<Response> {
  return call(await key(), method, path, body);
}

/** Status of the instance, or null when it was not created yet. */
export async function status(): Promise<{ status: string; jid?: string; qr_pending?: boolean } | null> {
  try {
    return await (await wax("GET", `/v1/instances/${instance}`)).json();
  } catch (e) {
    if (e instanceof WaxError && e.status === 404) return null;
    throw e;
  }
}

/** connect creates the instance with this application's webhook, or starts a new pairing. */
export async function connect(): Promise<void> {
  const hook = {
    webhook_url: process.env.WAX_WEBHOOK_URL ?? "http://web:3000/api/wax",
    webhook_secret: process.env.WAX_WEBHOOK_SECRET,
  };
  if ((await status()) === null) {
    await wax("POST", "/v1/instances", { id: instance, ...hook });
    return;
  }
  await wax("PATCH", `/v1/instances/${instance}`, hook);
  await wax("POST", `/v1/instances/${instance}/connect`);
}
