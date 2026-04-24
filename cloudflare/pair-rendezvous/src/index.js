// InfiniteStream pairing rendezvous.
//
// Lets a TV app (no camera, no QR scan) discover a server URL by showing a
// short pairing code. The user types the code into the dashboard on their
// phone/laptop; the dashboard publishes the dashboard's own URL to the
// rendezvous keyed by that code; the TV polls for the URL and connects.
//
// Endpoints:
//
//   POST /pair?code=ABC123        body: {"server_url": "http://..."}
//     Stores the URL keyed by code. TTL 10 minutes. CORS open.
//
//   GET  /pair?code=ABC123
//     Returns 200 with body = the stored server_url, or 204 if no entry.
//     Polled by the TV app every ~2s until non-empty.
//
//   DELETE /pair?code=ABC123
//     Clears the entry (TV calls this after consuming the URL).
//
//   POST /announce               body: {"server_id": "...", "url": "...", "label": "..."}
//     A server self-publishes that it exists at `url`. Stored keyed by
//     CF-Connecting-IP + server_id with a short TTL. Servers heartbeat
//     every ~30s; entries expire ~90s after the last heartbeat.
//
//   GET  /announce
//     Returns {servers: [{server_id, url, label, last_seen}]} for all
//     announces from the same public IP as the caller. Used by the
//     standalone pair page so the user just picks a server + types a
//     code instead of typing the URL by hand.
//
// TTL / heartbeat tuning: each announce expires after ANNOUNCE_TTL_SECONDS.
// Servers announce on boot, then every ~12h, plus on user demand from the
// dashboard's "Server Info" modal (covers a missed boot announce). At 2
// writes/day per server, hundreds of servers fit under Cloudflare KV's
// free-plan 1,000-writes/day account-wide budget.
//
// The KV namespace is bound as `PAIRING` in wrangler.toml.

const TTL_SECONDS = 600;        // 10 minutes for pair entries
const ANNOUNCE_TTL_SECONDS = 24 * 60 * 60; // server announces — heartbeat every ~12h, plus on boot and on demand
const CODE_PATTERN = /^[A-Z0-9]{4,12}$/;
const SERVER_ID_PATTERN = /^[A-Za-z0-9_-]{4,64}$/;
const ANNOUNCE_PREFIX = 'announce:';

