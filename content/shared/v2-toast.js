/**
 * v2-toast.js — toast notifications for v2 model error events.
 *
 * Subscribe a Toast container to any model's `error` event:
 *
 *   const toasts = new ToastHost();
 *   toasts.attach(document.body);
 *   playersStore.on('error', (e) => toasts.showError(e));
 *
 * RFC 7807 problem documents are surfaced as `<title>: <detail>`.
 *
 * No external deps. Globals: `window.V2Toast` (the constructor).
 */
(function (global) {
  "use strict";

  function ToastHost() {
    this._root = null;
    this._timers = new WeakMap();
  }

  ToastHost.prototype.attach = function (parent) {
    if (this._root) return;
    const root = document.createElement('div');
    root.className = 'v2-toast-host';
    root.style.cssText =
      'position:fixed;top:12px;right:12px;z-index:10000;display:flex;' +
      'flex-direction:column;gap:8px;max-width:420px;font-family:' +
      '-apple-system,BlinkMacSystemFont,sans-serif;font-size:13px;';
    (parent || document.body).appendChild(root);
    this._root = root;
  };

  ToastHost.prototype.show = function (kind, title, detail, opts) {
    if (!this._root) this.attach();
    opts = opts || {};
    const ttl = opts.ttl || 8000;
    const node = document.createElement('div');
    const palette = kind === 'error'
      ? { bg: '#fee2e2', border: '#ef4444', fg: '#7f1d1d' }
      : kind === 'warn'
        ? { bg: '#fef3c7', border: '#f59e0b', fg: '#78350f' }
        : { bg: '#d1fae5', border: '#10b981', fg: '#064e3b' };
    node.style.cssText =
      'background:' + palette.bg + ';color:' + palette.fg +
      ';border-left:4px solid ' + palette.border +
      ';padding:10px 14px;border-radius:6px;box-shadow:0 4px 12px rgba(0,0,0,0.12);' +
      'cursor:pointer;line-height:1.35;';
    const titleEl = document.createElement('div');
    titleEl.style.cssText = 'font-weight:600;margin-bottom:2px;';
    titleEl.textContent = String(title || '');
    node.appendChild(titleEl);
    if (detail) {
      const detailEl = document.createElement('div');
      detailEl.style.cssText = 'font-size:12px;opacity:0.85;';
      detailEl.textContent = String(detail);
      node.appendChild(detailEl);
    }
    node.addEventListener('click', () => this._dismiss(node));
    this._root.appendChild(node);
    if (ttl > 0) {
      this._timers.set(node, setTimeout(() => this._dismiss(node), ttl));
    }
    return node;
  };

  // Format a model `error` event ({ operation, response, willRetry }).
  ToastHost.prototype.showError = function (evt) {
    const op = evt && evt.operation || 'request';
    const resp = evt && evt.response;
    const body = resp && resp.body;
    let title = op + ' failed';
    let detail = '';
    if (body && typeof body === 'object') {
      // RFC 7807 problem document
      if (body.title) title = body.title;
      if (body.detail) detail = body.detail;
    } else if (resp && resp.status) {
      detail = 'HTTP ' + resp.status;
    } else if (evt && evt.message) {
      detail = evt.message;
    }
    if (evt && evt.willRetry) detail += ' (retrying…)';
    return this.show('error', title, detail);
  };

  ToastHost.prototype._dismiss = function (node) {
    const timer = this._timers.get(node);
    if (timer) clearTimeout(timer);
    this._timers.delete(node);
    if (node.parentNode) node.parentNode.removeChild(node);
  };

  global.V2Toast = ToastHost;
})(window);
