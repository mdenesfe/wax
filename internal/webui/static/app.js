// wax admin interface: a small single-page app over the REST API of the
// same origin. Every value from the server is inserted with textContent
// (never as HTML), so message contents cannot inject markup.
"use strict";

// --- DOM helper ----------------------------------------------------------------

// h(tag, attrs, ...children): attrs may hold on* handlers, class, and
// plain attributes; children are nodes or strings (set as text).
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const ICONS = {
  users: "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8M22 21v-2a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8",
  plus: "M12 5v14M5 12h14",
  trash: "M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14",
  refresh: "M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5",
  out: "M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9",
  send: "M22 2 11 13M22 2l-7 20-4-9-9-4 20-7z",
  download: "M12 3v12M7 10l5 5 5-5M5 21h14",
  back: "M15 18l-6-6 6-6",
  gear: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z",
  close: "M18 6 6 18M6 6l12 12",
  panel: "M3 4h18v16H3zM9 4v16",
  chat: "M21 12a8 8 0 0 1-11.6 7.1L4 21l1.9-5.4A8 8 0 1 1 21 12z",
  status: "M12 3a9 9 0 1 1-9 9M12 8a4 4 0 1 1 0 8 4 4 0 0 1 0-8",
  archive: "M3 4h18v4H3zM5 8v12h14V8M10 12h4",
  mute: "M11 5 6 9H3v6h3l5 4zM22 9l-6 6M16 9l6 6",
  pin: "M12 17v5M9 3h6l-1 6 4 4H6l4-4z",
  image: "M3 5h18v14H3zM3 16l5-5 4 4 3-3 6 6",
  video: "M3 6h12v12H3zM15 10l6-4v12l-6-4z",
  mic: "M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3zM19 11a7 7 0 0 1-14 0M12 18v4",
  file: "M14 2H6v20h12V6zM14 2v4h4",
  forward: "M9 18l6-6-6-6",
  clip: "M21 12.5 12.5 21a6 6 0 0 1-8.5-8.5L13 3.5a4 4 0 0 1 5.7 5.7l-8.6 8.5a2 2 0 0 1-2.8-2.8L15 7.2",
  play: "M7 4v16l13-8z",
  pause: "M7 5h3v14H7zM14 5h3v14h-3z",
  reply: "M9 14 4 9l5-5M4 9h11a5 5 0 0 1 5 5v6",
  share: "M15 14l5-5-5-5M20 9H9a5 5 0 0 0-5 5v6",
  once: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM10.5 9.5 12.5 8v8",
  community: "M3 21h18M5 21V10l7-5 7 5v11M9 21v-5h6v5",
  megaphone: "M3 11v2a1 1 0 0 0 1 1h3l6 4V6L7 10H4a1 1 0 0 0-1 1zM17 9a4 4 0 0 1 0 6"
};

function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", "icon");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", ICONS[name]);
  svg.append(path);
  return svg;
}

// wordmark is the product name: "wax" with a quiet "admin".
function wordmark(cls) {
  return h("span", { class: "wordmark " + (cls || "") }, h("b", {}, "wax"), h("span", {}, "admin"));
}

function button(label, opts = {}) {
  return h("button", { type: opts.type || "button", class: "btn " + (opts.class || ""), onclick: opts.onclick, disabled: opts.disabled, title: opts.title },
    opts.icon ? icon(opts.icon) : null, label);
}

function toast(text, error) {
  const t = h("div", { class: "toast" + (error ? " error" : "") }, text);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), error ? 6000 : 3000);
}

// fmtPhone shows digits as an international number; Turkish numbers are
// grouped as +90 5xx xxx xx xx.
function fmtPhone(digits) {
  if (!digits) return "";
  const m = /^90(\d{3})(\d{3})(\d{2})(\d{2})$/.exec(digits);
  return m ? `+90 ${m[1]} ${m[2]} ${m[3]} ${m[4]}` : "+" + digits;
}

// chatKind names a chat without a name or known number.
function chatKind(jid) {
  if (jid === "status@broadcast") return "Status updates";
  if (jid.endsWith("@g.us")) return "Group";
  if (jid.endsWith("@lid")) return "Unknown number"; // WhatsApp has not shared it
  return jid;
}

// phoneOf returns the formatted number of a phone-number JID ("" for LIDs
// and groups).
function phoneOf(jid) {
  const m = /^(\d+)(?::\d+)?@s\.whatsapp\.net$/.exec(jid || "");
  return m ? fmtPhone(m[1]) : "";
}

const fmtTime = (s) => (s ? new Date(s).toLocaleString("en-GB", { dateStyle: "short", timeStyle: "short" }) : "-");

// --- session and API -------------------------------------------------------------

// The key lives in sessionStorage: it is gone when the tab closes.
const session = {
  get key() { return sessionStorage.getItem("wax.key"); },
  get role() { return sessionStorage.getItem("wax.role"); },
  set(key, role) { sessionStorage.setItem("wax.key", key); sessionStorage.setItem("wax.role", role); },
  clear() { sessionStorage.removeItem("wax.key"); sessionStorage.removeItem("wax.role"); },
};

class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(method, path, body, raw) {
  const res = await fetch(path, {
    method,
    headers: Object.assign({ Authorization: "Bearer " + session.key }, body ? { "Content-Type": "application/json" } : {}),
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && session.key) {
    session.clear();
    location.hash = "#/login";
    throw new APIError(401, "Session expired");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    if (res.status === 429) msg = "Too many requests";
    throw new APIError(res.status, msg);
  }
  if (raw) return res;
  return res.status === 204 ? null : res.json();
}

// guard runs an action and reports its error as a toast.
async function guard(fn) {
  try { return await fn(); } catch (e) { toast(e.message, true); }
}

const enc = encodeURIComponent;

// --- dialogs -----------------------------------------------------------------------

function dialog(title, content, actions) {
  const close = () => overlay.remove();
  const overlay = h("div", { class: "overlay", onclick: (e) => { if (e.target === overlay) close(); } },
    h("div", { class: "dialog", role: "dialog", "aria-modal": "true" }, h("h3", {}, title), content,
      h("div", { class: "footer" }, actions(close))));
  document.body.append(overlay);
  overlay.querySelector("input, button.primary")?.focus();
  return close;
}

// formDialog collects named fields and calls submit(values).
function formDialog(title, fields, submitLabel, submit) {
  const inputs = {};
  const form = h("form", { class: "form", onsubmit: (e) => { e.preventDefault(); go(); } },
    fields.map((f) => {
      inputs[f.name] = h("input", { name: f.name, type: f.type || "text", placeholder: f.placeholder || "", value: f.value ?? "", required: f.required, autocomplete: "off" });
      return h("label", { class: "field" }, f.label, inputs[f.name]);
    }),
    h("button", { type: "submit", hidden: true }));
  let close;
  const go = () => guard(async () => {
    const values = {};
    for (const f of fields) {
      const v = inputs[f.name].value.trim();
      if (v === "" && f.optional) continue;
      values[f.name] = f.type === "number" ? Number(v || 0) : v;
    }
    await submit(values);
    close();
  });
  close = dialog(title, form, (c) => [button("Cancel", { onclick: c }), button(submitLabel, { class: "primary", onclick: go })]);
}

function confirmDialog(title, text, label, action) {
  dialog(title, h("p", { class: "muted m0" }, text), (close) => [
    button("Cancel", { onclick: close }),
    button(label, { class: "danger solid", onclick: () => guard(async () => { await action(); close(); }) }),
  ]);
}

// --- layout --------------------------------------------------------------------------

let timers = [];
function every(ms, fn) { timers.push(setInterval(fn, ms)); }
function clearTimers() { timers.forEach(clearInterval); timers = []; }

// The sidebar can be hidden; the choice is remembered in this browser.
const sidebarPref = {
  get closed() { try { return localStorage.getItem("wax.sidebar") !== "open"; } catch { return true; } }, // closed by default
  toggle() {
    const closed = !document.body.classList.toggle("nav-open") ;
    try { localStorage.setItem("wax.sidebar", closed ? "closed" : "open"); } catch { /* private mode */ }
  },
};

// The accounts listed in the sidebar (Notion-style, under "WhatsApp
// accounts"), kept between pages so the sidebar does not flicker; more are
// shown on request.
const accountNav = { list: null, shown: 8 };

// phoneStatus is a phone whose home button shows the account's state by
// its color (green connected, amber pairing or connecting, red failed).
function phoneStatus(status) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", "icon phone-status " + (STATUS[status]?.[0] || ""));
  const body = document.createElementNS(ns, "path");
  body.setAttribute("d", "M7 2h10a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z");
  const circle = (cls, cy, r) => {
    const c = document.createElementNS(ns, "circle");
    c.setAttribute("class", cls);
    c.setAttribute("cx", "12");
    c.setAttribute("cy", cy);
    c.setAttribute("r", r);
    return c;
  };
  // The home button shows the state.
  svg.append(body, circle("home", "18", "1.8"));
  return svg;
}

function accountsTree(here) {
  const box = h("div", { class: "tree" });
  const draw = () => {
    const list = accountNav.list || [];
    box.replaceChildren(...list.slice(0, accountNav.shown).map((i) => {
      const href = "#/instances/" + enc(i.id);
      return h("a", { class: "nav-item sub acct" + (here === href ? " active" : ""), href, title: STATUS[i.status]?.[1] || i.status },
        phoneStatus(i.status),
        h("span", { class: "acct-text" }, h("span", { class: "label" }, i.id), h("span", { class: "acct-num num" }, phoneOf(i.jid) || "Not linked")));
    }));
    if (list.length > accountNav.shown) {
      box.append(h("button", { class: "nav-item sub more", type: "button", onclick: () => { accountNav.shown += 8; draw(); } }, "Load more"));
    }
  };
  const refresh = () => api("GET", "/v1/instances").then((list) => { accountNav.list = list; draw(); }).catch(() => { /* keep the last list */ });
  draw();
  refresh();
  every(10000, refresh); // statuses change (scanned, logged out)
  return box;
}

// addAccount creates a WhatsApp account and opens it, which shows its QR code.
function addAccount() {
  formDialog("Add WhatsApp account", [{ name: "id", label: "Name", placeholder: "support", required: true }], "Add", async (v) => {
    await api("POST", "/v1/instances", v);
    location.hash = "#/instances/" + enc(v.id);
  });
}

