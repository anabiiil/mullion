/* Mullion's built-in terminal: tabs of real shells (PTYs served by the
 * panel at /api/term/*) rendered with the vendored xterm.js, themed to
 * the panel, with path/command suggestions as you type.
 *
 * Only this script needs including — it loads xterm.js, the fit addon
 * and both stylesheets itself:
 *
 *   <script src="/terminal.js"></script>
 *
 * MullionTerminal (global):
 *   mount(el, {site?, cwd?, title?, embedded?, standalone?}) → instance
 *       Several instances can be mounted at once; all share one output
 *       stream. The Terminal page (neither flag) shows every session;
 *       embedded + site shows only that site's sessions (compact); a
 *       standalone window shows the site's sessions, or all without one.
 *       Embedded/standalone-with-site open a session if none exist.
 *   instance: open(opts) · focus() · fit() · list() · destroy() · ready
 *       destroy() only detaches the UI; shells keep running until their
 *       tab's × is clicked.
 *   openInProject(site, {newTab?, mode?})  mode: 'page' | 'window' | 'project'
 *       (default: MullionPrefs 'terminal.openIn', else 'page')
 *   openWindow(site?, {cwd?}) · windowURL(site?, {cwd?}) · parseWindowHash(hash?)
 *   open(opts) · list() · focus() · fit() · instances()
 *   hooks: onRequestShow(site) — show the Terminal page;
 *          onRequestProject(site) — show that project's Terminal tab.
 */