// Standalone pair page served at the Worker's root. Lets the user pair from
// any browser on any network, including ones that can't reach the
// InfiniteStream dashboard.
const PAIR_PAGE_HTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>InfiniteStream — Pair TV</title>
<style>
  body { font-family: -apple-system, system-ui, sans-serif; max-width: 520px; margin: 40px auto; padding: 0 20px; color: #222; }
  h1 { font-size: 1.4em; margin-bottom: 4px; }
  h2 { font-size: 1.05em; margin-top: 32px; margin-bottom: 8px; color: #333; }
  p.lead { color: #666; margin-top: 0; margin-bottom: 24px; }
  label { display: block; margin-top: 14px; font-size: 0.9em; color: #444; }
  input { width: 100%; padding: 10px 12px; font-size: 1em; border: 1px solid #ccc; border-radius: 6px; box-sizing: border-box; -webkit-appearance: none; }
  input[type="text"].code { letter-spacing: 4px; font-family: ui-monospace, monospace; text-transform: uppercase; }
  button { padding: 10px 14px; font-size: 0.95em; background: #2563eb; color: #fff; border: 0; border-radius: 6px; cursor: pointer; }
  button:disabled { background: #94a3b8; cursor: not-allowed; }
  button.primary { width: 100%; padding: 12px; margin-top: 20px; font-size: 1em; }
  .servers { margin-top: 8px; }
  .server { border: 1px solid #e5e7eb; border-radius: 8px; padding: 12px 14px; margin-bottom: 10px; background: #fafafa; }
  .server .label { font-weight: 600; }
  .server .url { color: #555; font-family: ui-monospace, monospace; font-size: 0.85em; word-break: break-all; margin-top: 2px; }
  .server form { display: flex; gap: 8px; margin-top: 10px; align-items: center; }
  .server form input { flex: 1; }
  .empty { color: #888; font-style: italic; padding: 12px 0; }
  .msg { margin-top: 10px; padding: 8px 10px; border-radius: 6px; font-size: 0.85em; }
  .ok { background: #ecfdf5; color: #065f46; }
  .err { background: #fef2f2; color: #991b1b; }
  details { margin-top: 28px; }
  summary { cursor: pointer; color: #2563eb; font-size: 0.95em; }
  small { color: #888; display: block; margin-top: 24px; line-height: 1.5; }
</style>
</head>
<body>
<h1>Pair a TV</h1>
<p class="lead">Type the code shown on your TV. Pick the server it should connect to.</p>

<h2>Servers on your network</h2>
<div id="servers" class="servers"><div class="empty">Looking for servers…</div></div>

<details>
  <summary>Server URL not listed? Type it in.</summary>
  <form id="manual">
    <label for="m_code">Pairing code</label>
    <input id="m_code" class="code" type="text" autocomplete="off" autocapitalize="characters" required pattern="[A-Za-z0-9]{4,12}">
    <label for="m_url">Server URL</label>
    <input id="m_url" type="url" placeholder="http://hostname:30000" required>
    <button type="submit" class="primary">Pair</button>
    <div id="m_msg"></div>
  </form>
</details>

<small>Pair entries are stored briefly (10 minutes) keyed by your code; the TV picks them up. Server announces are kept for ~90 seconds and only shown to clients on the same public IP. By default the TV must come from the same public IP as you — see your worker's <code>RENDEZVOUS_ALLOW_CROSS_NETWORK</code> setting if you need to relax that.</small>

<script>
async function loadServers() {
  const wrap = document.getElementById('servers');
  try {
    const res = await fetch('/announce');
    const data = await res.json().catch(() => ({ servers: [] }));
    const servers = (data && data.servers) || [];
    if (!servers.length) {
      wrap.innerHTML = '<div class="empty">No servers detected on your network. Use the manual entry below.</div>';
      return;
    }
    wrap.innerHTML = '';
    servers.forEach(s => wrap.appendChild(renderServer(s)));
  } catch (err) {
    wrap.innerHTML = '<div class="empty">Could not load servers: ' + escapeHtml(err.message) + '</div>';
  }
}

function renderServer(s) {
  const div = document.createElement('div');
  div.className = 'server';
  const label = s.label || s.url;
  div.innerHTML =
    '<div class="label"></div>' +
    '<div class="url"></div>' +
    '<form>' +
    '  <input class="code" type="text" placeholder="CODE" autocomplete="off" autocapitalize="characters" required pattern="[A-Za-z0-9]{4,12}">' +
    '  <button type="submit">Pair</button>' +
    '</form>' +
    '<div class="msg-slot"></div>';
  div.querySelector('.label').textContent = label;
  div.querySelector('.url').textContent = s.url;
  div.querySelector('form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const codeInput = e.target.querySelector('input.code');
    const btn = e.target.querySelector('button');
    const code = codeInput.value.trim().toUpperCase();
    const msg = div.querySelector('.msg-slot');
    msg.className = 'msg-slot'; msg.textContent = '';
    btn.disabled = true; btn.textContent = 'Pairing…';
    try {
      const res = await fetch('/pair?code=' + encodeURIComponent(code), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_url: s.url }),
      });
      const body = await res.json().catch(() => ({}));
      if (res.ok) {
        msg.className = 'msg ok';
        msg.textContent = 'Paired! TV should pick this up within a couple of seconds.';
        codeInput.value = '';
      } else {
        msg.className = 'msg err';
        msg.textContent = body.error || ('HTTP ' + res.status);
      }
    } catch (err) {
      msg.className = 'msg err';
      msg.textContent = err.message;
    } finally {
      btn.disabled = false; btn.textContent = 'Pair';
    }
  });
  return div;
}

function escapeHtml(s) { return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }

document.getElementById('manual').addEventListener('submit', async (e) => {
  e.preventDefault();
  const code = document.getElementById('m_code').value.trim().toUpperCase();
  const url = document.getElementById('m_url').value.trim();
  const msg = document.getElementById('m_msg');
  const btn = e.target.querySelector('button');
  msg.className = ''; msg.textContent = '';
  btn.disabled = true; btn.textContent = 'Pairing…';
  try {
    const res = await fetch('/pair?code=' + encodeURIComponent(code), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ server_url: url }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.ok) {
      msg.className = 'msg ok';
      msg.textContent = 'Paired! The TV should pick this up within a couple of seconds.';
    } else {
      msg.className = 'msg err';
      msg.textContent = body.error || ('HTTP ' + res.status);
    }
  } catch (err) {
    msg.className = 'msg err';
    msg.textContent = err.message;
  } finally {
    btn.disabled = false; btn.textContent = 'Pair';
  }
});

loadServers();
setInterval(loadServers, 15000);
</script>
</body>
</html>`;

const corsHeaders = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET,POST,DELETE,OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type',
  'Access-Control-Max-Age': '86400',
};

function json(body, init = {}) {
  return new Response(JSON.stringify(body), {
    status: init.status || 200,
    headers: { 'Content-Type': 'application/json', ...corsHeaders, ...(init.headers || {}) },
  });
}

function text(body, init = {}) {
  return new Response(body, {
    status: init.status || 200,
    headers: { 'Content-Type': 'text/plain', ...corsHeaders, ...(init.headers || {}) },
  });
}

function validateCode(code) {
  if (!code) return 'code query parameter required';
  if (typeof code !== 'string') return 'code must be a string';
  const upper = code.toUpperCase();
  if (!CODE_PATTERN.test(upper)) return 'code must be 4–12 alphanumeric characters';
  return null;
}

function validateServerURL(url) {
  if (!url) return 'server_url required';
  if (typeof url !== 'string') return 'server_url must be a string';
  if (url.length > 1024) return 'server_url too long';
  try {
    const parsed = new URL(url);
    if (!['http:', 'https:'].includes(parsed.protocol)) return 'server_url must be http(s)';
  } catch (_e) {
    return 'server_url is not a valid URL';
  }
  return null;
}

function validateServerID(id) {
  if (!id) return 'server_id required';
  if (typeof id !== 'string') return 'server_id must be a string';
  if (!SERVER_ID_PATTERN.test(id)) return 'server_id must be 4-64 chars [A-Za-z0-9_-]';
  return null;
}

// Hash an IP into an opaque key fragment so the public IP doesn't appear in
// KV listings/logs. Hex-encoded SHA-256, truncated to 24 chars (96 bits) —
// plenty to avoid collisions across the announce keyspace.
async function ipHash(ip) {
  if (!ip) return 'unknown';
  const data = new TextEncoder().encode('infinitestream-announce:' + ip);
  const buf = await crypto.subtle.digest('SHA-256', data);
  const bytes = new Uint8Array(buf);
  let hex = '';
  for (let i = 0; i < 12; i++) hex += bytes[i].toString(16).padStart(2, '0');
  return hex;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    if (request.method === 'OPTIONS') {
      return new Response(null, { status: 204, headers: corsHeaders });
    }

    // Standalone pair page — usable from any browser even when the user
    // can't reach the InfiniteStream dashboard (e.g. phone on cellular).
    // Lets the user manually type both the pairing code AND the server URL.
    if (url.pathname === '/' || url.pathname === '/index.html') {
      return new Response(PAIR_PAGE_HTML, {
        status: 200,
        headers: { 'Content-Type': 'text/html; charset=utf-8', ...corsHeaders },
      });
    }

    const clientIP = request.headers.get('CF-Connecting-IP') || '';

    if (url.pathname === '/announce') {
      return handleAnnounce(request, env, clientIP);
    }

    if (url.pathname !== '/pair') {
      return text('Not found. Use /pair?code=... or /announce', { status: 404 });
    }

    const code = (url.searchParams.get('code') || '').toUpperCase();
    const codeErr = validateCode(code);
    if (codeErr) return json({ error: codeErr }, { status: 400 });

    const key = `pair:${code}`;

    if (request.method === 'GET') {
      const raw = await env.PAIRING.get(key);
      if (!raw) return new Response(null, { status: 204, headers: corsHeaders });
      let entry;
      try {
        entry = JSON.parse(raw);
      } catch (_e) {
        // Legacy plain-string entries (pre-IP-check). Just return them.
        return text(raw);
      }
      // Same-WAN check: the polling TV must come from the same public IP as
      // the dashboard that published the URL. Prevents cross-network pairing
      // attacks where a phone on a different network supplies a URL.
      // Allow opt-out via RENDEZVOUS_ALLOW_CROSS_NETWORK=1 for users whose
      // TV and phone egress different networks (rare).
      const allowCross = env.RENDEZVOUS_ALLOW_CROSS_NETWORK === '1';
      if (!allowCross && entry.ip && clientIP && entry.ip !== clientIP) {
        return json({
          error: 'cross-network pairing blocked',
          publisher_ip: entry.ip,
          your_ip: clientIP,
          hint: 'Publisher and TV must share the same public IP. Set RENDEZVOUS_ALLOW_CROSS_NETWORK=1 on the worker to disable this check.',
        }, { status: 403 });
      }
      return text(entry.server_url);
    }

    if (request.method === 'POST') {
      let body;
      try {
        body = await request.json();
      } catch (_e) {
        return json({ error: 'invalid JSON body' }, { status: 400 });
      }
      const serverURL = body && body.server_url;
      const urlErr = validateServerURL(serverURL);
      if (urlErr) return json({ error: urlErr }, { status: 400 });
      const entry = JSON.stringify({ server_url: serverURL, ip: clientIP });
      await env.PAIRING.put(key, entry, { expirationTtl: TTL_SECONDS });
      return json({ ok: true, code, expires_in: TTL_SECONDS, publisher_ip: clientIP });
    }

    if (request.method === 'DELETE') {
      await env.PAIRING.delete(key);
      return json({ ok: true });
    }

    return json({ error: 'method not allowed' }, { status: 405 });
  },
};

async function handleAnnounce(request, env, clientIP) {
  const ipKey = await ipHash(clientIP);
  const prefix = `${ANNOUNCE_PREFIX}${ipKey}:`;

  if (request.method === 'GET') {
    // KV list is eventually consistent but fine for ~30s heartbeat windows.
    // Cap results so a noisy IP can't blow up the response.
    const list = await env.PAIRING.list({ prefix, limit: 50 });
    const servers = [];
    for (const k of list.keys) {
      const raw = await env.PAIRING.get(k.name);
      if (!raw) continue;
      try {
        const entry = JSON.parse(raw);
        servers.push({
          server_id: entry.server_id,
          url: entry.server_url,
          label: entry.label || '',
          last_seen: entry.last_seen || 0,
        });
      } catch (_e) {
        // skip
      }
    }
    // Newest first.
    servers.sort((a, b) => (b.last_seen || 0) - (a.last_seen || 0));
    return json({ servers });
  }

  if (request.method === 'POST') {
    let body;
    try {
      body = await request.json();
    } catch (_e) {
      return json({ error: 'invalid JSON body' }, { status: 400 });
    }
    const serverID = body && body.server_id;
    const idErr = validateServerID(serverID);
    if (idErr) return json({ error: idErr }, { status: 400 });
    const serverURL = body && body.url;
    const urlErr = validateServerURL(serverURL);
    if (urlErr) return json({ error: urlErr }, { status: 400 });
    let label = (body && body.label) || '';
    if (typeof label !== 'string') label = '';
    if (label.length > 128) label = label.slice(0, 128);

    const key = `${prefix}${serverID}`;
    const entry = JSON.stringify({
      server_id: serverID,
      server_url: serverURL,
      label,
      last_seen: Date.now(),
    });
    await env.PAIRING.put(key, entry, { expirationTtl: ANNOUNCE_TTL_SECONDS });
    return json({ ok: true, expires_in: ANNOUNCE_TTL_SECONDS });
  }

  if (request.method === 'DELETE') {
    const serverID = (new URL(request.url)).searchParams.get('server_id') || '';
    const idErr = validateServerID(serverID);
    if (idErr) return json({ error: idErr }, { status: 400 });
    await env.PAIRING.delete(`${prefix}${serverID}`);
    return json({ ok: true });
  }

  return json({ error: 'method not allowed' }, { status: 405 });
}