function shell(crumbs, page) {
  document.body.classList.toggle("nav-open", !sidebarPref.closed);
  const admin = session.role === "admin";
  const here = location.hash;
  const nav = (href, ic, label) => h("a", { class: "nav-item" + (here.startsWith(href) ? " active" : ""), href }, icon(ic), label);
  return h("div", { class: "shell" },
    h("aside", { class: "sidebar" },
      h("a", { class: "brand", href: admin ? "#/tenants" : "#/instances" }, wordmark()),
      admin
        ? [h("div", { class: "nav-label" }, "Admin"), nav("#/tenants", "users", "Tenants")]
        : [h("div", { class: "nav-label with-action" }, "WhatsApp accounts",
          button("", { class: "ghost icon", icon: "plus", title: "Add WhatsApp account", onclick: addAccount })), accountsTree(here)],
      h("div", { class: "spacer" }),
      h("div", { class: "who" },
        button("Sign out", { class: "ghost", icon: "out", onclick: () => { session.clear(); location.hash = "#/login"; } }))),
    h("div", { class: "main" },
      h("div", { class: "topbar" },
        h("div", {}, button("", { class: "ghost icon", icon: "panel", title: "Toggle sidebar", onclick: () => sidebarPref.toggle() })),
        h("nav", { class: "crumbs" }, crumbs.flatMap((c, i) => [
          i ? h("span", { class: "sep" }, "/") : null,
          c.href ? h("a", { href: c.href }, c.label) : h("span", {}, c.label),
        ])),
        h("div", { class: "right" })),
      page));
}

function toolbar(title, ...actions) {
  return h("div", { class: "toolbar" }, h("h2", { class: "title" }, title), h("div", { class: "actions" }, actions));
}

function table(columns, rows, onRow, empty) {
  const card = h("div", { class: "card" });
  if (!rows.length) {
    card.append(h("div", { class: "empty" }, empty));
    return card;
  }
  card.append(h("table", { class: "table" },
    h("thead", {}, h("tr", {}, columns.map((c) => h("th", { class: c.right ? "right" : "" }, c.label)))),
    h("tbody", {}, rows.map((r) => h("tr", { class: onRow ? "link" : "", onclick: onRow ? () => onRow(r) : null },
      columns.map((c) => h("td", { class: (c.right ? "right " : "") + (c.cls || "") }, c.render(r))))))),
    h("div", { class: "statusbar num" }, rows.length + " records"));
  return card;
}

const STATUS = {
  connected: ["ok", "Connected"], qr: ["warn", "Waiting for QR"], connecting: ["warn", "Connecting"], starting: ["warn", "Starting"],
  logged_out: ["bad", "Logged out"], replaced: ["bad", "Replaced"], outdated: ["bad", "Outdated"], banned: ["bad", "Banned"],
  failed: ["bad", "Failed"], stopped: ["", "Stopped"], unassigned: ["", "Unassigned"],
};
function statusBadge(s) {
  const [cls, label] = STATUS[s] || ["", s];
  return h("span", { class: "badge " + cls }, h("span", { class: "dot" }), label);
}

function kv(pairs) {
  return h("div", { class: "kv" }, pairs.flatMap(([k, v]) => [h("div", {}, k), h("div", {}, v ?? "-")]));
}

function section(title, content, extra) {
  return h("div", { class: "section" }, h("header", {}, h("span", {}, title), extra || null), content);
}

// --- pages: login -----------------------------------------------------------------------

function loginPage() {
  const input = h("input", { type: "password", placeholder: "API key", "aria-label": "API key", autocomplete: "off", required: true });
  const submit = async (e) => {
    e.preventDefault();
    const key = input.value.trim();
    if (!key) return;
    const probe = async (path) => (await fetch(path, { headers: { Authorization: "Bearer " + key } })).status;
    await guard(async () => {
      if ((await probe("/v1/tenants")) === 200) {
        session.set(key, "admin");
        location.hash = "#/tenants";
      } else if ((await probe("/v1/instances")) === 200) {
        session.set(key, "tenant");
        location.hash = "#/instances";
      } else {
        throw new Error("Invalid key");
      }
    });
  };
  return h("div", { class: "login" },
    h("form", { class: "card", onsubmit: submit },
      h("div", { class: "login-head" }, wordmark("large")),
      input,
      button("Sign in", { type: "submit", class: "primary lg" })));
}

// --- pages: admin ---------------------------------------------------------------------

async function tenantsPage() {
  const tenants = await api("GET", "/v1/tenants");
  const create = () => formDialog("New tenant", [
    { name: "id", label: "ID", placeholder: "acme", required: true },
    { name: "name", label: "Name", placeholder: "Acme Inc." },
    { name: "max_instances", label: "Account quota", type: "number", value: 0 },
    { name: "requests_per_second", label: "API rate (requests/s)", type: "number", value: 0 },
  ], "Create", async (v) => { await api("POST", "/v1/tenants", v); toast("Tenant created"); render(); });
  return shell([{ label: "Tenants" }], h("div", {},
    toolbar("Tenants", button("New tenant", { icon: "plus", onclick: create })),
    h("div", { class: "body" }, table([
      { label: "ID", render: (t) => t.id, cls: "mono" },
      { label: "Name", render: (t) => t.name || "-" },
      { label: "Account quota", render: (t) => (t.max_instances || "Unlimited"), cls: "num" },
      { label: "API rate", render: (t) => (t.requests_per_second ? t.requests_per_second + "/s" : "Default"), cls: "num" },
      { label: "Created", render: (t) => fmtTime(t.created_at), cls: "num muted" },
    ], tenants, (t) => (location.hash = "#/tenants/" + enc(t.id)), "No tenants"))));
}

async function tenantPage(id) {
  const [t, keys] = await Promise.all([api("GET", "/v1/tenants/" + enc(id)), api("GET", "/v1/tenants/" + enc(id) + "/keys")]);
  const edit = () => formDialog("Edit limits", [
    { name: "name", label: "Name", value: t.name },
    { name: "max_instances", label: "Account quota", type: "number", value: t.max_instances },
    { name: "requests_per_second", label: "API rate (requests/s)", type: "number", value: t.requests_per_second },
  ], "Save", async (v) => { await api("PATCH", "/v1/tenants/" + enc(id), v); toast("Saved"); render(); });
  const issue = () => guard(async () => {
    const k = await api("POST", "/v1/tenants/" + enc(id) + "/keys");
    const copy = button("Copy", { onclick: () => navigator.clipboard.writeText(k.key).then(() => toast("Copied")) });
    dialog("New key", h("div", { class: "form" },
      h("p", { class: "muted m0" }, "Shown only once."),
      h("div", { class: "secret" }, k.key)), (close) => [copy, button("Done", { class: "primary", onclick: () => { close(); render(); } })]);
  });
  const revoke = (k) => confirmDialog("Revoke key", "This cannot be undone.", "Revoke",
    async () => { await api("DELETE", `/v1/tenants/${enc(id)}/keys/${enc(k.id)}`); toast("Key revoked"); render(); });
  const remove = () => confirmDialog("Delete tenant", "This cannot be undone.", "Delete",
    async () => { await api("DELETE", "/v1/tenants/" + enc(id)); toast("Tenant deleted"); location.hash = "#/tenants"; });
  return shell([{ label: "Tenants", href: "#/tenants" }, { label: t.id }], h("div", {},
    toolbar(t.id, button("Edit", { onclick: edit }), button("Delete", { class: "danger", icon: "trash", onclick: remove })),
    h("div", { class: "body" },
      h("div", { class: "grid2" },
        section("Tenant", kv([["ID", h("span", { class: "mono" }, t.id)], ["Name", t.name || "-"],
          ["Account quota", t.max_instances || "Unlimited"], ["API rate", t.requests_per_second ? t.requests_per_second + " requests/s" : "Server default"],
          ["Created", fmtTime(t.created_at)]]))),
      section("API keys", keys.length
        ? h("div", { class: "rows" }, keys.map((k) => h("div", { class: "line" },
          h("span", { class: "mono" }, k.id), h("span", { class: "muted num" }, fmtTime(k.created_at)),
          button("Revoke", { class: "danger ghost", onclick: () => revoke(k) }))))
        : h("div", { class: "empty" }, "No keys"),
      button("Create key", { icon: "plus", class: "ghost", onclick: issue })))));
}

// --- pages: tenant --------------------------------------------------------------------

// instancesPage opens the first account; accounts are managed from the
// sidebar. Without any, it offers to add one.
async function instancesPage() {
  const list = await api("GET", "/v1/instances");
  if (list.length) {
    location.replace("#/instances/" + enc(list[0].id));
    return document.createDocumentFragment();
  }
  return shell([{ label: "WhatsApp accounts" }], h("div", { class: "page desk" },
    h("div", { class: "wa-blank" }, h("div", { class: "start" }, h("span", { class: "wa-logo" }, icon("chat")),
      button("Add WhatsApp account", { class: "primary", icon: "plus", onclick: addAccount })))));
}

// --- instance: a WhatsApp Desktop-like screen ------------------------------------

async function instancePage(name) {
  const base = "/v1/instances/" + enc(name);
  const inst = await api("GET", base);
  const app = whatsappApp(base, inst);
  // Watch the status; a change of state (scan, disconnect) redraws the page.
  every(5000, async () => {
    try {
      const now = await api("GET", base);
      if ((now.status === "connected") !== (inst.status === "connected") || now.qr_pending !== inst.qr_pending) render(true);
      else app.setStatus(now.status);
    } catch { /* next tick */ }
  });
  // The account is shown as a remote screen: a desktop window on a backdrop.
  const win = h("div", { class: "window" },
    h("div", { class: "titlebar" }, h("span", { class: "lights" }, h("i"), h("i"), h("i")),
      h("span", { class: "title-text" }, "WhatsApp", h("span", { class: "muted" }, " · " + (phoneOf(inst.jid) || name))),
      h("span", { class: "lights" })),
    app.el);
  return shell([{ label: "WhatsApp accounts" }, { label: name }], h("div", { class: "page desk" }, win));
}

// whatsappApp: a rail of sections (chats, status, communities, settings), the section's
// list in a resizable side panel, and its content (conversation, status
// viewer, pairing code) on the right.
function whatsappApp(base, inst) {
  const online = inst.status === "connected";
  const me = (inst.jid || "").replace(/:\d+@/, "@");
  const chats = chatsView(base, inst);
  const sections = {
    chats: online ? chats : null,
    status: online ? statusView(base, me) : null,
    communities: online ? communitiesView(base, (g) => { show("chats"); chats.open({ jid: g.jid, name: g.name }); }) : null,
    settings: settingsView(base, inst, online ? chats.main : pairingView(base, inst)),
  };
  const side = h("div", { class: "wa-side" });
  const main = h("div", { class: "wa-main" });
  const dot = h("span", { class: "rail-dot" });
  const railButton = (key, ic, title) => sections[key]
    ? h("button", { class: "rail-btn", type: "button", title, "aria-label": title, onclick: () => show(key) }, icon(ic))
    : null;
  const buttons = { chats: railButton("chats", "chat", "Chats"), status: railButton("status", "status", "Status"),
    communities: railButton("communities", "community", "Communities"), settings: railButton("settings", "gear", "Settings") };
  const rail = h("nav", { class: "rail" }, buttons.chats, buttons.status, buttons.communities, h("div", { class: "spacer" }), buttons.settings,
    h("button", { class: "rail-me", type: "button", title: "Profile", onclick: () => show("settings") }, avatar(inst.id, me, base), dot));

  const loaded = new Set(); // sections that load on first view
  function show(key) {
    for (const [k, b] of Object.entries(buttons)) b?.classList.toggle("active", k === key);
    side.replaceChildren(sections[key].side);
    main.replaceChildren(sections[key].main);
    if (sections[key].load && !loaded.has(key)) {
      loaded.add(key);
      sections[key].load();
    }
  }
  const setStatus = (s) => { dot.className = "rail-dot " + (STATUS[s]?.[0] || ""); };
  setStatus(inst.status);
  show(online ? "chats" : "settings");
  return { el: h("div", { class: "wa" }, rail, side, resizer(side, "wax.pane.chats", 1, 280, 560), main), setStatus };
}

