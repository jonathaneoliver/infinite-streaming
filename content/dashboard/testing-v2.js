/**
 * testing-v2.js — multi-player monitor bootstrap for the v2-native
 * Testing Monitor page (/dashboard/testing.html).
 *
 * Mirrors the v1 page: a session-tab strip on top, group controls
 * directly below, then one full session card per player. Each card
 * mounts the same TestingSessionSections panels used by
 * testing-session.html (Q5c–Q5j) so the two pages share rendering.
 *
 * Driven entirely by V2Models PlayersStore + GroupsStore. SSE
 * connection state surfaces in the panel header (#sseMissedBadge slot).
 *
 * Hard rule, same as Q5: no innerHTML for any string sourced from a
 * model. Use t() helper.
 */
(() => {
  "use strict";
  const M = window.V2Models;

  const client = new window.HarnessV2();
  const repo = new window.V2Repo(client);
  const players = new M.PlayersStore(repo);
  const groups = new M.GroupsStore(repo);
  const toasts = new window.V2Toast();
  toasts.attach(document.body);

  // Element handles
  const els = {
    container: document.getElementById('sessionsContainer'),
    sseBadge: document.getElementById('sseMissedBadge'),
    refreshBtn: document.getElementById('refreshSessions'),
    clearBtn: document.getElementById('clearSessions'),
    subtitle: document.getElementById('testingSubtitle'),
  };
  function t(s) { return document.createTextNode(String(s == null ? '' : s)); }

  // Selected players for "Link as Group". Tracked here so it survives
  // re-renders driven by SSE updates.
  const selected = new Set();
  // Track the set of mounts so SSE updates only re-mount what's new.
  const mounts = new Map(); // playerId → { card, teardown }

  // ---- Connection pill in the SSE Missed slot ----------------------
  let sseMissed = 0;
  players.on('connection', (e) => {
    if (!els.sseBadge) return;
    const palette = {
      connected:    { bg: '#d1fae5', fg: '#064e3b', text: 'live' },
      connecting:   { bg: '#fef3c7', fg: '#78350f', text: 'connecting…' },
      reconnecting: { bg: '#fef3c7', fg: '#78350f', text: 'reconnecting…' },
      disconnected: { bg: '#fee2e2', fg: '#7f1d1d', text: 'disconnected' },
    }[e.state] || { bg: '#fee2e2', fg: '#7f1d1d', text: e.state };
    els.sseBadge.style.cssText = 'background:' + palette.bg + ';color:' + palette.fg +
      ';padding:2px 8px;border-radius:999px;font-size:11px;font-weight:600;margin-left:6px;';
    els.sseBadge.textContent = palette.text + (sseMissed > 0 ? ' · missed ' + sseMissed : '');
  });
  players.on('error', (e) => {
    if (e.operation === 'sse') sseMissed++;
    toasts.showError(e);
  });
  groups.on('error', (e) => toasts.showError(e));

  // ---- Refresh / Release All ---------------------------------------
  els.refreshBtn && els.refreshBtn.addEventListener('click', () => {
    players.hydrate();
    groups.hydrate();
  });
  els.clearBtn && els.clearBtn.addEventListener('click', () => {
    if (!confirm('Release ALL active players?')) return;
    repo.deleteAllPlayers().then(() => players.hydrate());
  });

  // ---- Session-tabs strip ------------------------------------------
  // One pill per player. Click → scrolls the matching card into view.
  // Group affiliation paints a left bar; the badge shows the group id.
  function renderTabs() {
    let tabs = document.getElementById('sessionTabsStrip');
    if (!tabs) {
      tabs = document.createElement('div');
      tabs.id = 'sessionTabsStrip';
      tabs.className = 'session-tabs';
      els.container.parentNode.insertBefore(tabs, els.container);
    }
    tabs.innerHTML = '';
    const list = players.list();
    for (const p of list) {
      const tab = document.createElement('button');
      tab.className = 'session-tab';
      const myGroup = groups.list().find(g => g.memberPlayerIds.includes(p.id));
      if (myGroup) tab.classList.add('grouped');
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.className = 'session-checkbox';
      checkbox.checked = selected.has(p.id);
      checkbox.addEventListener('click', (e) => {
        e.stopPropagation();
        if (checkbox.checked) selected.add(p.id); else selected.delete(p.id);
        updateGroupControls();
      });
      tab.appendChild(checkbox);
      tab.appendChild(t('#' + p.displayId + ' · ' + p.id.slice(0, 8)));
      if (myGroup) {
        const badge = document.createElement('span');
        badge.className = 'group-badge';
        badge.appendChild(t(myGroup.label || myGroup.id.slice(0, 6)));
        tab.appendChild(badge);
      }
      tab.addEventListener('click', () => {
        const card = document.getElementById('player-card-' + p.id);
        if (card) card.scrollIntoView({ behavior: 'smooth', block: 'start' });
      });
      tabs.appendChild(tab);
    }
  }

  // ---- Group controls ----------------------------------------------
  // "Link selected as group" + per-row disband. Lives in a strip just
  // under the session-tabs.
  function renderGroupControls() {
    let host = document.getElementById('groupControlsStrip');
    if (!host) {
      host = document.createElement('div');
      host.id = 'groupControlsStrip';
      host.className = 'group-controls';
      els.container.parentNode.insertBefore(host, els.container);
    }
    host.innerHTML = '';
    const linkBtn = document.createElement('button');
    linkBtn.id = 'linkSelectedSessions';
    linkBtn.className = 'btn btn-primary';
    linkBtn.style.padding = '4px 12px';
    linkBtn.textContent = 'Link selected as group (' + selected.size + ')';
    linkBtn.disabled = selected.size < 2;
    linkBtn.addEventListener('click', () => {
      if (selected.size < 2) return;
      groups.create({
        member_player_ids: Array.from(selected),
        label: 'group-' + Date.now().toString(36),
      }).then((r) => {
        if (r.ok) {
          selected.clear();
          rerenderAll();
        } else {
          toasts.showError({ operation: 'createGroup', response: r });
        }
      });
    });
    host.appendChild(linkBtn);

    // Compact group list with disband actions
    const list = groups.list();
    if (list.length > 0) {
      const sep = document.createElement('span');
      sep.className = 'group-hint';
      sep.style.cssText = 'margin-left:8px;font-size:11px;';
      sep.appendChild(t(list.length + ' active group' + (list.length === 1 ? '' : 's') + ':'));
      host.appendChild(sep);
      for (const g of list) {
        const chip = document.createElement('span');
        chip.style.cssText = 'background:#10b981;color:#fff;padding:2px 8px;border-radius:999px;font-size:11px;font-weight:600;display:inline-flex;align-items:center;gap:4px;margin-left:6px;';
        chip.appendChild(t((g.label || g.id.slice(0, 6)) + ' (' + g.memberPlayerIds.length + ')'));
        const x = document.createElement('button');
        x.className = 'unlink-btn';
        x.textContent = 'Disband';
        x.addEventListener('click', () => {
          if (confirm('Disband ' + (g.label || g.id) + '?')) g.disband().then(() => groups.hydrate());
        });
        chip.appendChild(x);
        host.appendChild(chip);
      }
    }
  }
  function updateGroupControls() { renderGroupControls(); renderTabs(); }

  // ---- One session card per player ---------------------------------
  // Re-renders the card list when PlayersStore mutates. A card is a
  // panel with a header (player id, display, IP, group chip) followed
  // by all the TestingSessionSections.mountAll panels.
  function renderCards() {
    const list = players.list();
    const seen = new Set();

    for (const p of list) {
      seen.add(p.id);
      let card = document.getElementById('player-card-' + p.id);
      if (!card) {
        card = document.createElement('div');
        card.id = 'player-card-' + p.id;
        card.className = 'session-card session-panel';
        card.style.cssText = 'margin-top:14px;';
        els.container.appendChild(card);

        // Card header
        const header = document.createElement('div');
        header.className = 'session-header';
        const title = document.createElement('span');
        title.className = 'session-title';
        title.appendChild(t('Player #' + p.displayId));
        header.appendChild(title);
        const meta = document.createElement('span');
        meta.className = 'session-meta';
        meta.appendChild(t(p.id));
        header.appendChild(meta);
        const actions = document.createElement('div');
        actions.style.cssText = 'display:flex;gap:6px;margin-left:auto;';
        const openBtn = document.createElement('a');
        openBtn.className = 'btn btn-secondary btn-mini';
        openBtn.href = '/dashboard/testing-session.html?player_id=' + encodeURIComponent(p.id);
        openBtn.appendChild(t('Open'));
        actions.appendChild(openBtn);
        const releaseBtn = document.createElement('button');
        releaseBtn.className = 'btn btn-danger btn-mini';
        releaseBtn.appendChild(t('Release'));
        releaseBtn.addEventListener('click', () => {
          if (!confirm('Release player #' + p.displayId + '?')) return;
          p.delete();
        });
        actions.appendChild(releaseBtn);
        header.appendChild(actions);
        card.appendChild(header);

        // Sections host
        const sectionsHost = document.createElement('div');
        sectionsHost.className = 'sections-host';
        card.appendChild(sectionsHost);

        const teardown = window.TestingSessionSections.mountAll(sectionsHost, p, { groups });
        mounts.set(p.id, { card, teardown });
      }
    }

    // Drop cards for players that no longer exist.
    for (const [pid, mount] of mounts) {
      if (!seen.has(pid)) {
        try { mount.teardown && mount.teardown(); } catch (_) {}
        if (mount.card.parentNode) mount.card.parentNode.removeChild(mount.card);
        mounts.delete(pid);
        selected.delete(pid);
      }
    }
  }

  function rerenderAll() {
    renderTabs();
    renderGroupControls();
    renderCards();
  }

  // ---- PlayersStore + GroupsStore wiring ---------------------------
  players.on('change', rerenderAll);
  players.on('player.added', rerenderAll);
  players.on('player.removed', rerenderAll);
  groups.on('change', rerenderAll);

  // ---- Bootstrap ----------------------------------------------------
  players.connect();
  Promise.all([
    players.hydrate(),
    groups.hydrate(),
  ]).then(() => {
    rerenderAll();
    if (players.list().length === 0) {
      // Surface an inviting empty state instead of a blank panel.
      const empty = document.createElement('div');
      empty.style.cssText = 'padding:30px;text-align:center;color:#6b7280;font-size:13px;';
      const lead = document.createElement('div');
      lead.style.cssText = 'font-weight:600;margin-bottom:6px;';
      lead.appendChild(t('No active players.'));
      empty.appendChild(lead);
      empty.appendChild(t('Launch a player app (Apple TV / iOS / Android TV / Roku) or open '));
      const link = document.createElement('a');
      link.href = '/dashboard/testing-session.html';
      link.appendChild(t('Testing Playback'));
      empty.appendChild(link);
      empty.appendChild(t(' to mint a synthetic.'));
      els.container.appendChild(empty);
    }
  }).catch(err => {
    toasts.show('error', 'Bootstrap failed', err && err.message);
  });

  // Expose for debugging.
  window.TestingV2 = { repo, players, groups, toasts, mounts };
})();