(function () {
  'use strict';
  if (window.MullionTerminal) return;

  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  let token = new URLSearchParams(location.search).get('t') || '';

  /* ── tiny helpers ─────────────────────────────────────────── */
  const h = (tag, cls, html) => {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (html != null) el.innerHTML = html;
    return el;
  };
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);
  async function api(path, body) {
    const res = await fetch(path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: { 'X-Mullion-Token': token, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let out;
    try { out = await res.json(); } catch { throw new Error('request failed (' + res.status + ')'); }
    if (!out.ok) throw new Error(out.error || 'request failed');
    return out.data;
  }
  function b64bytes(s) {
    const bin = atob(s);
    const u = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
    return u;
  }
  const ICON = {
    x: '<svg viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2 2l6 6M8 2L2 8"/></svg>',
    plus: '<svg viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M7 2v10M2 7h10"/></svg>',
    dir: '<svg viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 4A1.5 1.5 0 0 1 3 2.5h3.1l1.6 1.6H13A1.5 1.5 0 0 1 14.5 5.6v6.9A1.5 1.5 0 0 1 13 14H3a1.5 1.5 0 0 1-1.5-1.5z" opacity=".85"/></svg>',
    file: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.3"><path d="M4 1.8h5l3.2 3.2v9.2H4z"/><path d="M9 1.8V5h3.2"/></svg>',
    cmd: '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M3 4.5l3.5 3.5L3 11.5M8.5 12h4.5"/></svg>',
    script: '<svg viewBox="0 0 16 16" fill="currentColor"><path d="M5 3.2v9.6L12.6 8z"/></svg>',
  };

  /* ── assets ───────────────────────────────────────────────── */
  let assetsReady = null;
  function loadAssets() {
    if (assetsReady) return assetsReady;
    const css = href => {
      if (document.querySelector('link[href="' + href + '"]')) return;
      const l = document.createElement('link');
      l.rel = 'stylesheet'; l.href = href;
      document.head.appendChild(l);
    };
    const js = src => new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = src; s.onload = resolve; s.onerror = () => reject(new Error('failed to load ' + src));
      document.head.appendChild(s);
    });
    css('/vendor/xterm.css');
    css('/terminal.css');
    assetsReady = (window.Terminal ? Promise.resolve() : js('/vendor/xterm.js'))
      .then(() => (window.FitAddon ? null : js('/vendor/addon-fit.js')));
    return assetsReady;
  }

  /* ── theme ────────────────────────────────────────────────── */
  const MONO = '"SF Mono", "Cascadia Code", "Cascadia Mono", Menlo, Consolas, monospace';
  const THEMES = {
    light: {
      background: '#ffffff', foreground: '#1a1f2b',
      cursor: '#ff6b4a', cursorAccent: '#ffffff',
      selectionBackground: 'rgba(255, 107, 74, .22)', selectionInactiveBackground: 'rgba(255, 107, 74, .12)',
      scrollbarSliderBackground: 'rgba(26, 31, 43, .12)', scrollbarSliderHoverBackground: 'rgba(26, 31, 43, .22)',
      scrollbarSliderActiveBackground: 'rgba(255, 107, 74, .45)',
      black: '#1a1f2b', red: '#d93a3a', green: '#1a9553', yellow: '#a8680f',
      blue: '#3563d6', magenta: '#a347bf', cyan: '#12808f', white: '#8c94a6',
      brightBlack: '#6b7385', brightRed: '#f0552f', brightGreen: '#21a862', brightYellow: '#c98a12',
      brightBlue: '#4f7ff0', brightMagenta: '#bb5cd6', brightCyan: '#1597a8', brightWhite: '#a3a9b7',
    },
    dark: {
      background: '#161c28', foreground: '#e8ebf2',
      cursor: '#ff7a5c', cursorAccent: '#161c28',
      selectionBackground: 'rgba(255, 122, 92, .28)', selectionInactiveBackground: 'rgba(255, 122, 92, .14)',
      scrollbarSliderBackground: 'rgba(232, 235, 242, .10)', scrollbarSliderHoverBackground: 'rgba(232, 235, 242, .18)',
      scrollbarSliderActiveBackground: 'rgba(255, 122, 92, .45)',
      black: '#232a38', red: '#f26d6d', green: '#3ecf7a', yellow: '#e5b85c',
      blue: '#6ea8fe', magenta: '#d68cf0', cyan: '#4fd1c5', white: '#c9cfdb',
      brightBlack: '#5e6678', brightRed: '#ff8e74', brightGreen: '#6be39b', brightYellow: '#f2cf85',
      brightBlue: '#93bfff', brightMagenta: '#e5adf7', brightCyan: '#7ee3da', brightWhite: '#ffffff',
    },
  };
  function themeName() {
    const t = document.documentElement.dataset.theme;
    if (t === 'dark' || t === 'light') return t;
    return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  function themeFor(name) {
    // Follow the panel's live card/text colours when it defines them.
    const t = Object.assign({}, THEMES[name]);
    const cs = getComputedStyle(document.documentElement);
    const card = cs.getPropertyValue('--card').trim();
    const text = cs.getPropertyValue('--text').trim();
    if (card) { t.background = card; t.cursorAccent = card; }
    if (text) t.foreground = text;
    return t;
  }
  function applyTheme() {
    const name = themeName();
    for (const s of allViews()) {
      if (!s.term) continue;
      s.term.options.theme = themeFor(name);
      // Light backgrounds: let xterm nudge low-contrast ANSI colours
      // (yellow, white) until they're readable.
      s.term.options.minimumContrastRatio = name === 'light' ? 4.5 : 1;
    }
  }
  new MutationObserver(applyTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  matchMedia('(prefers-color-scheme: dark)').addEventListener?.('change', applyTheme);

  /* ── shared state ─────────────────────────────────────────── */
  // Sessions are global (one per shell on the panel); every mounted
  // instance renders its own xterm "view" of the sessions it shows, all
  // fed from ONE EventSource. A per-session history buffer lets a view
  // created later (another instance, a reopened page) replay the output.
  const HIST_MAX = 256 << 10;
  const sessions = new Map(); // id → {info, alive, exitCode, exited, chunks, size, inQ, inFlight}
  const instances = new Set();
  const pending = []; // [{match(inst), opts}] opens waiting for a matching instance to mount
  let es = null, lastEventId = '', syncing = null, syncAgain = false;

  function* allViews() {
    for (const inst of instances) yield* inst.views.values();
  }
  function viewsOf(id) {
    const out = [];
    for (const inst of instances) { const v = inst.views.get(id); if (v) out.push(v); }
    return out;
  }

  function ensureSession(info) {
    let g = sessions.get(info.id);
    if (!g) {
      g = { info, alive: info.alive, exitCode: info.exitCode || 0, exited: false, chunks: [], size: 0,
            inQ: '', inFlight: false };
      sessions.set(info.id, g);
    } else {
      g.info = info;
    }
    return g;
  }

  /* ── stream (one EventSource for everything) ──────────────── */
  function connect() {
    if (es) return;
    const url = '/api/term/stream?t=' + encodeURIComponent(token) + (lastEventId ? '&last=' + encodeURIComponent(lastEventId) : '');
    es = new EventSource(url);
    es.addEventListener('out', ev => {
      if (ev.lastEventId) lastEventId = ev.lastEventId;
      const m = JSON.parse(ev.data);
      const bytes = b64bytes(m.d);
      let g = sessions.get(m.s);
      if (!g) {
        // Opened elsewhere (another instance/window): keep its output and
        // learn about it from the list.
        g = ensureSession({ id: m.s, alive: true, title: '', cwd: '' });
        g.unknown = true;
        resync();
      }
      g.chunks.push(bytes);
      g.size += bytes.length;
      while (g.size > HIST_MAX && g.chunks.length > 1) g.size -= g.chunks.shift().length;
      for (const v of viewsOf(m.s)) v.term.write(bytes);
    });
    es.addEventListener('exit', ev => {
      const m = JSON.parse(ev.data);
      const g = sessions.get(m.s) || ensureSession({ id: m.s, alive: false, title: '', cwd: '' });
      g.alive = false; g.exited = true; g.exitCode = m.code;
      for (const v of viewsOf(m.s)) markDead(v);
    });
    es.addEventListener('gone', ev => dropSession(JSON.parse(ev.data).s));
    es.onerror = () => {
      // EventSource reconnects by itself (resuming via Last-Event-ID);
      // if the panel refused us for good, resync after a pause.
      if (es && es.readyState === EventSource.CLOSED) { es = null; setTimeout(resync, 1000); }
    };
  }

  // resync reconciles the known sessions with the panel's list and lets
  // every instance add/remove views. Concurrent calls share one request.
  function resync() {
    if (syncing) { syncAgain = true; return syncing; }
    syncing = (async () => {
      try {
        let list;
        try { list = await api('/api/term/list'); } catch { return; }
        const ids = new Set(list.map(x => x.id));
        for (const id of [...sessions.keys()]) if (!ids.has(id)) dropSession(id);
        for (const info of list) {
          const g = ensureSession(info);
          g.unknown = false;
          if (!info.alive && !g.exited) g.alive = false;
        }
        for (const inst of instances) inst.sync();
        connect();
      } finally {
        syncing = null;
        // A request made while this one was in flight may need a newer list.
        if (syncAgain) { syncAgain = false; resync(); }
      }
    })();
    return syncing;
  }

  function dropSession(id) {
    sessions.delete(id);
    for (const inst of instances) inst.removeView(id);
  }

  function addSession(info) {
    ensureSession(info);
    for (const inst of instances) inst.sync();
    connect();
  }

  /* ── input (per session, ordered) ─────────────────────────── */
  // Keystrokes are posted in order; while one request is in flight the
  // next ones queue up and go together, so fast typing or a paste never
  // reorders or floods the panel with requests.
  function send(id, data) {
    const g = sessions.get(id);
    if (!g || !g.alive) return;
    g.inQ += data;
    if (!g.inFlight) flush(g);
  }
  function flush(g) {
    if (!g.inQ) { g.inFlight = false; return; }
    const data = g.inQ;
    g.inQ = '';
    g.inFlight = true;
    fetch('/api/term/input', {
      method: 'POST',
      headers: { 'X-Mullion-Token': token, 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: g.info.id, data }),
    }).catch(() => {}).finally(() => flush(g));
  }

  const opening = new Set(); // in-flight opens, so a mounting instance can wait for them
  async function openSession(opts, size) {
    const info = await api('/api/term/open', {
      cwd: opts.cwd || '', site: opts.site || '', title: opts.title || '',
      cols: size ? size.cols : 0, rows: size ? size.rows : 0,
    });
    addSession(info);
    return info;
  }

  function closeSession(id) {
    api('/api/term/close', { id }).catch(() => {});
    dropSession(id);
  }

  /* ── an instance: tab strip + header + views ──────────────── */
  function Instance(el, opts) {
    opts = opts || {};
    const inst = this;
    this.el = el;
    this.embedded = !!opts.embedded;
    this.standalone = !!opts.standalone;
    this.isPage = !this.embedded && !this.standalone;
    // site filters what an embedded/standalone instance shows; for the
    // Terminal page it's only the default for new tabs (it shows all).
    this.site = this.isPage ? '' : (opts.site || '');
    this.defaults = { cwd: opts.cwd || '', site: opts.site || '', title: opts.title || '' };
    this.views = new Map();
    this.active = null;
    this.destroyed = false;

    el.classList.add('mt-root');
    if (this.embedded) el.classList.add('mt-embedded');
    if (this.standalone) el.classList.add('mt-standalone');
    el.innerHTML = '';
    const ui = this.ui = {
      bar: h('div', 'mt-bar'), tabs: h('div', 'mt-tabs'),
      plus: h('button', 'mt-new', ICON.plus), head: h('div', 'mt-head'),
      body: h('div', 'mt-body'), empty: h('div', 'mt-empty'), sug: h('div', 'mt-suggest'),
    };
    ui.plus.type = 'button';
    ui.plus.title = 'New terminal';
    ui.plus.onclick = () => {
      const i = inst.site ? inst.defaults : (inst.active ? inst.active.info : inst.defaults);
      inst.open({ cwd: i.cwd, site: i.site }).catch(() => {});
    };
    ui.bar.append(ui.tabs, ui.plus);
    ui.body.append(ui.empty, ui.sug);
    el.append(ui.bar, ui.head, ui.body);
    this.renderHead();

    let raf = 0;
    this.ro = new ResizeObserver(() => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        inst.fit();
        if (inst.active && inst.active.sug) placeSug(inst.active);
      });
    });
    this.ro.observe(ui.body);
  }

  Instance.prototype.shows = function (info) {
    return !this.site || info.site === this.site;
  };

  // sync adds a view for every shown session that lacks one and drops
  // views of sessions no longer known.
  Instance.prototype.sync = function () {
    if (this.destroyed || !window.Terminal || !window.FitAddon) return;
    for (const [id, g] of sessions) {
      if (!g.unknown && !this.views.has(id) && this.shows(g.info)) this.addView(g);
    }
    for (const id of [...this.views.keys()]) if (!sessions.has(id)) this.removeView(id);
    if (!this.active && this.views.size) this.activate(this.views.values().next().value);
    this.renderEmpty();
  };

  Instance.prototype.addView = function (g) {
    const inst = this, info = g.info, ui = this.ui;
    const v = {
      id: info.id, info, inst, term: null, fit: null, el: null, tab: null,
      anchor: null, pre: '', preOk: true, suppress: false, resizeT: 0, sugT: 0, sugSeq: 0, sug: null, deadShown: false,
    };
    this.views.set(v.id, v);

    v.tab = h('div', 'mt-tab' + (g.alive ? '' : ' dead'));
    v.tab.title = info.cwd || '';
    v.tab.innerHTML = '<i class="mt-dot"></i><span class="mt-name">' + esc(info.title || 'Terminal') +
      '</span><button class="mt-x" title="Close terminal" aria-label="Close terminal">' + ICON.x + '</button>';
    v.tab.addEventListener('mousedown', e => { if (e.button === 1) { e.preventDefault(); closeSession(v.id); } });
    v.tab.onclick = e => {
      if (e.target.closest('.mt-x')) { closeSession(v.id); return; }
      inst.activate(v);
    };
    ui.tabs.appendChild(v.tab);

    v.el = h('div', 'mt-term');
    ui.body.appendChild(v.el);
    createTerm(v);
    for (const b of g.chunks) v.term.write(b);
    if (g.exited) markDead(v);
    return v;
  };

  Instance.prototype.removeView = function (id) {
    const v = this.views.get(id);
    if (!v) return;
    const order = [...this.views.values()];
    const idx = order.indexOf(v);
    this.views.delete(id);
    hideSug(v);
    v.term.dispose();
    v.el.remove();
    v.tab.remove();
    if (this.active === v) {
      this.active = null;
      const rest = [...this.views.values()];
      if (rest.length) this.activate(rest[Math.min(idx, rest.length - 1)]);
      else this.renderHead();
    }
    this.renderEmpty();
  };

  Instance.prototype.activate = function (v) {
    if (!v || this.destroyed) return;
    const a = this.active;
    if (a && a !== v) { hideSug(a); a.el.classList.remove('active'); a.tab.classList.remove('active'); }
    this.active = v;
    v.el.classList.add('active');
    v.tab.classList.add('active');
    v.tab.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    this.renderHead();
    requestAnimationFrame(() => { this.fit(); v.term.focus(); });
  };

  Instance.prototype.fit = function () {
    const v = this.active;
    if (!v || !v.fit || !v.el.offsetWidth || !v.el.offsetHeight) return;
    try { v.fit.fit(); } catch {}
  };

  Instance.prototype.focus = function () {
    if (this.active) this.active.term.focus();
  };

  // The size a new tab's terminal will have, so the shell starts at its
  // real size (a mismatch makes zsh's first prompt wrap oddly).
  Instance.prototype.measure = function () {
    const a = this.active;
    if (a && a.term && a.el.offsetWidth) return { cols: a.term.cols, rows: a.term.rows };
    if (!this.ui.body.offsetWidth) return { cols: 0, rows: 0 };
    const el = h('div', 'mt-term active');
    el.style.visibility = 'hidden';
    this.ui.body.appendChild(el);
    const term = new window.Terminal({ fontFamily: MONO, fontSize: 13, lineHeight: 1.22 });
    const fit = new window.FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(el);
    let d = null;
    try { d = fit.proposeDimensions(); } catch {}
    term.dispose();
    el.remove();
    return d && d.cols ? { cols: d.cols, rows: d.rows } : { cols: 0, rows: 0 };
  };

  // open starts a new session (in this instance's site, if it has one)
  // and shows it here; every other instance that shows it gets a view.
  Instance.prototype.open = function (o) {
    o = Object.assign({}, this.defaults, o || {});
    if (this.site) o.site = this.site;
    const p = (async () => {
      await loadAssets();
      let info;
      try {
        info = await openSession(o, this.measure());
      } catch (e) {
        this.renderEmpty(e.message);
        throw e;
      }
      const v = this.views.get(info.id);
      if (v) this.activate(v);
      return info;
    })();
    opening.add(p);
    const done = () => opening.delete(p);
    p.then(done, done);
    return p;
  };

  // openOrFocus shows the site's live session if this instance has one,
  // else opens one.
  Instance.prototype.openOrFocus = function (o) {
    o = o || {};
    if (o.site && !o.newTab) {
      for (const v of this.views.values()) {
        const g = sessions.get(v.id);
        if (g && g.alive && v.info.site === o.site) { this.activate(v); return Promise.resolve(v.info); }
      }
    }
    return this.open(o);
  };

  Instance.prototype.list = function () {
    return api('/api/term/list').then(l => l.filter(i => this.shows(i)));
  };

  Instance.prototype.destroy = function () {
    if (this.destroyed) return;
    this.destroyed = true;
    instances.delete(this);
    this.ro.disconnect();
    for (const v of this.views.values()) { hideSug(v); v.term.dispose(); }
    this.views.clear();
    this.active = null;
    this.el.innerHTML = '';
    this.el.classList.remove('mt-root', 'mt-embedded', 'mt-standalone');
  };

  Instance.prototype.renderHead = function () {
    const ui = this.ui, v = this.active;
    if (!v) { ui.head.innerHTML = ''; ui.head.style.display = 'none'; return; }
    ui.head.style.display = '';
    const i = v.info;
    const badge = i.site
      ? '<span class="mt-badge ' + esc(i.kind || '') + '"><i></i>' + esc(i.site) + '</span>'
      : '<span class="mt-badge plain"><i></i>' + esc(i.title || 'Terminal') + '</span>';
    const chips = [];
    if (i.php && (!i.site || i.kind === 'php')) chips.push('PHP ' + i.php);
    if (i.node) chips.push('Node ' + i.node.replace(/^v/, ''));
    if (i.shell) chips.push(i.shell.replace(/\.exe$/i, ''));
    ui.head.innerHTML = badge + '<span class="mt-cwd" title="' + esc(i.cwd || '') + '"><bdi>' + esc(prettyPath(i.cwd)) + '</bdi></span>' +
      '<span class="mt-chips">' + chips.map(c => '<span class="mt-chip">' + esc(c) + '</span>').join('') + '</span>';
  };

  Instance.prototype.renderEmpty = function (err) {
    const ui = this.ui;
    if (this.views.size) { ui.empty.style.display = 'none'; return; }
    ui.empty.style.display = '';
    ui.empty.innerHTML = '<div>No terminals open<br><button type="button">Open a terminal</button>' +
      (err ? '<div class="mt-error">' + esc(err) + '</div>' : '') + '</div>';
    ui.empty.querySelector('button').onclick = () => this.open().catch(() => {});
  };

  function prettyPath(p) {
    const m = /^(\/Users\/[^/]+|\/home\/[^/]+)(\/.*)?$/.exec(p || '');
    return m ? '~' + (m[2] || '') : (p || '');
  }

  /* ── a view's xterm ───────────────────────────────────────── */
  function createTerm(v) {
    const name = themeName();
    const term = new window.Terminal({
      fontFamily: MONO, fontSize: 13, lineHeight: 1.22, letterSpacing: 0,
      cursorBlink: true, cursorStyle: 'bar', cursorWidth: 2, cursorInactiveStyle: 'outline',
      scrollback: 5000, allowProposedApi: false, macOptionIsMeta: true,
      theme: themeFor(name), minimumContrastRatio: name === 'light' ? 4.5 : 1,
      smoothScrollDuration: 0, drawBoldTextInBrightColors: false,
      windowsPty: v.info.os === 'windows' ? { backend: 'conpty' } : undefined,
    });
    const fit = new window.FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(v.el);
    v.term = term; v.fit = fit;

    term.onData(d => onInput(v, d));
    term.onBinary(d => send(v.id, d));
    term.onResize(({ cols, rows }) => {
      // Reflow moves the line: a half-typed one can't be followed any
      // more (until Enter); an empty one still can.
      if (v.anchor || v.pre) v.preOk = false;
      v.anchor = null; v.pre = '';
      hideSug(v);
      // Only the view being looked at drives the shell's size.
      if (v.inst.active !== v || !v.el.offsetWidth) return;
      clearTimeout(v.resizeT);
      v.resizeT = setTimeout(() => {
        const g = sessions.get(v.id);
        if (g && g.alive) api('/api/term/resize', { id: v.id, cols, rows }).catch(() => {});
      }, 40);
    });
    // Several views of one session may differ in size; the one being
    // typed into wins.
    if (term.textarea) term.textarea.addEventListener('focus', () => {
      const g = sessions.get(v.id);
      if (g && g.alive && v.el.offsetWidth) api('/api/term/resize', { id: v.id, cols: term.cols, rows: term.rows }).catch(() => {});
    });
    // Entering/leaving a full-screen app: a fresh prompt follows.
    term.buffer.onBufferChange(() => { v.anchor = null; v.pre = ''; v.preOk = true; hideSug(v); });
    term.attachCustomKeyEventHandler(ev => keyHandler(v, ev));
  }

  function markDead(v) {
    if (v.deadShown) return;
    v.deadShown = true;
    const g = sessions.get(v.id);
    const code = g ? g.exitCode : 0;
    v.tab.classList.add('dead');
    hideSug(v);
    v.term.write('\r\n\x1b[2m[process exited' + (code ? ' with code ' + code : '') +
      ' — press Enter to start a new shell here]\x1b[0m\r\n');
  }

  async function restart(v) {
    const inst = v.inst, info = v.info;
    closeSession(v.id);
    try { await inst.open({ cwd: info.cwd, site: info.site, title: info.title }); } catch {}
  }

  /* ── keyboard ─────────────────────────────────────────────── */
  function onInput(v, d) {
    const g = sessions.get(v.id);
    if (!g || !g.alive) {
      if (d === '\r') restart(v);
      return;
    }
    const buf = v.term.buffer.active;
    // The line being typed starts where the cursor was at its first
    // keystroke (the end of the prompt); it's read back from the screen,
    // so shell editing, history and zsh's own completion all stay in sync.
    let text = d;
    if (text.startsWith('\x1b[200~')) text = text.slice(6).replace(/\x1b\[201~$/, '');
    const printable = text.length > 0 && !/[\x00-\x1f\x7f]/.test(text) && !text.startsWith('\x1b');
    if (buf.type === 'alternate') {
      v.anchor = null;
    } else if (/[\r\n\x03\x04\x0c\x15]/.test(d)) {
      v.anchor = null; v.suppress = false; v.pre = ''; v.preOk = true;
    } else if (printable) {
      // Until the line's start is known, remember what was typed; once
      // its echo is on screen, the start is where that text begins.
      if (!v.anchor && v.preOk) v.pre += text;
      v.suppress = false;
    } else if (!v.anchor && (d === '\x7f' || d === '\b')) {
      v.pre = [...v.pre].slice(0, -1).join('');
    } else {
      if (!v.anchor) v.preOk = false; // cursor moves/edits we can't follow
      if (d === '\t' || d === '\x1b[A' || d === '\x1b[B' || d === '\x1bOA' || d === '\x1bOB') {
        v.suppress = true; // shell completion / history: don't pop up over it
      }
    }
    // Keep an open popup honest right away — before the echo arrives —
    // so Enter/Tab never act on a list that no longer fits the word.
    if (v.sug) {
      if (printable && !/\s/.test(text)) narrowSug(v, v.sug.token + text);
      else if ((d === '\x7f' || d === '\b') && v.sug.token) narrowSug(v, [...v.sug.token].slice(0, -1).join(''));
      else hideSug(v);
    }
    send(v.id, d);
    const tracking = v.anchor || (v.preOk && v.pre);
    if (tracking && !v.suppress && buf.type !== 'alternate' && (printable || d === '\x7f' || d === '\b')) { v.retries = 0; scheduleSug(v); }
    else hideSug(v);
  }

  function keyHandler(v, ev) {
    const mod = isMac ? ev.metaKey : ev.ctrlKey;
    const k = ev.key;
    // Suggestion popup — only while it's open; otherwise these keys go to
    // the shell untouched (Tab → zsh completion, Enter → run).
    if (v.sug && v.sug.items.length) {
      const nav = { ArrowDown: 1, ArrowUp: 1, Tab: 1, ArrowRight: 1, Enter: 1, Escape: 1 };
      if (nav[k] && !ev.metaKey && !ev.ctrlKey && !ev.altKey && !ev.shiftKey && !ev.isComposing) {
        if (k === 'ArrowRight' && !atLineEnd(v)) return true;
        // Nothing left to insert (the word is already exactly the
        // highlighted item): the popup steps aside — Enter runs the line.
        if (k !== 'ArrowDown' && k !== 'ArrowUp' && k !== 'Escape' && isExact(v.sug.items[v.sug.sel], v.sug.token)) {
          hideSug(v);
          return true;
        }
        if (ev.type === 'keydown') {
          ev.preventDefault();
          ev.stopPropagation();
          if (k === 'ArrowDown') moveSel(v, 1);
          else if (k === 'ArrowUp') moveSel(v, -1);
          else if (k === 'Escape') { hideSug(v); v.suppress = true; }
          else accept(v, v.sug.sel); // Tab, →, Enter
        }
        return false;
      }
    }
    if (ev.type !== 'keydown') return true;
    // Copy: with a selection, let the browser's native copy run (xterm
    // fills the clipboard from its copy event — this is also what the
    // Edit menu does in Mullion.app). Without one, Ctrl+C is ^C as usual;
    // Cmd+C on a Mac never interrupts anything.
    if (mod && !ev.altKey && (k === 'c' || k === 'C')) {
      if (v.term.hasSelection()) {
        const text = v.term.getSelection();
        try { navigator.clipboard && navigator.clipboard.writeText(text).catch(() => {}); } catch {}
        if (!isMac && !ev.shiftKey) setTimeout(() => v.term.clearSelection(), 0);
        return false;
      }
      return !isMac && !ev.shiftKey; // Ctrl+C → ^C (xterm); Cmd+C / Ctrl+Shift+C → nothing
    }
    // Paste: let the browser deliver a native paste event to xterm's
    // textarea (xterm handles bracketed paste). Returning false stops
    // xterm from turning Ctrl+V into ^V first.
    if (mod && !ev.altKey && (k === 'v' || k === 'V')) return false;
    // Cmd+K / Ctrl+Shift+K clears the scrollback, like Terminal.app.
    if ((isMac && ev.metaKey && k === 'k') || (!isMac && ev.ctrlKey && ev.shiftKey && (k === 'K' || k === 'k'))) {
      ev.preventDefault();
      v.term.clear();
      return false;
    }
    // Leave other Cmd shortcuts (Cmd+T, Cmd+W, reload…) to the app.
    if (isMac && ev.metaKey) return false;
    return true;
  }

  /* ── suggestions ──────────────────────────────────────────── */
  // findAnchor places the line's start from the text typed so far: it
  // must be what's just before the cursor on the cursor's row.
  function findAnchor(v) {
    if (v.anchor || !v.preOk || !v.pre) return;
    const buf = v.term.buffer.active;
    const y = buf.baseY + buf.cursorY, x = buf.cursorX;
    const line = buf.getLine(y);
    if (!line || v.pre.length > x) return;
    if (line.translateToString(false, x - v.pre.length, x) === v.pre) {
      v.anchor = { x: x - v.pre.length, y };
      v.pre = '';
    }
  }

  function currentLine(v) {
    findAnchor(v);
    const a = v.anchor;
    if (!a) return null;
    const buf = v.term.buffer.active;
    const cy = buf.baseY + buf.cursorY, cx = buf.cursorX;
    if (cy < a.y || (cy === a.y && cx < a.x)) { v.anchor = null; return null; }
    if (cy - a.y > 8) return null;
    let text = '';
    for (let y = a.y; y <= cy; y++) {
      const line = buf.getLine(y);
      if (!line) return null;
      if (y > a.y && !line.isWrapped) return null; // a continuation prompt, not one wrapped line
      text += line.translateToString(false, y === a.y ? a.x : 0, y === cy ? cx : v.term.cols);
    }
    return text;
  }
  function atLineEnd(v) {
    const buf = v.term.buffer.active;
    const line = buf.getLine(buf.baseY + buf.cursorY);
    return !line || line.translateToString(true, buf.cursorX).length === 0;
  }

  function scheduleSug(v) {
    clearTimeout(v.sugT);
    v.sugT = setTimeout(() => requestSug(v), 40);
  }
  async function requestSug(v) {
    if (v.inst.active !== v || v.suppress || v.inst.destroyed) return;
    const line = currentLine(v);
    if (line == null && !v.anchor && v.preOk && v.pre) {
      // The echo of what was typed isn't on screen yet: look again shortly.
      if ((v.retries = (v.retries || 0) + 1) <= 10) scheduleSug(v);
      return;
    }
    v.retries = 0;
    if (line == null || !line.trim()) { hideSug(v); return; }
    const seq = ++v.sugSeq;
    let res;
    try { res = await api('/api/term/complete', { id: v.id, input: line }); } catch { return; }
    if (seq !== v.sugSeq || v.suppress || !v.anchor) return;
    // The line may have moved on while we waited; drop stale answers.
    if (currentLine(v) !== line) { scheduleSug(v); return; }
    let items = res.items || [];
    const token = line.slice(res.replaceFrom);
    // Right after a space (nothing typed yet) only argument lists are
    // worth a popup (scripts, artisan commands, branches) — never a bare
    // folder listing, where Enter most likely means "run it" (`cd `, `ls `).
    if (!token && items.every(it => it.kind === 'dir' || it.kind === 'file')) items = [];
    if (items.length === 1 && isExact(items[0], token)) items = [];
    if (!items.length) { hideSug(v); return; }
    v.sug = { items, all: items, sel: 0, token };
    renderSug(v);
  }

  // narrowSug filters the open list to what still matches the word as
  // it's being typed (prefix, then word-start fuzzy — like the panel),
  // hiding it when nothing does.
  function narrowSug(v, token) {
    const sg = v.sug;
    const plain = token.replace(/\\(.)/g, '$1').toLowerCase();
    const base = plain.slice(plain.search(/[^/\\]*$/));
    const items = sg.all.filter(it => {
      const l = it.label.toLowerCase(), ins = it.insert.replace(/\\(.)/g, '$1').toLowerCase();
      return !base || l.startsWith(base) || ins.startsWith(plain) || fuzzyWord(l, base);
    });
    if (!items.length || (!token && sg.token)) { hideSug(v); return; }
    // The exact word goes first (and is highlighted); if it's all that's
    // left there's nothing to suggest.
    const ex = items.findIndex(it => isExact(it, token));
    if (ex >= 0) {
      if (items.length === 1) { hideSug(v); return; }
      items.unshift(items.splice(ex, 1)[0]);
    }
    const keep = sg.items[sg.sel];
    sg.items = items;
    sg.token = token;
    sg.sel = ex >= 0 ? 0 : Math.max(0, items.indexOf(keep));
    renderSug(v);
  }
  function isExact(it, token) {
    return !!it && (it.insert === token || it.insert.replace(/ +$/, '') === token);
  }
  function fuzzyWord(s, pat) {
    if (pat.length < 2) return false;
    for (let i = 0; i < s.length; i++) {
      if ((i === 0 || ' -_.:/'.includes(s[i - 1])) && s[i] === pat[0]) {
        let j = 0;
        for (let k = i; k < s.length && j < pat.length; k++) if (s[k] === pat[j]) j++;
        if (j === pat.length) return true;
      }
    }
    return false;
  }

  function renderSug(v) {
    const box = v.inst.ui.sug, sg = v.sug;
    const tok = (sg.token || '').replace(/\\(.)/g, '$1').toLowerCase();
    const base = tok.slice(tok.search(/[^/\\]*$/)); // what's typed after the last separator
    box.innerHTML = '<ul role="listbox">' + sg.items.map((it, i) => {
      const lbl = it.label;
      let lh = esc(lbl);
      const low = lbl.toLowerCase();
      for (const pre of [tok, base]) {
        if (pre && low.startsWith(pre)) { lh = '<b>' + esc(lbl.slice(0, pre.length)) + '</b>' + esc(lbl.slice(pre.length)); break; }
      }
      return '<li role="option" class="k-' + it.kind + (i === sg.sel ? ' sel' : '') + '" data-i="' + i + '"' +
        (i === sg.sel ? ' aria-selected="true"' : '') + '>' +
        (ICON[it.kind] || ICON.cmd) + '<span class="mt-l">' + lh + '</span>' +
        (it.detail ? '<span class="mt-d">' + esc(it.detail) + '</span>' : '') + '</li>';
    }).join('') + '</ul><div class="mt-hint"><span><kbd>Tab</kbd><kbd>↵</kbd> accept</span>' +
      '<span><kbd>↑</kbd><kbd>↓</kbd> choose</span><span><kbd>Esc</kbd> dismiss</span></div>';
    box.querySelectorAll('li').forEach(li => {
      li.onmousedown = e => { e.preventDefault(); accept(v, +li.dataset.i); };
    });
    box.classList.add('show');
    placeSug(v);
    const sel = box.querySelector('li.sel');
    if (sel) sel.scrollIntoView({ block: 'nearest' });
  }

  function placeSug(v) {
    const ui = v.inst.ui, box = ui.sug;
    const screen = v.el.querySelector('.xterm-screen');
    if (!screen || !v.sug) return;
    const buf = v.term.buffer.active;
    const cw = screen.clientWidth / v.term.cols, ch = screen.clientHeight / v.term.rows;
    const bodyRect = ui.body.getBoundingClientRect(), scr = screen.getBoundingClientRect();
    const typed = [...(v.sug.token || '')].length;
    const col = Math.max(0, buf.cursorX - typed);
    let left = scr.left - bodyRect.left + col * cw - 10;
    let top = scr.top - bodyRect.top + (buf.cursorY + 1) * ch + 4;
    const w = box.offsetWidth, hgt = box.offsetHeight;
    left = Math.max(4, Math.min(left, bodyRect.width - w - 6));
    if (top + hgt > bodyRect.height - 4) top = Math.max(4, scr.top - bodyRect.top + buf.cursorY * ch - hgt - 4);
    box.style.left = left + 'px';
    box.style.top = top + 'px';
  }

  function moveSel(v, d) {
    const n = v.sug.items.length;
    v.sug.sel = (v.sug.sel + d + n) % n;
    const box = v.inst.ui.sug;
    box.querySelectorAll('li').forEach((li, i) => {
      li.classList.toggle('sel', i === v.sug.sel);
      if (i === v.sug.sel) li.setAttribute('aria-selected', 'true'); else li.removeAttribute('aria-selected');
    });
    const sel = box.querySelector('li.sel');
    if (sel) sel.scrollIntoView({ block: 'nearest' });
  }

  function accept(v, i) {
    const sg = v.sug;
    if (!sg) return;
    const it = sg.items[i] || sg.items[0];
    const typed = sg.token;
    let out;
    if (it.insert.startsWith(typed)) out = it.insert.slice(typed.length);
    else out = '\x7f'.repeat([...typed].length) + it.insert;
    hideSug(v);
    if (out) send(v.id, out);
    v.term.focus();
    // Keep drilling into a folder; otherwise we're done with this word.
    if (it.kind === 'dir') scheduleSug(v);
  }

  function hideSug(v) {
    v.sug = null; v.sugSeq++; clearTimeout(v.sugT);
    if (v.inst.active === v) v.inst.ui.sug.classList.remove('show');
  }

  /* ── public API ───────────────────────────────────────────── */
  function pageInstance() {
    for (const inst of instances) if (inst.isPage) return inst;
    return null;
  }
  function projectInstance(site) {
    for (const inst of instances) if (inst.embedded && inst.site === site) return inst;
    return null;
  }

  // mount renders a terminal UI into el and returns its instance
  // (synchronously; instance.ready resolves once it's populated).
  function mount(el, opts) {
    opts = opts || {};
    if (opts.token) token = opts.token;
    for (const inst of instances) if (inst.el === el) inst.destroy();
    const inst = new Instance(el, opts);
    instances.add(inst);
    inst.ready = (async () => {
      await loadAssets();
      if (inst.destroyed) return inst;
      await resync();
      inst.sync();
      // Opens that were waiting for an instance like this one.
      for (let i = 0; i < pending.length; i++) {
        if (pending[i].match(inst)) {
          const o = pending.splice(i--, 1)[0].opts;
          await inst.openOrFocus(o).catch(() => {});
        }
      }
      // Another instance may be opening one right now (e.g. a project tab
      // mounted together with the Terminal page): wait before adding ours.
      if (!inst.views.size && opening.size) { await Promise.allSettled([...opening]); inst.sync(); }
      if (!inst.views.size && !inst.destroyed) await inst.open().catch(() => {});
      return inst;
    })();
    return inst;
  }

  function prefMode() {
    const P = window.MullionPrefs;
    try {
      if (P && typeof P.get === 'function') return P.get('terminal.openIn', 'page');
    } catch {}
    return 'page';
  }

  // openInProject opens (or focuses) a terminal for a site where the
  // user wants terminals: the Terminal page, a separate window, or the
  // project's own page — mode, else the terminal.openIn preference.
  async function openInProject(site, o) {
    o = Object.assign({}, o || {});
    let mode = o.mode || prefMode();
    if (mode && typeof mode.then === 'function') mode = await mode.catch(() => 'page');
    const opts = { site, newTab: !!o.newTab, cwd: o.cwd || '' };
    if (mode === 'window') { openWindow(site, { cwd: o.cwd }); return null; }
    if (mode === 'project' && typeof MT.onRequestProject === 'function') {
      try { MT.onRequestProject(site); } catch {}
      const inst = projectInstance(site);
      if (inst) { await inst.ready; return inst.openOrFocus(opts); }
      // Its page mounts the embedded terminal, which opens one if needed;
      // only an explicit new tab has to wait for it.
      if (opts.newTab) pending.push({ match: i => i.embedded && i.site === site, opts });
      return null;
    }
    if (typeof MT.onRequestShow === 'function') {
      try { MT.onRequestShow(site); } catch {}
    }
    const inst = pageInstance();
    if (!inst) { pending.push({ match: i => i.isPage, opts }); return null; }
    await inst.ready;
    return inst.openOrFocus(opts);
  }

  // The URL a standalone terminal window loads; parseWindowHash reads it back.
  function windowURL(site, o) {
    let hash = '#termwin' + (site ? '/' + encodeURIComponent(site) : '');
    if (o && o.cwd) hash += '?cwd=' + encodeURIComponent(o.cwd);
    return location.origin + '/?t=' + encodeURIComponent(token) + hash;
  }
  function openWindow(site, o) {
    return window.open(windowURL(site, o), 'mullion-term-' + Date.now(), 'popup,width=1100,height=700');
  }
  function parseWindowHash(hash) {
    const m = /^#termwin(?:\/([^?]*))?(?:\?(.*))?$/.exec(hash == null ? location.hash : hash);
    if (!m) return null;
    const q = new URLSearchParams(m[2] || '');
    return { site: m[1] ? decodeURIComponent(m[1]) : '', cwd: q.get('cwd') || '' };
  }

  // Module-level calls act on the Terminal page's instance (falling back
  // to any mounted one) — the single-instance API from before.
  function defaultInstance() {
    return pageInstance() || instances.values().next().value || null;
  }

  const MT = {
    mount,
    openInProject,
    openWindow,
    windowURL,
    parseWindowHash,
    open: o => {
      const inst = defaultInstance();
      if (!inst) { pending.push({ match: i => i.isPage, opts: o || {} }); return Promise.resolve(null); }
      return inst.ready.then(() => inst.open(o));
    },
    list: () => api('/api/term/list'),
    focus: () => { const i = defaultInstance(); if (i) i.focus(); },
    fit: () => { for (const i of instances) i.fit(); },
    instances: () => [...instances],
    onRequestShow: null,
    onRequestProject: null,
  };
  window.MullionTerminal = MT;
})();