// sideHead is the title bar of the side panel.
function sideHead(title, ...actions) {
  return h("div", { class: "bar side-head" }, h("h2", {}, title), h("div", { class: "actions" }, actions));
}

const placeholder = () => h("div", { class: "wa-blank" }, h("span", { class: "wa-logo" }, icon("chat")));

// --- chats -------------------------------------------------------------------------

const MEDIA = { image: ["image", "Photo"], video: ["video", "Video"], audio: ["mic", "Audio"], voice: ["mic", "Voice message"],
  document: ["file", "Document"], sticker: ["image", "Sticker"] };

// People are shown like WhatsApp does: the address book name; otherwise
// the number, the "~push name" they chose, or their @username.
const chatTitle = (c) => c.name || fmtPhone(c.phone) || (c.push_name && "~" + c.push_name) || (c.username && "@" + c.username) || chatKind(c.jid);

// chatSubtitle: what the title does not already say about a contact.
function chatSubtitle(c) {
  if (c.jid.endsWith("@g.us")) return "Group";
  const title = chatTitle(c);
  return [fmtPhone(c.phone), c.push_name && "~" + c.push_name, c.username && "@" + c.username]
    .filter((x) => x && x !== title).join(" · ");
}

// senderLabel names the author of a message in a group or a status.
function senderLabel(m) {
  if (m.sender_name) return m.sender_name;
  const id = fmtPhone(m.sender_phone) || phoneOf(m.sender) || (m.sender_username && "@" + m.sender_username);
  const push = m.push_name && "~" + m.push_name;
  return [id, push].filter(Boolean).join(" ") || "Unknown number";
}

// personLabel names a user the way chatTitle names a chat.
const personLabel = (p) => p.name || fmtPhone(p.phone) || (p.push_name && "~" + p.push_name) || (p.username && "@" + p.username) || "Unknown number";

// userOf is the user part of a JID ("905551234567@s.whatsapp.net" -> "905551234567").
const userOf = (jid) => (jid || "").split("@")[0].split(":")[0];

// richText shows a text with its mentions ("@<user>" for each mentioned
// user) as highlighted names.
function richText(text, mentions) {
  if (!mentions?.length) return text;
  const byUser = new Map(mentions.map((p) => [userOf(p.jid), p]));
  return text.split(/(@\d+)/).map((part) => {
    const p = part.startsWith("@") && byUser.get(part.slice(1));
    return p ? h("span", { class: "mention" }, "@" + personLabel(p)) : part;
  });
}

// System messages (group changes, notices), by stub name.
const STUBS = {
  revoke: "This message was deleted", admin_revoke: "Deleted by an admin", ciphertext: "Waiting for this message",
  futureproof: "Unsupported message", e2e_encrypted: "Messages are end-to-end encrypted", e2e_encrypted_now: "Messages are now end-to-end encrypted",
  group_create: "Group created", group_change_subject: "Group name changed", group_change_icon: "Group icon changed",
  group_change_description: "Group description changed", group_change_invite_link: "Invite link reset",
  group_change_restrict: "Group info permissions changed", group_change_announce: "Send permissions changed",
  group_participant_add: "Added", sub_group_participant_add_rich: "Added", group_participant_remove: "Removed",
  group_participant_promote: "Now an admin", group_participant_demote: "No longer an admin", group_participant_invite: "Joined with an invite link",
  group_participant_leave: "Left", group_participant_change_number: "Changed their phone number", individual_change_number: "Changed their phone number",
  group_participant_linked_group_join: "Joined from the community", group_participant_accept: "Joined",
  group_membership_join_approval_request: "Asked to join", group_membership_join_approval_mode: "Join approval setting changed",
  community_create: "Community created", community_link_parent_group: "Group added to a community", community_unlink_parent_group: "Group removed from its community",
  community_link_sibling_group: "A group was added to the community", community_link_sub_group: "A group was added to the community",
  community_unlink_sibling_group: "A group was removed from the community", community_unlink_sub_group: "A group was removed from the community",
  community_participant_promote: "Now a community admin", community_participant_demote: "No longer a community admin",
  disappearing_mode: "Disappearing messages setting changed", block_contact: "Contact blocked or unblocked", change_username: "Username changed",
  change_limit_sharing: "Sharing settings changed", call_missed_voice: "Missed voice call", call_missed_video: "Missed video call",
  call_missed_group_voice: "Missed group voice call", call_missed_group_video: "Missed group video call",
  biz_privacy_mode_init_fb: "This business uses a secure service from Meta to manage this chat",
  biz_privacy_mode_to_fb: "This business uses a secure service from Meta to manage this chat",
};

function stubText(s) {
  const text = STUBS[s.name] || "System message";
  return s.people?.length ? text + ": " + s.people.map(personLabel).join(", ") : text;
}

// lastLine: a message as one line, for chat list previews.
function lastLine(m) {
  if (m.stub) return stubText(m.stub);
  const [, label] = MEDIA[m.media?.kind] || [];
  if (m.view_once) return "View once " + (label || "message").toLowerCase();
  return m.text || label || "";
}

function chatsView(base, inst) {
  let chats = [];
  let active = null;      // the open chat
  let archived = false;   // showing the archive
  let filter = "all";
  let drawn = "";         // what the list shows, to skip redundant redraws
  let lastID = "";        // newest message shown, likewise
  let stick = true;       // keep the conversation scrolled to the bottom
  let replying = null;    // the message being replied to
  let members = [];       // the open group's participants, for mentions
  const picked = new Map(); // "@Name" in the draft -> the mentioned user
  const pending = [];     // sent from here, not yet in the stored history
  const own = new Set([userOf(inst.jid), userOf(inst.lid)].filter(Boolean));
  const isMe = (jid) => own.has(userOf(jid));

  const search = h("input", { type: "search", placeholder: "Search", "aria-label": "Search chats" });
  const list = h("div", { class: "wa-list" });
  const chips = h("div", { class: "chips" });
  const head = h("div");
  const convHead = h("div", { class: "bar conv-head" });
  const msgs = h("div", { class: "wa-msgs" });
  const input = h("textarea", { rows: 1, placeholder: "Type a message", "aria-label": "Message" });
  const replyBar = h("div", { class: "reply-bar", hidden: true });
  const suggest = h("div", { class: "mention-pop", hidden: true });
  // Attach, type, then send; with nothing typed the send button records a
  // voice message instead.
  const fileInput = h("input", { type: "file", hidden: true, onchange: () => {
    const f = fileInput.files[0];
    fileInput.value = "";
    if (f) attach(f);
  } });
  const sendBtn = button("", { type: "submit", class: "primary icon", icon: "send", title: "Send" });
  const micBtn = button("", { class: "primary icon", icon: "mic", title: "Record a voice message", onclick: () => record() });
  const composer = h("form", { class: "bar composer", onsubmit: (e) => { e.preventDefault(); send(); } },
    button("", { class: "ghost icon", icon: "clip", title: "Attach", onclick: () => fileInput.click() }), fileInput, input, sendBtn, micBtn);
  const recBar = h("div", { class: "bar composer recording", hidden: true });
  const footer = h("div", { class: "conv-foot" }, suggest, replyBar, composer, recBar);
  const toggleSend = () => {
    const empty = !input.value.trim();
    sendBtn.hidden = empty;
    micBtn.hidden = !empty;
  };
  const conv = h("div", { class: "wa-conv" }, placeholder());
  const main = h("div", { class: "wa-split" }, conv);
  let info = null;        // the contact or group info pane, when open

  const visibleChat = (c) => !c.jid.endsWith("@broadcast") && !c.jid.endsWith("@newsletter");

  function drawHead() {
    head.replaceChildren(archived
      ? sideHead(h("span", { class: "with-back" }, button("", { class: "ghost icon", icon: "back", title: "Back", onclick: () => { archived = false; redraw(); } }), "Archived"))
      : sideHead("Chats", button("", { class: "ghost icon", icon: "plus", title: "New chat", onclick: () => newChat(open) })));
    chips.replaceChildren(...[["all", "All"], ["unread", "Unread"], ["groups", "Groups"]].map(([k, label]) =>
      h("button", { type: "button", class: "chip" + (filter === k ? " active" : ""), onclick: () => { filter = k; redraw(); } }, label)));
  }

  function drawList(force) {
    const q = search.value.trim().toLowerCase();
    const pool = chats.filter(visibleChat);
    const shown = pool.filter((c) => c.archived === archived
      && (filter !== "unread" || c.unread_count > 0)
      && (filter !== "groups" || c.jid.endsWith("@g.us"))
      && (!q || [chatTitle(c), c.phone, c.push_name, c.username].join(" ").toLowerCase().includes(q)));
    // Pinned chats first (the archive has none), each part newest first.
    shown.sort((a, b) => (b.pinned && !archived) - (a.pinned && !archived));
    const inArchive = pool.filter((c) => c.archived);
    const archivedUnread = inArchive.filter((c) => c.unread_count > 0).length;
    const key = JSON.stringify([archived, filter, q, active?.jid, inArchive.length, archivedUnread, shown.map((c) =>
      [c.jid, chatTitle(c), c.unread_count, c.pinned, c.last_message_at, c.last_message?.id])]);
    if (key === drawn && !force) return;
    drawn = key;
    const top = !archived && !q && inArchive.length
      ? h("button", { class: "wa-chat archive-row", type: "button", onclick: () => { archived = true; redraw(); } },
        h("span", { class: "avatar flat" }, icon("archive")), h("span", { class: "wa-name" }, "Archived"),
        archivedUnread ? h("span", { class: "count num", title: "Unread chats" }, archivedUnread) : null)
      : null;
    list.replaceChildren(...[top, ...shown.map(chatRow)].filter(Boolean));
    if (!shown.length && !top) list.append(h("div", { class: "empty" }, pool.length ? "No chats" : "No chats yet"));
  }

  function chatRow(c) {
    const m = c.last_message;
    const muted = c.muted_until && new Date(c.muted_until) > new Date();
    return h("button", { class: "wa-chat" + (active && active.jid === c.jid ? " active" : "") + (c.unread_count ? " unread" : ""), type: "button", onclick: () => openChat(c) },
      avatar(chatTitle(c), c.jid, base),
      h("div", { class: "wa-chat-main" },
        h("div", { class: "line" }, h("span", { class: "wa-name" }, chatTitle(c)), h("span", { class: "wa-when num" }, shortTime(c.last_message_at))),
        h("div", { class: "line" }, h("span", { class: "wa-preview" }, m ? preview(c, m) : chatSubtitle(c)),
          h("span", { class: "wa-flags" }, muted ? icon("mute") : null, c.pinned && !c.archived ? icon("pin") : null,
            c.unread_count ? h("span", { class: "count num" }, c.unread_count) : null))));
  }

  // preview: the last message as one line (author or tick, media kind, text).
  function preview(c, m) {
    if (m.stub) return stubText(m.stub);
    const [ic] = MEDIA[m.media?.kind] || [];
    const author = c.jid.endsWith("@g.us") && (m.sender_name || m.push_name || fmtPhone(m.sender_phone));
    const who = m.from_me ? h("span", { class: "wa-tick" }, "✓") : author ? author + ": " : null;
    return [who, m.view_once ? icon("once") : ic ? icon(ic) : null, m.text && !m.view_once ? richText(m.text, m.mentions) : lastLine(m)];
  }

  const redraw = () => { drawHead(); drawList(true); };

  async function loadChats() {
    chats = await api("GET", base + "/chats?limit=500");
    drawList();
  }

  function openChat(c) {
    active = c;
    lastID = "";
    stick = true;
    setReply(null);
    picked.clear();
    members = [];
    convHead.replaceChildren(h("button", { class: "conv-who", type: "button", title: "Info", onclick: toggleInfo },
      avatar(chatTitle(c), c.jid, base),
      h("div", { class: "ident" }, h("div", { class: "wa-name" }, chatTitle(c)), h("div", { class: "muted small" }, chatSubtitle(c)))));
    conv.replaceChildren(convHead, msgs, footer);
    msgs.replaceChildren();
    drawList();
    drawInfo();
    loadMessages(true);
    if (c.jid.endsWith("@g.us")) {
      api("GET", `${base}/groups/${enc(c.jid)}`).then((g) => { if (active === c) members = g.participants.filter((p) => !isMe(p.jid) && !isMe(p.phone)); }).catch(() => {});
    }
    input.focus();
  }

  // The info pane sits beside the conversation, resizable, and follows
  // the open chat.
  function toggleInfo() {
    if (info) {
      info.remove();
      info = null;
      return;
    }
    info = h("div", { class: "pane-wrap" });
    main.append(info);
    drawInfo();
  }

  function drawInfo() {
    if (!info || !active) return;
    const pick = (p) => {
      const c = chats.find((x) => x.jid === p.jid || (p.phone && x.phone === p.phone));
      if (c) openChat(c);
      else if (p.jid?.endsWith("@g.us")) openChat({ jid: p.jid, name: p.name || "" });
    };
    const chatName = (jid) => { const c = chats.find((x) => x.jid === jid); return c ? chatTitle(c) : ""; };
    const panel = infoPanel(base, active, toggleInfo, pick, chatName);
    info.replaceChildren(resizer(panel, "wax.pane.info", -1, 280, 560), panel);
  }

  // What a message bubble needs from the conversation.
  const ctx = {
    isMe,
    reply: (m) => { setReply(m); input.focus(); },
    forward: (m) => forwardDialog(base, chats.filter(visibleChat), m),
    jump: (id) => {
      const el = [...msgs.querySelectorAll(".wa-bubble")].find((b) => b.dataset.id === id);
      if (!el) return toast("Message is not loaded");
      el.scrollIntoView({ block: "center", behavior: "smooth" });
      el.classList.remove("flash");
      void el.offsetWidth; // restart the animation
      el.classList.add("flash");
    },
  };

  async function loadMessages(scroll) {
    if (!active) return;
    const c = active;
    const rows = (await api("GET", `${base}/chats/${enc(c.jid)}/messages?limit=100`)).reverse();
    if (c !== active) return; // switched meanwhile
    for (let i = pending.length - 1; i >= 0; i--) if (rows.some((m) => m.id === pending[i].id)) pending.splice(i, 1);
    // Protocol messages (edits, revokes), reactions and empty stubs have nothing to show.
    const visible = (m) => m.stub || m.text || m.media || !["text", "protocol", "reaction"].includes(m.type);
    const all = rows.filter(visible).concat(pending.filter((p) => p.chat === c.jid));
    const newest = all.length ? all[all.length - 1].id + all.length : "";
    if (newest === lastID && !scroll) return;
    lastID = newest;
    let day = "";
    let prev = null;
    msgs.replaceChildren(...all.flatMap((m) => {
      const d = dayLabel(m.timestamp);
      const sep = d !== day ? h("div", { class: "wa-day" }, (day = d)) : null;
      // A run of messages from one sender shows its name and picture once.
      const first = !!sep || !prev || !!prev.stub || prev.from_me !== m.from_me || prev.sender !== m.sender;
      prev = m;
      const b = bubble(base, c, m, ctx, first);
      return sep ? [sep, b] : [b];
    }));
    if (!all.length) msgs.append(h("div", { class: "wa-blank muted" }, "No messages"));
    if (scroll || stick) msgs.scrollTop = msgs.scrollHeight;
  }

  function setReply(m) {
    replying = m;
    replyBar.hidden = !m;
    if (!m) return replyBar.replaceChildren();
    const who = m.from_me ? "You" : senderLabel(m);
    replyBar.replaceChildren(quoteBlock({ type: m.media?.kind || m.type, text: lastLine(m) }, who),
      button("", { class: "ghost icon", icon: "close", title: "Cancel reply", onclick: () => setReply(null) }));
  }

  // --- mentions: "@" in a group suggests its participants ----------------------

  let choices = [];
  let chosen = 0;
  function mentionQuery() {
    const before = input.value.slice(0, input.selectionStart);
    const m = /(^|\s)@([^\s@]*)$/.exec(before);
    return m && members.length ? m[2].toLowerCase() : null;
  }
  function drawSuggest() {
    const q = mentionQuery();
    choices = q === null ? [] : members.filter((p) => [personLabel(p), p.phone, p.push_name, p.username].join(" ").toLowerCase().includes(q)).slice(0, 8);
    chosen = Math.min(chosen, Math.max(0, choices.length - 1));
    suggest.hidden = !choices.length;
    suggest.replaceChildren(...choices.map((p, i) => h("button", { type: "button", class: "wa-chat compact" + (i === chosen ? " active" : ""),
      onmousedown: (e) => { e.preventDefault(); choose(p); } },
      avatar(personLabel(p), p.jid, base), h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, personLabel(p))),
        h("div", { class: "line" }, h("span", { class: "wa-preview" }, chatSubtitle(p)))))));
  }
  function choose(p) {
    const pos = input.selectionStart;
    const before = input.value.slice(0, pos).replace(/@([^\s@]*)$/, "");
    const token = "@" + personLabel(p);
    picked.set(token, p);
    input.value = before + token + " " + input.value.slice(pos);
    input.selectionStart = input.selectionEnd = before.length + token.length + 1;
    suggest.hidden = true;
    autosize();
  }

  async function send() {
    let text = input.value.trim();
    if (!text || !active) return;
    // "@Name" picked from the suggestions is sent as "@<user>" plus the mention.
    const mentions = [];
    for (const [token, p] of picked) {
      if (!text.includes(token)) continue;
      text = text.split(token).join("@" + userOf(p.jid));
      mentions.push(p.jid);
    }
    const reply = takeReply();
    input.value = "";
    picked.clear();
    autosize();
    await guard(() => post({ text, reply_to: reply?.id, mentions: mentions.length ? mentions : undefined },
      { text, quoted: quoteOf(reply), mentions: mentions.map((jid) => members.find((x) => x.jid === jid)).filter(Boolean) }));
  }

  // takeReply returns the message being replied to and clears the reply.
  function takeReply() {
    const r = replying;
    setReply(null);
    return r;
  }

  const quoteOf = (m) => (m ? { id: m.id, type: m.media?.kind || m.type, text: lastLine(m), sender: m.from_me ? null : { jid: m.sender, name: senderLabel(m) } } : null);

  // post queues a message for the open chat and shows it at once (as
  // shown) with its state, until the stored history has it.
  async function post(body, shown) {
    const c = active;
    const r = await api("POST", base + "/messages", { to: c.jid, ...body });
    const p = { id: r.message_id, chat: c.jid, from_me: true, type: "text", timestamp: new Date().toISOString(), state: "queued", ...shown };
    pending.push(p);
    await loadMessages(true);
    // Media uploads take longer than text.
    for (let i = 0; i < (body.media ? 180 : 30) && p.state === "queued"; i++) {
      await new Promise((ok) => setTimeout(ok, 1000));
      const o = await api("GET", base + "/outbox/" + r.outbox_id);
      if (o.status !== "pending") p.state = o.status;
    }
    if (p.state === "failed") toast("Message not sent", true);
    lastID = "";
    await loadMessages(false);
    loadChats();
  }

  // --- media: attach (button, paste, drop), preview with caption, send --------

  function attach(file) {
    if (!active) return;
    if (file.size > 64 << 20) return toast("Files can be up to 64 MB", true);
    const kind = fileKind(file);
    const url = URL.createObjectURL(file);
    const preview = kind === "image" ? h("img", { src: url, alt: "", class: "attach-preview" })
      : kind === "video" ? h("video", { src: url, controls: true, class: "attach-preview" })
        : h("div", { class: "attach-file" }, h("span", { class: "avatar flat" }, icon(kind === "audio" ? "mic" : "file")),
          h("div", { class: "ident" }, h("div", { class: "wa-name" }, file.name), h("div", { class: "muted small" }, fmtSize(file.size))));
    const caption = h("input", { placeholder: "Add a caption", "aria-label": "Caption" });
    let close;
    const go = () => {
      close();
      sendFile(file, kind === "audio" ? "" : caption.value.trim());
    };
    caption.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); go(); } });
    const title = { image: "Send photo", video: "Send video", audio: "Send audio" }[kind] || "Send document";
    close = dialog(title, h("div", { class: "form" }, preview, kind === "audio" ? null : caption),
      (c) => [button("Cancel", { onclick: c }), button("Send", { class: "primary", onclick: go })]);
  }

  async function sendFile(file, caption, extra = {}) {
    if (!active) return;
    const kind = extra.kind || fileKind(file);
    const reply = takeReply();
    await guard(async () => {
      const [data, pv] = await Promise.all([fileBase64(file), mediaPreview(file, kind)]);
      const media = { kind, mimetype: file.type || undefined, caption: caption || undefined, file_name: kind === "document" ? file.name : undefined,
        data_base64: data, thumbnail_base64: pv.thumbnail, width: pv.width, height: pv.height, seconds: extra.seconds || pv.seconds,
        waveform_base64: extra.waveform ? bytesBase64(extra.waveform) : undefined };
      await post({ media, reply_to: reply?.id }, { type: kind, text: caption,
        media: { kind, file_name: file.name, seconds: media.seconds, local: URL.createObjectURL(file) }, quoted: quoteOf(reply) });
    });
  }

  // record captures a voice message; the bar offers discard or send.
  async function record() {
    if (!active || !navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) return toast("Recording is not available in this browser", true);
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch {
      return toast("Microphone access was not allowed", true);
    }
    // WhatsApp plays Ogg Opus voice notes; WebM Opus is converted by the server.
    const type = ["audio/ogg;codecs=opus", "audio/webm;codecs=opus"].find((t) => MediaRecorder.isTypeSupported(t));
    const rec = new MediaRecorder(stream, type ? { mimeType: type } : undefined);
    const chunks = [];
    rec.ondataavailable = (e) => chunks.push(e.data);
    const started = Date.now();
    const time = h("span", { class: "num" }, "0:00");
    const timer = setInterval(() => { time.textContent = fmtDuration((Date.now() - started) / 1000); }, 250);
    const stop = (keep) => new Promise((done) => {
      rec.onstop = () => {
        clearInterval(timer);
        stream.getTracks().forEach((t) => t.stop());
        recBar.hidden = true;
        composer.hidden = false;
        done(keep ? new Blob(chunks, { type: rec.mimeType }) : null);
      };
      rec.stop();
    });
    recBar.replaceChildren(button("", { class: "ghost icon", icon: "trash", title: "Discard", onclick: () => stop(false) }),
      h("span", { class: "rec-dot" }), time, h("span", { class: "spacer" }),
      button("", { class: "primary icon", icon: "send", title: "Send", onclick: async () => {
        const blob = await stop(true);
        if (!blob?.size) return;
        const voice = /ogg|webm/.test(blob.type); // anything else (Safari's MP4) goes as an audio file
        const ext = blob.type.includes("ogg") ? "ogg" : blob.type.includes("webm") ? "webm" : "m4a";
        sendFile(new File([blob], "voice." + ext, { type: blob.type.split(";")[0] + (voice ? ";codecs=opus" : "") }), "",
          { kind: voice ? "voice" : "audio", ...(await audioShape(blob)) });
      } }));
    composer.hidden = true;
    recBar.hidden = false;
    rec.start(250);
  }

  const autosize = () => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 140) + "px";
    toggleSend();
  };
  input.addEventListener("input", () => { autosize(); drawSuggest(); });
  input.addEventListener("paste", (e) => {
    const f = e.clipboardData?.files?.[0];
    if (f) {
      e.preventDefault();
      attach(f);
    }
  });
  conv.addEventListener("dragover", (e) => {
    if (!active || !e.dataTransfer?.types.includes("Files")) return;
    e.preventDefault();
    conv.classList.add("drop");
  });
  conv.addEventListener("dragleave", (e) => { if (!conv.contains(e.relatedTarget)) conv.classList.remove("drop"); });
  conv.addEventListener("drop", (e) => {
    conv.classList.remove("drop");
    const f = e.dataTransfer?.files?.[0];
    if (!f) return;
    e.preventDefault();
    attach(f);
  });
  toggleSend();
  input.addEventListener("click", drawSuggest);
  input.addEventListener("blur", () => { suggest.hidden = true; });
  input.addEventListener("keydown", (e) => {
    if (!suggest.hidden && choices.length) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        chosen = (chosen + (e.key === "ArrowDown" ? 1 : choices.length - 1)) % choices.length;
        return drawSuggest();
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        return choose(choices[chosen]);
      }
      if (e.key === "Escape") return void (suggest.hidden = true);
    }
    if (e.key === "Escape" && replying) return setReply(null);
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  });
  search.addEventListener("input", () => drawList());
  msgs.addEventListener("scroll", () => { stick = msgs.scrollHeight - msgs.scrollTop - msgs.clientHeight < 40; });
  // Images load after the bubbles are drawn: stay at the bottom meanwhile.
  msgs.addEventListener("load", () => { if (stick) msgs.scrollTop = msgs.scrollHeight; }, true);

  drawHead();
  guard(loadChats);
  every(10000, () => loadChats().catch(() => {}));
  every(4000, () => loadMessages(false).catch(() => {}));

  const open = (c) => openChat(chats.find((x) => x.jid === c.jid) || c);
  return {
    side: h("div", { class: "side" }, head, h("div", { class: "side-tools" }, search, chips), list),
    main,
    open,
  };
}

// forwardDialog sends a copy of message m to a chat picked from the list.
function forwardDialog(base, chats, m) {
  const search = h("input", { type: "search", placeholder: "Search", "aria-label": "Search chats" });
  const list = h("div", { class: "pick-list" });
  let close;
  const draw = () => {
    const q = search.value.trim().toLowerCase();
    list.replaceChildren(...chats.filter((c) => !q || [chatTitle(c), c.phone].join(" ").toLowerCase().includes(q)).slice(0, 50).map((c) =>
      h("button", { type: "button", class: "wa-chat compact", onclick: () => guard(async () => {
        await api("POST", base + "/messages", { to: c.jid, forward: { chat: m.chat, message_id: m.id } });
        toast("Forwarded to " + chatTitle(c));
        close();
      }) }, avatar(chatTitle(c), c.jid, base),
      h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, chatTitle(c)))))));
  };
  search.addEventListener("input", draw);
  draw();
  close = dialog("Forward to", h("div", { class: "form" }, search, list), (c) => [button("Cancel", { onclick: c })]);
}

// quoteBlock is a quoted message: its author and a line of it.
function quoteBlock(q, who, onclick) {
  const [ic, label] = MEDIA[q.type] || [];
  return h(onclick ? "button" : "div", { class: "wa-quote", type: onclick ? "button" : undefined, onclick },
    h("div", { class: "q-who" }, who || ""), h("div", { class: "q-text" }, ic ? icon(ic) : null, q.text || label || ""));
}

// infoPanel shows everything known about a chat: for a contact its about
// text, business profile, details and groups in common; for a group its
// description, settings, community and participants; for both the shared
// media and documents. pick opens a chat ({jid} or {phone}).
function infoPanel(base, c, close, pick, chatName) {
  const group = c.jid.endsWith("@g.us");
  const card = h("div", { class: "info-card" }, avatar(chatTitle(c), c.jid, base, "xxl"),
    h("div", { class: "info-title" }, chatTitle(c)),
    h("div", { class: "muted num" }, chatSubtitle(c)));
  const top = h("div", {}, h("div", { class: "empty" }, "Loading"));
  const media = h("div");
  const body = h("div", { class: "info-body" }, card, top, media);
  const path = `${base}/chats/${enc(c.jid)}`;
  const failed = () => top.replaceChildren(h("div", { class: "empty" }, "Info unavailable"));
  api("GET", path + "/profile").then((p) => {
    media.replaceChildren(sharedMedia(base, c, p.messages));
    if (group) {
      return api("GET", `${base}/groups/${enc(c.jid)}`).then((g) => {
        card.lastChild.textContent = "Group · " + (g.size || g.participants.length) + " members";
        top.replaceChildren(groupDetails(base, g, pick, chatName));
      });
    }
    if (p.is_business) card.append(h("span", { class: "tag" }, "Business account"));
    top.replaceChildren(contactDetails(base, p, c, pick));
  }).catch(failed);
  return h("aside", { class: "pane" },
    h("div", { class: "bar pane-head" }, h("span", {}, group ? "Group info" : "Contact info"), button("", { class: "ghost icon", icon: "close", title: "Close", onclick: close })),
    body);
}

// infoSection is a titled block of the info pane.
function infoSection(title, ...content) {
  const parts = content.flat().filter(Boolean);
  return parts.length ? h("div", { class: "info-section" }, title ? h("div", { class: "list-label" }, title) : null, parts) : null;
}

function infoRows(pairs) {
  const rows = pairs.filter(([, v]) => v !== undefined && v !== null && v !== "" && v !== false);
  return rows.length ? h("div", { class: "info-rows" }, rows.map(([k, v]) => h("div", { class: "info-row" }, h("div", { class: "k" }, k), h("div", {}, v)))) : null;
}

const fmtDate = (s) => (s ? new Date(s).toLocaleDateString("en-GB", { day: "numeric", month: "long", year: "numeric" }) : "");

// safeLink links to an http(s) address only.
function safeLink(url) {
  return /^https?:\/\//i.test(url) ? h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, url) : url;
}

function contactDetails(base, p, c, pick) {
  const muted = c.muted_until && new Date(c.muted_until) > new Date();
  const biz = p.business;
  const fields = biz?.fields || {};
  const fieldLabel = (k) => k.charAt(0).toUpperCase() + k.slice(1).replace(/_/g, " ");
  const known = ["description", "address", "email", "website"];
  const others = Object.keys(fields).filter((k) => !known.includes(k)).sort();
  const value = (k, v) => (k === "website" ? safeLink(v) : k === "email" ? h("a", { href: "mailto:" + v }, v) : v);
  return h("div", {},
    infoSection("About", p.about ? h("div", { class: "info-row" }, h("div", { class: "about" }, p.about),
      p.about_set_at ? h("div", { class: "k" }, fmtDate(p.about_set_at)) : null)
      : h("div", { class: "info-row muted" }, !p.live ? "WhatsApp did not answer, try again in a minute" : p.about_hidden ? "Hidden by privacy settings" : "No about")),
    biz ? infoSection("Business",
      biz.categories?.length ? infoRows([["Category", biz.categories.join(", ")]]) : null,
      infoRows([...known, ...others].flatMap((k) => (fields[k] || []).map((v) => [fieldLabel(k), value(k, v)]))),
      biz.hours?.length ? infoRows([["Hours" + (biz.time_zone ? " (" + biz.time_zone + ")" : ""),
        h("div", { class: "hours" }, biz.hours.map((d) => h("div", {}, h("span", {}, d.day), h("span", { class: "muted" }, [d.mode.replace(/_/g, " "), d.open && d.close ? d.open + "-" + d.close : ""].filter(Boolean).join(" "))))) ]]) : null) : null,
    infoSection("Details", infoRows([
      ["Phone", fmtPhone(p.phone)], ["Name in address book", p.name], ["Their name", p.push_name && "~" + p.push_name],
      ["Username", p.username && "@" + p.username], ["Devices", p.devices ? (p.devices === 1 ? "Phone only" : "Phone + " + (p.devices - 1) + " linked device" + (p.devices > 2 ? "s" : "")) : ""], ["LID", p.lid],
      ["Muted", muted ? (new Date(c.muted_until).getFullYear() > 2100 ? "Always" : "Until " + fmtTime(c.muted_until)) : ""],
      ["Pinned", c.pinned ? "Yes" : ""], ["Archived", c.archived ? "Yes" : ""],
    ])),
    infoSection(p.common_groups.length ? p.common_groups.length + " groups in common" : "",
      p.common_groups.map((g) => h("button", { type: "button", class: "wa-chat compact", onclick: () => pick({ jid: g.jid, name: g.name }) },
        avatar(g.name, g.jid, base), h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, g.name)))))));
}

// fmtTimer formats a disappearing messages timer.
function fmtTimer(sec) {
  if (!sec) return "Off";
  if (sec % 86400 === 0) return sec / 86400 + (sec === 86400 ? " day" : " days");
  if (sec % 3600 === 0) return sec / 3600 + " hours";
  return sec + " seconds";
}

function groupDetails(base, g, pick, chatName) {
  const rank = (p) => (p.is_super_admin ? 0 : p.is_admin ? 1 : 2);
  const people = g.participants.map((p) => ({ ...p, title: chatTitle(p) }))
    .sort((a, b) => rank(a) - rank(b) || a.title.localeCompare(b.title));
  // who names a user of the group's metadata (creator, editors).
  const who = (jid, pn) => {
    if (!jid && !pn) return "";
    const p = people.find((x) => x.jid === jid || (pn && x.phone_number === pn) || x.lid === jid);
    return p ? p.title : phoneOf(pn) || phoneOf(jid) || "Former member";
  };
  const byOn = (jid, at, pn) => [who(jid, pn), fmtDate(at)].filter(Boolean).join(", ");
  const addMode = { admin_add: "Only admins", all_member_add: "All members" };
  return h("div", {},
    g.topic ? infoSection("Description", h("div", { class: "info-row" }, h("div", { class: "about" }, g.topic),
      g.topic_set_at ? h("div", { class: "k" }, byOn(g.topic_set_by, g.topic_set_at)) : null)) : null,
    infoSection("Details", infoRows([
      ["Community", g.community ? chatName(g.community) || "Yes" : g.is_community ? "This is a community" : ""],
      ["Announcement group", g.default_sub_group ? "Yes" : ""],
      ["Created", byOn(g.owner, g.created, g.owner_pn)],
      ["Name set", g.name_set_at ? byOn(g.name_set_by, g.name_set_at) : ""],
      ["Disappearing messages", fmtTimer(g.ephemeral)],
      ["Send messages", g.announce ? "Only admins" : "All members"],
      ["Edit group info", g.locked ? "Only admins" : "All members"],
      ["Add members", addMode[g.member_add_mode] || g.member_add_mode],
      ["Approve new members", g.join_approval ? "On" : "Off"],
      ["Suspended", g.suspended ? "Yes" : ""],
    ])),
    infoSection(people.length + " participants",
      people.map((p) => h("button", { class: "wa-chat compact", type: "button", onclick: () => pick(p) },
        avatar(p.title, p.jid, base),
        h("div", { class: "wa-chat-main" },
          h("div", { class: "line" }, h("span", { class: "wa-name" }, p.title), p.is_admin || p.is_super_admin ? h("span", { class: "tag" }, p.is_super_admin ? "Owner" : "Admin") : null),
          h("div", { class: "line" }, h("span", { class: "wa-preview" }, chatSubtitle(p))))))));
}

// sharedMedia: counts of the chat's media, a grid of the latest photos
// and videos, and the latest documents.
function sharedMedia(base, c, counts) {
  const n = (...types) => types.reduce((sum, t) => sum + (counts?.[t] || 0), 0);
  const summary = [["Photos", n("image")], ["Videos", n("video", "gif")], ["Documents", n("document")],
    ["Voice and audio", n("ptt", "voice", "audio")], ["Stickers", n("sticker")]].filter(([, v]) => v);
  if (!summary.length) return infoSection("Media and documents", h("div", { class: "info-row muted" }, "None"));
  const grid = h("div", { class: "media-grid" });
  const docs = h("div");
  const path = `${base}/chats/${enc(c.jid)}/messages`;
  if (n("image", "video", "gif")) {
    api("GET", path + "?types=image,video,gif&limit=9").then((ms) => grid.replaceChildren(...ms.map((m) => {
      const url = `${base}/chats/${enc(m.chat)}/messages/${enc(m.id)}/media`;
      if (m.view_once) return h("div", { class: "tile" }, icon("once"));
      return m.media?.kind === "image" ? mediaImage(url, "image") : h("button", { type: "button", class: "tile", title: "Download video",
        onclick: () => guard(async () => h("a", { href: await mediaURL(url), download: m.id }).click()) }, icon("video"));
    }))).catch(() => {});
  }
  if (n("document")) {
    api("GET", path + "?types=document&limit=5").then((ms) => docs.replaceChildren(...ms.map((m) => {
      const url = `${base}/chats/${enc(m.chat)}/messages/${enc(m.id)}/media`;
      return h("button", { type: "button", class: "wa-chat compact", onclick: () => guard(async () => h("a", { href: await mediaURL(url), download: m.media?.file_name || m.id }).click()) },
        h("span", { class: "avatar flat" }, icon("file")),
        h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, m.media?.file_name || "Document"), h("span", { class: "wa-when" }, shortTime(m.timestamp)))));
    }))).catch(() => {});
  }
  return infoSection("Media and documents", infoRows(summary.map(([k, v]) => [k, String(v)])), grid, docs);
}

// bubble draws one message: a system line, or a bubble with its author
// (first of a run in groups, with the sender's picture), forwarding mark,
// quote, media, text and time; hovering shows reply and forward.
function bubble(base, chat, m, ctx, first) {
  if (m.stub && m.stub.name !== "revoke") return h("div", { class: "wa-system" }, stubText(m.stub));
  const group = chat.jid.endsWith("@g.us");
  const kind = m.media?.kind;
  const deleted = !!m.stub;
  const url = `${base}/chats/${enc(m.chat || chat.jid)}/messages/${enc(m.id)}/media`; // m.chat: the history may span both addresses of a contact
  let media = null;
  if (m.view_once) {
    // Like WhatsApp on linked devices: view once media opens on the phone only.
    media = h("div", { class: "wa-once" }, icon("once"), "View once " + (MEDIA[kind]?.[1] || "message").toLowerCase());
  } else if (m.media?.local && kind === "image") {
    media = h("div", { class: "wa-img image loaded" }, h("img", { src: m.media.local, alt: "" })); // not stored yet: the file itself
  } else if (kind === "image" || kind === "sticker") {
    media = mediaImage(url, kind);
  } else if (["audio", "voice", "video"].includes(kind)) {
    media = mediaPlayer(url, m.media);
  } else if (m.media) {
    media = button(m.media.file_name || MEDIA[kind]?.[1] || kind, { icon: "download", class: "ghost", onclick: () => guard(async () => {
      h("a", { href: await mediaURL(url), download: m.media.file_name || m.id }).click();
    }) });
  }
  let body = null;
  if (deleted) body = h("span", { class: "text deleted" }, stubText(m.stub));
  else if (m.text && !m.view_once) body = h("span", { class: "text" }, richText(m.text, m.mentions));
  else if (!media && m.type !== "text") body = h("span", { class: "text deleted" }, "Unsupported message");
  const quoteWho = (q) => (!q.sender ? "" : ctx.isMe(q.sender.jid) || ctx.isMe(q.sender.phone) ? "You" : personLabel(q.sender));
  const tick = m.from_me ? h("span", { class: "wa-tick" }, m.state === "queued" ? "…" : m.state === "failed" ? "!" : "✓") : null;
  const actions = deleted || m.state ? null : h("div", { class: "wa-actions" },
    button("", { class: "ghost icon", icon: "reply", title: "Reply", onclick: () => ctx.reply(m) }),
    m.view_once ? null : button("", { class: "ghost icon", icon: "share", title: "Forward", onclick: () => ctx.forward({ ...m, chat: m.chat || chat.jid }) }));
  const el = h("div", { class: "wa-bubble" + (m.from_me ? " me" : "") + (kind === "sticker" && !m.view_once ? " bare" : ""), "data-id": m.id },
    actions,
    !m.from_me && group && first ? h("div", { class: "wa-author" }, senderLabel(m)) : null,
    m.forwarded ? h("div", { class: "wa-fwd" }, icon("share"), m.forwarding_score >= 5 ? "Forwarded many times" : "Forwarded") : null,
    m.quoted ? quoteBlock(m.quoted, quoteWho(m.quoted), () => ctx.jump(m.quoted.id)) : null,
    media, body,
    h("span", { class: "wa-time num" }, clock(m.timestamp), tick));
  if (!group || m.from_me) return el;
  return h("div", { class: "wa-row" }, first ? avatar(senderLabel(m), m.sender, base, "sm") : h("span", { class: "avatar-gap" }), el);
}

// --- communities ---------------------------------------------------------------------

// communitiesView lists the communities the account is in, each with its
// groups (the announcement group first); a group opens in the chats.
function communitiesView(base, openGroup) {
  const list = h("div", { class: "wa-list" }, h("div", { class: "empty" }, "Loading"));
  async function load() {
    const groups = await api("GET", base + "/groups");
    const communities = groups.filter((g) => g.is_community).sort((a, b) => a.name.localeCompare(b.name));
    const groupsOf = (c) => groups.filter((g) => g.community === c.jid)
      .sort((a, b) => b.default_sub_group - a.default_sub_group || a.name.localeCompare(b.name));
    list.replaceChildren(...communities.map((c) => h("div", { class: "community" },
      h("div", { class: "community-head" }, avatar(c.name, c.jid, base, "square"),
        h("div", { class: "ident" }, h("div", { class: "wa-name" }, c.name), h("div", { class: "muted small" }, groupsOf(c).length + " groups"))),
      groupsOf(c).map((g) => h("button", { type: "button", class: "wa-chat compact", onclick: () => openGroup(g) },
        g.default_sub_group ? h("span", { class: "avatar flat" }, icon("megaphone")) : avatar(g.name, g.jid, base),
        h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, g.default_sub_group ? "Announcements" : g.name)),
          h("div", { class: "line" }, h("span", { class: "wa-preview" }, g.participants + " participants"))))))));
    if (!communities.length) list.replaceChildren(h("div", { class: "empty" }, "No communities"));
  }
  return { side: h("div", { class: "side" }, sideHead("Communities"), list), main: placeholder(), load: () => load().catch((e) => list.replaceChildren(h("div", { class: "empty" }, e.message))) };
}

// fileKind is how a file is sent: photos and MP4 videos inline, audio as
// audio, anything else as a document.
function fileKind(file) {
  if (/^image\/(jpeg|png|webp|gif)$/.test(file.type)) return "image";
  if (/^video\/(mp4|3gpp)$/.test(file.type)) return "video";
  if (file.type.startsWith("audio/")) return "audio";
  return "document";
}

const fmtDuration = (s) => Math.floor(s / 60) + ":" + String(Math.floor(s % 60)).padStart(2, "0");

function fmtSize(n) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  for (; n >= 1024 && i < units.length - 1; i++) n /= 1024;
  return (i ? n.toFixed(1) : n) + " " + units[i];
}

const fileBase64 = (file) => new Promise((ok, fail) => {
  const r = new FileReader();
  r.onload = () => ok(String(r.result).split(",")[1] || "");
  r.onerror = () => fail(new Error("Could not read the file"));
  r.readAsDataURL(file);
});

const bytesBase64 = (bytes) => btoa(String.fromCharCode(...bytes));

// mediaPreview makes what WhatsApp shows before a photo or video is
// downloaded: a small JPEG, the size and (video) the length.
async function mediaPreview(file, kind) {
  const thumb = (src, w, h) => {
    const scale = Math.min(1, 96 / Math.max(w, h));
    const c = document.createElement("canvas");
    c.width = Math.max(1, Math.round(w * scale));
    c.height = Math.max(1, Math.round(h * scale));
    c.getContext("2d").drawImage(src, 0, 0, c.width, c.height);
    return c.toDataURL("image/jpeg", 0.6).split(",")[1];
  };
  try {
    if (kind === "image") {
      const bmp = await createImageBitmap(file);
      return { thumbnail: thumb(bmp, bmp.width, bmp.height), width: bmp.width, height: bmp.height };
    }
    if (kind === "video") {
      const v = h("video", { muted: true, preload: "auto", src: URL.createObjectURL(file) });
      await within(new Promise((ok, fail) => { v.onloadeddata = ok; v.onerror = fail; }), 5000);
      v.currentTime = Math.min(0.1, v.duration / 2 || 0);
      await within(new Promise((ok) => { v.onseeked = ok; }), 5000);
      return { thumbnail: thumb(v, v.videoWidth, v.videoHeight), width: v.videoWidth, height: v.videoHeight, seconds: Math.round(v.duration) || undefined };
    }
  } catch { /* previews are optional */ }
  return {};
}

// within fails p if it takes longer than ms (a file the browser cannot
// decode may never answer).
const within = (p, ms) => Promise.race([p, new Promise((_, fail) => setTimeout(() => fail(new Error("timeout")), ms))]);

// audioShape measures a recording: its length and a 64-value waveform.
async function audioShape(blob) {
  try {
    const ctx = new AudioContext();
    const buf = await within(ctx.decodeAudioData(await blob.arrayBuffer()), 10000);
    ctx.close();
    const data = buf.getChannelData(0);
    const step = Math.max(1, Math.floor(data.length / 64));
    const levels = Array.from({ length: 64 }, (_, i) => {
      let sum = 0;
      for (let j = i * step; j < Math.min(data.length, (i + 1) * step); j++) sum += data[j] * data[j];
      return Math.sqrt(sum / step);
    });
    const top = Math.max(...levels) || 1;
    return { seconds: Math.round(buf.duration) || undefined, waveform: Uint8Array.from(levels, (l) => Math.round((l / top) * 100)) };
  } catch {
    return {};
  }
}

// mediaPlayer shows a video as a play button (the file is fetched when
// played) and audio with audioPlayer.
function mediaPlayer(url, media) {
  if (media.kind !== "video") return audioPlayer(url, media);
  const player = (src) => h("video", { src, controls: true, autoplay: true, class: "wa-video" });
  if (media.local) return player(media.local);
  const el = h("button", { type: "button", class: "wa-play video", onclick: () => guard(async () => {
    el.disabled = true;
    el.replaceWith(player(await mediaURL(url)));
  }) }, h("span", { class: "play" }, icon("play")), "Video" + (media.seconds ? " · " + fmtDuration(media.seconds) : ""));
  return el;
}

let playing = null; // the audio element playing now: one at a time
const BARS = 36;    // waveform bars per player

// audioPlayer is a WhatsApp-like player: play/pause, the waveform (which
// fills as it plays and seeks on click), the time and, for voice notes,
// the speed. The file is fetched on first play.
function audioPlayer(url, media) {
  const voice = media.kind === "voice";
  const audio = new Audio();
  audio.preload = "none";
  let loaded = !!media.local;
  if (loaded) audio.src = media.local;
  // Levels: the voice note's own waveform, otherwise a flat line.
  let levels = [];
  try { levels = media.waveform ? Array.from(atob(media.waveform), (c) => c.charCodeAt(0)) : []; } catch { /* ignore */ }
  if (levels.length < 8) levels = new Array(BARS).fill(voice ? 30 : 12);
  // Fewer, wider bars than the 64 levels: the loudest of each group.
  const per = levels.length / BARS;
  levels = Array.from({ length: BARS }, (_, i) => Math.max(...levels.slice(Math.floor(i * per), Math.max(Math.floor(i * per) + 1, Math.floor((i + 1) * per)))));
  const bars = levels.map((v) => {
    const bar = h("span", { class: "lvl" });
    bar.style.height = Math.max(12, Math.min(100, v)) + "%";
    return bar;
  });
  const wave = h("div", { class: "wave", role: "slider", "aria-label": "Position" }, bars);
  const playBtn = h("button", { type: "button", class: "play", title: "Play" }, icon("play"));
  const time = h("span", { class: "time num" }, media.seconds ? fmtDuration(media.seconds) : "0:00");
  const speeds = [1, 1.5, 2];
  const speed = voice ? h("button", { type: "button", class: "speed num", title: "Speed" }, "1x") : null;
  const el = h("div", { class: "wa-audio" + (voice ? " voice" : "") }, playBtn, h("div", { class: "track" }, wave, h("div", { class: "meta" }, time)), speed);

  const duration = () => (Number.isFinite(audio.duration) && audio.duration > 0 ? audio.duration : media.seconds || 0);
  const draw = () => {
    const d = duration();
    const at = d ? audio.currentTime / d : 0;
    const lit = Math.round(at * bars.length);
    bars.forEach((b, i) => b.classList.toggle("on", i < lit));
    time.textContent = fmtDuration(audio.paused && audio.currentTime === 0 ? d : audio.currentTime);
  };
  const setIcon = (name) => { playBtn.replaceChildren(icon(name)); playBtn.title = name === "play" ? "Play" : "Pause"; };
  const load = async () => {
    if (loaded) return;
    el.classList.add("loading");
    try {
      audio.src = await mediaURL(url);
      loaded = true;
    } finally {
      el.classList.remove("loading");
    }
  };
  const play = () => guard(async () => {
    await load();
    if (playing && playing !== audio) playing.pause();
    playing = audio;
    await audio.play();
  });
  playBtn.addEventListener("click", () => (audio.paused ? play() : audio.pause()));
  wave.addEventListener("click", (e) => guard(async () => {
    await load();
    const r = wave.getBoundingClientRect();
    const seek = () => { audio.currentTime = Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * duration(); draw(); };
    if (audio.readyState >= 1) seek();
    else audio.addEventListener("loadedmetadata", seek, { once: true });
    if (audio.paused) play();
  }));
  speed?.addEventListener("click", () => {
    audio.playbackRate = speeds[(speeds.indexOf(audio.playbackRate) + 1) % speeds.length];
    speed.textContent = audio.playbackRate + "x";
  });
  audio.addEventListener("play", () => setIcon("pause"));
  audio.addEventListener("pause", () => setIcon("play"));
  audio.addEventListener("ended", () => { audio.currentTime = 0; draw(); });
  audio.addEventListener("timeupdate", draw);
  audio.addEventListener("loadedmetadata", draw);
  return el;
}

// --- pictures and media --------------------------------------------------------------

// Pictures and attachments are fetched with the API key and kept as blob
// URLs for the life of the page; lookups start when they scroll into view.
const blobs = new Map();
function blobURL(path, retry) {
  if (!blobs.has(path)) {
    blobs.set(path, api("GET", path, null, true).then(async (r) => URL.createObjectURL(await r.blob())).catch((e) => {
      if (retry && e.status !== 404) setTimeout(() => blobs.delete(path), 60000); // try again later
      return null;
    }));
  }
  return blobs.get(path);
}
const mediaURL = (path) => blobURL(path, true).then((u) => u || Promise.reject(new Error("Media unavailable")));

const loaders = new WeakMap();
const lazy = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    lazy.unobserve(e.target);
    loaders.get(e.target)?.();
  }
}, { rootMargin: "300px" });
function whenVisible(el, load) {
  loaders.set(el, load);
  lazy.observe(el);
  return el;
}

// avatar shows the profile picture of jid, or an initial until (unless) it loads.
function avatar(label, jid, base, cls) {
  const ch = jid && jid.endsWith("@g.us") ? icon("users") : (String(label).replace(/^\+/, "").trim()[0] || "?").toUpperCase();
  const el = h("span", { class: "avatar" + (cls ? " " + cls : "") }, ch);
  if (!base || !jid || jid.endsWith("@broadcast")) return el;
  return whenVisible(el, () => blobURL(`${base}/chats/${enc(jid)}/picture`, true).then((u) => {
    if (u) el.replaceChildren(h("img", { src: u, alt: "" }));
  }));
}

function mediaImage(path, kind) {
  const box = h("div", { class: "wa-img " + kind });
  return whenVisible(box, () => blobURL(path, true).then((u) => {
    if (!u) return box.replaceChildren(h("span", { class: "muted small" }, "Image unavailable"));
    box.classList.add("loaded");
    box.replaceChildren(h("a", { href: u, target: "_blank", rel: "noopener" }, h("img", { src: u, alt: "" })));
  }));
}

// --- status ------------------------------------------------------------------------

const STATUS_MS = 6000; // how long a photo or text update stays on screen

// statusView lists the status updates of the last 24 hours by author and
// plays them like WhatsApp: segments on top, tap or arrows to move.
function statusView(base, me) {
  let groups = [];
  let current = null; // { group, index }
  let timer = 0;
  const seen = new Set();
  const list = h("div", { class: "wa-list" });
  const main = h("div", { class: "wa-status" }, placeholder());

  async function load() {
    const since = Date.now() - 864e5;
    const rows = await api("GET", `${base}/chats/${enc("status@broadcast")}/messages?limit=500`);
    const by = new Map();
    for (const m of rows.reverse()) {
      if (new Date(m.timestamp) < since || !(m.text || m.media)) continue;
      const key = m.from_me ? me : m.sender;
      if (!by.has(key)) by.set(key, { jid: key, mine: m.from_me, items: [] });
      const g = by.get(key);
      g.name = m.from_me ? "My status" : senderLabel(m);
      g.items.push(m);
    }
    groups = [...by.values()].sort((a, b) => new Date(b.items.at(-1).timestamp) - new Date(a.items.at(-1).timestamp));
    draw();
  }

  function draw() {
    const mine = groups.find((g) => g.mine);
    const others = groups.filter((g) => !g.mine);
    const row = (g, sub) => h("button", { class: "wa-chat" + (current?.group.jid === g.jid ? " active" : ""), type: "button", onclick: () => play(g, 0) },
      h("span", { class: "ring" + (seen.has(g.jid) ? " seen" : "") }, avatar(g.name, g.jid, base)),
      h("div", { class: "wa-chat-main" }, h("div", { class: "line" }, h("span", { class: "wa-name" }, g.name)),
        h("div", { class: "line" }, h("span", { class: "wa-preview" }, sub || shortTime(g.items.at(-1).timestamp)))));
    list.replaceChildren(
      mine ? row(mine) : h("div", { class: "wa-chat static" }, avatar("Me", me, base), h("div", { class: "wa-chat-main" },
        h("div", { class: "line" }, h("span", { class: "wa-name" }, "My status")), h("div", { class: "line" }, h("span", { class: "wa-preview" }, "No updates")))),
      others.length ? h("div", { class: "list-label" }, "Recent") : h("div", { class: "empty" }, "No updates"),
      ...others.map((g) => row(g)));
  }

  function play(group, index) {
    clearTimeout(timer);
    if (index < 0) index = 0;
    if (index >= group.items.length) {
      const next = groups[groups.indexOf(group) + 1];
      return next ? play(next, 0) : close();
    }
    current = { group, index };
    seen.add(group.jid);
    draw();
    const m = group.items[index];
    const kind = m.media?.kind;
    const path = `${base}/chats/${enc("status@broadcast")}/messages/${enc(m.id)}/media`;
    const stage = h("div", { class: "stage" + (m.media ? "" : " text") });
    if (kind === "image") blobURL(path, true).then((u) => u && stage.replaceChildren(h("img", { src: u, alt: "" })));
    else if (kind === "video") blobURL(path, true).then((u) => u && stage.replaceChildren(h("video", { src: u, autoplay: true, playsinline: true, onended: () => play(group, index + 1) })));
    else if (m.media) stage.append(h("span", { class: "muted" }, MEDIA[kind]?.[1] || kind));
    else stage.append(h("span", {}, m.text));
    main.replaceChildren(h("div", { class: "viewer" },
      h("div", { class: "segments" }, group.items.map((_, i) => h("span", { class: i < index ? "done" : i === index ? (kind === "video" ? "done" : "run") : "" }))),
      h("div", { class: "viewer-head" }, avatar(group.name, group.jid, base),
        h("div", { class: "ident" }, h("div", { class: "wa-name" }, group.name), h("div", { class: "small" }, shortTime(m.timestamp))),
        button("", { class: "ghost icon", icon: "close", title: "Close", onclick: close })),
      h("div", { class: "viewer-body" },
        h("button", { class: "nav prev", type: "button", "aria-label": "Previous", onclick: () => play(group, index - 1) }, icon("back")),
        stage,
        h("button", { class: "nav next", type: "button", "aria-label": "Next", onclick: () => play(group, index + 1) }, icon("forward"))),
      m.media && m.text ? h("div", { class: "caption" }, m.text) : null));
    if (kind !== "video") timer = setTimeout(() => main.isConnected && current?.group === group && play(group, index + 1), STATUS_MS);
  }

  function close() {
    clearTimeout(timer);
    current = null;
    main.replaceChildren(placeholder());
    draw();
  }

  // One handler per page (assigned, not added): arrows move, Escape closes.
  window.onkeydown = (e) => {
    if (!current || !main.isConnected || e.target.closest?.("input, textarea")) return;
    if (e.key === "ArrowRight") play(current.group, current.index + 1);
    else if (e.key === "ArrowLeft") play(current.group, current.index - 1);
    else if (e.key === "Escape") close();
  };
  guard(load);
  every(30000, () => load().catch(() => {}));
  return { side: h("div", { class: "side" }, sideHead("Status"), list), main };
}

// pairingView shows the QR code (or the state) of an account that is not connected.
function pairingView(base, inst) {
  const qr = h("div", { class: "qr" }, h("div", { class: "muted" }, STATUS[inst.status]?.[1] || inst.status));
  const load = async () => {
    try {
      const res = await api("GET", base + "/qr?format=png", null, true);
      qr.replaceChildren(h("img", { src: URL.createObjectURL(await res.blob()), alt: "QR code" }));
    } catch { /* no code pending */ }
  };
  if (inst.qr_pending) {
    load();
    every(4000, load); // codes rotate every 20 s
  }
  return h("div", { class: "wa-blank" }, qr);
}

const clock = (s) => (s ? new Date(s).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" }) : "");

// shortTime: today's time, "Yesterday", otherwise the date.
function shortTime(s) {
  if (!s) return "";
  const d = dayLabel(s);
  return d === "Today" ? clock(s) : d === "Yesterday" ? d : new Date(s).toLocaleDateString("en-GB", { day: "2-digit", month: "2-digit", year: "2-digit" });
}

function dayLabel(s) {
  const d = new Date(s);
  const today = new Date();
  const yesterday = new Date(today.getTime() - 864e5);
  if (d.toDateString() === today.toDateString()) return "Today";
  if (d.toDateString() === yesterday.toDateString()) return "Yesterday";
  return d.toLocaleDateString("en-GB", { day: "numeric", month: "long", year: "numeric" });
}

function newChat(open) {
  formDialog("New chat", [{ name: "phone", label: "Phone number", placeholder: "905551234567", required: true }], "Open", async (v) => {
    const digits = v.phone.replace(/\D/g, "");
    if (digits.length < 8) throw new Error("Invalid number");
    open({ jid: digits + "@s.whatsapp.net", phone: digits, name: "" });
  });
}

// settingsView: the profile, webhook and send rate in the side panel;
// main is what stays on the right meanwhile.
function settingsView(base, inst, main) {
  const me = (inst.jid || "").replace(/:\d+@/, "@");
  const url = h("input", { value: inst.webhook_url || "", placeholder: "https://" });
  const secret = h("input", { type: "password", placeholder: "••••••", autocomplete: "off" });
  const rate = h("input", { type: "number", min: 0, max: 600, value: inst.rate_per_minute });
  const save = (e) => {
    e.preventDefault();
    guard(async () => {
      const patch = { webhook_url: url.value.trim(), rate_per_minute: Number(rate.value || 0) };
      if (secret.value) patch.webhook_secret = secret.value;
      await api("PATCH", base, patch);
      secret.value = "";
      toast("Saved");
    });
  };
  const side = h("div", { class: "side" }, sideHead("Settings"),
    h("div", { class: "profile" }, avatar(inst.id, me, base, "xl"),
      h("div", { class: "ident" }, h("div", { class: "wa-name" }, inst.id), h("div", { class: "muted num" }, phoneOf(inst.jid) || "Not linked")),
      statusBadge(inst.status)),
    h("form", { class: "form side-body", onsubmit: save },
      h("label", { class: "field" }, "Webhook URL", url),
      h("label", { class: "field" }, "Webhook secret", secret),
      h("label", { class: "field" }, "Send rate (messages/min)", rate),
      h("div", {}, button("Save", { type: "submit", class: "primary" }))),
    h("div", { class: "bar side-foot" },
      button("Reconnect", { icon: "refresh", onclick: () => guard(async () => { await api("POST", base + "/connect"); toast("Reconnecting"); }) }),
      button("Delete account", { class: "danger", icon: "trash", onclick: () => confirmDialog("Delete account", "This cannot be undone.", "Delete",
        async () => { await api("DELETE", base); toast("Account deleted"); location.hash = "#/instances"; }) })));
  return { side, main };
}

// resizer is a draggable divider that sets target's width (dir 1: the
// target is left of it, -1: right of it). The width is remembered in this
// browser under key.
function resizer(target, key, dir, min, max) {
  let saved = 0;
  try { saved = Number(localStorage.getItem(key)) || 0; } catch { /* private mode */ }
  if (saved) target.style.width = Math.min(max, Math.max(min, saved)) + "px";
  const bar = h("div", { class: "resizer", role: "separator", "aria-orientation": "vertical" });
  bar.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    bar.setPointerCapture(e.pointerId);
    const start = e.clientX;
    const width = target.getBoundingClientRect().width;
    document.body.classList.add("resizing");
    const move = (ev) => { target.style.width = Math.min(max, Math.max(min, width + dir * (ev.clientX - start))) + "px"; };
    const up = () => {
      bar.removeEventListener("pointermove", move);
      document.body.classList.remove("resizing");
      try { localStorage.setItem(key, String(Math.round(target.getBoundingClientRect().width))); } catch { /* private mode */ }
    };
    bar.addEventListener("pointermove", move);
    bar.addEventListener("pointerup", up, { once: true });
  });
  return bar;
}

// --- router --------------------------------------------------------------------------

const routes = [
  [/^#\/tenants$/, () => tenantsPage(), "admin"],
  [/^#\/tenants\/([^/]+)$/, (m) => tenantPage(decodeURIComponent(m[1])), "admin"],
  [/^#\/instances$/, () => instancesPage(), "tenant"],
  [/^#\/instances\/([^/]+)$/, (m) => instancePage(decodeURIComponent(m[1])), "tenant"],
];


// render draws the current route. A quiet render (periodic refresh) keeps
// the scroll position and skips the error toast on failures.
async function render(quiet) {
  if (!quiet) {
    clearTimers();
    // Dialogs live outside #app: close them when the page changes.
    document.querySelectorAll(".overlay").forEach((o) => o.remove());
  }
  const app = document.getElementById("app");
  if (!session.key) {
    if (location.hash !== "#/login") location.hash = "#/login";
    app.replaceChildren(loginPage());
    return;
  }
  const hash = location.hash;
  const route = routes.find(([re, , role]) => re.test(hash) && role === session.role);
  if (!route) {
    location.hash = session.role === "admin" ? "#/tenants" : "#/instances";
    return;
  }
  if (!quiet) document.body.classList.add("loading");
  try {
    if (quiet) clearTimers();
    const scroll = window.scrollY;
    app.replaceChildren(await route[1](hash.match(route[0])));
    if (quiet) window.scrollTo(0, scroll);
  } catch (e) {
    if (!quiet) toast(e.message, true);
  } finally {
    document.body.classList.remove("loading");
  }
}

window.addEventListener("hashchange", () => render());
render();
