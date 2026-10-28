/* The project page — "Manage" one linked site: an overview strip and
 * tabs that adapt to the project (Commands, Workers, Packages, Git,
 * Cache, Watch, Terminal, Domains, Settings), backed by /api/project/* (project.go).
 *
 * Usage from the panel page:
 *
 *   <script src="/project.js"></script>        (loads /project.css itself)
 *   const p = MullionProject.mount(containerEl, 'my-site');
 *   p.showTab('terminal'); p.reload(); p.fit(); p.unmount();
 *
 * Any number of instances may be alive at once, each in its own
 * container (e.g. one per in-app project tab, kept alive while hidden).
 * Each has its own state, timers and listeners; worker polling runs only
 * while its container is visible. MullionProject.unmount() with no
 * argument unmounts the most recent instance; current() is its site.
 *
 * It reuses the panel's globals — api(), toast(), modal(), confirmModal(),
 * promptModal(), alertModal(), refresh(), the #busy overlay, MullionPrefs,
 * MullionTerminal and the design tokens — and falls back quietly where
 * one is missing.
 *
 * Events on window (so the shell can follow along):
 *   mullion:project-renamed  {detail: {from, to}}   (the instance follows)
 *   mullion:project-unlinked {detail: {name, kind}} (the instance unmounts)
 *   mullion:watch-exception  {detail: {site, count}} (new exceptions, unseen)
 *   mullion:watch-seen       {detail: {site}}        (the user looked)
 */
(function () {
  'use strict';
  if (window.MullionProject) return;

  // The stylesheet ships next to this script.
  if (!document.querySelector('link[href="/project.css"]')) {
    const l = document.createElement('link');
    l.rel = 'stylesheet';
    l.href = '/project.css';
    document.head.appendChild(l);
  }

  /* ── panel plumbing (globals from index.html, with fallbacks) ───── */
  const G = name => (typeof window[name] === 'function' ? window[name] : null);
  const TOKEN = new URLSearchParams(location.search).get('t') || '';
  async function call(path, body) {
    if (G('api')) return G('api')(path, body === undefined ? {} : body);
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'X-Mullion-Token': TOKEN, 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    });
    const out = await res.json();
    if (!out.ok) throw new Error(out.error || 'request failed');
    return out.data;
  }
  const say = msg => (G('toast') ? G('toast')(msg) : console.log(msg));
  const confirmBox = opts => (G('confirmModal') ? G('confirmModal')(opts) : Promise.resolve(false));
  const promptBox = opts => (G('promptModal') ? G('promptModal')(opts) : Promise.resolve(null));
  const alertBox = opts => (G('alertModal') ? G('alertModal')(opts) : Promise.resolve());
  const modalBox = opts => (G('modal') ? G('modal')(opts) : Promise.resolve(null));
  const refreshPanel = () => { try { if (G('refresh')) G('refresh')(); } catch {} };

  // busy shows the panel's blocking overlay around a short action.
  async function busy(label, fn) {
    const b = document.getElementById('busy'), l = document.getElementById('busy-label');
    if (b && l) { l.textContent = label; b.classList.add('show'); }
    try { return await fn(); } finally { if (b) b.classList.remove('show'); }
  }
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  // Preferences live in window.MullionPrefs (server-backed — the panel's
  // origin changes port every launch, which wipes localStorage); without
  // it, localStorage is the fallback.
  const Prefs = () => (window.MullionPrefs && typeof window.MullionPrefs.get === 'function' ? window.MullionPrefs : null);
  function prefGet(k, def) {
    const P = Prefs();
    if (P) { try { const v = P.get(k, def); return v === undefined ? def : v; } catch { return def; } }
    try { const v = localStorage.getItem('mullion.' + k); return v == null ? def : JSON.parse(v); } catch { return def; }
  }
  function prefSet(k, v) {
    const P = Prefs();
    if (P) { try { P.set(k, v); } catch {} return; }
    try { localStorage.setItem('mullion.' + k, JSON.stringify(v)); } catch {}
  }
  const prefsReady = () => {
    const P = Prefs();
    return P && P.ready && typeof P.ready.then === 'function' ? P.ready.catch(() => {}) : Promise.resolve();
  };
  // favorites are stored as a JSON array; accept a JSON string too.
  function favsOf(site) {
    let v = prefGet('fav:' + site, []);
    if (typeof v === 'string') { try { v = JSON.parse(v); } catch { v = []; } }
    return Array.isArray(v) ? v : [];
  }

  async function copyText(text) {
    try { await navigator.clipboard.writeText(text); return true; } catch {}
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0;top:0;left:0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch {}
    ta.remove();
    return ok;
  }

  const fmtNum = n => {
    if (!n) return '0';
    if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  };
  function fmtDur(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + 's';
    const m = Math.floor(s / 60);
    if (m < 60) return m + 'm ' + (s % 60) + 's';
    const h = Math.floor(m / 60);
    if (h < 24) return h + 'h ' + (m % 60) + 'm';
    return Math.floor(h / 24) + 'd ' + (h % 24) + 'h';
  }
  function fmtAgo(iso) {
    const t = Date.parse(iso);
    if (!t) return '';
    const d = Math.floor((Date.now() - t) / 86400000);
    if (d < 1) return 'today';
    if (d < 31) return d + 'd ago';
    if (d < 365) return Math.floor(d / 30) + 'mo ago';
    return Math.floor(d / 365) + 'y ago';
  }

  /* ── ANSI → HTML (SGR colours only; everything else stripped) ───── */
  function ansiToHtml(text) {
    // Carriage-return progress bars: keep only what the last \r wrote.
    text = text.replace(/\r\n/g, '\n').split('\n').map(line => {
      const i = line.lastIndexOf('\r', line.length - 2);
      return i >= 0 ? line.slice(i + 1) : line.replace(/\r$/, '');
    }).join('\n');
    text = text
      .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, '')       // OSC (titles, links)
      .replace(/\x1b\[[0-9;?]*[A-La-ln-z]/g, '')              // cursor moves, erase…
      .replace(/\x1b[()][A-Za-z0-9]/g, '')
      .replace(/[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f]/g, '');
    let out = '', open = false;
    let st = { b: false, d: false, i: false, u: false, fg: 0, bg: 0 };
    const re = /\x1b\[([0-9;]*)m/g;
    let last = 0, m;
    const flushText = s => { if (s) out += esc(s); };
    const openSpan = () => {
      const cls = [];
      if (st.b) cls.push('a-b');
      if (st.d) cls.push('a-dim');
      if (st.i) cls.push('a-i');
      if (st.u) cls.push('a-u');
      if (st.fg) cls.push('a-' + st.fg);
      if (st.bg) cls.push('a-bg' + st.bg);
      if (cls.length) { out += '<span class="' + cls.join(' ') + '">'; open = true; }
    };
    while ((m = re.exec(text))) {
      flushText(text.slice(last, m.index));
      last = re.lastIndex;
      if (open) { out += '</span>'; open = false; }
      const codes = m[1] === '' ? [0] : m[1].split(';').map(Number);
      for (let k = 0; k < codes.length; k++) {
        const c = codes[k];
        if (c === 0) st = { b: false, d: false, i: false, u: false, fg: 0, bg: 0 };
        else if (c === 1) st.b = true;
        else if (c === 2) st.d = true;
        else if (c === 3) st.i = true;
        else if (c === 4) st.u = true;
        else if (c === 22) { st.b = false; st.d = false; }
        else if (c === 23) st.i = false;
        else if (c === 24) st.u = false;
        else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) st.fg = c;
        else if (c === 39) st.fg = 0;
        else if (c >= 41 && c <= 44) st.bg = c;
        else if (c === 49 || c === 40 || (c >= 45 && c <= 47)) st.bg = 0;
        else if (c === 38 || c === 48) { k += codes[k + 1] === 5 ? 2 : 4; } // 256/true colour: skip
      }
      openSpan();
    }
    flushText(text.slice(last));
    if (open) out += '</span>';
    return out.replace(/\x1b/g, '');
  }

  /* ── icons ─────────────────────────────────────────────────────── */
  const I = (d, extra) => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"${extra || ''}>${d}</svg>`;
  const ICON = {
    open: I('<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><path d="M15 3h6v6"/><path d="M10 14L21 3"/>'),
    term: I('<path d="M4 17l6-6-6-6"/><path d="M12 19h8"/>'),
    copy: I('<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>'),
    folder: I('<path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>'),
    star: I('<path d="M12 2l3.09 6.26L22 9.27l-5 4.87 1.18 6.88L12 17.77l-6.18 3.25L7 14.14 2 9.27l6.91-1.01z"/>'),
    search: I('<circle cx="11" cy="11" r="7"/><path d="M21 21l-4.35-4.35"/>'),
    x: I('<path d="M18 6L6 18M6 6l12 12"/>', ' stroke-width="2.4"'),
    chev: I('<path d="M6 9l6 6 6-6"/>', ' stroke-width="2.4"'),
    play: I('<path d="M6 4l14 8-14 8z"/>'),
    cmd: I('<path d="M4 17l6-6-6-6"/><path d="M12 19h8"/>'),
    workers: I('<path d="M12 2v4M12 18v4M4.93 4.93l2.83 2.83M16.24 16.24l2.83 2.83M2 12h4M18 12h4M4.93 19.07l2.83-2.83M16.24 7.76l2.83-2.83"/>'),
    pkg: I('<path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/><path d="M3.27 6.96L12 12.01l8.73-5.05M12 22.08V12"/>'),
    globe: I('<circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/>'),
    gear: I('<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>'),
    bolt: I('<path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/>'),
    lock: I('<rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>'),
    alert: I('<path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/>'),
    plus: I('<path d="M12 5v14M5 12h14"/>', ' stroke-width="2.4"'),
    minus: I('<path d="M5 12h14"/>', ' stroke-width="2.4"'),
    check: I('<path d="M20 6L9 17l-5-5"/>', ' stroke-width="2.4"'),
    branch: I('<circle cx="6" cy="5" r="2.2"/><circle cx="6" cy="19" r="2.2"/><circle cx="18" cy="7" r="2.2"/><path d="M6 7.2v9.6M18 9.2c0 5-6 3.5-11 7.6"/>'),
    gitLogo: I('<circle cx="12" cy="5" r="2.3"/><circle cx="12" cy="19" r="2.3"/><circle cx="19" cy="12" r="2.3"/><path d="M12 7.3v9.4M13.7 6.6l3.7 3.7"/>'),
    fetch: I('<path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/>'),
    pull: I('<path d="M12 3v12"/><path d="M7 10l5 5 5-5"/><path d="M5 21h14"/>'),
    push: I('<path d="M12 21V9"/><path d="M7 14l5-5 5 5"/><path d="M5 3h14"/>'),
    trash: I('<path d="M3 6h18"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>'),
    undo: I('<path d="M3 7v6h6"/><path d="M3.5 13A9 9 0 1 0 6 6.3L3 9"/>'),
    dots: I('<circle cx="5" cy="12" r="1.2"/><circle cx="12" cy="12" r="1.2"/><circle cx="19" cy="12" r="1.2"/>', ' stroke-width="2.4"'),
    rebase: I('<circle cx="6" cy="18" r="2.2"/><circle cx="18" cy="6" r="2.2"/><path d="M6 15.8V9a3 3 0 0 1 3-3h6.8"/><path d="M13 3l3 3-3 3"/>'),
    cache: I('<path d="M12 2 2 7l10 5 10-5-10-5z"/><path d="m2 17 10 5 10-5"/><path d="m2 12 10 5 10-5"/>'),
    stash: I('<rect x="3" y="4" width="18" height="5" rx="1.5"/><path d="M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9"/><path d="M10 13h4"/>'),
    cloud: I('<path d="M18 10h-1.26A8 8 0 1 0 9 20h9a5 5 0 0 0 0-10z"/>'),
    commit: I('<circle cx="12" cy="12" r="3.5"/><path d="M2 12h6.5M15.5 12H22"/>'),
    file: I('<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/>'),
    eye: I('<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>'),
    tag: I('<path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z"/><circle cx="7" cy="7" r="1.2"/>'),
  };

  // Framework marks: brand colour + a short glyph.
  const MARKS = {
    laravel: ['#FF2D20', 'L'], symfony: ['#7e8794', 'Sf'], wordpress: ['#21759B', 'W'],
    drupal: ['#0678BE', 'D'], codeigniter: ['#DD4814', 'CI'], yii: ['#40B3D8', 'Yii'],
    cakephp: ['#D33C43', 'C'], slim: ['#719E40', 'S'], php: ['#777BB4', 'php'],
    nuxt: ['#00C16A', 'N'], next: ['#8b93a6', 'N'], vue: ['#42B883', 'V'], react: ['#1ea7c9', 'R'],
    svelte: ['#FF3E00', 'S'], sveltekit: ['#FF3E00', 'SK'], astro: ['#BC52EE', 'A'],
    angular: ['#DD0031', 'A'], vite: ['#646CFF', 'V'], remix: ['#8b93a6', 'R'],
    gatsby: ['#663399', 'G'], solid: ['#446b9e', 'S'], qwik: ['#AC7EF4', 'Q'],
    express: ['#8b93a6', 'ex'], nest: ['#E0234E', 'N'], node: ['#5FA04E', 'JS'],
    static: ['#8b93a6', '</>'], unknown: ['#8b93a6', '?'],
  };
  function markHTML(icon) {
    const m = MARKS[icon] || MARKS.unknown;
    return `<div class="mp-mark" style="--mp-fw:${m[0]}">${esc(m[1])}</div>`;
  }

  /* ════════════════════════════════════════════════════════════════
     INSTANCE — each mount() gets its own closure: state, timers,
     listeners and DOM queries are all scoped to its container.
     ════════════════════════════════════════════════════════════════ */
  function createInstance(rootEl, siteName) {
  let root = rootEl;     // the mount element
  let cur = siteName;    // site name
  let info = null;       // /api/project/info
  let gen = 0;           // bumps on every (re)load/destroy — stale async work checks it
  let tab = null;
  let els = {};
  let pollTimer = 0;
  let tabs = [];
  let destroyed = false;
  let term = null;       // embedded MullionTerminal instance (Terminal tab)
  let termBox = null;    // its container — kept across tab switches
  let wasVisible = false;
  let pendingTab = null; // showTab() before the page loaded
  // per-site caches
  let cmdState = null, wkState = null, pkgState = null, gitState = null, cacheState = null, watchState = null;

  const live = g => g === gen && !destroyed;
  // visible: the container is laid out (not display:none, e.g. a hidden
  // project tab) and the window isn't hidden.
  const visible = () => !destroyed && document.visibilityState === 'visible' && !!(root.offsetParent || root.getClientRects().length);

  // action runs a site-changing call behind the overlay, toasts, and
  // re-syncs both this page and the panel. Returns true on success.
  async function action(label, path, body, done) {
    try {
      await busy(label, () => call(path, body));
      if (done) say(done);
      return true;
    } catch (e) {
      say('Error: ' + e.message);
      return false;
    } finally {
      refreshPanel();
      if (!destroyed) reloadInfo();
    }
  }

  // becameVisible catches up when a hidden instance is shown again.
  function onShown() {
    if (tab === 'workers') loadWorkers(true);
    if (tab === 'git') loadGitStatus(true);
    if (tab === 'terminal') fitTerminal();
    if (tab === 'watch' && watchState) { wPaintSubs(); wPaintBody(); if (watchState.sub === 'exceptions') wSeen(); }
  }
  function checkVisible() {
    const v = visible();
    if (v && !wasVisible) onShown();
    wasVisible = v;
  }
  const onVisibility = () => checkVisible();
  document.addEventListener('visibilitychange', onVisibility);
  window.addEventListener('mullion:mterm-status', syncMterm);
  let io = null;
  if (typeof IntersectionObserver === 'function') {
    io = new IntersectionObserver(() => checkVisible());
    io.observe(root);
  }

  root.classList.add('mp-root');

  function load() {
    gen++;
    const g = gen;
    clearInterval(pollTimer);
    pollTimer = 0;
    closeLogModal();
    destroyTerminal();
    tab = null;
    info = null;
    els = {};
    cmdState = { cmds: null, error: '', q: '', sel: null, job: null, loading: false };
    wkState = { list: null, templates: null };
    pkgState = { data: null, source: null, q: '', results: null, searching: false, job: null, filter: '' };
    stopMonitor();
    cacheState = cacheFresh();
    stopWatchStream();
    watchState = watchFresh();
    closeBranchPop();
    gitState = gitFresh();
    root.innerHTML = `<div class="mp"><section class="card"><div class="mp-loading"><div class="spin"></div>Loading ${esc(cur)}…</div></section></div>`;
    Promise.all([call('/api/project/info', { site: cur }), prefsReady()]).then(([d]) => {
      if (!live(g)) return;
      info = d;
      renderShell();
      wasVisible = visible();
    }).catch(e => {
      if (!live(g)) return;
      root.innerHTML = `<div class="mp"><section class="card"><div class="card-body"><div class="mp-empty"><b>Couldn't open ${esc(cur)}</b>${esc(e.message)}</div></div></section></div>`;
    });
  }

  function destroy() {
    if (destroyed) return;
    gen++;
    destroyed = true;
    clearInterval(pollTimer);
    pollTimer = 0;
    closeLogModal();
    closeBranchPop();
    destroyTerminal();
    stopMonitor();
    stopWatchStream();
    if (watchState && watchState.unseen) window.dispatchEvent(new CustomEvent('mullion:watch-seen', { detail: { site: cur } }));
    document.removeEventListener('visibilitychange', onVisibility);
    window.removeEventListener('mullion:mterm-status', syncMterm);
    if (io) io.disconnect();
    root.innerHTML = '';
    root.classList.remove('mp-root');
    els = {};
    forget(api);
  }

  async function reloadInfo() {
    const g = gen;
    try {
      const d = await call('/api/project/info', { site: cur });
      if (!live(g) || !info) return;
      info = d;
      renderHero();
      if (tab === 'domains') renderDomains();
      if (tab === 'settings') renderSettings();
    } catch {}
  }

  /* ── shell: hero + tabs ────────────────────────────────────────── */
  const isPHP = () => info.kind === 'php';
  const isNode = () => info.kind === 'node';
  const fw = () => (info.detect.framework || '').toLowerCase();
  const isLaravel = () => info.detect.icon === 'laravel';

  function applicableTabs() {
    const t = [];
    const hasProject = info.hasComposer || info.hasPackage || isLaravel() || info.detect.icon === 'symfony';
    if (hasProject) t.push(['commands', 'Commands', ICON.cmd]);
    if (info.detect.language === 'PHP' || info.hasPackage || info.workers > 0) t.push(['workers', 'Workers', ICON.workers]);
    if (info.hasComposer || info.hasPackage) t.push(['packages', 'Packages', ICON.pkg]);
    if (isPHP() || isLaravel()) t.push(['cache', 'Cache', ICON.cache]);
    if (isPHP()) t.push(['watch', 'Watch', ICON.eye]);
    t.push(['git', 'Git', ICON.branch]);
    t.push(['terminal', 'Terminal', ICON.term]);
    t.push(['domains', 'Domains', ICON.globe]);
    t.push(['settings', 'Settings', ICON.gear]);
    return t;
  }

  function renderShell() {
    tabs = applicableTabs();
    root.innerHTML = `<div class="mp">
      <section class="card mp-hero" data-id="mp-hero"></section>
      <div class="mp-tabs" role="tablist">${tabs.map(([k, label, ic]) =>
        `<button role="tab" data-mptab="${k}">${ic}${label}${k === 'workers' ? '<span class="n" data-id="mp-wk-n" hidden></span>' : ''}${k === 'git' ? '<span class="n" data-id="mp-git-n" hidden></span>' : ''}${k === 'watch' ? '<span class="n" data-id="mp-watch-n" hidden></span>' : ''}</button>`).join('')}</div>
      <div class="mp-panel" data-id="mp-panel"></div>
      <div class="mp-panel" data-id="mp-term-panel"></div>
    </div>`;
    els.hero = root.querySelector('[data-id="mp-hero"]');
    els.panel = root.querySelector('[data-id="mp-panel"]');
    els.termPanel = root.querySelector('[data-id="mp-term-panel"]');
    root.querySelectorAll('[data-mptab]').forEach(b => b.onclick = () => showTab(b.dataset.mptab));
    // The tab bar is one scrolling row: a mouse wheel scrolls it sideways.
    const strip = root.querySelector('.mp-tabs');
    if (strip) strip.addEventListener('wheel', e => {
      if (strip.scrollWidth <= strip.clientWidth || Math.abs(e.deltaX) > Math.abs(e.deltaY)) return;
      e.preventDefault();
      strip.scrollLeft += e.deltaY;
    }, { passive: false });
    if (pendingTab && tabs.some(t => t[0] === pendingTab)) {
      const t = pendingTab;
      pendingTab = null;
      renderHero();
      if (watchOn()) startWatchStream();
      showTab(t);
      updateWorkerCount(info.workers);
      return;
    }
    renderHero();
    if (watchOn()) startWatchStream();
    const saved = prefGet('project.tab', 'commands');
    showTab(tabs.some(t => t[0] === saved) ? saved : tabs[0][0]);
    updateWorkerCount(info.workers);
  }

  function updateWorkerCount(n) {
    const el = root && root.querySelector('[data-id="mp-wk-n"]');
    if (!el) return;
    el.hidden = !n;
    el.textContent = n;
  }

  function versionChip() {
    if (isPHP()) {
      if (!info.php) return '<span class="badge warnb">no PHP installed</span>';
      return `<button class="mp-ver" data-hero="php" title="${info.phpPinned ? 'pinned for this site' : 'follows the global PHP'} — click to change">PHP ${esc(info.php)}${info.phpPinned ? '' : ' · global'}${ICON.chev}</button>`;
    }
    if (isNode()) {
      if (!info.nodeInstalled.length) return '<span class="badge warnb">no Node installed</span>';
      const v = info.node || info.nodePinned || info.globalNode || '—';
      return `<button class="mp-ver" data-hero="node" title="click to change">Node ${esc(v)}${info.nodePinned ? '' : ' · auto'}${ICON.chev}</button>`;
    }
    return '';
  }

  function statusHTML() {
    if (isNode()) {
      if (info.mode === 'build') return `<span class="mp-status"><span class="mp-dot on"></span>Serving the build <span class="mono muted">${esc(info.buildDir || 'dist')}/</span></span>`;
      if (info.devRunning) return `<span class="mp-status"><span class="mp-dot on"></span>Dev server on <span class="mono">:${info.devRunning}</span></span>`;
      if (info.devPaused) return `<span class="mp-status"><span class="mp-dot"></span>Dev server stopped</span>`;
      return `<span class="mp-status" title="wakes when you open the link"><span class="mp-dot sleep"></span>Sleeping</span>`;
    }
    if (info.kind === 'static') return `<span class="mp-status"><span class="mp-dot ${info.caddy ? 'on' : ''}"></span>${info.caddy ? 'Serving static files' : 'Stack stopped'}</span>`;
    return `<span class="mp-status"><span class="mp-dot ${info.caddy ? 'on' : ''}"></span>${info.caddy ? 'Serving' : 'Stack stopped'}</span>`;
  }

  function renderHero() {
    if (!els.hero) return;
    const d = info.detect || {};
    const facts = [];
    if (d.framework) facts.push(`<span class="badge fw">${esc(d.framework)}${d.frameworkVersion ? ' ' + esc(d.frameworkVersion) : ''}</span>`);
    if (d.language && d.language !== 'Unknown') facts.push(`<span class="badge">${esc(d.language)}</span>`);
    if (d.packageManager) facts.push(`<span class="badge mono">${esc(d.packageManager)}</span>`);
    else if (info.hasComposer) facts.push('<span class="badge mono">composer</span>');
    if (isNode()) facts.push(`<span class="badge">${info.mode === 'build' ? 'build' : 'dev'} mode</span>`);
    const hosts = (info.hosts || []).map((h, i) => h.startsWith('*.')
      ? `<span class="mp-host" title="every subdomain">${esc(h)}</span>`
      : `<a class="mp-host${i === 0 ? ' apex' : ''}" href="${info.secure ? 'https' : 'http'}://${esc(h)}" target="_blank">${esc(h)}</a>`).join('');
    const reveal = info.windows ? 'Show in Explorer' : 'Show in Finder';
    els.hero.innerHTML = `
      <div class="mp-hero-top">
        ${markHTML(d.icon)}
        <div class="mp-title">
          <div class="mp-name">${esc(info.name)}</div>
          <div class="mp-facts">${facts.join('')}${versionChip()}</div>
        </div>
        <div class="mp-hero-actions">
          <button class="primary sm" data-hero="open">${ICON.open}Open site</button>
          <button class="sm" data-hero="term">${ICON.term}Terminal here</button>
          <button class="sm" data-hero="mterm" data-mterm hidden title="Open this folder as a new tab in Mullion Terminal">${ICON.open}Mullion Terminal</button>
          <button class="sm" data-hero="copy" title="copy ${esc(info.path)}">${ICON.copy}Copy path</button>
          <button class="sm" data-hero="reveal">${ICON.folder}${reveal}</button>
        </div>
      </div>
      <div class="mp-hero-meta">
        <div class="mp-hosts">${hosts}</div>
        <div class="mp-hero-right">
          ${statusHTML()}
          <label class="toggle" title="serve over HTTPS with Mullion's local certificate">
            <span class="switch"><input type="checkbox" data-hero="secure" ${info.secure ? 'checked' : ''}><i></i></span>HTTPS</label>
        </div>
        <div class="mp-path" title="${esc(info.path)}">${ICON.folder.replace('<svg', '<svg width="13" height="13" style="flex:none"')}<span><bdi>${esc(info.path)}</bdi></span></div>
      </div>`;
    const q = s => els.hero.querySelector(`[data-hero="${s}"]`);
    q('open').onclick = () => window.open(info.url, '_blank');
    q('term').onclick = () => openTerminal();
    q('mterm').onclick = () => openInMterm();
    syncMterm();
    q('copy').onclick = async () => say(await copyText(info.path) ? 'Path copied' : 'Could not copy — ' + info.path);
    q('reveal').onclick = () => call('/api/open-path', { path: info.path }).catch(e => say('Error: ' + e.message));
    q('secure').onchange = e => setSecure(e.target.checked);
    const ver = q('php') || q('node');
    if (ver) ver.onclick = () => (isPHP() ? changePHP() : changeNode());
  }

  function setSecure(on) {
    return action(on ? 'Securing ' + info.host + '… (you may be asked to trust the local certificate)' : 'Switching to HTTP…',
      '/api/sites/secure', { name: cur, secure: on }, on ? 'HTTPS on' : 'HTTPS off');
  }

  async function changePHP() {
    const options = [{ value: '', label: 'Global (' + (info.globalPhp || '—') + ')' }]
      .concat(info.phpInstalled.map(v => ({ value: v, label: 'PHP ' + v })));
    const v = await promptBox({
      title: 'PHP version for ' + esc(cur),
      message: 'Pin this site to one installed PHP version, or let it follow the global one.',
      input: { type: 'select', label: 'Version', options, value: info.phpPinned || '' },
      confirmText: 'Switch',
    });
    if (v === null || v === (info.phpPinned || '')) return;
    action('Switching PHP version…', '/api/sites/isolate', { name: cur, version: v }, 'PHP version updated');
  }

  async function changeNode() {
    const options = [{ value: '', label: '.nvmrc / default (' + (info.globalNode || '—') + ')' }]
      .concat(info.nodeInstalled.map(v => ({ value: v, label: 'Node ' + v })));
    const v = await promptBox({
      title: 'Node version for ' + esc(cur),
      message: 'The dev server restarts on the new version.',
      input: { type: 'select', label: 'Version', options, value: info.nodePinned || '' },
      confirmText: 'Switch',
    });
    if (v === null || v === (info.nodePinned || '')) return;
    action('Switching Node version…', '/api/sites/node', { name: cur, version: v }, 'Node version updated');
  }

  // Mullion Terminal (the standalone app; index.html's MullionExtTerm).
  // Only the chosen terminal gets a button: with Mullion Terminal chosen
  // (and installed) the hero shows "Mullion Terminal" instead of
  // "Terminal here"; otherwise just "Terminal here". The project's own
  // Terminal tab stays built-in either way.
  function mtermChosen() {
    const X = window.MullionExtTerm;
    return !!(X && typeof X.installed === 'function' && X.installed() &&
      typeof X.preferred === 'function' && X.preferred());
  }
  function syncMterm() {
    const ext = mtermChosen();
    root.querySelectorAll('[data-mterm]').forEach(b => { b.hidden = !ext; });
    const builtin = root.querySelector('[data-hero="term"]');
    if (builtin) builtin.hidden = ext;
  }
  function openInMterm() {
    const X = window.MullionExtTerm;
    if (!X) return;
    Promise.resolve(X.open(cur)).then(() => say('Opened in Mullion Terminal')).catch(e => say('Mullion Terminal: ' + e.message));
  }

  // openTerminal opens (or focuses) a terminal in the project; with a
  // command it opens a fresh tab and types it there — or, when the
  // terminal isn't available, copies it for pasting. A plain open (no
  // command) follows the terminal.app preference; a command always needs
  // the built-in terminal, since it's typed into the new shell.
  async function openTerminal(command) {
    const X = window.MullionExtTerm;
    if (!command && X && typeof X.route === 'function' && X.route(cur, () => openBuiltinTerminal())) return;
    return openBuiltinTerminal(command);
  }
  async function openBuiltinTerminal(command) {
    const MT = window.MullionTerminal;
    if (!MT || typeof MT.openInProject !== 'function') {
      if (command) {
        const ok = await copyText(command);
        say(ok ? 'Terminal unavailable — command copied to the clipboard' : 'Terminal unavailable');
      } else say('The terminal is not available in this build');
      return;
    }
    let s = null;
    try { s = await MT.openInProject(cur, command ? { newTab: true } : undefined); } catch {}
    if (!command) return;
    if (s && s.id) {
      await sleep(400); // let the shell draw its prompt first
      try { await call('/api/term/input', { id: s.id, data: command + '\r' }); return; } catch {}
    }
    const ok = await copyText(command);
    say(ok ? 'Terminal opened — command copied, paste it to run' : 'Terminal opened');
  }

  // revealTab scrolls the tab bar sideways (never the page) so the
  // active tab is fully visible.
  function revealTab(name) {
    const strip = root.querySelector('.mp-tabs');
    const b = strip && strip.querySelector(`[data-mptab="${name}"]`);
    if (!b) return;
    const left = b.offsetLeft - strip.offsetLeft, right = left + b.offsetWidth;
    if (left < strip.scrollLeft) strip.scrollLeft = left - 8;
    else if (right > strip.scrollLeft + strip.clientWidth) strip.scrollLeft = right - strip.clientWidth + 8;
  }

  function showTab(name, remember) {
    if (destroyed || !info) return;
    if (!tabs.some(t => t[0] === name)) return;
    tab = name;
    if (remember !== false) prefSet('project.tab', name);
    root.querySelectorAll('[data-mptab]').forEach(b => b.classList.toggle('active', b.dataset.mptab === name));
    revealTab(name);
    clearInterval(pollTimer);
    pollTimer = 0;
    // The embedded terminal stays alive while other tabs show.
    if (termBox) termBox.hidden = name !== 'terminal';
    els.panel.hidden = name === 'terminal';
    if (name !== 'cache') stopMonitor();
    if (name === 'terminal') { showTerminal(); return; }
    if (name === 'commands') renderCommands();
    else if (name === 'workers') renderWorkers();
    else if (name === 'packages') renderPackages();
    else if (name === 'git') renderGit();
    else if (name === 'domains') renderDomains();
    else if (name === 'cache') renderCache();
    else if (name === 'watch') renderWatch();
    else renderSettings();
  }

  /* ════════════════════════════════════════════════════════════════
     TERMINAL — an embedded MullionTerminal, mounted on first view and
     kept alive (hidden) while other tabs show.
     ════════════════════════════════════════════════════════════════ */
  function terminalSupported() {
    const MT = window.MullionTerminal;
    // The embedded (multi-instance) API ships with openWindow().
    return !!(MT && typeof MT.mount === 'function' && typeof MT.openWindow === 'function');
  }

  function showTerminal() {
    const box = els.termPanel;
    if (!box) return;
    if (!box.firstChild) {
      box.innerHTML = `<section class="card mp-term-card">
          <div class="card-head">${ICON.term}<h2>Terminal</h2><span class="hint" title="${esc(info.path)}">In the project folder, with the site's ${isNode() ? 'Node' : 'PHP'}</span>
            <button class="sm mp-btn-ic" data-term="mterm" data-mterm hidden title="Open this folder as a new tab in Mullion Terminal">${ICON.term}Open in Mullion Terminal</button>
            <button class="sm mp-btn-ic" data-term="window">${ICON.open}Open in window</button></div>
          <div class="mp-term-host"></div>
        </section>`;
      box.querySelector('[data-term="mterm"]').onclick = () => openInMterm();
      syncMterm();
      box.querySelector('[data-term="window"]').onclick = () => {
        const MT = window.MullionTerminal;
        if (MT && typeof MT.openWindow === 'function') {
          try { Promise.resolve(MT.openWindow(cur)).catch(e => say('Error: ' + e.message)); } catch (e) { say('Error: ' + e.message); }
        } else openTerminal();
      };
    }
    termBox = box;
    box.hidden = false;
    const host = box.querySelector('.mp-term-host');
    if (!term && !host.dataset.mounted) {
      if (!terminalSupported()) {
        host.innerHTML = `<div class="mp-empty"><b>The embedded terminal isn't available</b>Use the terminal page instead.<div style="margin-top:12px"><button class="sm" data-term="page">Open terminal</button></div></div>`;
        host.querySelector('[data-term="page"]').onclick = () => openTerminal();
        return;
      }
      host.dataset.mounted = '1';
      const g = gen;
      let r;
      try { r = window.MullionTerminal.mount(host, { site: cur, embedded: true }); } catch (e) {
        host.innerHTML = `<div class="mp-empty"><b>Couldn't start the terminal</b>${esc(e.message)}</div>`;
        return;
      }
      Promise.resolve(r).then(inst => {
        if (!live(g) || host.dataset.mounted !== '1') { try { inst && inst.destroy && inst.destroy(); } catch {} return; }
        term = inst || null;
        fitTerminal();
      }).catch(e => {
        if (live(g)) host.innerHTML = `<div class="mp-empty"><b>Couldn't start the terminal</b>${esc(e.message)}</div>`;
      });
      return;
    }
    fitTerminal();
  }

  function fitTerminal() {
    if (!term || tab !== 'terminal') return;
    requestAnimationFrame(() => {
      try { if (typeof term.fit === 'function') term.fit(); } catch {}
      try { if (typeof term.focus === 'function') term.focus(); } catch {}
    });
  }

  function destroyTerminal() {
    if (term) { try { if (typeof term.destroy === 'function') term.destroy(); } catch {} }
    term = null;
    if (termBox) { termBox.innerHTML = ''; termBox = null; }
  }

  /* ── streaming jobs ────────────────────────────────────────────── */
  // follow polls a streaming job until done, calling onUpdate(st) with
  // every new status. Stops quietly when the page is unmounted.
  async function follow(jobId, onUpdate) {
    const g = gen;
    let lastLen = -1;
    for (;;) {
      let st;
      try { st = await call('/api/job', { id: jobId }); } catch (e) { st = { done: true, error: e.message, log: '' }; }
      if (!live(g)) return st;
      const len = (st.log || '').length + (st.logDropped || 0);
      if (len !== lastLen || st.done) { lastLen = len; onUpdate(st); }
      if (st.done) return st;
      await sleep(450);
    }
  }
  function paintTerm(pre, st, placeholder) {
    const nearBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    const log = st.log || '';
    let html = st.logDropped ? `<span class="trim">… ${fmtNum(st.logDropped)} bytes of earlier output trimmed</span>\n` : '';
    html += log ? ansiToHtml(log) : `<span class="dim">${esc(placeholder || '')}</span>`;
    pre.innerHTML = html;
    if (nearBottom) pre.scrollTop = pre.scrollHeight;
  }

  /* ════════════════════════════════════════════════════════════════
     COMMANDS
     ════════════════════════════════════════════════════════════════ */
  const GROUPS = { artisan: 'Artisan', console: 'Symfony console', npm: 'npm scripts', composer: 'Composer scripts' };
  const DANGER_RE = /\b(migrate:fresh|migrate:reset|migrate:refresh|db:wipe|doctrine:schema:drop|doctrine:database:drop|cache:clear --all)\b/;
  // Commands that want a TTY or never exit — better in the terminal.
  const INTERACTIVE_RE = /\b(artisan\s+(tinker|serve|pail)|psysh|bin\/console\s+server:run|(npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|serve|watch))\b/;

  function quickActions() {
    const q = [];
    if (isLaravel()) {
      q.push({ label: 'migrate', cmd: 'php artisan migrate' });
      q.push({ label: 'migrate:fresh --seed', cmd: 'php artisan migrate:fresh --seed', danger: true });
      q.push({ label: 'optimize:clear', cmd: 'php artisan optimize:clear' });
      q.push({ label: 'route:list', cmd: 'php artisan route:list' });
      q.push({ label: 'tinker', cmd: 'php artisan tinker', terminal: true });
      q.push({ label: 'storage:link', cmd: 'php artisan storage:link' });
      q.push({ label: 'queue:restart', cmd: 'php artisan queue:restart' });
      q.push({ label: 'key:generate', cmd: 'php artisan key:generate', danger: true });
    } else if (info.detect.icon === 'symfony') {
      q.push({ label: 'cache:clear', cmd: 'php bin/console cache:clear' });
      q.push({ label: 'debug:router', cmd: 'php bin/console debug:router' });
      q.push({ label: 'doctrine:migrations:migrate', cmd: 'php bin/console doctrine:migrations:migrate --no-interaction' });
    }
    const npm = (cmdState.cmds || []).filter(c => c.group === 'npm');
    for (const name of ['dev', 'build', 'lint', 'test', 'format', 'typecheck']) {
      const c = npm.find(x => x.name === name);
      if (c) q.push({ label: c.command, cmd: c.command, terminal: name === 'dev' });
    }
    return q;
  }

  function renderCommands() {
    els.panel.innerHTML = `<div class="mp-cmd-layout">
      <div style="display:flex; flex-direction:column; gap:20px; min-width:0">
        <section class="card" data-id="mp-quick-card" hidden>
          <div class="card-head">${ICON.bolt}<h2>Quick actions</h2></div>
          <div class="card-body"><div class="mp-quick" data-id="mp-quick"></div></div>
        </section>
        <section class="card">
          <div class="card-head">${ICON.cmd}<h2>Commands</h2><span class="hint" data-id="mp-cmd-count"></span>
            <button class="sm" data-id="mp-cmd-reload" title="list again (after installing packages, say)">Reload</button></div>
          <div class="card-body">
            <div class="mp-search" style="margin-bottom:12px">${ICON.search}<input type="text" data-id="mp-cmd-q" placeholder="Search commands…" value="${esc(cmdState.q)}"></div>
            <div data-id="mp-cmd-warn"></div>
            <div class="mp-cmds" data-id="mp-cmds"></div>
          </div>
        </section>
      </div>
      <section class="card mp-run" data-id="mp-run"></section>
    </div>`;
    const qInput = els.panel.querySelector('[data-id="mp-cmd-q"]');
    qInput.oninput = () => { cmdState.q = qInput.value; paintCommandList(); };
    els.panel.querySelector('[data-id="mp-cmd-reload"]').onclick = () => loadCommands(true);
    renderRunPanel();
    if (cmdState.cmds) { paintCommandList(); paintQuick(); } else loadCommands(false);
  }

  async function loadCommands(force) {
    const g = gen;
    const list = els.panel.querySelector('[data-id="mp-cmds"]');
    if (list) list.innerHTML = `<div class="mp-loading"><div class="spin"></div>${isLaravel() || info.detect.icon === 'symfony' ? 'Asking the framework for its commands…' : 'Reading scripts…'}</div>`;
    try {
      const d = await call('/api/project/commands', { site: cur });
      if (!live(g)) return;
      cmdState.cmds = d.commands || [];
      cmdState.error = d.error || '';
    } catch (e) {
      if (!live(g)) return;
      cmdState.cmds = [];
      cmdState.error = e.message;
    }
    if (tab !== 'commands') return;
    paintCommandList();
    paintQuick();
    if (force) say('Commands reloaded');
  }

  function paintQuick() {
    const card = els.panel.querySelector('[data-id="mp-quick-card"]');
    if (!card) return;
    const q = quickActions();
    card.hidden = !q.length;
    const box = card.querySelector('[data-id="mp-quick"]');
    box.innerHTML = q.map((a, i) => `<button class="sm${a.danger ? ' danger' : ''}" data-q="${i}" title="${esc(a.cmd)}${a.terminal ? ' — opens in the terminal' : ''}">${a.terminal ? ICON.term.replace('<svg', '<svg width="12" height="12" style="vertical-align:-1px;margin-right:5px"') : ''}${esc(a.label)}</button>`).join('');
    box.querySelectorAll('[data-q]').forEach(b => b.onclick = () => {
      const a = q[+b.dataset.q];
      if (a.terminal) { openTerminal(a.cmd); return; }
      selectCommand({ command: a.cmd, name: a.label, group: 'quick' }, true);
    });
  }

  function paintCommandList() {
    const list = els.panel.querySelector('[data-id="mp-cmds"]');
    if (!list || !cmdState.cmds) return;
    const warn = els.panel.querySelector('[data-id="mp-cmd-warn"]');
    warn.innerHTML = cmdState.error
      ? `<div class="mp-warn" style="margin-bottom:12px">Some commands couldn't be listed — <span class="mono">${esc(cmdState.error.split('\n')[0])}</span></div>` : '';
    const favs = favsOf(cur);
    const q = cmdState.q.trim().toLowerCase();
    const match = c => !q || c.name.toLowerCase().includes(q) || (c.description || '').toLowerCase().includes(q) || c.command.toLowerCase().includes(q);
    const all = cmdState.cmds.filter(match);
    els.panel.querySelector('[data-id="mp-cmd-count"]').textContent = cmdState.cmds.length ? (q ? all.length + ' of ' : '') + cmdState.cmds.length : '';
    if (!cmdState.cmds.length) {
      list.innerHTML = `<div class="mp-empty"><b>No commands found</b>Add scripts to package.json or composer.json — or type any command in the run panel.</div>`;
      return;
    }
    if (!all.length) { list.innerHTML = `<div class="mp-empty"><b>Nothing matches “${esc(cmdState.q)}”</b>Try another word.</div>`; return; }
    const row = c => `<div class="mp-cmd${cmdState.sel === c.command ? ' sel' : ''}" data-cmd="${esc(c.command)}" title="${esc(c.command)}">
        <div class="t"><span class="nm">${esc(c.name)}</span>${c.description ? `<span class="ds">${esc(c.description)}</span>` : ''}</div>
        <button class="mp-icon-btn${favs.includes(c.command) ? ' on' : ''}" data-fav="${esc(c.command)}" title="${favs.includes(c.command) ? 'unstar' : 'star'}">${ICON.star}</button>
      </div>`;
    let html = '';
    const favCmds = all.filter(c => favs.includes(c.command));
    if (favCmds.length) html += `<div class="mp-grp">★ Favorites</div>` + favCmds.map(row).join('');
    for (const g of ['artisan', 'console', 'npm', 'composer']) {
      const cs = all.filter(c => c.group === g);
      if (!cs.length) continue;
      html += `<div class="mp-grp">${GROUPS[g]}<span class="c">${cs.length}</span></div>` + cs.map(row).join('');
    }
    list.innerHTML = html;
    const byCmd = new Map(cmdState.cmds.map(c => [c.command, c]));
    list.querySelectorAll('[data-cmd]').forEach(el => el.onclick = e => {
      if (e.target.closest('[data-fav]')) return;
      selectCommand(byCmd.get(el.dataset.cmd), false);
    });
    list.querySelectorAll('[data-fav]').forEach(b => b.onclick = e => {
      e.stopPropagation();
      const c = b.dataset.fav;
      let f = favsOf(cur);
      f = f.includes(c) ? f.filter(x => x !== c) : f.concat(c);
      prefSet('fav:' + cur, f);
      paintCommandList();
    });
  }

  function selectCommand(c, runNow) {
    if (cmdState.job && cmdState.job.running) {
      say('A command is still running — stop it or wait for it first');
      return;
    }
    cmdState.sel = c.command;
    cmdState.job = null;
    cmdState.draft = c.command;
    cmdState.title = c.name;
    cmdState.desc = c.description || '';
    renderRunPanel();
    paintCommandList();
    const run = els.panel.querySelector('[data-id="mp-run"]');
    if (run && run.getBoundingClientRect().top > window.innerHeight - 120) run.scrollIntoView({ behavior: 'smooth', block: 'start' });
    if (runNow) runCommand();
    else { const inp = els.panel.querySelector('[data-id="mp-cmdline"]'); if (inp) inp.focus(); }
  }

  function renderRunPanel() {
    const box = els.panel.querySelector('[data-id="mp-run"]');
    if (!box) return;
    const j = cmdState.job;
    let badge = '';
    if (j && j.running) badge = '<span class="badge mp-exit run">running…</span>';
    else if (j && j.error) badge = `<span class="badge mp-exit bad" title="${esc(j.error)}">${/^stopped$|cancel/i.test(j.error) ? 'stopped' : 'failed'}</span>`;
    else if (j && j.exitCode != null) badge = `<span class="badge mp-exit ${j.exitCode === 0 ? 'ok' : 'bad'}">exit ${j.exitCode}</span>` +
      (j.ms != null ? `<span class="mp-sub">${j.ms < 1000 ? j.ms + ' ms' : (j.ms / 1000).toFixed(1) + ' s'}</span>` : '');
    const draft = cmdState.draft != null ? cmdState.draft : '';
    box.innerHTML = `
      <div class="card-head">${ICON.play}<h2>${esc(cmdState.title || 'Run a command')}</h2><span class="hint">${badge}</span></div>
      <div class="card-body">
        ${cmdState.desc ? `<div class="mp-sub" style="margin-top:-4px">${esc(cmdState.desc)}</div>` : ''}
        <div class="mp-cmdline"><span class="pr">$</span>
          <input type="text" data-id="mp-cmdline" spellcheck="false" autocomplete="off" placeholder="${isLaravel() ? 'php artisan …' : 'any command — runs in ' + esc(info.path)}" value="${esc(draft)}"></div>
        <div class="mp-run-bar">
          ${j && j.running
            ? '<button class="danger sm" data-id="mp-run-stop">Stop</button>'
            : '<button class="primary sm mp-btn-ic" data-id="mp-run-go">' + ICON.play + 'Run</button>'}
          <button class="sm mp-btn-ic" data-id="mp-run-term" title="run it in a terminal tab instead (for interactive commands)">${ICON.term}Run in terminal</button>
          <span class="spacer"></span>
          <button class="mp-icon-btn" data-id="mp-run-copy" title="copy the command">${ICON.copy}</button>
        </div>
        <pre class="mp-term" data-id="mp-run-out"></pre>
      </div>`;
    const inp = box.querySelector('[data-id="mp-cmdline"]');
    inp.oninput = () => { cmdState.draft = inp.value; };
    inp.onkeydown = e => { if (e.key === 'Enter' && !(cmdState.job && cmdState.job.running)) { e.preventDefault(); runCommand(); } };
    const go = box.querySelector('[data-id="mp-run-go"]');
    if (go) go.onclick = () => runCommand();
    const stop = box.querySelector('[data-id="mp-run-stop"]');
    if (stop) stop.onclick = () => call('/api/project/cancel', { id: cmdState.job.id }).catch(e => say('Error: ' + e.message));
    box.querySelector('[data-id="mp-run-term"]').onclick = () => { const c = inp.value.trim(); openTerminal(c || undefined); };
    box.querySelector('[data-id="mp-run-copy"]').onclick = async () => { const c = inp.value.trim(); if (c) say(await copyText(c) ? 'Command copied' : 'Could not copy'); };
    const out = box.querySelector('[data-id="mp-run-out"]');
    if (j) paintTerm(out, j.st || {}, j.running ? 'Waiting for output…' : (j.error ? j.error : '(no output)'));
    else out.innerHTML = `<span class="dim">${cmdState.draft ? 'Press Run (or Enter) to run it here — output streams in as it happens.' : 'Pick a command on the left, or type one above.'}\nInteractive commands (tinker, a REPL, prompts) belong in the terminal.</span>`;
  }

  async function runCommand() {
    const command = (cmdState.draft || '').trim();
    if (!command) { say('Type a command first'); return; }
    if (INTERACTIVE_RE.test(command)) {
      const where = await mpModal({ title: 'Run in the terminal?', message: `<span class="mono">${esc(command)}</span> looks interactive, or keeps running until stopped — it works best in a terminal tab.` }, (body, close) => {
        body.innerHTML = '<div class="modal-actions"><button data-w="">Cancel</button><button data-w="here">Run here anyway</button><button class="primary" data-w="term" autofocus>Open in terminal</button></div>';
        body.querySelectorAll('[data-w]').forEach(b => b.onclick = () => close(b.dataset.w || null));
      });
      if (!where) return;
      if (where === 'term') { openTerminal(command); return; }
    }
    if (DANGER_RE.test(command) || /\bkey:generate\b/.test(command)) {
      const ok = await confirmBox({
        title: 'Run ' + esc(command.split(/\s+/).slice(-2).join(' ')) + '?',
        message: /key:generate/.test(command)
          ? `This replaces <span class="mono">APP_KEY</span> in .env — existing encrypted data and sessions become unreadable.`
          : `<span class="mono">${esc(command)}</span> drops data in <b>${esc(cur)}</b>'s database. This can't be undone.`,
        confirmText: 'Run it', danger: true,
      });
      if (!ok) return;
    }
    const g = gen;
    let id;
    try {
      ({ job: id } = await call('/api/project/run', { site: cur, command }));
    } catch (e) { say('Error: ' + e.message); return; }
    const j = { id, running: true, st: { log: '' } };
    cmdState.job = j;
    cmdState.sel = cmdState.sel || command;
    if (tab === 'commands' && live(g)) renderRunPanel();
    const st = await follow(id, s => {
      j.st = s;
      if (tab !== 'commands' || !live(g)) return;
      const out = els.panel.querySelector('[data-id="mp-run-out"]');
      if (out && !s.done) paintTerm(out, s, 'Waiting for output…');
    });
    j.running = false;
    j.st = st || j.st;
    if (st && st.error) j.error = st.error;
    else if (st && st.result) { j.exitCode = st.result.exitCode; j.ms = st.result.ms; }
    if (tab === 'commands' && live(g)) renderRunPanel();
    if (live(g) && j.exitCode === 0 && /\b(migrate|db:seed|storage:link|key:generate)\b/.test(command)) say('Done');
  }

  /* ════════════════════════════════════════════════════════════════
     WORKERS
     ════════════════════════════════════════════════════════════════ */
  const STATE_LABEL = { running: 'running', stopped: 'stopped', paused: 'paused', restarting: 'restarting', 'crash-looping': 'crash-looping' };
  const KIND_LABEL = { queue: 'queue', scheduler: 'scheduler', custom: 'custom' };

  function renderWorkers() {
    els.panel.innerHTML = `
      <section class="card">
        <div class="card-head">${ICON.workers}<h2>Workers</h2>
          <span class="hint">Supervised background processes — restarted if they crash</span>
          <button class="primary sm mp-btn-ic" data-id="mp-wk-add">${ICON.plus}Add worker</button></div>
        <div class="card-body"><div class="mp-workers" data-id="mp-wk-list"><div class="mp-loading"><div class="spin"></div>Loading workers…</div></div></div>
      </section>
      <div class="mp-note" data-id="mp-wk-note" hidden></div>`;
    els.panel.querySelector('[data-id="mp-wk-add"]').onclick = () => addWorker();
    if (wkState.list) paintWorkers();
    loadWorkers(false);
    pollTimer = setInterval(() => {
      if (tab === 'workers' && visible()) loadWorkers(true);
    }, 3000);
  }

  async function loadWorkers(quiet) {
    const g = gen;
    try {
      const d = await call('/api/project/workers', { site: cur });
      if (!live(g)) return;
      wkState.list = d.workers || [];
    } catch (e) {
      if (!live(g) || quiet) return;
      wkState.list = [];
      say('Error: ' + e.message);
    }
    updateWorkerCount(wkState.list.length);
    if (tab === 'workers') paintWorkers();
  }

  function paintWorkers() {
    const box = els.panel.querySelector('[data-id="mp-wk-list"]');
    if (!box) return;
    const list = wkState.list || [];
    const note = els.panel.querySelector('[data-id="mp-wk-note"]');
    if (note) {
      note.hidden = !list.length;
      note.innerHTML = 'Workers with <b>Auto-start</b> come up with the stack and are restarted when they crash. Long-running PHP workers don\'t see code or .env changes — restart them after you deploy changes.';
    }
    if (!list.length) {
      box.innerHTML = `<div class="mp-empty"><b>No workers yet</b>${isLaravel()
        ? 'Add a queue worker to process jobs, or the Scheduler to run your scheduled tasks — no cron needed.'
        : 'Run queue consumers, schedulers or any long-running script alongside the site.'}</div>`;
      return;
    }
    // Keep a worker's row (and a focused switch) stable across polls by
    // re-rendering only when something visible changed.
    const sig = JSON.stringify(list.map(w => [w.id, w.name, w.kind, w.command, w.autoStart, w.status.state, w.status.pid, w.status.restarts, w.status.lastExit, w.status.startedAt]));
    const upOnly = box.dataset.sig === sig;
    box.dataset.sig = sig;
    if (upOnly) {
      box.querySelectorAll('[data-uptime]').forEach(el => { el.textContent = fmtDur(Date.now() - Date.parse(el.dataset.uptime)); });
      return;
    }
    box.innerHTML = list.map(w => {
      const s = w.status || {};
      const state = s.state || (s.running ? 'running' : 'stopped');
      const running = state === 'running' || state === 'restarting';
      const meta = [];
      if (s.running && s.pid) meta.push(`pid <b>${s.pid}</b>`);
      if (s.running && s.startedAt) meta.push(`up <b data-uptime="${esc(s.startedAt)}">${fmtDur(Date.now() - Date.parse(s.startedAt))}</b>`);
      if (s.restarts) meta.push(`<span class="${s.restarts > 2 ? 'bad' : ''}">restarts <b>${s.restarts}</b></span>`);
      if (!s.running && s.lastExit) meta.push(`last exit: ${esc(s.lastExit)}`);
      return `<div class="mp-worker" data-wid="${esc(w.id)}">
        <div style="min-width:0">
          <div class="w-name">${esc(w.name)} <span class="mp-state ${state}">${STATE_LABEL[state] || esc(state)}</span>${w.kind && w.kind !== 'custom' ? `<span class="badge">${esc(KIND_LABEL[w.kind] || w.kind)}</span>` : ''}</div>
          <div class="w-cmd" title="${esc(w.command)}">${esc(w.command)}</div>
          ${meta.length ? `<div class="w-meta">${meta.join('')}</div>` : ''}
          ${state === 'crash-looping' ? '<div class="mp-warn" style="margin-top:10px">It crashed 5 times in 2 minutes, so Mullion stopped restarting it. Check the logs, fix the cause, then Start it again.</div>' : ''}
        </div>
        <div class="w-actions">
          <label class="mp-auto" title="start with the stack and restart after crashes"><span class="switch"><input type="checkbox" data-wact="auto" ${w.autoStart ? 'checked' : ''}><i></i></span>Auto-start</label>
          ${running
            ? '<button class="sm" data-wact="stop">Stop</button><button class="sm" data-wact="restart">Restart</button>'
            : '<button class="sm primary" data-wact="start">Start</button>'}
          <button class="sm" data-wact="logs">Logs</button>
          <button class="sm" data-wact="edit">Edit</button>
          <button class="sm danger" data-wact="remove">Remove</button>
        </div>
      </div>`;
    }).join('');
    const byId = new Map(list.map(w => [w.id, w]));
    box.querySelectorAll('[data-wact]').forEach(el => {
      const w = byId.get(el.closest('[data-wid]').dataset.wid);
      const act = el.dataset.wact;
      if (act === 'auto') el.onchange = () => workerCall('update', { ...workerFields(w), autoStart: el.checked }, el.checked ? 'Auto-start on' : 'Auto-start off');
      else el.onclick = () => workerAction(act, w, el);
    });
  }

  const workerFields = w => ({ id: w.id, name: w.name, kind: w.kind, command: w.command, autoStart: !!w.autoStart });

  async function workerCall(verb, body, done, btn) {
    const g = gen;
    if (btn) btn.disabled = true;
    try {
      await call('/api/project/workers/' + verb, { site: cur, ...body });
      if (done && live(g)) say(done);
    } catch (e) {
      if (live(g)) say('Error: ' + e.message);
    } finally {
      if (btn) btn.disabled = false;
      if (live(g)) { const box = els.panel.querySelector('[data-id="mp-wk-list"]'); if (box) delete box.dataset.sig; loadWorkers(true); }
    }
  }

  async function workerAction(act, w, btn) {
    if (act === 'start') return workerCall('start', { id: w.id }, w.name + ' started', btn);
    if (act === 'stop') return workerCall('stop', { id: w.id }, w.name + ' stopped — it stays down until you start it', btn);
    if (act === 'restart') return workerCall('restart', { id: w.id }, w.name + ' restarted', btn);
    if (act === 'logs') return openLogModal(w);
    if (act === 'edit') {
      const f = await workerForm({ title: 'Edit worker', worker: w, confirmText: 'Save' });
      if (f) workerCall('update', { ...f, id: w.id }, 'Saved' + (f.command !== w.command && w.status && w.status.running ? ' — restarted with the new command' : ''));
      return;
    }
    if (act === 'remove') {
      const ok = await confirmBox({ title: 'Remove worker?', message: `Stop and remove <b>${esc(w.name)}</b> (<span class="mono">${esc(w.command)}</span>)? Its log is deleted too.`, confirmText: 'Remove', danger: true });
      if (ok) workerCall('remove', { id: w.id }, 'Removed ' + w.name, btn);
    }
  }

  function templateHint(t) {
    const c = t.command || '';
    if (t.kind === 'scheduler' && /schedule/.test(c)) return 'Runs your scheduled tasks every minute — replaces the cron entry, nothing to set up.';
    if (/horizon/.test(c)) return 'Horizon supervises your Redis queues with its own dashboard.';
    if (/reverb/.test(c)) return 'Laravel\'s WebSocket server for broadcasting.';
    if (/queue:work/.test(c)) return 'Processes queued jobs (mail, notifications, …) as they arrive.';
    if (/messenger:consume/.test(c)) return 'Consumes messages from the async transport.';
    return 'A long-running script from package.json.';
  }

  // mpModal: a panel-styled dialog with custom content (the global
  // modal() only supports a single field). build(card, close) wires it.
  function mpModal(opts, build) {
    return new Promise(resolve => {
      const back = document.createElement('div');
      back.className = 'modal-backdrop mp-modal' + (opts.wide ? ' wide' : '');
      const card = document.createElement('div');
      card.className = 'modal-card';
      card.setAttribute('role', 'dialog');
      card.setAttribute('aria-modal', 'true');
      card.innerHTML = `<h3 class="modal-title">${opts.title}</h3>${opts.message ? `<p class="modal-message">${opts.message}</p>` : ''}<div class="mp-mbody"></div>`;
      back.appendChild(card);
      document.body.appendChild(back);
      const prev = document.activeElement;
      let closed = false;
      const close = v => {
        if (closed) return;
        closed = true;
        document.removeEventListener('keydown', onKey, true);
        back.classList.remove('show');
        setTimeout(() => back.remove(), 180);
        if (opts.onClose) opts.onClose();
        if (prev && prev.focus) prev.focus();
        resolve(v);
      };
      const onKey = e => { if (e.key === 'Escape') { e.preventDefault(); close(null); } };
      document.addEventListener('keydown', onKey, true);
      back.onclick = e => { if (e.target === back) close(null); };
      build(card.querySelector('.mp-mbody'), close, card);
      requestAnimationFrame(() => {
        back.classList.add('show');
        const f = card.querySelector('[autofocus]') || card.querySelector('input, select, button');
        if (f) f.focus();
      });
    });
  }

  function workerForm({ title, worker, confirmText, hint }) {
    const w = worker || {};
    return mpModal({ title, message: hint || '' }, (body, close) => {
      body.innerHTML = `<div class="mp-form">
        <div class="row2">
          <label class="f">Name<input type="text" data-id="mpw-name" value="${esc(w.name || '')}" placeholder="Queue worker"></label>
          <label class="f">Kind<select data-id="mpw-kind">${['queue', 'scheduler', 'custom'].map(k => `<option value="${k}" ${(w.kind || 'custom') === k ? 'selected' : ''}>${k}</option>`).join('')}</select></label>
        </div>
        <label class="f">Command<input type="text" data-id="mpw-cmd" class="mono" value="${esc(w.command || '')}" placeholder="${isLaravel() ? 'php artisan queue:work' : 'npm run worker'}" spellcheck="false" autofocus></label>
        <label class="modal-check"><input type="checkbox" data-id="mpw-auto" ${w.autoStart || !w.command ? 'checked' : ''}>
          <span><span class="ct">Auto-start</span><span class="ch">Start with the stack, and restart it if it crashes.</span></span></label>
        <div class="mp-sub">Runs in <span class="mono">${esc(info.path)}</span> with the site's ${isNode() ? 'Node' : 'PHP'} on PATH.</div>
        <div class="mp-err" data-id="mpw-err"></div>
      </div>
      <div class="modal-actions"><button data-id="mpw-cancel">Cancel</button><button class="primary" data-id="mpw-ok">${confirmText || 'Add worker'}</button></div>`;
      const $f = id => body.querySelector('[data-id="' + id.slice(1) + '"]');
      const submit = () => {
        const f = { name: $f('#mpw-name').value.trim(), kind: $f('#mpw-kind').value, command: $f('#mpw-cmd').value.trim(), autoStart: $f('#mpw-auto').checked };
        if (!f.command) { $f('#mpw-err').textContent = 'A worker needs a command.'; $f('#mpw-cmd').focus(); return; }
        close(f);
      };
      $f('#mpw-cancel').onclick = () => close(null);
      $f('#mpw-ok').onclick = submit;
      body.querySelectorAll('input[type=text]').forEach(i => i.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); submit(); } });
    });
  }

  async function addWorker() {
    const g = gen;
    if (!wkState.templates) {
      try { wkState.templates = (await call('/api/project/workers/templates', { site: cur })).templates || []; } catch { wkState.templates = []; }
      if (!live(g)) return;
    }
    const existing = new Set((wkState.list || []).map(w => w.command));
    const tpls = wkState.templates.filter(t => !existing.has(t.command));
    let chosen = { kind: 'custom', autoStart: true };
    if (tpls.length) {
      const pick = await mpModal({ title: 'Add a worker', message: 'Start from a template for this project, or run any command.', wide: tpls.length > 2 }, (body, close) => {
        body.innerHTML = `<div class="mp-tpls">${tpls.map((t, i) => `<button class="mp-tpl" data-t="${i}"><b>${esc(t.name)}</b><code>${esc(t.command)}</code><span>${esc(templateHint(t))}</span></button>`).join('')}
          <button class="mp-tpl custom" data-t="custom"><b>Custom command</b><code>any long-running command</code><span>A daemon, a watcher, a second queue… anything that should keep running.</span></button></div>
          <div class="modal-actions"><button data-id="mpt-cancel">Cancel</button></div>`;
        body.querySelectorAll('[data-t]').forEach(b => b.onclick = () => close(b.dataset.t === 'custom' ? { kind: 'custom', autoStart: true } : tpls[+b.dataset.t]));
        body.querySelector('[data-id="mpt-cancel"]').onclick = () => close(null);
      });
      if (!pick) return;
      chosen = pick;
    }
    const f = await workerForm({ title: chosen.name ? 'Add ' + esc(chosen.name) : 'Add a worker', worker: chosen, hint: chosen.command ? esc(templateHint(chosen)) : '' });
    if (!f || !live(g)) return;
    await workerCall('add', f, f.autoStart ? 'Worker added and started' : 'Worker added — start it when you need it');
  }

  let logModal = null;
  function closeLogModal() { if (logModal) { logModal(); logModal = null; } }
  function openLogModal(w) {
    closeLogModal();
    const g = gen;
    let timer = 0, auto = true;
    mpModal({ title: 'Logs — ' + esc(w.name), wide: true, onClose: () => { clearInterval(timer); logModal = null; } }, (body, close) => {
      logModal = () => close(null);
      body.innerHTML = `<div class="mp-sub mono" style="margin-top:4px">${esc(w.command)}</div>
        <div class="mp-log-bar">
          <select data-id="mpl-lines" style="font-size:12px">${[100, 200, 500, 1000].map(n => `<option value="${n}" ${n === 200 ? 'selected' : ''}>last ${n} lines</option>`).join('')}</select>
          <label class="toggle"><span class="switch"><input type="checkbox" data-id="mpl-follow" checked><i></i></span>Auto-refresh</label>
          <span class="spacer"></span><span class="mp-sub" data-id="mpl-at"></span>
          <button class="sm" data-id="mpl-copy">Copy</button>
        </div>
        <pre class="mp-term" data-id="mpl-out"><span class="dim">Loading…</span></pre>
        <div class="modal-actions"><button class="primary" data-id="mpl-close">Close</button></div>`;
      const out = body.querySelector('[data-id="mpl-out"]');
      let text = '';
      const load = async () => {
        try {
          const d = await call('/api/project/workers/log', { site: cur, id: w.id, lines: +body.querySelector('[data-id="mpl-lines"]').value });
          if (!live(g)) { close(null); return; }
          text = d.log || '';
          paintTerm(out, { log: text }, 'No output yet.');
          body.querySelector('[data-id="mpl-at"]').textContent = 'updated ' + new Date().toLocaleTimeString();
        } catch (e) { out.textContent = e.message; }
      };
      body.querySelector('[data-id="mpl-lines"]').onchange = () => { out.scrollTop = out.scrollHeight; load(); };
      body.querySelector('[data-id="mpl-follow"]').onchange = e => { auto = e.target.checked; };
      body.querySelector('[data-id="mpl-copy"]').onclick = async () => say(await copyText(text) ? 'Log copied' : 'Could not copy');
      body.querySelector('[data-id="mpl-close"]').onclick = () => close(null);
      load().then(() => { out.scrollTop = out.scrollHeight; });
      timer = setInterval(() => { if (auto && document.visibilityState === 'visible' && !destroyed) load(); }, 2000);
    });
  }

  /* ════════════════════════════════════════════════════════════════
     PACKAGES
     ════════════════════════════════════════════════════════════════ */
  function renderPackages() {
    const sources = [];
    if (info.hasComposer) sources.push(['composer', 'Packagist']);
    if (info.hasPackage) sources.push(['npm', 'npm']);
    if (!pkgState.source || !sources.some(s => s[0] === pkgState.source)) pkgState.source = sources.length ? sources[0][0] : 'npm';
    els.panel.innerHTML = `
      <section class="card">
        <div class="card-head">${ICON.search}<h2>Add a package</h2>
          ${sources.length > 1 ? `<div class="mp-seg" style="margin-left:auto">${sources.map(([k, l]) => `<button data-src="${k}" class="${pkgState.source === k ? 'active' : ''}">${l}</button>`).join('')}</div>` : `<span class="hint">from ${sources[0] ? sources[0][1] : 'npm'}</span>`}
        </div>
        <div class="card-body">
          <div class="mp-search">${ICON.search}<input type="text" data-id="mp-pkg-q" placeholder="Search ${pkgState.source === 'composer' ? 'Packagist — e.g. spatie/laravel-permission' : 'npm — e.g. axios'}" value="${esc(pkgState.q)}"></div>
          <div class="mp-pkg-results" data-id="mp-pkg-results"></div>
        </div>
      </section>
      <section class="card" data-id="mp-pkg-job" hidden>
        <div class="card-head">${ICON.term}<h2 data-id="mp-pkg-job-title">Output</h2><span class="hint" data-id="mp-pkg-job-badge"></span>
          <button class="danger sm" data-id="mp-pkg-stop" hidden>Stop</button></div>
        <div class="card-body"><pre class="mp-term short" data-id="mp-pkg-out"></pre></div>
      </section>
      <div data-id="mp-pkg-installed"><section class="card"><div class="mp-loading"><div class="spin"></div>Reading installed packages…</div></section></div>`;
    els.panel.querySelectorAll('[data-src]').forEach(b => b.onclick = () => {
      pkgState.source = b.dataset.src;
      pkgState.results = null;
      renderPackages();
      if (pkgState.q.trim()) searchPackages();
    });
    const q = els.panel.querySelector('[data-id="mp-pkg-q"]');
    let t = 0;
    q.oninput = () => { pkgState.q = q.value; clearTimeout(t); t = setTimeout(searchPackages, 380); };
    q.onkeydown = e => { if (e.key === 'Enter') { clearTimeout(t); searchPackages(); } };
    paintResults();
    paintPkgJob();
    if (pkgState.data) paintInstalled();
    loadInstalled();
  }

  async function loadInstalled() {
    const g = gen;
    try {
      const d = await call('/api/project/packages', { site: cur });
      if (!live(g)) return;
      pkgState.data = d;
    } catch (e) {
      if (!live(g)) return;
      pkgState.data = { composer: [], npm: [], error: e.message };
    }
    if (tab === 'packages') paintInstalled();
  }

  let searchSeq = 0;
  async function searchPackages() {
    const g = gen, seq = ++searchSeq;
    const q = pkgState.q.trim();
    if (!q) { pkgState.results = null; paintResults(); return; }
    pkgState.searching = true;
    paintResults();
    try {
      const r = await call('/api/project/packages/search', { manager: pkgState.source, q });
      if (!live(g) || seq !== searchSeq) return;
      pkgState.results = r || [];
      pkgState.showAll = false;
      pkgState.searchError = '';
    } catch (e) {
      if (!live(g) || seq !== searchSeq) return;
      pkgState.results = [];
      pkgState.searchError = e.message;
    }
    pkgState.searching = false;
    if (tab === 'packages') paintResults();
  }

  // cleanRepo turns npm's "git+https://….git" into a browsable URL (and
  // drops anything that isn't http/https).
  function cleanRepo(u) {
    if (!u) return '';
    u = String(u).replace(/^git\+/, '').replace(/^git:\/\//, 'https://').replace(/\.git$/, '');
    return /^https?:\/\//.test(u) ? u : '';
  }

  function installedNames() {
    const d = pkgState.data || {};
    return new Set([...(d.composer || []), ...(d.npm || [])].map(p => p.name));
  }

  function paintResults() {
    const box = els.panel.querySelector('[data-id="mp-pkg-results"]');
    if (!box) return;
    if (pkgState.searching) { box.innerHTML = '<div class="mp-loading"><div class="spin"></div>Searching…</div>'; return; }
    if (pkgState.searchError) { box.innerHTML = `<div class="mp-warn" style="margin-top:4px">Search failed — ${esc(pkgState.searchError)}</div>`; return; }
    if (!pkgState.results) { box.innerHTML = ''; return; }
    if (!pkgState.results.length) { box.innerHTML = `<div class="mp-empty"><b>No packages match “${esc(pkgState.q)}”</b>Check the spelling, or search by vendor.</div>`; return; }
    const have = installedNames();
    const shown = pkgState.showAll ? pkgState.results : pkgState.results.slice(0, 8);
    box.innerHTML = shown.map((p, i) => {
      const meta = [];
      if (p.downloads) meta.push(`${fmtNum(p.downloads)} ${pkgState.source === 'npm' ? 'weekly downloads' : 'downloads'}`);
      if (p.stars) meta.push(`★ ${fmtNum(p.stars)}`);
      if (p.updated) meta.push(`updated ${fmtAgo(p.updated)}`);
      if (p.url && /^https?:\/\//.test(p.url)) meta.push(`<a href="${esc(p.url)}" target="_blank">${pkgState.source === 'npm' ? 'npm' : 'Packagist'}</a>`);
      const repo = cleanRepo(p.repository);
      if (repo) meta.push(`<a href="${esc(repo)}" target="_blank">repository</a>`);
      return `<div class="mp-pkg">
        <div style="min-width:0">
          <div class="p-name">${esc(p.name)}${p.version ? `<span class="badge mono">${esc(p.version)}</span>` : ''}${have.has(p.name) ? '<span class="badge on">installed</span>' : ''}</div>
          ${p.description ? `<div class="p-desc">${esc(p.description)}</div>` : ''}
          ${meta.length ? `<div class="p-meta">${meta.join('<span>·</span>')}</div>` : ''}
        </div>
        <button class="sm ${have.has(p.name) ? '' : 'primary'}" data-inst="${i}" ${pkgState.job && pkgState.job.running ? 'disabled' : ''}>${have.has(p.name) ? 'Change…' : 'Install'}</button>
      </div>`;
    }).join('') + (shown.length < pkgState.results.length
      ? `<div style="text-align:center; padding-top:12px"><button class="sm" data-id="mp-pkg-more">Show all ${pkgState.results.length} results</button></div>` : '');
    const more = box.querySelector('[data-id="mp-pkg-more"]');
    if (more) more.onclick = () => { pkgState.showAll = true; paintResults(); };
    box.querySelectorAll('[data-inst]').forEach(b => b.onclick = () => installPackage(pkgState.results[+b.dataset.inst]));
  }

  async function installPackage(p) {
    const mgr = pkgState.source;
    const tool = mgr === 'composer' ? 'composer require' : (pkgState.data && pkgState.data.packageManager || 'npm') + ' add';
    const r = await modalBox({
      title: 'Install ' + esc(p.name),
      message: `Runs <span class="mono">${esc(tool)} ${esc(p.name)}</span> in <span class="mono">${esc(cur)}</span>.`,
      input: { label: 'Version (optional)', placeholder: p.version ? (mgr === 'composer' ? '^' : '^') + p.version.replace(/^v/, '') + ' — blank for latest' : 'blank for latest', value: '' },
      checkboxes: [{ id: 'dev', label: 'Development dependency', hint: mgr === 'composer' ? 'adds it to require-dev' : 'adds it to devDependencies', checked: false }],
      confirmText: 'Install',
    });
    if (!r) return;
    runPackageJob('install', { manager: mgr, name: p.name, version: (r.input || '').trim(), dev: !!(r.checks && r.checks.dev) }, 'Installing ' + p.name);
  }

  async function removePackage(mgr, p) {
    const ok = await confirmBox({
      title: 'Remove ' + esc(p.name) + '?',
      message: `Runs <span class="mono">${mgr === 'composer' ? 'composer remove' : esc((pkgState.data && pkgState.data.packageManager) || 'npm') + ' remove'} ${esc(p.name)}</span> — code that uses it will break.`,
      confirmText: 'Remove', danger: true,
    });
    if (ok) runPackageJob('remove', { manager: mgr, name: p.name }, 'Removing ' + p.name);
  }

  async function runPackageJob(verb, body, title) {
    if (pkgState.job && pkgState.job.running) { say('Another package operation is running'); return; }
    const g = gen;
    let id;
    try { ({ job: id } = await call('/api/project/packages/' + verb, { site: cur, ...body })); }
    catch (e) { say('Error: ' + e.message); return; }
    const j = { id, title, running: true, st: { log: '' } };
    pkgState.job = j;
    if (tab === 'packages' && live(g)) { paintPkgJob(); paintResults(); paintInstalled(); }
    const card = els.panel && els.panel.querySelector('[data-id="mp-pkg-job"]');
    if (card) card.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    const st = await follow(id, s => {
      j.st = s;
      if (tab === 'packages' && live(g) && !s.done) { const out = els.panel.querySelector('[data-id="mp-pkg-out"]'); if (out) paintTerm(out, s, 'Starting…'); }
    });
    j.running = false;
    j.st = st || j.st;
    j.error = st && st.error;
    if (!live(g)) return;
    say(j.error ? title + ' failed' : (verb === 'install' ? 'Installed ' : 'Removed ') + body.name);
    if (tab === 'packages') { paintPkgJob(); paintResults(); }
    await loadInstalled();
    if (tab === 'packages') paintResults();
    cmdState.cmds = null; // scripts/commands may have changed
  }

  function paintPkgJob() {
    const card = els.panel.querySelector('[data-id="mp-pkg-job"]');
    if (!card) return;
    const j = pkgState.job;
    card.hidden = !j;
    if (!j) return;
    card.querySelector('[data-id="mp-pkg-job-title"]').textContent = j.title + (j.running ? '…' : '');
    card.querySelector('[data-id="mp-pkg-job-badge"]').innerHTML = j.running ? '<span class="badge mp-exit run">running…</span>'
      : j.error ? `<span class="badge mp-exit bad" title="${esc(j.error)}">failed</span>` : '<span class="badge mp-exit ok">done</span>';
    const stop = card.querySelector('[data-id="mp-pkg-stop"]');
    stop.hidden = !j.running;
    stop.onclick = () => call('/api/project/cancel', { id: j.id }).catch(e => say('Error: ' + e.message));
    const out = card.querySelector('[data-id="mp-pkg-out"]');
    const st = Object.assign({}, j.st);
    if (!j.running && j.error && !(st.log || '').includes(j.error)) st.log = (st.log || '') + '\n\x1b[31m' + j.error + '\x1b[0m\n';
    paintTerm(out, st, j.running ? 'Starting…' : '(no output)');
  }

  function paintInstalled() {
    const box = els.panel.querySelector('[data-id="mp-pkg-installed"]');
    if (!box || !pkgState.data) return;
    const d = pkgState.data;
    if (d.error) { box.innerHTML = `<section class="card"><div class="card-body"><div class="mp-warn">${esc(d.error)}</div></div></section>`; return; }
    const busyNow = pkgState.job && pkgState.job.running;
    const section = (mgr, title, list, has) => {
      if (!has) return '';
      const rows = list.length ? list.map(p => `<tr data-pkgname="${esc(p.name.toLowerCase())}">
          <td class="nm">${esc(p.name)}${p.dev ? ' <span class="badge">dev</span>' : ''}</td>
          <td class="v" title="constraint">${esc(p.constraint || '')}</td>
          <td class="v" title="installed">${p.installed ? esc(p.installed) : '<span class="badge warnb" title="not installed yet — run ' + (mgr === 'composer' ? 'composer install' : 'npm install') + '">missing</span>'}</td>
          <td class="act"><button class="sm danger" data-rm="${mgr}:${esc(p.name)}" ${busyNow ? 'disabled' : ''}>Remove</button></td></tr>`).join('')
        : '<tr><td colspan="4" class="mp-empty">No dependencies yet.</td></tr>';
      return `<section class="card">
        <div class="card-head">${ICON.pkg}<h2>${title}</h2><span class="hint">${list.length} package${list.length === 1 ? '' : 's'} · ${list.filter(p => p.dev).length} dev</span></div>
        <div class="card-body" style="padding-top:12px"><table class="mp-table"><thead><tr><th>Package</th><th>Constraint</th><th>Installed</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>
      </section>`;
    };
    const total = (d.composer || []).length + (d.npm || []).length;
    box.innerHTML = `<div style="display:flex; flex-direction:column; gap:20px">
      ${total > 12 ? `<div class="mp-search">${ICON.search}<input type="text" data-id="mp-pkg-filter" placeholder="Filter installed packages…" value="${esc(pkgState.filter)}"></div>` : ''}
      ${section('composer', 'Composer', d.composer || [], d.hasComposer)}
      ${section('npm', d.packageManager && d.packageManager !== 'npm' ? 'Node packages (' + esc(d.packageManager) + ')' : 'npm', d.npm || [], d.hasPackage)}
    </div>`;
    const flt = box.querySelector('[data-id="mp-pkg-filter"]');
    const applyFilter = () => {
      const v = pkgState.filter.trim().toLowerCase();
      box.querySelectorAll('[data-pkgname]').forEach(tr => tr.classList.toggle('hide', !!v && !tr.dataset.pkgname.includes(v)));
    };
    if (flt) { flt.oninput = () => { pkgState.filter = flt.value; applyFilter(); }; applyFilter(); }
    const all = { composer: d.composer || [], npm: d.npm || [] };
    box.querySelectorAll('[data-rm]').forEach(b => b.onclick = () => {
      const i = b.dataset.rm.indexOf(':');
      const mgr = b.dataset.rm.slice(0, i), name = b.dataset.rm.slice(i + 1);
      removePackage(mgr, all[mgr].find(p => p.name === name) || { name });
    });
  }

  /* ════════════════════════════════════════════════════════════════
     DOMAINS
     ════════════════════════════════════════════════════════════════ */
  // Mirrors app.NormalizeAlias: lowercase, trimmed dots, a pasted
  // ".<host>" suffix removed; DNS labels (a-z0-9-, ≤63) or exactly "*".
  function normalizeAlias(label) {
    const host = info.host.toLowerCase();
    let l = label.trim().toLowerCase().replace(/^\.+|\.+$/g, '');
    if (l.endsWith('.' + host)) l = l.slice(0, -(host.length + 1));
    if (l === '*') return { value: l };
    if (!l) return { error: '' };
    for (const part of l.split('.')) {
      if (!/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(part)) {
        return { error: part === '' ? 'Empty label between dots.' : `“${part}” isn't a valid label — use letters, digits and dashes (not at the start or end).` };
      }
    }
    if (l.length + 1 + host.length > 253) return { error: 'That makes the hostname longer than 253 characters.' };
    return { value: l };
  }

  function renderDomains() {
    const aliases = (info.aliases || []).filter(a => a !== '*');
    const wildcard = (info.aliases || []).includes('*');
    const wd = info.wildcardDNS || {};
    const proto = info.secure ? 'https' : 'http';
    let wildNote = '';
    if (wildcard && !wd.enabled) wildNote = `<div class="mp-warn">Every-subdomain routing is on, but <b>wildcard DNS</b> is off — names other than the ones listed above won't resolve yet. <a data-go-ports>Turn it on in Ports &amp; SSL</a>.</div>`;
    else if (wildcard && wd.note) wildNote = `<div class="mp-warn">${esc(wd.note)} — <a data-go-ports>open Ports &amp; SSL</a>.</div>`;
    else if (!wildcard && !wd.enabled) wildNote = `<div class="mp-sub">Needs Mullion's wildcard DNS (one admin prompt) — <a data-go-ports>set it up in Ports &amp; SSL</a>.</div>`;
    let html = `
      <section class="card">
        <div class="card-head">${ICON.globe}<h2>Domains</h2><span class="hint">Every name here serves this project</span></div>
        <div class="card-body">
          <div class="mp-kv">
            <div class="k">Primary domain<small>from the site name</small></div>
            <div><a class="mono" href="${proto}://${esc(info.host)}" target="_blank">${proto}://${esc(info.host)}</a>
              <label class="toggle" style="margin-left:auto"><span class="switch"><input type="checkbox" data-id="mp-dom-secure" ${info.secure ? 'checked' : ''}><i></i></span>HTTPS</label></div>
            <div class="k">Subdomains<small>aliases of this site</small></div>
            <div style="flex-direction:column; align-items:stretch; gap:12px">
              <div class="mp-chips" data-id="mp-alias-chips">${aliases.length ? aliases.map(a => `<span class="mp-chip"><a href="${proto}://${esc(a)}.${esc(info.host)}" target="_blank">${esc(a)}.${esc(info.host)}</a><button data-rmalias="${esc(a)}" title="remove">${ICON.x}</button></span>`).join('') : '<span class="mp-sub">None yet — add <span class="mono">api</span>, <span class="mono">admin</span>… to serve this project on those names too.</span>'}</div>
              <div>
                <div class="mp-alias-add"><div class="fld" data-id="mp-alias-fld"><input type="text" data-id="mp-alias-in" placeholder="api" spellcheck="false" autocomplete="off"><span class="sfx">.${esc(info.host)}</span></div>
                  <button class="primary sm" data-id="mp-alias-add" disabled>Add</button></div>
                <div class="mp-alias-msg" data-id="mp-alias-msg"></div>
              </div>
            </div>
            <div class="k">Every subdomain<small><span class="mono">*.${esc(info.host)}</span></small></div>
            <div style="flex-direction:column; align-items:stretch; gap:10px">
              <label class="toggle"><span class="switch"><input type="checkbox" data-id="mp-alias-wild" ${wildcard ? 'checked' : ''}><i></i></span>Serve any <span class="mono">&lt;anything&gt;.${esc(info.host)}</span> — for multi-tenant apps</label>
              ${wildNote}
            </div>
          </div>
        </div>
      </section>`;
    if (isNode()) {
      html += `
      <section class="card">
        <div class="card-head">${ICON.bolt}<h2>Dev server port</h2><span class="hint">Mullion proxies ${esc(info.host)} to it</span></div>
        <div class="card-body">
          <div class="row" style="gap:16px">
            <div><div class="mp-port">:${info.devPort || '—'}</div><div class="mp-sub">${info.devRunning ? (info.devRunning === info.devPort ? 'dev server running here' : `dev server is actually listening on :${info.devRunning}`) : 'assigned (the dev server is not running)'}</div></div>
            <span class="spacer"></span>
            <button class="sm" data-id="mp-port-change">Change…</button>
          </div>
          <div data-id="mp-port-hints"></div>
        </div>
      </section>`;
    }
    els.panel.innerHTML = html;
    const p = els.panel;
    p.querySelector('[data-id="mp-dom-secure"]').onchange = e => setSecure(e.target.checked);
    p.querySelectorAll('[data-go-ports]').forEach(a => a.onclick = goPorts);
    p.querySelectorAll('[data-rmalias]').forEach(b => b.onclick = async () => {
      const a = b.dataset.rmalias;
      const ok = await confirmBox({ title: 'Remove subdomain?', message: `<span class="mono">${esc(a)}.${esc(info.host)}</span> stops serving this project.`, confirmText: 'Remove', danger: true });
      if (ok) saveAliases((info.aliases || []).filter(x => x !== a), 'Removed ' + a + '.' + info.host);
    });
    const inp = p.querySelector('[data-id="mp-alias-in"]'), fld = p.querySelector('[data-id="mp-alias-fld"]'), msg = p.querySelector('[data-id="mp-alias-msg"]'), add = p.querySelector('[data-id="mp-alias-add"]');
    const check = () => {
      const r = normalizeAlias(inp.value);
      const dup = r.value && (info.aliases || []).includes(r.value);
      fld.classList.toggle('bad', !!r.error || !!dup);
      msg.classList.toggle('bad', !!r.error || !!dup);
      add.disabled = !r.value || !!dup;
      if (r.error) msg.textContent = r.error;
      else if (dup) msg.innerHTML = `<span class="mono">${esc(r.value)}.${esc(info.host)}</span> is already there.`;
      else if (r.value === '*') msg.innerHTML = 'That\'s every subdomain — same as the switch below.';
      else if (r.value) msg.innerHTML = `Will serve <span class="mono">${proto}://${esc(r.value)}.${esc(info.host)}</span>`;
      else msg.textContent = '';
      return r;
    };
    const submit = () => {
      const r = check();
      if (!r.value || (info.aliases || []).includes(r.value)) return;
      saveAliases((info.aliases || []).concat(r.value), 'Added ' + (r.value === '*' ? '*.' : r.value + '.') + info.host, r.value === '*');
    };
    inp.oninput = check;
    inp.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); submit(); } };
    add.onclick = submit;
    p.querySelector('[data-id="mp-alias-wild"]').onchange = e => {
      const on = e.target.checked;
      const next = (info.aliases || []).filter(x => x !== '*');
      if (on) next.push('*');
      saveAliases(next, on ? 'Serving every subdomain of ' + info.host : 'Stopped serving every subdomain', on);
    };
    if (isNode()) p.querySelector('[data-id="mp-port-change"]').onclick = changeDevPort;
  }

  // The wildcard DNS switch lives on the panel's Ports & SSL page.
  function goPorts() { location.hash = 'ports'; }

  async function saveAliases(aliases, done, wildcardAdded) {
    const g = gen;
    let d;
    try {
      d = await busy('Updating domains… (updating the hosts file may ask for your password)', () => call('/api/project/aliases', { site: cur, aliases }));
    } catch (e) {
      say('Error: ' + e.message);
      if (live(g) && tab === 'domains') renderDomains(); // reset switches
      return;
    }
    if (!live(g)) return;
    info = d;
    say(done);
    renderHero();
    if (tab === 'domains') renderDomains();
    refreshPanel();
    if (wildcardAdded && !(info.wildcardDNS || {}).enabled) {
      const go = await confirmBox({
        title: 'Wildcard DNS is off',
        message: `<span class="mono">*.${esc(info.host)}</span> is routed, but arbitrary names only resolve once Mullion's wildcard DNS is on (the hosts file can't hold wildcards). Explicit subdomains keep working either way.`,
        confirmText: 'Open Ports & SSL', cancelText: 'Later',
      });
      if (go) goPorts();
    }
  }

  async function changeDevPort() {
    const g = gen;
    let suggested = '';
    try { suggested = (await call('/api/project/devport/suggest', { site: cur })).port || ''; } catch {}
    if (!live(g)) return;
    const v = await promptBox({
      title: 'Dev server port',
      message: `Current port <span class="mono">${info.devPort || '—'}</span>. A running dev server restarts on the new one.${suggested ? ` <span class="mono">${suggested}</span> is free.` : ''}`,
      input: { label: 'Port (1024–65535)', value: suggested || info.devPort || '', placeholder: '5173' },
      confirmText: 'Change',
    });
    if (v === null || !live(g)) return;
    const port = parseInt(String(v).trim(), 10);
    if (!(port >= 1024 && port <= 65535)) { say('Pick a port between 1024 and 65535'); return; }
    await setDevPort(port, false);
  }

  async function setDevPort(port, force) {
    const res = await jobResult('Moving the dev server to :' + port + '…', '/api/project/devport', { site: cur, port, force });
    if (!res) return;
    if (res.conflict) {
      const ok = await confirmBox({
        title: 'Port ' + port + ' is in use',
        message: esc(res.message) + '<br><br>Use it anyway? The dev server can\'t start until that process lets go.',
        confirmText: 'Use it anyway', danger: true,
      });
      if (ok) await setDevPort(port, true);
      return;
    }
    say('Dev server port set to ' + port);
    reloadInfo();
    refreshPanel();
    if (res.hints && res.hints.length) {
      alertBox({ title: 'Heads up', message: 'The project pins its own port, which wins over the one Mullion passes:', detail: res.hints.map(esc).join('<br>') });
    }
  }

  // jobResult runs a plain job behind the busy overlay and returns its
  // result (undefined on error, already toasted).
  async function jobResult(label, path, body) {
    try {
      return await busy(label, async () => {
        const { job: id } = await call(path, body);
        for (;;) {
          await sleep(600);
          const st = await call('/api/job', { id });
          if (st.log) { const l = document.getElementById('busy-label'); if (l) l.textContent = st.log; }
          if (st.done) { if (st.error) throw new Error(st.error); return st.result; }
        }
      });
    } catch (e) { say('Error: ' + e.message); return undefined; }
  }

  /* ════════════════════════════════════════════════════════════════
     GIT — status, staging, diffs, commits, branches, history, stashes
     and the network operations, over /api/project/git/* (project.go).
     ════════════════════════════════════════════════════════════════ */
  const GIT_KIND = {
    modified: ['M', 'mod', 'Modified'], added: ['A', 'add', 'Added'], deleted: ['D', 'del', 'Deleted'],
    renamed: ['R', 'ren', 'Renamed'], untracked: ['U', 'unt', 'Untracked'], conflict: ['!', 'con', 'Conflict'],
  };
  const PULL_MODES = { merge: 'Pull', rebase: 'Pull (rebase)', 'ff-only': 'Pull (fast-forward)' };
  const gitFresh = () => ({
    st: null, err: '', view: 'changes', sel: null, diff: null, diffKey: '', log: null, logLimit: 100,
    stashes: null, job: null, consoleOpen: false, draft: '', amend: false, all: false,
    collapsed: {}, sig: '', built: false, head: '',
  });
  let gitSeq = 0, diffSeq = 0, branchPop = null;

  const $g = k => els.panel && els.panel.querySelector(`[data-g="${k}"]`);
  const gitTotal = st => st && st.isRepo ? new Set([].concat(st.staged, st.unstaged, st.untracked, st.conflicted).map(f => f.path)).size : 0;
  const gitRunning = () => !!(gitState.job && gitState.job.running);

  function updateGitCount() {
    const el = root && root.querySelector('[data-id="mp-git-n"]');
    if (!el) return;
    const n = gitTotal(gitState.st);
    el.hidden = !n;
    el.textContent = n > 999 ? '999+' : n;
  }

  function renderGit() {
    gitState.built = false;
    gitState.sig = '';
    els.panel.innerHTML = `<section class="card"><div class="mp-loading"><div class="spin"></div>Reading the repository…</div></section>`;
    loadGitStatus(false);
    pollTimer = setInterval(() => { if (tab === 'git' && visible()) loadGitStatus(true); }, 4000);
  }

  async function loadGitStatus(quiet) {
    const g = gen, seq = ++gitSeq;
    let st;
    try { st = await call('/api/project/git/status', { site: cur }); }
    catch (e) {
      if (!live(g) || seq !== gitSeq) return;
      if (!quiet || !gitState.built) { gitState.err = e.message; gitState.built = false; if (tab === 'git') paintGitMessage('Couldn\'t read the repository', esc(e.message), true); }
      return;
    }
    if (!live(g) || seq !== gitSeq) return;
    gitState.err = '';
    gitState.st = st;
    updateGitCount();
    if (tab !== 'git') return;
    const sig = JSON.stringify(st);
    if (gitState.built && sig === gitState.sig) return;
    gitState.sig = sig;
    paintGit();
  }

  function paintGitMessage(title, body, retry) {
    gitState.built = false;
    els.panel.innerHTML = `<section class="card"><div class="mp-git-empty">${ICON.gitLogo}<b>${title}</b><span>${body}</span>
      ${retry ? '<button class="sm" data-g="retry">Try again</button>' : ''}</div></section>`;
    const r = $g('retry');
    if (r) r.onclick = () => { els.panel.innerHTML = `<section class="card"><div class="mp-loading"><div class="spin"></div>Reading the repository…</div></section>`; loadGitStatus(false); };
  }

  function paintGit() {
    const st = gitState.st;
    if (st.noGit) { paintGitMessage('Git isn\'t installed', esc(st.message), true); return; }
    if (!st.isRepo) {
      gitState.built = false;
      els.panel.innerHTML = `<section class="card"><div class="mp-git-empty">${ICON.gitLogo}<b>Not a Git repository yet</b>
        <span>Track every change to <b>${esc(cur)}</b>, commit snapshots you can go back to, and sync with GitHub, GitLab or any remote.</span>
        <button class="primary mp-btn-ic" data-g="init">${ICON.plus}Initialize repository</button>
        <span class="mp-sub">Runs <span class="mono">git init</span> in <span class="mono">${esc(info.path)}</span></span></div></section>`;
      $g('init').onclick = async e => {
        e.target.disabled = true;
        if (await gitWrite('init', {}, 'Repository created')) gitState.built = false;
        else e.target.disabled = false;
      };
      return;
    }
    if (!gitState.built) buildGit();
    followSelection();
    paintGitBar();
    paintGitBanner();
    paintGitConsole();
    paintGitViews();
    paintGitList();
    paintCommitBox();
    // The HEAD moved (commit, pull, checkout): the history is stale.
    const head = (st.head || '') + '|' + st.branch;
    if (head !== gitState.head) {
      const first = !gitState.head;
      gitState.head = head;
      if (!first && gitState.log) { gitState.log = null; if (gitState.view === 'history') paintGitList(); }
    }
    if (gitState.stashes && gitState.stashes.length !== st.stashCount) {
      gitState.stashes = null;
      if (gitState.view === 'stashes') paintGitList();
    }
    // A shown file diff follows the file's changes.
    if (gitState.sel && gitState.sel.kind === 'file') loadGitDiff(true);
    else if (!gitState.sel) paintGitDiff();
  }

  function buildGit() {
    gitState.built = true;
    els.panel.innerHTML = `<div class="mp-git">
      <section class="card mp-git-bar" data-g="bar"></section>
      <div data-g="banner"></div>
      <section class="card mp-git-console" data-g="console" hidden></section>
      <div class="mp-git-layout">
        <section class="card mp-git-side">
          <div class="mp-git-views" data-g="views"></div>
          <div class="mp-git-list" data-g="list"></div>
          <div class="mp-git-commit" data-g="commit"></div>
        </section>
        <section class="card mp-git-diffcard">
          <div class="mp-git-diffhead" data-g="diffhead"></div>
          <div class="mp-diff" data-g="diff" tabindex="0"></div>
        </section>
      </div>
    </div>`;
    buildCommitBox();
    const list = $g('list');
    list.addEventListener('click', onGitListClick);
    list.addEventListener('keydown', onGitListKey);
  }

  /* ── header bar ──────────────────────────────────────────────── */
  function branchLabel(st) {
    if (st.rebaseInProgress && st.rebaseBranch) return st.rebaseBranch;
    if (st.detached) return st.head ? st.head.slice(0, 7) : 'detached';
    return st.branch || '—';
  }

  function paintGitBar() {
    const bar = $g('bar');
    if (!bar) return;
    const st = gitState.st;
    const running = gitRunning();
    const busyNote = running ? gitState.job.title : st.busy;
    let up = '';
    if (st.rebaseInProgress) up = `<span class="badge warnb" title="HEAD is detached while the rebase replays commits">rebasing${st.rebaseBranch ? '' : ' (detached HEAD)'}</span>`;
    else if (st.initial) up = '<span class="badge">no commits yet</span>';
    else if (st.upstream) {
      const ab = (st.ahead ? `<b class="ahead" title="${st.ahead} commit${st.ahead === 1 ? '' : 's'} to push">↑${st.ahead}</b>` : '') +
        (st.behind ? `<b class="behind" title="${st.behind} commit${st.behind === 1 ? '' : 's'} to pull">↓${st.behind}</b>` : '');
      up = `<span class="mp-git-up" title="tracking ${esc(st.upstream)}">${ICON.cloud}<span class="mono">${esc(st.upstream)}</span>${ab || '<span class="sync">in sync</span>'}</span>`;
    } else if (st.detached) up = '<span class="badge warnb" title="HEAD points at a commit, not a branch">detached HEAD</span>';
    else if (st.hasRemote) up = '<span class="badge warnb" title="this branch only exists here">not published</span>';
    else up = '<span class="badge" title="add one with: git remote add origin <url>">no remote</span>';

    const mode = PULL_MODES[prefGet('git.pullMode', 'merge')] ? prefGet('git.pullMode', 'merge') : 'merge';
    const publish = st.hasRemote && !st.upstream && !st.detached;
    const canPull = !!st.upstream && !running;
    const canPush = st.hasRemote && !st.detached && !st.initial && !running;
    const pushTitle = !st.hasRemote ? 'No remote configured' : st.detached ? 'Check out a branch to push' : publish ? 'Push this branch to ' + esc(st.remote) + ' and track it' : 'Push to ' + esc(st.upstream);
    const last = st.lastCommit;
    bar.innerHTML = `
      <div class="mp-git-bar-top">
        <button class="mp-git-branch" data-g="branch" title="Switch or create a branch">${ICON.branch}<span>${esc(branchLabel(st))}</span>${ICON.chev}</button>
        ${up}
        ${busyNote ? `<span class="mp-git-busy"><span class="spin"></span>${esc(busyNote)}…</span>` : ''}
        <span class="spacer"></span>
        <div class="mp-git-actions">
          <button class="sm mp-btn-ic" data-g="fetch" ${st.hasRemote && !running ? '' : 'disabled'} title="Download new commits and branches from the remote (changes nothing locally)">${ICON.fetch}Fetch</button>
          <div class="mp-split">
            <button class="sm mp-btn-ic${st.behind ? ' primary' : ''}" data-g="pull" ${canPull ? '' : 'disabled'} title="${st.upstream ? esc(PULL_MODES[mode]) + ' from ' + esc(st.upstream) : 'This branch has no upstream — publish it first'}">${ICON.pull}${PULL_MODES[mode]}${st.behind ? `<span class="n">${st.behind}</span>` : ''}</button>
            <button class="sm mp-split-more${st.behind ? ' primary' : ''}" data-g="pull-more" ${canPull ? '' : 'disabled'} aria-label="Pull options" title="Pull options">${ICON.chev}</button>
          </div>
          <div class="mp-split">
            <button class="sm mp-btn-ic${st.ahead || publish ? ' primary' : ''}" data-g="push" ${canPush ? '' : 'disabled'} title="${pushTitle}">${ICON.push}${publish ? 'Publish branch' : 'Push'}${st.ahead ? `<span class="n">${st.ahead}</span>` : ''}</button>
            <button class="sm mp-split-more${st.ahead || publish ? ' primary' : ''}" data-g="push-more" ${canPush ? '' : 'disabled'} aria-label="Push options" title="Push options">${ICON.chev}</button>
          </div>
          <button class="mp-icon-btn" data-g="more" title="More Git actions" aria-label="More Git actions">${ICON.dots}</button>
        </div>
      </div>
      <div class="mp-git-bar-meta">
        ${last ? `<span class="last" title="${esc(last.hash)}">${ICON.commit}<span class="mono">${esc(last.hash.slice(0, 7))}</span><span class="s">${esc(last.subject)}</span><span class="w">${esc(last.author)} · ${esc(last.relTime)}</span></span>` : '<span class="last">No commits yet — stage your files and make the first commit.</span>'}
        ${st.remoteURL ? `<span class="url mono" title="${esc(st.remote)}: ${esc(st.remoteURL)}">${esc(st.remote)} · ${esc(st.remoteURL)}</span>` : ''}
        ${st.root && st.root !== info.path ? `<span class="url mono" title="The repository starts above the project folder">repo: ${esc(st.root)}</span>` : ''}
      </div>`;
    $g('branch').onclick = e => openBranchPop(e.currentTarget);
    $g('fetch').onclick = () => runGitJob('fetch', {}, 'Fetching', 'Fetched');
    $g('pull').onclick = () => gitPull(mode);
    $g('pull-more').onclick = e => gitMenu(e.currentTarget, Object.keys(PULL_MODES).map(m => ({
      icon: m === 'rebase' ? ICON.rebase : ICON.pull,
      label: { merge: 'Pull (merge)', rebase: 'Pull with rebase', 'ff-only': 'Fast-forward only' }[m],
      right: m === mode ? 'default' : '',
      run: () => { prefSet('git.pullMode', m); gitPull(m); },
    })));
    $g('push').onclick = () => gitPush({ setUpstream: publish });
    $g('push-more').onclick = e => gitMenu(e.currentTarget, [
      { icon: ICON.push, label: publish ? 'Publish branch' : 'Push', run: () => gitPush({ setUpstream: publish }) },
      '-',
      { icon: ICON.alert, label: 'Force push (with lease)…', danger: true, run: () => gitForcePush() },
    ]);
    $g('more').onclick = e => gitMenu(e.currentTarget, [
      { icon: ICON.rebase, label: 'Rebase current branch onto…', run: () => gitRebasePick() },
      { icon: ICON.stash, label: 'Stash changes…', run: () => gitStashNew() },
      '-',
      { icon: ICON.fetch, label: 'Refresh', run: () => { loadGitStatus(false); } },
      { icon: ICON.term, label: 'Open terminal here', run: () => openTerminal() },
      ...(st.remoteURL ? [{ icon: ICON.copy, label: 'Copy remote URL', run: async () => say(await copyText(st.remoteURL) ? 'Remote URL copied' : 'Could not copy') }] : []),
    ]);
  }

  // gitMenu uses the panel's popup menu (openMenu) when it's there.
  function gitMenu(anchor, items) {
    if (G('openMenu')) { G('openMenu')(anchor, items.map(it => it === '-' ? it : { icon: '', ...it })); return; }
    const pick = items.filter(it => it !== '-');
    mpModal({ title: 'Git' }, (body, close) => {
      body.innerHTML = '<div class="mp-git-menu">' + pick.map((it, i) => `<button class="sm${it.danger ? ' danger' : ''}" data-i="${i}">${esc(it.label)}</button>`).join('') + '</div>';
      body.querySelectorAll('[data-i]').forEach(b => b.onclick = () => { close(null); pick[+b.dataset.i].run(); });
    });
  }

  /* ── merge / rebase banner ───────────────────────────────────── */
  function paintGitBanner() {
    const box = $g('banner');
    if (!box) return;
    const st = gitState.st;
    const kind = st.rebaseInProgress ? 'rebase' : st.mergeInProgress ? 'merge' : st.cherryPickInProgress ? 'cherry-pick' : '';
    if (!kind) { box.innerHTML = ''; return; }
    const n = st.conflicted.length;
    const title = { rebase: 'Rebase in progress', merge: 'Merge in progress', 'cherry-pick': 'Cherry-pick in progress' }[kind];
    const msg = n
      ? `${n} file${n === 1 ? ' has' : 's have'} conflicts. Open ${n === 1 ? 'it' : 'each one'}, keep the right lines (remove the <span class="mono">&lt;&lt;&lt;&lt;&lt;&lt;&lt;</span> / <span class="mono">&gt;&gt;&gt;&gt;&gt;&gt;&gt;</span> markers), stage ${n === 1 ? 'it' : 'them'}, then Continue.`
      : kind === 'cherry-pick' ? 'Conflicts resolved — commit to finish, or abort.' : 'All conflicts are resolved — Continue to finish.';
    box.innerHTML = `<div class="mp-git-banner">
      <div class="ic">${ICON.alert}</div>
      <div class="t"><b>${title}</b><span>${msg}</span>
        ${n ? `<div class="files">${st.conflicted.map(f => `<button class="mp-git-cfile" data-cfile="${esc(f.path)}">${esc(f.path)}</button>`).join('')}</div>` : ''}</div>
      <div class="a">
        ${kind !== 'cherry-pick' ? `<button class="primary sm" data-seq="${kind}/continue" ${n ? 'disabled' : ''}>Continue ${kind}</button>` : ''}
        ${kind === 'rebase' ? '<button class="sm" data-seq="rebase/skip" title="drop the commit being replayed">Skip commit</button>' : ''}
        <button class="danger sm" data-seq="${kind}/abort">Abort</button>
      </div>
    </div>`;
    box.querySelectorAll('[data-cfile]').forEach(b => b.onclick = () => { setGitView('changes'); selectGitFile(b.dataset.cfile, false); });
    box.querySelectorAll('[data-seq]').forEach(b => b.onclick = async () => {
      const op = b.dataset.seq;
      if (/abort$/.test(op)) {
        const ok = await confirmBox({
          title: 'Abort the ' + kind + '?',
          message: kind === 'rebase' ? 'Your branch goes back to exactly where it was before the rebase started.' : 'Everything goes back to how it was before the ' + kind + ' — conflict fixes you made are thrown away.',
          confirmText: 'Abort ' + kind, danger: true,
        });
        if (!ok) return;
      }
      if (/skip$/.test(op) && !(await confirmBox({ title: 'Skip this commit?', message: 'The commit being replayed is dropped from your branch.', confirmText: 'Skip it', danger: true }))) return;
      b.disabled = true;
      const done = { continue: kind === 'rebase' ? 'Rebase continued' : 'Merge committed', abort: kind[0].toUpperCase() + kind.slice(1) + ' aborted', skip: 'Commit skipped' }[op.split('/')[1]];
      if (!(await gitWrite(op, {}, done))) b.disabled = false;
    });
  }

  /* ── streamed operations (fetch / pull / push / rebase) ──────── */
  async function runGitJob(path, body, title, doneMsg) {
    if (gitRunning()) { say('Git is still busy — wait for it to finish'); return null; }
    const g = gen;
    let id;
    try { ({ job: id } = await call('/api/project/git/' + path, { site: cur, ...body })); }
    catch (e) { gitError(e.message); return null; }
    const noun = { fetch: 'Fetch', pull: 'Pull', push: 'Push', rebase: 'Rebase' }[path] || title;
    const j = { id, path, title, noun, doneMsg, running: true, st: { log: '' } };
    gitState.job = j;
    gitState.consoleOpen = true;
    if (tab === 'git' && live(g)) { paintGitConsole(); paintGitBar(); }
    const st = await follow(id, s => {
      j.st = s;
      if (tab === 'git' && live(g) && !s.done) paintConsoleOut();
    });
    j.running = false;
    j.st = st || j.st;
    const res = st && st.result;
    if (st && st.error) { j.error = st.error; j.failed = true; }
    else if (res && res.authFailed) { j.auth = res.message; j.failed = true; }
    if (!live(g)) return j;
    if (j.failed) {
      gitState.consoleOpen = true;
      j.rejected = !j.auth && /\[rejected\]|non-fast-forward|fetch first|failed to push/i.test((j.st.log || '') + (j.error || ''));
      say(j.auth ? noun + ' needs you to sign in' : j.rejected ? 'Push rejected — the remote has commits you don\'t have yet' : noun + ' failed');
    } else {
      gitState.consoleOpen = false;
      say(doneMsg);
    }
    gitState.log = null;
    if (tab === 'git') { paintGitConsole(); paintGitBar(); if (gitState.view === 'history') paintGitList(); }
    loadGitStatus(true);
    return j;
  }

  function paintGitConsole() {
    const box = $g('console');
    if (!box) return;
    const j = gitState.job;
    box.hidden = !j;
    if (!j) return;
    const state = j.running ? 'run' : j.failed ? 'bad' : 'ok';
    const icon = j.running ? '<span class="spin"></span>' : j.failed ? ICON.x : ICON.check;
    const verb = j.path === 'push' ? 'push' : j.path === 'pull' ? 'pull' : j.path;
    box.className = 'card mp-git-console ' + state;
    box.innerHTML = `
      <div class="h">
        <span class="st">${icon}</span><b>${j.running ? esc(j.title) + '…' : j.failed ? esc(j.noun) + ' failed' : esc(j.doneMsg || j.noun)}</b>
        <span class="mp-sub" data-c="status">${j.running ? esc(j.st.status || '') : ''}</span>
        <span class="spacer"></span>
        ${j.running ? '<button class="sm danger" data-c="stop">Stop</button>' : ''}
        <button class="sm mp-btn-ic" data-c="toggle" aria-expanded="${gitState.consoleOpen}">${gitState.consoleOpen ? 'Hide output' : 'Show output'}</button>
        ${j.running ? '' : `<button class="mp-icon-btn" data-c="close" title="Dismiss" aria-label="Dismiss">${ICON.x}</button>`}
      </div>
      ${j.auth ? `<div class="mp-git-help">${ICON.lock}<div><b>Sign-in needed</b><span>${esc(j.auth)}</span></div><button class="primary sm mp-btn-ic" data-c="term">${ICON.term}Open terminal here</button></div>` : ''}
      ${j.rejected ? `<div class="mp-git-help">${ICON.alert}<div><b>The remote has new commits</b><span>Pull them first (your commits stay), then push again.</span></div><button class="primary sm mp-btn-ic" data-c="pull">${ICON.pull}Pull now</button></div>` : ''}
      <pre class="mp-term short" data-c="out" ${gitState.consoleOpen ? '' : 'hidden'}></pre>`;
    paintConsoleOut();
    const q = k => box.querySelector(`[data-c="${k}"]`);
    if (q('stop')) q('stop').onclick = () => call('/api/project/cancel', { id: j.id }).catch(e => say('Error: ' + e.message));
    q('toggle').onclick = () => { gitState.consoleOpen = !gitState.consoleOpen; paintGitConsole(); };
    if (q('close')) q('close').onclick = () => { gitState.job = null; paintGitConsole(); };
    if (q('term')) q('term').onclick = async () => {
      await openTerminal();
      const cmd = 'git ' + verb;
      say(await copyText(cmd) ? `Terminal opened — run “${cmd}” there once to sign in (copied)` : `Terminal opened — run “${cmd}” there once to sign in`);
    };
    if (q('pull')) q('pull').onclick = () => gitPull(prefGet('git.pullMode', 'merge'));
  }

  function paintConsoleOut() {
    const box = $g('console');
    const j = gitState.job;
    if (!box || !j) return;
    const out = box.querySelector('[data-c="out"]');
    const status = box.querySelector('[data-c="status"]');
    if (status && j.running) status.textContent = j.st.status || '';
    if (!out) return;
    const st = Object.assign({}, j.st);
    if (!j.running && j.error && !(st.log || '').includes(j.error)) st.log = (st.log || '') + '\n\x1b[31m' + j.error + '\x1b[0m\n';
    paintTerm(out, st, j.running ? 'Connecting…' : '(no output)');
  }

  function gitPull(mode) {
    return runGitJob('pull', { mode }, mode === 'rebase' ? 'Pulling with rebase' : mode === 'ff-only' ? 'Pulling (fast-forward only)' : 'Pulling', 'Pulled');
  }
  function gitPush(opts) {
    const st = gitState.st || {};
    return runGitJob('push', { setUpstream: !!opts.setUpstream, force: !!opts.force },
      opts.force ? 'Force pushing' : opts.setUpstream && !st.upstream ? 'Publishing ' + branchLabel(st) : 'Pushing',
      opts.setUpstream && !st.upstream ? 'Published ' + branchLabel(st) : 'Pushed');
  }
  async function gitForcePush() {
    const st = gitState.st;
    const ok = await confirmBox({
      title: 'Force push ' + esc(branchLabel(st)) + '?',
      message: `This replaces <span class="mono">${esc(st.upstream || (st.remote || 'origin') + '/' + branchLabel(st))}</span> with your local history. It uses <span class="mono">--force-with-lease</span>: if someone else pushed in the meantime, it refuses instead of overwriting their work.`,
      confirmText: 'Force push', danger: true,
    });
    if (ok) return gitPush({ force: true, setUpstream: !st.upstream });
  }

  async function gitRebasePick() {
    const g = gen;
    let list = [];
    try { list = (await call('/api/project/git/branches', { site: cur })).branches || []; } catch (e) { gitError(e.message); return; }
    if (!live(g)) return;
    const options = list.filter(b => !b.current).map(b => ({ value: b.name, label: b.name + (b.remote ? '  (remote)' : '') }));
    if (!options.length) { say('There\'s no other branch to rebase onto'); return; }
    const up = gitState.st.upstream;
    const onto = await promptBox({
      title: 'Rebase ' + esc(branchLabel(gitState.st)) + ' onto…',
      message: 'Your commits are replayed on top of the branch you pick — history stays linear. If they conflict, you resolve each one and Continue.',
      input: { type: 'search-select', label: 'Branch', options, value: up && options.some(o => o.value === up) ? up : options[0].value, placeholder: 'Filter branches…' },
      confirmText: 'Rebase',
    });
    if (!onto || !live(g)) return;
    const j = await runGitJob('rebase', { onto }, 'Rebasing onto ' + onto, 'Rebased onto ' + onto);
    if (j && j.failed && !j.auth) {
      gitState.consoleOpen = true;
      say('The rebase stopped on conflicts — resolve them, then Continue (or Abort)');
    }
  }

  /* ── branch switcher ─────────────────────────────────────────── */
  function closeBranchPop() {
    if (!branchPop) return;
    branchPop.close();
    branchPop = null;
  }

  function openBranchPop(anchor) {
    if (branchPop) { closeBranchPop(); return; }
    const g = gen;
    const pop = document.createElement('div');
    pop.className = 'mp-git-pop';
    pop.setAttribute('role', 'dialog');
    pop.setAttribute('aria-label', 'Switch branch');
    pop.innerHTML = `<div class="mp-git-pop-search">${ICON.search}<input type="text" placeholder="Switch to, or type a new branch name…" spellcheck="false" autocomplete="off"></div>
      <div class="mp-git-pop-list"><div class="mp-loading"><div class="spin"></div>Loading branches…</div></div>`;
    document.body.appendChild(pop);
    const place = () => {
      const r = anchor.getBoundingClientRect();
      const w = pop.offsetWidth;
      pop.style.left = Math.max(8, Math.min(r.left, innerWidth - w - 8)) + 'px';
      pop.style.top = Math.min(r.bottom + 6, innerHeight - 120) + 'px';
      pop.style.maxHeight = Math.max(220, innerHeight - r.bottom - 20) + 'px';
    };
    place();
    const input = pop.querySelector('input');
    const listEl = pop.querySelector('.mp-git-pop-list');
    let branches = null, active = 0;
    const onDown = e => { if (!pop.contains(e.target) && !anchor.contains(e.target)) closeBranchPop(); };
    const onKey = e => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeBranchPop(); anchor.focus(); } };
    document.addEventListener('mousedown', onDown, true);
    document.addEventListener('keydown', onKey, true);
    window.addEventListener('resize', closeBranchPop);
    branchPop = {
      close() {
        document.removeEventListener('mousedown', onDown, true);
        document.removeEventListener('keydown', onKey, true);
        window.removeEventListener('resize', closeBranchPop);
        pop.remove();
      },
    };
    const st = gitState.st;
    const paint = () => {
      if (!branches) return;
      const q = input.value.trim();
      const ql = q.toLowerCase();
      const local = branches.filter(b => !b.remote && (!ql || b.name.toLowerCase().includes(ql)));
      const localNames = new Set(branches.filter(b => !b.remote).map(b => b.name));
      const remote = branches.filter(b => b.remote && !localNames.has(b.short) && (!ql || b.name.toLowerCase().includes(ql)));
      let html = '';
      if (q && !localNames.has(q)) {
        html += `<button class="mp-git-br create" data-create="${esc(q)}">${ICON.plus}<span class="nm">Create branch <b class="mono">${esc(q)}</b></span><span class="meta">from ${esc(branchLabel(st))}</span></button>`;
      } else if (!q) {
        html += `<button class="mp-git-br create" data-create="">${ICON.plus}<span class="nm">Create a new branch…</span><span class="meta">from ${esc(branchLabel(st))}</span></button>`;
      }
      const row = b => {
        const meta = [];
        if (b.ahead) meta.push(`<b class="ahead">↑${b.ahead}</b>`);
        if (b.behind) meta.push(`<b class="behind">↓${b.behind}</b>`);
        if (b.gone) meta.push('<span class="badge warnb">upstream gone</span>');
        if (b.lastCommitRel) meta.push(`<span>${esc(b.lastCommitRel)}</span>`);
        const acts = !b.remote && !b.current
          ? `<span class="acts"><span class="mp-icon-btn" role="button" data-rebase="${esc(b.name)}" title="Rebase ${esc(branchLabel(st))} onto ${esc(b.name)}">${ICON.rebase}</span><span class="mp-icon-btn del" role="button" data-del="${esc(b.name)}" title="Delete ${esc(b.name)}">${ICON.trash}</span></span>` : '';
        return `<button class="mp-git-br${b.current ? ' cur' : ''}" data-switch="${esc(b.remote ? b.short : b.name)}" data-remote="${b.remote ? '1' : ''}" title="${b.current ? 'current branch' : 'switch to ' + esc(b.name)}">
          <span class="ck">${b.current ? ICON.check : b.remote ? ICON.cloud : ICON.branch}</span><span class="nm mono">${esc(b.name)}</span><span class="meta">${meta.join('')}</span>${acts}</button>`;
      };
      if (local.length) html += '<div class="mp-git-pop-grp">Branches</div>' + local.map(row).join('');
      if (remote.length) html += '<div class="mp-git-pop-grp">Remote branches</div>' + remote.map(row).join('');
      if (!local.length && !remote.length && ql) html += `<div class="mp-git-pop-none">No branch matches “${esc(q)}”.</div>`;
      listEl.innerHTML = html;
      const items = [...listEl.querySelectorAll('.mp-git-br')];
      active = Math.min(active, items.length - 1);
      items.forEach((el, i) => el.classList.toggle('act', i === active));
    };
    const move = d => {
      const items = [...listEl.querySelectorAll('.mp-git-br')];
      if (!items.length) return;
      active = (active + d + items.length) % items.length;
      items.forEach((el, i) => el.classList.toggle('act', i === active));
      items[active].scrollIntoView({ block: 'nearest' });
    };
    input.oninput = () => { active = 0; paint(); };
    input.onkeydown = e => {
      if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
      else if (e.key === 'Enter') {
        e.preventDefault();
        const el = listEl.querySelectorAll('.mp-git-br')[active];
        if (el) el.click();
      }
    };
    listEl.onclick = async e => {
      const reb = e.target.closest('[data-rebase]');
      const del = e.target.closest('[data-del]');
      const btn = e.target.closest('.mp-git-br');
      if (reb) {
        e.stopPropagation();
        const onto = reb.dataset.rebase;
        closeBranchPop();
        if (await confirmBox({ title: 'Rebase onto ' + esc(onto) + '?', message: `Replays the commits of <span class="mono">${esc(branchLabel(st))}</span> on top of <span class="mono">${esc(onto)}</span>.`, confirmText: 'Rebase' })) {
          runGitJob('rebase', { onto }, 'Rebasing onto ' + onto, 'Rebased onto ' + onto);
        }
        return;
      }
      if (del) { e.stopPropagation(); closeBranchPop(); gitDeleteBranch(del.dataset.del); return; }
      if (!btn) return;
      if (btn.dataset.create !== undefined) {
        let name = btn.dataset.create;
        closeBranchPop();
        if (!name) {
          name = await promptBox({ title: 'Create a branch', message: `Starts from <span class="mono">${esc(branchLabel(st))}</span> and switches to it — your uncommitted changes come along.`, input: { label: 'Branch name', placeholder: 'feature/login' }, confirmText: 'Create branch' });
          if (!name || !live(g)) return;
        }
        gitWrite('branch/create', { name: name.trim() }, 'Created and switched to ' + name.trim(), true);
        return;
      }
      const name = btn.dataset.switch;
      closeBranchPop();
      if (btn.classList.contains('cur')) return;
      gitWrite('checkout', { branch: name }, 'Switched to ' + name + (btn.dataset.remote ? ' (tracking the remote)' : ''), true);
    };
    requestAnimationFrame(() => input.focus());
    call('/api/project/git/branches', { site: cur }).then(d => {
      if (!live(g) || !branchPop) return;
      branches = d.branches || [];
      paint();
      place();
    }).catch(e => { listEl.innerHTML = `<div class="mp-git-pop-none">${esc(e.message)}</div>`; });
  }

  async function gitDeleteBranch(name) {
    const ok = await confirmBox({ title: 'Delete branch ' + esc(name) + '?', message: `Deletes the local branch <span class="mono">${esc(name)}</span>. Git refuses if it has commits that aren't merged anywhere.`, confirmText: 'Delete', danger: true });
    if (!ok) return;
    try {
      await call('/api/project/git/branch/delete', { site: cur, name, force: false });
      say('Deleted ' + name);
    } catch (e) {
      if (!/force/.test(e.message)) { gitError(e.message); return; }
      const force = await confirmBox({ title: esc(name) + ' isn\'t merged', message: `Its commits aren't on any other branch — deleting it loses them (unless you have them elsewhere). Delete anyway?`, confirmText: 'Delete anyway', danger: true });
      if (force) await gitWrite('branch/delete', { name, force: true }, 'Deleted ' + name);
    }
    loadGitStatus(true);
  }

  /* ── views: Changes / History / Stashes ──────────────────────── */
  function setGitView(v) {
    if (gitState.view === v) return;
    gitState.view = v;
    paintGitViews();
    paintGitList();
    paintCommitBox();
  }

  function paintGitViews() {
    const box = $g('views');
    if (!box) return;
    const st = gitState.st;
    const n = gitTotal(st);
    box.innerHTML = `<div class="mp-seg" role="tablist">
      <button role="tab" data-gview="changes" class="${gitState.view === 'changes' ? 'active' : ''}">Changes${n ? `<span class="n">${n}</span>` : ''}</button>
      <button role="tab" data-gview="history" class="${gitState.view === 'history' ? 'active' : ''}">History</button>
      <button role="tab" data-gview="stashes" class="${gitState.view === 'stashes' ? 'active' : ''}">Stashes${st.stashCount ? `<span class="n">${st.stashCount}</span>` : ''}</button>
    </div>`;
    box.querySelectorAll('[data-gview]').forEach(b => b.onclick = () => setGitView(b.dataset.gview));
  }

  function paintGitList() {
    const box = $g('list');
    if (!box) return;
    if (gitState.view === 'history') return paintHistory(box);
    if (gitState.view === 'stashes') return paintStashes(box);
    paintChanges(box);
  }

  // Which sections a file row belongs to, and its per-row actions.
  const SECS = [
    ['conflicted', 'Merge conflicts', false],
    ['staged', 'Staged changes', true],
    ['unstaged', 'Changes', false],
    ['untracked', 'Untracked files', false],
  ];

  function fileRow(f, sec) {
    const [letter, cls, label] = GIT_KIND[f.kind] || GIT_KIND.modified;
    const i = f.path.lastIndexOf('/');
    const dir = i >= 0 ? f.path.slice(0, i) : '', name = f.path.slice(i + 1);
    const s = gitState.sel;
    const staged = sec === 'staged';
    const selected = s && s.kind === 'file' && s.path === f.path && s.staged === staged;
    const counts = f.binary ? '<span class="bin">binary</span>'
      : (f.additions || f.deletions ? `${f.additions ? `<span class="plus">+${f.additions}</span>` : ''}${f.deletions ? `<span class="minus">−${f.deletions}</span>` : ''}` : '');
    let acts = '';
    if (staged) acts = `<button class="mp-icon-btn" data-fact="unstage" title="Unstage" aria-label="Unstage ${esc(f.path)}">${ICON.minus}</button>`;
    else if (sec === 'conflicted') acts = `<button class="mp-icon-btn" data-fact="stage" title="Mark resolved (stage)" aria-label="Mark ${esc(f.path)} resolved">${ICON.check}</button>`;
    else acts = `<button class="mp-icon-btn danger" data-fact="discard" title="${sec === 'untracked' ? 'Delete file' : 'Discard changes'}" aria-label="Discard ${esc(f.path)}">${sec === 'untracked' ? ICON.trash : ICON.undo}</button>
      <button class="mp-icon-btn" data-fact="stage" title="Stage" aria-label="Stage ${esc(f.path)}">${ICON.plus}</button>`;
    const from = f.origPath ? `<span class="dir"><bdi>← ${esc(f.origPath)}</bdi></span>` : (dir ? `<span class="dir"><bdi>${esc(dir)}</bdi></span>` : '');
    return `<div class="mp-gf${selected ? ' sel' : ''}" tabindex="0" role="button" data-path="${esc(f.path)}" data-sec="${sec}" title="${esc(label)}: ${esc(f.origPath ? f.origPath + ' → ' + f.path : f.path)}">
      <span class="mp-gs ${cls}">${letter}</span>
      <span class="p"><span class="nm">${esc(name)}</span>${from}</span>
      <span class="cnt">${counts}</span>
      <span class="acts">${acts}</span>
    </div>`;
  }

  function paintChanges(box) {
    const st = gitState.st;
    const scroll = box.scrollTop;
    if (!gitTotal(st)) {
      box.innerHTML = `<div class="mp-git-clean">${ICON.check}<b>Working tree clean</b><span>${st.ahead
        ? `Nothing to commit — ${st.ahead} commit${st.ahead === 1 ? '' : 's'} waiting to be pushed.`
        : 'Nothing to commit. Edits you make show up here.'}</span></div>`;
      return;
    }
    let html = st.truncated ? '<div class="mp-warn" style="margin:10px 12px 4px">Showing the first 1000 files of each list — is a folder like <span class="mono">node_modules</span> missing from .gitignore?</div>' : '';
    for (const [key, title] of SECS) {
      const files = st[key];
      if (!files.length) continue;
      const collapsed = !!gitState.collapsed[key];
      let acts = '';
      if (key === 'staged') acts = `<button class="sm" data-sact="unstage-all">Unstage all</button>`;
      else if (key === 'unstaged') acts = `<button class="mp-icon-btn danger" data-sact="discard-all" title="Discard all changes" aria-label="Discard all changes">${ICON.undo}</button><button class="sm" data-sact="stage-sec">Stage all</button>`;
      else if (key === 'untracked') acts = `<button class="sm" data-sact="stage-sec">Stage all</button>`;
      html += `<div class="mp-git-sec${key === 'conflicted' ? ' con' : ''}" data-seck="${key}">
        <button class="mp-git-sec-h" data-toggle="${key}" aria-expanded="${!collapsed}"><span class="cv${collapsed ? ' shut' : ''}">${ICON.chev}</span>${title}<span class="n">${files.length}</span></button>
        <span class="acts">${acts}</span>
      </div>
      <div class="mp-git-files"${collapsed ? ' hidden' : ''}>${files.map(f => fileRow(f, key)).join('')}</div>`;
    }
    box.innerHTML = html;
    box.scrollTop = scroll;
  }

  function onGitListClick(e) {
    const t = e.target;
    const tog = t.closest('[data-toggle]');
    if (tog) { const k = tog.dataset.toggle; gitState.collapsed[k] = !gitState.collapsed[k]; paintGitList(); return; }
    const sact = t.closest('[data-sact]');
    if (sact) { sectionAction(sact.dataset.sact, sact.closest('[data-seck]').dataset.seck, sact); return; }
    const fact = t.closest('[data-fact]');
    const row = t.closest('.mp-gf');
    if (fact && row) { e.stopPropagation(); fileAction(fact.dataset.fact, row.dataset.path, row.dataset.sec, fact); return; }
    if (row) { selectGitFile(row.dataset.path, row.dataset.sec === 'staged'); return; }
    const c = t.closest('[data-hash]');
    if (c) { selectGitCommit(c.dataset.hash); return; }
    const more = t.closest('[data-g="log-more"]');
    if (more) { gitState.logLimit += 100; gitState.log = null; paintGitList(); return; }
    const sb = t.closest('[data-st]');
    if (sb) { stashAction(sb.dataset.st, sb.closest('[data-stash]'), sb); return; }
    const sr = t.closest('[data-stash]');
    if (sr) selectGitStash(+sr.dataset.stash);
  }

  function onGitListKey(e) {
    const row = e.target.closest('.mp-gf, .mp-gc');
    if (!row || e.target !== row) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); row.click(); return; }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const rows = [...$g('list').querySelectorAll('.mp-gf, .mp-gc')].filter(r => r.offsetParent);
      const i = rows.indexOf(row) + (e.key === 'ArrowDown' ? 1 : -1);
      if (rows[i]) { rows[i].focus(); rows[i].click(); }
    }
  }

  function sectionPaths(key) { return (gitState.st[key] || []).map(f => f.path); }

  async function sectionAction(act, key, btn) {
    if (act === 'unstage-all') return gitWrite('unstage', { all: true }, null, false, btn);
    if (act === 'stage-sec') return gitWrite('stage', { paths: sectionPaths(key) }, null, false, btn);
    if (act === 'discard-all') {
      const n = gitState.st.unstaged.length;
      const ok = await confirmBox({ title: `Discard changes to ${n} file${n === 1 ? '' : 's'}?`, message: 'Every unstaged edit to tracked files is thrown away and the files go back to their last staged or committed content. Staged changes and untracked files are kept. <b>This can\'t be undone.</b>', confirmText: 'Discard all', danger: true });
      if (ok) return gitWrite('discard', { paths: sectionPaths('unstaged') }, 'Changes discarded', false, btn);
    }
  }

  async function fileAction(act, path, sec, btn) {
    if (act === 'stage') return gitWrite('stage', { paths: [path] }, sec === 'conflicted' ? 'Marked ' + path + ' resolved' : null, false, btn);
    if (act === 'unstage') return gitWrite('unstage', { paths: [path] }, null, false, btn);
    if (act === 'discard') {
      const untracked = sec === 'untracked';
      const ok = await confirmBox({
        title: untracked ? 'Delete ' + esc(path) + '?' : 'Discard changes to ' + esc(path) + '?',
        message: untracked ? 'Git isn\'t tracking this file, so once it\'s deleted it can\'t be recovered.' : 'The file goes back to its last staged or committed content. <b>This can\'t be undone.</b>',
        confirmText: untracked ? 'Delete file' : 'Discard', danger: true,
      });
      if (ok) return gitWrite('discard', { paths: [path] }, untracked ? 'Deleted ' + path : 'Discarded changes to ' + path, false, btn);
    }
  }

  // gitWrite runs a local write, toasts, and re-reads the status.
  async function gitWrite(path, body, done, rebuild, btn) {
    const g = gen;
    if (btn) btn.disabled = true;
    try {
      const r = await call('/api/project/git/' + path, { site: cur, ...body });
      if (done && live(g)) say(done);
      if (rebuild && live(g)) { gitState.log = null; gitState.sel = null; gitState.diff = null; }
      return r || true;
    } catch (e) {
      if (live(g)) gitError(e.message);
      return null;
    } finally {
      if (btn && btn.isConnected) btn.disabled = false;
      if (live(g)) loadGitStatus(true);
    }
  }

  // gitError toasts a short error; a long one (hook output, a conflict
  // list) opens in a dialog so it can be read.
  function gitError(msg) {
    msg = String(msg || 'failed');
    if (msg.length < 160 && !msg.includes('\n')) { say('Error: ' + msg); return; }
    const first = msg.split('\n')[0];
    alertBox({ title: 'Git says…', message: esc(first.length > 200 ? first.slice(0, 200) + '…' : first), detail: `<pre class="mp-git-pre">${esc(msg)}</pre>` });
  }

  /* ── selection + diff pane ───────────────────────────────────── */
  function followSelection() {
    const s = gitState.sel, st = gitState.st;
    if (!s || s.kind !== 'file') return;
    const inList = (keys) => keys.some(k => st[k].some(f => f.path === s.path));
    const here = s.staged ? inList(['staged']) : inList(['unstaged', 'untracked', 'conflicted']);
    if (here) return;
    // The file moved (staged / unstaged): follow it.
    const there = s.staged ? inList(['unstaged', 'untracked', 'conflicted']) : inList(['staged']);
    if (there) { s.staged = !s.staged; return; }
    gitState.sel = null;
    gitState.diff = null;
  }

  function markGitSelected() {
    const list = $g('list');
    if (!list) return;
    const s = gitState.sel;
    list.querySelectorAll('.mp-gf').forEach(r => r.classList.toggle('sel', !!s && s.kind === 'file' && r.dataset.path === s.path && (r.dataset.sec === 'staged') === s.staged));
    list.querySelectorAll('[data-hash]').forEach(r => r.classList.toggle('sel', !!s && s.kind === 'commit' && r.dataset.hash === s.hash));
    list.querySelectorAll('[data-stash]').forEach(r => r.classList.toggle('sel', !!s && s.kind === 'stash' && +r.dataset.stash === s.index));
  }

  function selectGitFile(path, staged) {
    gitState.sel = { kind: 'file', path, staged };
    gitState.diff = null;
    markGitSelected();
    loadGitDiff(false);
  }
  function selectGitCommit(hash) {
    gitState.sel = { kind: 'commit', hash };
    gitState.diff = null;
    markGitSelected();
    loadGitDiff(false);
  }
  function selectGitStash(index) {
    gitState.sel = { kind: 'stash', index };
    gitState.diff = null;
    markGitSelected();
    loadGitDiff(false);
  }

  const selKey = s => !s ? '' : s.kind === 'file' ? 'f:' + (s.staged ? 's:' : 'w:') + s.path : s.kind === 'commit' ? 'c:' + s.hash : 's:' + s.index;

  async function loadGitDiff(quiet) {
    const s = gitState.sel;
    if (!s) { paintGitDiff(); return; }
    const g = gen, seq = ++diffSeq;
    if (!quiet) paintGitDiff();
    let d;
    try {
      if (s.kind === 'file') d = await call('/api/project/git/diff', { site: cur, path: s.path, staged: s.staged });
      else if (s.kind === 'commit') d = await call('/api/project/git/show', { site: cur, hash: s.hash });
      else d = await call('/api/project/git/stash', { site: cur, action: 'show', index: s.index });
    } catch (e) { d = { error: e.message }; }
    if (!live(g) || seq !== diffSeq || gitState.sel !== s) return;
    const same = gitState.diff && gitState.diff.diff === d.diff && gitState.diffKey === selKey(s);
    gitState.diff = d;
    if (tab === 'git' && !same) paintGitDiff();
    else if (tab === 'git') paintDiffHead();
  }

  function paintDiffHead() {
    const head = $g('diffhead');
    if (!head) return;
    const s = gitState.sel, st = gitState.st;
    if (!s) { head.innerHTML = `<div class="t"><b>Diff</b></div>`; return; }
    if (s.kind === 'file') {
      const lists = s.staged ? ['staged'] : ['conflicted', 'unstaged', 'untracked'];
      let f = null, sec = '';
      for (const k of lists) { f = st[k].find(x => x.path === s.path); if (f) { sec = k; break; } }
      const [letter, cls] = GIT_KIND[f ? f.kind : 'modified'] || GIT_KIND.modified;
      const where = s.staged ? '<span class="badge on">staged</span>' : sec === 'untracked' ? '<span class="badge">untracked</span>' : sec === 'conflicted' ? '<span class="badge bad">conflict</span>' : '<span class="badge">working tree</span>';
      head.innerHTML = `<div class="t"><span class="mp-gs ${cls}">${letter}</span><span class="mono pth" title="${esc(s.path)}">${esc(s.path)}</span>${where}</div>
        <div class="a">
          ${s.staged ? '<button class="sm mp-btn-ic" data-d="unstage">' + ICON.minus + 'Unstage</button>'
            : `${sec === 'conflicted' ? '' : `<button class="sm mp-btn-ic danger" data-d="discard">${sec === 'untracked' ? ICON.trash + 'Delete' : ICON.undo + 'Discard'}</button>`}<button class="sm mp-btn-ic${sec === 'conflicted' ? ' primary' : ''}" data-d="stage">${sec === 'conflicted' ? ICON.check + 'Mark resolved' : ICON.plus + 'Stage'}</button>`}
          <button class="mp-icon-btn" data-d="copy" title="Copy path" aria-label="Copy path">${ICON.copy}</button>
        </div>`;
      const q = k => head.querySelector(`[data-d="${k}"]`);
      if (q('unstage')) q('unstage').onclick = e => fileAction('unstage', s.path, 'staged', e.currentTarget);
      if (q('stage')) q('stage').onclick = e => fileAction('stage', s.path, sec, e.currentTarget);
      if (q('discard')) q('discard').onclick = e => fileAction('discard', s.path, sec, e.currentTarget);
      q('copy').onclick = async () => say(await copyText(s.path) ? 'Path copied' : 'Could not copy');
      return;
    }
    if (s.kind === 'commit') {
      const c = (gitState.log || []).find(x => x.hash === s.hash) || { short: s.hash.slice(0, 7), subject: '' };
      head.innerHTML = `<div class="t"><span class="mp-git-hash">${esc(c.short)}</span><span class="pth">${esc(c.subject)}</span></div>
        <div class="a"><button class="mp-icon-btn" data-d="copy" title="Copy commit hash" aria-label="Copy commit hash">${ICON.copy}</button></div>`;
      head.querySelector('[data-d="copy"]').onclick = async () => say(await copyText(s.hash) ? 'Hash copied' : 'Could not copy');
      return;
    }
    const sh = (gitState.stashes || []).find(x => x.index === s.index) || { ref: 'stash@{' + s.index + '}', message: '' };
    head.innerHTML = `<div class="t"><span class="mp-git-hash">${esc(sh.ref)}</span><span class="pth">${esc(sh.message)}</span></div>
      <div class="a"><button class="sm primary" data-d="pop">Pop</button><button class="sm danger" data-d="drop">Drop</button></div>`;
    head.querySelector('[data-d="pop"]').onclick = e => stashAction('pop', null, e.currentTarget, s.index);
    head.querySelector('[data-d="drop"]').onclick = e => stashAction('drop', null, e.currentTarget, s.index);
  }

  function paintGitDiff() {
    const box = $g('diff');
    if (!box) return;
    paintDiffHead();
    const s = gitState.sel, d = gitState.diff;
    const key = selKey(s);
    const keep = key && key === gitState.diffKey ? [box.scrollTop, box.scrollLeft] : [0, 0];
    gitState.diffKey = key;
    if (!s) {
      const st = gitState.st;
      box.innerHTML = `<div class="mp-diff-empty">${ICON.file}<b>${gitTotal(st) ? 'Select a file to see its changes' : 'Nothing changed'}</b><span>${gitState.view === 'history' ? 'Pick a commit to see what it changed.' : gitState.view === 'stashes' ? 'Pick a stash to see what it holds.' : 'Line-by-line diffs show up here.'}</span></div>`;
      return;
    }
    if (!d) { box.innerHTML = '<div class="mp-loading"><div class="spin"></div>Loading diff…</div>'; return; }
    if (d.error) { box.innerHTML = `<div class="mp-diff-empty">${ICON.alert}<b>Couldn't load the diff</b><span>${esc(d.error)}</span></div>`; return; }
    if (!d.diff || !d.diff.trim()) { box.innerHTML = `<div class="mp-diff-empty">${ICON.file}<b>No text changes</b><span>Only the file mode (or nothing visible) changed.</span></div>`; return; }
    box.innerHTML = (d.truncated ? '<div class="mp-diff-note">Diff truncated at 200 KB — open it in your editor for the rest.</div>' : '') +
      '<div class="mp-diff-body">' + diffHTML(d.diff, s.kind === 'file') + '</div>';
    box.scrollTop = keep[0];
    box.scrollLeft = keep[1];
  }

  // diffHTML renders unified (and combined "diff --cc") diffs: file
  // headers, hunk headers, numbered +/− lines; text before the first
  // file (a commit's header and --stat) as a preamble block.
  function diffHTML(text, single) {
    const lines = text.replace(/\r\n/g, '\n').split('\n');
    if (lines[lines.length - 1] === '') lines.pop();
    const out = [];
    let pre = [], oldN = 0, newN = 0, inHunk = false, width = 1;
    const flushPre = () => {
      if (!pre.length) return;
      while (pre.length && !pre[pre.length - 1].trim()) pre.pop();
      if (pre.length) out.push(`<div class="dpre">${pre.map(l => statLine(l)).join('\n')}</div>`);
      pre = [];
    };
    const row = (cls, a, b, sign, code) =>
      `<div class="dl ${cls}"><span class="ln">${a}</span><span class="ln">${b}</span><span class="sg">${sign}</span><span class="cd">${esc(code) || ' '}</span></div>`;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (line.startsWith('diff --git ') || line.startsWith('diff --cc ') || line.startsWith('diff --combined ')) {
        flushPre();
        inHunk = false;
        const m = / b\/(.*)$/.exec(line) || /^diff --(?:cc|combined) (.*)$/.exec(line);
        // One file's diff: the pane's header already names it.
        out.push(single ? '<div class="dfile-gap"></div>' : `<div class="dfile">${ICON.file}<span>${esc(m ? m[1] : line)}</span></div>`);
        continue;
      }
      const hm = /^(@@+) -(\d+)(?:,\d+)?(?: -\d+(?:,\d+)?)* \+(\d+)(?:,\d+)? @@+(.*)$/.exec(line);
      if (hm && (inHunk || out.length)) {
        flushPre();
        width = hm[1].length - 1;
        oldN = +hm[2];
        newN = +hm[3];
        inHunk = true;
        out.push(`<div class="dl hunk"><span class="ln"></span><span class="ln"></span><span class="sg"></span><span class="cd">${esc(line)}</span></div>`);
        continue;
      }
      if (!inHunk) {
        if (out.length && /^(index |--- |\+\+\+ |similarity index|dissimilarity index|copy from|copy to)/.test(line)) continue;
        if (out.length && /^(new file mode|deleted file mode|old mode|new mode|rename from|rename to)/.test(line)) { out.push(`<div class="dmeta">${esc(line)}</div>`); continue; }
        if (/^Binary files .* differ$/.test(line)) { out.push('<div class="dmeta">Binary file — no line diff</div>'); continue; }
        pre.push(line);
        continue;
      }
      const sign = line.slice(0, width);
      const code = line.slice(width);
      if (line.startsWith('\\')) { out.push(`<div class="dl nonl"><span class="ln"></span><span class="ln"></span><span class="sg"></span><span class="cd">${esc(line.slice(2))}</span></div>`); continue; }
      if (sign.includes('+')) { out.push(row('add', '', newN++, '+', code)); continue; }
      if (sign.includes('-')) { out.push(row('del', oldN++, '', '−', code)); continue; }
      if (/^ *$/.test(sign) && (line.length >= width || line === '')) { out.push(row('ctx', oldN++, newN++, '', code)); continue; }
      // Anything else ends the hunk (e.g. the next commit in a log).
      inHunk = false;
      pre.push(line);
    }
    flushPre();
    return out.join('');
  }

  // statLine colours a --stat line's +/− bar ("file | 12 ++++---").
  function statLine(l) {
    const m = /^( .*\| +\d+ )(\+*)(-*)$/.exec(l);
    if (m) return esc(m[1]) + `<span class="plus">${m[2]}</span><span class="minus">${m[3]}</span>`;
    return esc(l) || ' ';
  }

  /* ── commit box ──────────────────────────────────────────────── */
  function buildCommitBox() {
    const box = $g('commit');
    if (!box) return;
    const mod = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent) ? '⌘' : 'Ctrl+';
    box.innerHTML = `
      <div class="mp-git-msg">
        <textarea data-g="msg" rows="3" spellcheck="true" aria-label="Commit message" placeholder="Commit message"></textarea>
        <span class="cnt" data-g="msgcount" title="subject line length — keep it under 72"></span>
      </div>
      <div class="mp-git-opts">
        <label class="mp-git-check"><input type="checkbox" data-g="amend"><span>Amend last commit</span></label>
        <label class="mp-git-check"><input type="checkbox" data-g="all"><span>Commit all changes</span></label>
      </div>
      <div class="mp-git-hint" data-g="commithint"></div>
      <div class="mp-git-cbtns">
        <button class="primary mp-btn-ic" data-g="commitbtn" title="Commit (${mod}Enter)">${ICON.check}Commit</button>
        <button class="mp-btn-ic" data-g="commitpush" title="Commit, then push">${ICON.push}Commit &amp; Push</button>
      </div>`;
    const ta = $g('msg');
    ta.value = gitState.draft;
    ta.dataset.mod = mod;
    ta.oninput = () => { gitState.draft = ta.value; paintCommitBox(); };
    ta.onkeydown = e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); gitCommit(false); } };
    $g('amend').onchange = e => { gitState.amend = e.target.checked; paintCommitBox(); };
    $g('all').onchange = e => { gitState.all = e.target.checked; paintCommitBox(); };
    $g('commitbtn').onclick = () => gitCommit(false);
    $g('commitpush').onclick = () => gitCommit(true);
  }

  // commitState says whether Commit can run, and why not.
  function commitState() {
    const st = gitState.st;
    const merging = st.mergeInProgress || st.cherryPickInProgress;
    const msg = gitState.draft.trim();
    if (st.rebaseInProgress) return { ok: false, why: 'A rebase is in progress — use Continue in the banner above.' };
    if (st.conflicted.length) return { ok: false, why: 'Resolve the conflicts first, then stage the files.' };
    const anything = st.staged.length || (gitState.all && gitTotal(st)) || gitState.amend || merging;
    if (!anything) return { ok: false, why: gitTotal(st) ? 'Nothing staged — stage files above, or tick “Commit all changes”.' : 'Nothing to commit.' };
    if (!msg && !gitState.amend && !merging) return { ok: false, why: 'Write a message to commit.', soft: true };
    let why = '';
    if (gitState.amend && !msg) why = 'Leaving the message empty keeps the last commit\'s message.';
    else if (merging && !msg) why = 'Leave the message empty to use Git\'s merge message.';
    else if (gitState.all && !st.staged.length) why = `All ${gitTotal(st)} changed file${gitTotal(st) === 1 ? '' : 's'} will be staged and committed.`;
    return { ok: true, why };
  }

  function paintCommitBox() {
    const box = $g('commit');
    if (!box) return;
    box.hidden = gitState.view !== 'changes';
    const st = gitState.st;
    const ta = $g('msg');
    const subj = (gitState.draft.split('\n')[0] || '').trim();
    const cnt = $g('msgcount');
    cnt.textContent = subj.length ? subj.length + '/72' : '';
    cnt.classList.toggle('over', subj.length > 72);
    ta.placeholder = st.mergeInProgress ? 'Merge message (optional)'
      : `Message — commit to “${branchLabel(st)}” (${ta.dataset.mod}Enter)`;
    $g('amend').checked = gitState.amend;
    $g('amend').disabled = !st.lastCommit;
    $g('all').checked = gitState.all;
    const cs = commitState();
    const hint = $g('commithint');
    hint.textContent = cs.why || '';
    hint.classList.toggle('bad', !cs.ok && !cs.soft);
    const running = gitRunning();
    $g('commitbtn').disabled = !cs.ok;
    $g('commitpush').disabled = !cs.ok || !st.hasRemote || st.detached || running;
    $g('commitpush').title = !st.hasRemote ? 'No remote to push to' : 'Commit, then push';
  }

  async function gitCommit(andPush) {
    const cs = commitState();
    if (!cs.ok) { say(cs.why); return; }
    const st = gitState.st;
    const g = gen;
    // Amending a commit that's already on the remote rewrites history.
    const pushedAmend = gitState.amend && st.upstream && !st.ahead && !st.initial;
    if (pushedAmend) {
      const ok = await confirmBox({
        title: 'Amend a pushed commit?',
        message: `The last commit is already on <span class="mono">${esc(st.upstream)}</span>. Amending rewrites it, so pushing afterwards needs a force push${andPush ? ' (with lease — done for you)' : ''}.`,
        confirmText: 'Amend', danger: true,
      });
      if (!ok) return;
    }
    const btns = [$g('commitbtn'), $g('commitpush')];
    btns.forEach(b => { if (b) b.disabled = true; });
    const r = await gitWrite('commit', { message: gitState.draft, amend: gitState.amend, all: gitState.all });
    if (!live(g)) return;
    if (!r) { paintCommitBox(); return; }
    const amended = gitState.amend;
    gitState.draft = '';
    gitState.amend = false;
    gitState.all = false;
    const ta = $g('msg');
    if (ta) ta.value = '';
    gitState.log = null;
    say((amended ? 'Amended ' : 'Committed ') + (r.short || ''));
    paintCommitBox();
    if (andPush) gitPush({ setUpstream: !st.upstream, force: !!pushedAmend });
  }

  /* ── history ─────────────────────────────────────────────────── */
  function refBadges(refs) {
    const out = [];
    for (const r of refs || []) {
      if (r.startsWith('HEAD -> ')) { out.push('<span class="mp-ref head">HEAD</span>', `<span class="mp-ref local">${esc(r.slice(8))}</span>`); continue; }
      if (r === 'HEAD') { out.push('<span class="mp-ref head">HEAD</span>'); continue; }
      if (r.startsWith('tag: ')) { out.push(`<span class="mp-ref tag">${ICON.tag}${esc(r.slice(5))}</span>`); continue; }
      if (r.endsWith('/HEAD')) continue;
      out.push(`<span class="mp-ref ${r.includes('/') && gitState.st && gitState.st.remote && r.startsWith(gitState.st.remote + '/') ? 'remote' : 'local'}">${esc(r)}</span>`);
    }
    return out.join('');
  }

  async function paintHistory(box) {
    const g = gen;
    if (!gitState.log) {
      box.innerHTML = '<div class="mp-loading"><div class="spin"></div>Reading history…</div>';
      try {
        const d = await call('/api/project/git/log', { site: cur, limit: gitState.logLimit });
        if (!live(g)) return;
        gitState.log = d.commits || [];
      } catch (e) {
        if (!live(g)) return;
        box.innerHTML = `<div class="mp-empty"><b>Couldn't read the history</b>${esc(e.message)}</div>`;
        return;
      }
      if (tab !== 'git' || gitState.view !== 'history') return;
    }
    const list = gitState.log;
    if (!list.length) { box.innerHTML = '<div class="mp-git-clean">' + ICON.commit + '<b>No commits yet</b><span>Your first commit starts the history.</span></div>'; return; }
    const s = gitState.sel;
    const st = gitState.st;
    const unpushed = st.upstream ? st.ahead : 0;
    box.innerHTML = '<div class="mp-git-hist">' + list.map((c, i) => `<div class="mp-gc${s && s.kind === 'commit' && s.hash === c.hash ? ' sel' : ''}" tabindex="0" role="button" data-hash="${esc(c.hash)}" title="${esc(c.subject)}">
        <span class="dot${i < unpushed ? ' out' : ''}" title="${i < unpushed ? 'not pushed yet' : ''}"></span>
        <div class="t"><div class="s">${esc(c.subject)}</div>
          <div class="m">${refBadges(c.refs)}<span class="mp-git-hash">${esc(c.short)}</span><span>${esc(c.author)}</span><span title="${esc(c.date)}">${esc(c.rel)}</span></div></div>
      </div>`).join('') + '</div>' +
      (list.length >= gitState.logLimit ? '<div style="text-align:center; padding:10px 0 14px"><button class="sm" data-g="log-more">Load older commits</button></div>' : '');
  }

  /* ── stashes ─────────────────────────────────────────────────── */
  async function paintStashes(box) {
    const g = gen;
    if (!gitState.stashes) {
      box.innerHTML = '<div class="mp-loading"><div class="spin"></div>Reading stashes…</div>';
      try {
        const d = await call('/api/project/git/stash', { site: cur, action: 'list' });
        if (!live(g)) return;
        gitState.stashes = d.stashes || [];
      } catch (e) {
        if (!live(g)) return;
        box.innerHTML = `<div class="mp-empty"><b>Couldn't read the stashes</b>${esc(e.message)}</div>`;
        return;
      }
      if (tab !== 'git' || gitState.view !== 'stashes') return;
    }
    const list = gitState.stashes;
    const s = gitState.sel;
    const n = gitTotal(gitState.st);
    box.innerHTML = `<div class="mp-git-tools"><button class="sm primary mp-btn-ic" data-g="stash-new" ${n ? '' : 'disabled'}>${ICON.stash}Stash changes…</button>
        <span class="mp-sub">${n ? n + ' changed file' + (n === 1 ? '' : 's') : 'Nothing to stash'}</span></div>` +
      (list.length ? list.map(x => `<div class="mp-gc${s && s.kind === 'stash' && s.index === x.index ? ' sel' : ''}" tabindex="0" role="button" data-stash="${x.index}">
          <span class="mp-git-hash">${esc(x.ref)}</span>
          <div class="t"><div class="s">${esc(x.message)}</div><div class="m"><span>${esc(x.rel)}</span></div></div>
          <span class="acts"><button class="sm" data-st="pop" title="Apply it and remove it from the list">Pop</button><button class="mp-icon-btn danger" data-st="drop" title="Drop (delete) this stash" aria-label="Drop">${ICON.trash}</button></span>
        </div>`).join('')
        : `<div class="mp-git-clean">${ICON.stash}<b>No stashes</b><span>Stashing sets your uncommitted work aside — to switch branches or pull — and brings it back later.</span></div>`);
    const nb = box.querySelector('[data-g="stash-new"]');
    if (nb) nb.onclick = () => gitStashNew();
  }

  async function gitStashNew() {
    const r = await modalBox({
      title: 'Stash changes',
      message: 'Sets your uncommitted changes aside and gives you a clean working tree. Pop the stash to bring them back.',
      input: { label: 'Message (optional)', placeholder: 'WIP: what you were doing' },
      checkboxes: [{ id: 'untracked', label: 'Include untracked files', hint: 'new files Git isn\'t tracking yet', checked: true }],
      confirmText: 'Stash',
    });
    if (!r) return;
    const ok = await gitWrite('stash', { action: 'push', message: r.input || '', includeUntracked: !!(r.checks && r.checks.untracked) }, 'Changes stashed');
    if (ok) { gitState.stashes = null; if (gitState.view === 'stashes') paintGitList(); }
  }

  async function stashAction(act, rowEl, btn, index) {
    if (index == null) index = +rowEl.dataset.stash;
    if (act === 'drop') {
      const ok = await confirmBox({ title: 'Drop stash@{' + index + '}?', message: 'The stashed changes are deleted. <b>This can\'t be undone.</b>', confirmText: 'Drop', danger: true });
      if (!ok) return;
    }
    const r = await gitWrite('stash', { action: act, index }, act === 'pop' ? 'Stash applied and removed' : 'Stash dropped', false, btn);
    gitState.stashes = null;
    if (r && gitState.sel && gitState.sel.kind === 'stash') { gitState.sel = null; gitState.diff = null; paintGitDiff(); }
    if (gitState.view === 'stashes') paintGitList();
  }

  /* ════════════════════════════════════════════════════════════════
     CACHE — this project's keys in Mullion's Redis: only those under
     its prefix (from its .env, see redis.ProjectSettings), a bounded
     preview, delete, flush-by-prefix, a live MONITOR, and the "Use
     Redis in this project" .env edit.
     ════════════════════════════════════════════════════════════════ */
  function cacheFresh() {
    return { info: null, err: '', db: null, q: '', keys: [], cursor: '0', loading: false, sel: null, preview: null,
      mon: null, monLines: [], monPaused: false, monDropped: 0, monUntil: 0, monMinutes: 5, stats: null };
  }
  const fmtTTL = ms => {
    if (ms == null || ms < 0) return '—';
    const s = Math.round(ms / 1000);
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
    if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
    return Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h';
  };
  const fmtBytes = n => {
    if (!n) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1048576).toFixed(1) + ' MB';
  };

  async function renderCache() {
    const g = gen;
    const cs = cacheState;
    els.panel.innerHTML = `<section class="card"><div class="mp-loading"><div class="spin"></div>Reading this project's Redis settings…</div></section>`;
    try {
      cs.info = await call('/api/project/cache/info', { site: cur });
      cs.err = '';
    } catch (e) {
      cs.info = null;
      cs.err = e.message;
    }
    if (!live(g) || tab !== 'cache') return;
    if (cs.db === null && cs.info) cs.db = cs.info.settings.cacheDb;
    paintCache();
    if (cs.info && cs.info.running) {
      loadCacheKeys(true);
      loadCacheStats();
    }
  }

  function paintCache() {
    const cs = cacheState, inf = cs.info;
    if (!inf) {
      els.panel.innerHTML = `<section class="card"><div class="card-body"><div class="mp-empty"><b>Couldn't read the cache settings</b>${esc(cs.err)}</div></div></section>`;
      return;
    }
    const st = inf.settings;
    const drivers = [['Cache', st.cacheStore], ['Sessions', st.sessionDriver], ['Queue', st.queueConnection]]
      .map(([k, v]) => `<span class="mp-chip" style="padding:3px 9px">${k}: <b class="mono">${esc(v || 'default')}</b></span>`).join(' ');
    let warn = '';
    if (!inf.installed) warn = `<div class="mp-warn">Mullion's Redis isn't installed — <a data-go-db>install it on the Databases page</a> (Redis tab).</div>`;
    else if (!inf.running) warn = `<div class="mp-warn">Redis isn't running — <a data-go-db>start it on the Databases page</a>.</div>`;
    else if (!st.pointsHere && st.hasEnv && (st.usesRedis || st.host !== '127.0.0.1' || st.port !== inf.port))
      warn = `<div class="mp-warn">This project's <span class="mono">.env</span> points at <span class="mono">${esc(st.host)}:${st.port}</span>, not Mullion's Redis on <span class="mono">127.0.0.1:${inf.port}</span>.</div>`;
    const plan = inf.plan || [];
    const keysNote = inf.running ? `${fmtNum(inf.keys)}${inf.more ? '+' : ''} key${inf.keys === 1 ? '' : 's'} in db${st.db}` +
      (st.cacheDb !== st.db ? ` · ${fmtNum(inf.cacheKeys)}${inf.more ? '+' : ''} in db${st.cacheDb} (cache)` : '') : '';
    let html = `
      <section class="card">
        <div class="card-head">${ICON.cache}<h2>Redis</h2><span class="hint">${esc(keysNote)}</span><span class="spacer"></span>
          ${plan.length ? `<button class="sm primary" data-id="mp-c-use">Use Redis in this project…</button>` : (st.usesRedis ? '<span class="mp-state running">using Redis</span>' : '')}
          ${inf.running ? `<button class="sm mp-btn-ic" data-id="mp-c-admin">${ICON.open}phpRedisAdmin</button>` : ''}
          ${inf.running ? `<button class="sm danger" data-id="mp-c-flush">Flush this project's cache…</button>` : ''}
        </div>
        <div class="card-body">
          ${warn}
          <div class="mp-kv">
            <div class="k">Connection<small>REDIS_HOST / PORT / CLIENT</small></div>
            <div class="mono">${esc(st.host)}:${st.port} <span class="mp-sub">${esc(st.client)}</span></div>
            <div class="k">Key prefix<small>REDIS_PREFIX — every key</small></div>
            <div class="mono">${st.prefix ? esc(st.prefix) : '<span class="mp-sub">none</span>'}</div>
            <div class="k">Cache prefix<small>CACHE_PREFIX — after the key prefix</small></div>
            <div class="mono">${st.cachePrefix ? esc(st.cachePrefix) : '<span class="mp-sub">none</span>'}</div>
            <div class="k">Databases<small>REDIS_DB / REDIS_CACHE_DB</small></div>
            <div class="mono">db${st.db} (sessions, queues) · db${st.cacheDb} (cache)</div>
            <div class="k">Drivers<small>from .env</small></div>
            <div>${drivers}</div>
          </div>
          <div class="redis-stats mp-c-stats" data-id="mp-c-stats" style="margin-top:14px"></div>
        </div>
      </section>`;
    if (inf.running) {
      const dbs = [st.db];
      if (st.cacheDb !== st.db) dbs.push(st.cacheDb);
      html += `
      <section class="card">
        <div class="card-head">${ICON.search}<h2>Keys</h2><span class="hint">only keys starting with <span class="mono">${esc(st.prefix || '(any)')}</span></span><span class="spacer"></span>
          <div class="mp-seg" role="tablist">${dbs.map(d => `<button data-cdb="${d}" class="${cs.db === d ? 'active' : ''}">db${d}${d === st.cacheDb ? ' · cache' : ''}${d === st.db ? (d === st.cacheDb ? '' : ' · default') : ''}</button>`).join('')}</div>
        </div>
        <div class="card-body">
          <div class="row" style="gap:8px; margin-bottom:12px">
            <div class="mp-search">${ICON.search}<input type="text" data-id="mp-c-q" placeholder="Filter keys containing…" value="${esc(cs.q)}" spellcheck="false"></div>
            <button class="sm mp-btn-ic" data-id="mp-c-refresh">${ICON.fetch}Refresh</button>
          </div>
          <div data-id="mp-c-keys"></div>
          <div data-id="mp-c-preview"></div>
        </div>
      </section>
      <section class="card">
        <div class="card-head">${ICON.bolt}<h2>Live monitor</h2><span class="hint" data-id="mp-c-mon-hint">every command touching this project's keys, as it happens</span><span class="spacer"></span>
          <select data-id="mp-c-mon-min" class="sm" title="stops by itself after">${[1, 5, 15, 30].map(m => `<option value="${m}"${cs.monMinutes === m ? ' selected' : ''}>${m} min</option>`).join('')}</select>
          <button class="sm primary mp-btn-ic" data-id="mp-c-mon-start">${ICON.play}Start</button>
          <button class="sm" data-id="mp-c-mon-pause" hidden>Pause</button>
          <button class="sm" data-id="mp-c-mon-clear">Clear</button>
          <button class="sm danger" data-id="mp-c-mon-stop" hidden>Stop</button>
        </div>
        <div class="card-body">
          <div class="mp-sub" style="margin-bottom:8px">Uses Redis's MONITOR, which slows the server a little while it runs — so it stops by itself after the time you pick. Mullion's own lookups are left out.</div>
          <pre class="mp-term short" data-id="mp-c-mon-out"></pre>
        </div>
      </section>`;
    }
    els.panel.innerHTML = html;
    const p = els.panel;
    p.querySelectorAll('[data-go-db]').forEach(a => a.onclick = () => { location.hash = 'database'; });
    const q = sel => p.querySelector(`[data-id="${sel}"]`);
    if (q('mp-c-use')) q('mp-c-use').onclick = useRedisInProject;
    if (q('mp-c-admin')) q('mp-c-admin').onclick = () => openAdminDb(cs.db);
    if (q('mp-c-flush')) q('mp-c-flush').onclick = flushProjectCache;
    if (!inf.running) return;
    p.querySelectorAll('[data-cdb]').forEach(b => b.onclick = () => {
      cs.db = +b.dataset.cdb; cs.sel = null; cs.preview = null;
      p.querySelectorAll('[data-cdb]').forEach(x => x.classList.toggle('active', +x.dataset.cdb === cs.db));
      loadCacheKeys(true);
    });
    let qt = 0;
    q('mp-c-q').oninput = e => {
      clearTimeout(qt);
      qt = setTimeout(() => { cs.q = e.target.value.trim(); loadCacheKeys(true); }, 300);
    };
    q('mp-c-refresh').onclick = () => { loadCacheKeys(true); loadCacheStats(); };
    q('mp-c-mon-min').onchange = e => { cs.monMinutes = +e.target.value; };
    q('mp-c-mon-start').onclick = startMonitor;
    q('mp-c-mon-stop').onclick = () => { stopMonitor(); paintMonitor('stopped'); };
    q('mp-c-mon-pause').onclick = () => { cs.monPaused = !cs.monPaused; paintMonitor(); };
    q('mp-c-mon-clear').onclick = () => { cs.monLines = []; cs.monDropped = 0; paintMonitor(); };
    paintMonitor();
    paintCacheStats();
  }

  async function loadCacheStats() {
    const g = gen;
    try {
      const st = await call('/api/project/cache/stats', {});
      if (!live(g)) return;
      cacheState.stats = st;
      paintCacheStats();
    } catch {}
  }
  function paintCacheStats() {
    const box = els.panel && els.panel.querySelector('[data-id="mp-c-stats"]');
    const st = cacheState.stats;
    if (!box || !st) return;
    const tiles = [
      [st.memoryHuman || '0B', 'Redis memory (all projects)'],
      [st.hitRate < 0 ? '–' : st.hitRate.toFixed(1) + '%', 'hit rate (all projects)'],
      [String(st.opsPerSec), 'ops / second'],
      [String(st.clients), 'connected clients'],
    ];
    box.innerHTML = tiles.map(([v, l]) => `<div class="st"><b>${esc(v)}</b><span>${esc(l)}</span></div>`).join('');
  }

  async function loadCacheKeys(reset) {
    const g = gen, cs = cacheState;
    if (cs.loading) return;
    if (reset) { cs.keys = []; cs.cursor = '0'; }
    cs.loading = true;
    paintKeys();
    try {
      const page = await call('/api/project/cache/keys', { site: cur, db: cs.db, q: cs.q, cursor: reset ? '' : cs.cursor, count: 100 });
      if (!live(g)) return;
      const seen = new Set(cs.keys.map(k => k.key));
      page.keys.forEach(k => { if (!seen.has(k.key)) cs.keys.push(k); });
      cs.cursor = page.cursor;
      cs.err = '';
    } catch (e) {
      cs.err = e.message;
    } finally {
      cs.loading = false;
    }
    if (live(g) && tab === 'cache') paintKeys();
  }

  function paintKeys() {
    const cs = cacheState;
    const box = els.panel && els.panel.querySelector('[data-id="mp-c-keys"]');
    if (!box) return;
    if (cs.err && !cs.keys.length) { box.innerHTML = `<div class="mp-warn">${esc(cs.err)}</div>`; return; }
    if (!cs.keys.length) {
      box.innerHTML = cs.loading ? `<div class="mp-loading"><div class="spin"></div>Scanning…</div>`
        : `<div class="mp-empty"><b>No keys${cs.q ? ' matching that' : ''}</b>${cs.q ? 'Try another filter.' : 'Nothing under this project\'s prefix in db' + cs.db + ' yet.'}</div>`;
      return;
    }
    const pfx = (cs.info && cs.info.settings.prefix) || '';
    const rows = cs.keys.map((k, i) => `<tr data-ki="${i}" class="${cs.sel === k.key ? 'sel' : ''}" style="cursor:pointer">
        <td class="mono" style="word-break:break-all"><span class="mp-sub">${esc(pfx)}</span>${esc(k.key.slice(pfx.length))}</td>
        <td><span class="mp-chip" style="padding:1px 7px">${esc(k.type)}</span></td>
        <td class="mono">${fmtTTL(k.ttl)}</td>
        <td class="mono">${fmtBytes(k.memory)}</td>
        <td style="text-align:right"><button class="sm danger mp-btn-ic" data-kdel="${i}" title="delete this key">${ICON.trash}</button></td>
      </tr>`).join('');
    const more = cs.cursor && cs.cursor !== '0';
    box.innerHTML = `<table class="mp-table"><thead><tr><th>Key</th><th>Type</th><th>TTL</th><th>Size</th><th></th></tr></thead><tbody>${rows}</tbody></table>
      <div class="row" style="margin-top:10px; gap:10px"><span class="mp-sub">${cs.keys.length} key${cs.keys.length === 1 ? '' : 's'} shown${more ? ' — more to scan' : ''}</span><span class="spacer"></span>
      ${more ? `<button class="sm" data-id="mp-c-more"${cs.loading ? ' disabled' : ''}>${cs.loading ? 'Scanning…' : 'Load more'}</button>` : ''}</div>`;
    box.querySelectorAll('[data-ki]').forEach(tr => tr.onclick = e => {
      if (e.target.closest('[data-kdel]')) return;
      showKey(cs.keys[+tr.dataset.ki].key);
    });
    box.querySelectorAll('[data-kdel]').forEach(b => b.onclick = () => deleteKey(cs.keys[+b.dataset.kdel].key));
    const m = box.querySelector('[data-id="mp-c-more"]');
    if (m) m.onclick = () => loadCacheKeys(false);
  }

  async function showKey(key) {
    const g = gen, cs = cacheState;
    cs.sel = key;
    paintKeys();
    const box = els.panel.querySelector('[data-id="mp-c-preview"]');
    box.innerHTML = `<div class="mp-loading"><div class="spin"></div>Reading ${esc(key)}…</div>`;
    let pv;
    try {
      pv = await call('/api/project/cache/key', { site: cur, db: cs.db, key });
    } catch (e) {
      if (live(g)) box.innerHTML = `<div class="mp-warn">${esc(e.message)}</div>`;
      return;
    }
    if (!live(g) || cs.sel !== key) return;
    let body = '';
    if (pv.type === 'string') body = esc(prettyMaybe(pv.value));
    else if (pv.fields) body = pv.fields.map(f => `<span class="a-36">${esc(f.name)}</span>  ${esc(f.value)}`).join('\n');
    else if (pv.items) body = pv.items.map((x, i) => `<span class="dim">${i}</span>  ${esc(x)}`).join('\n');
    const size = pv.type === 'string' ? fmtBytes(pv.size) : pv.size + ' element' + (pv.size === 1 ? '' : 's');
    box.innerHTML = `<div style="margin-top:14px">
      <div class="row" style="gap:8px; margin-bottom:8px"><b class="mono" style="word-break:break-all">${esc(key)}</b>
        <span class="mp-chip" style="padding:1px 7px">${esc(pv.type)}</span><span class="mp-sub">${esc(size)} · TTL ${fmtTTL(pv.ttl)} · ${fmtBytes(pv.memory)} in memory${pv.truncated ? ' · preview truncated' : ''}</span>
        <span class="spacer"></span><button class="sm mp-btn-ic" data-id="mp-c-copy">${ICON.copy}Copy</button><button class="sm mp-btn-ic" data-id="mp-c-close" title="close">${ICON.x}</button></div>
      <pre class="mp-term short">${body || '<span class="dim">(empty)</span>'}</pre></div>`;
    box.querySelector('[data-id="mp-c-close"]').onclick = () => { cs.sel = null; box.innerHTML = ''; paintKeys(); };
    box.querySelector('[data-id="mp-c-copy"]').onclick = async () => {
      const text = pv.type === 'string' ? pv.value
        : pv.fields ? pv.fields.map(f => f.name + '\t' + f.value).join('\n') : (pv.items || []).join('\n');
      say((await copyText(text)) ? 'Copied' : 'Copy failed');
    };
  }
  // prettyMaybe indents JSON values; PHP-serialized and other values show as they are.
  function prettyMaybe(v) {
    const t = (v || '').trim();
    if (t.length > 1 && (t[0] === '{' || t[0] === '[')) { try { return JSON.stringify(JSON.parse(t), null, 2); } catch {} }
    return v || '';
  }

  async function deleteKey(key) {
    const ok = await confirmBox({ title: 'Delete this key?', message: `Delete <span class="mono">${esc(key)}</span> from db${cacheState.db}?`, confirmText: 'Delete', danger: true });
    if (!ok) return;
    try {
      await call('/api/project/cache/delete', { site: cur, db: cacheState.db, key });
      cacheState.keys = cacheState.keys.filter(k => k.key !== key);
      if (cacheState.sel === key) { cacheState.sel = null; const b = els.panel.querySelector('[data-id="mp-c-preview"]'); if (b) b.innerHTML = ''; }
      paintKeys();
      say('Deleted');
    } catch (e) { say('Error: ' + e.message); }
  }

  async function flushProjectCache() {
    const st = cacheState.info.settings;
    const ok = await confirmBox({
      title: "Flush this project's cache?",
      message: `Delete every key starting with <span class="mono">${esc(st.prefix + st.cachePrefix)}</span> in <span class="mono">db${st.cacheDb}</span>?`,
      detail: 'Only this project\'s cache entries go — sessions, queued jobs and other projects are left alone. Keys are found with SCAN and removed with UNLINK, so Redis isn\'t blocked.',
      confirmText: 'Flush cache', danger: true,
    });
    if (!ok) return;
    try {
      const r = await busy('Flushing…', () => call('/api/project/cache/flush', { site: cur }));
      say(`Deleted ${r.deleted} key${r.deleted === 1 ? '' : 's'}`);
    } catch (e) { say('Error: ' + e.message); }
    renderCache();
  }

  async function useRedisInProject() {
    const plan = (cacheState.info && cacheState.info.plan) || [];
    if (!plan.length) return;
    const diff = plan.map(c => (c.exists ? `<span style="color:var(--bad)">- ${esc(c.key)}=${esc(c.old)}</span>\n` : '') +
      `<span style="color:var(--ok)">+ ${esc(c.key)}=${esc(c.new)}</span>`).join('\n');
    const r = await modalBox({
      title: 'Use Redis in this project?',
      message: `Update <span class="mono">.env</span> so this project uses Mullion's Redis (127.0.0.1:${cacheState.info.port}). The current file is saved as <span class="mono">.env.mullion-backup</span> first.`,
      detail: `<pre class="mono" style="white-space:pre-wrap; margin:0; font-size:12px">${diff}</pre>`,
      checkboxes: plan.map(c => ({ id: c.key, label: `<span class="mono">${esc(c.key)}=${esc(c.new)}</span>`, hint: c.exists ? 'was ' + esc(c.old || '(empty)') : 'new', checked: true })),
      confirmText: 'Update .env',
    });
    if (!r) return;
    const keys = plan.map(c => c.key).filter(k => r.checks && r.checks[k]);
    if (!keys.length) return say('Nothing selected');
    try {
      const res = await busy('Updating .env…', () => call('/api/project/cache/use-redis', { site: cur, keys }));
      say('.env updated' + (res.backup ? ' — backup in .env.mullion-backup' : '') + '. Restart queue workers to pick it up.');
    } catch (e) { say('Error: ' + e.message); }
    renderCache();
  }

  function openAdminDb(db) {
    const known = window.__adminTools && window.__adminTools.phpredisadmin;
    (known ? Promise.resolve({ adminTools: [known] }) : call('/api/state', {})).then(s => {
      const t = (s.adminTools || []).find(x => x.name === 'phpredisadmin');
      if (t && t.installed && t.url) return void window.open(t.url.replace(/\/?$/, '/?s=0&d=' + db), '_blank');
      say('Opening phpRedisAdmin — set it up from Databases → Redis first');
      location.hash = 'database';
    }).catch(e => say('Error: ' + e.message));
  }

  /* ── live monitor (SSE) ── */
  function startMonitor() {
    const cs = cacheState;
    stopMonitor();
    const url = '/api/redis/monitor?t=' + encodeURIComponent(TOKEN) + '&site=' + encodeURIComponent(cur) + '&minutes=' + cs.monMinutes;
    const es = new EventSource(url);
    cs.mon = es;
    cs.monPaused = false;
    es.addEventListener('start', ev => { const d = JSON.parse(ev.data); cs.monUntil = d.until * 1000; paintMonitor('live'); });
    cs.monTick = setInterval(paintMonitorHint, 1000);
    es.addEventListener('cmds', ev => {
      if (cs.mon !== es) return;
      const d = JSON.parse(ev.data);
      cs.monDropped += d.dropped || 0;
      if (!cs.monPaused) {
        d.events.forEach(e => cs.monLines.push(e));
        if (cs.monLines.length > 500) cs.monLines.splice(0, cs.monLines.length - 500);
      }
      paintMonitor();
    });
    es.addEventListener('end', ev => { const d = JSON.parse(ev.data); stopMonitor(); paintMonitor(d.reason); });
    es.onerror = () => {
      if (cs.mon !== es) return;
      stopMonitor();
      paintMonitor('disconnected (Redis stopped, or too many monitors open)');
    };
    paintMonitor('connecting…');
  }
  function stopMonitor() {
    if (!cacheState) return;
    clearInterval(cacheState.monTick);
    cacheState.monTick = 0;
    if (cacheState.mon) {
      try { cacheState.mon.close(); } catch {}
      cacheState.mon = null;
    }
  }
  function paintMonitorHint() {
    const cs = cacheState, h = els.panel && els.panel.querySelector('[data-id="mp-c-mon-hint"]');
    if (!h) return;
    const left = cs.mon && cs.monUntil ? ' · stops in ' + fmtTTL(Math.max(0, cs.monUntil - Date.now())) : '';
    h.textContent = (cs.monStatus || 'every command touching this project\'s keys, as it happens') + (cs.monPaused ? ' · paused' : '') + left +
      (cs.monDropped ? ` · ${cs.monDropped} skipped (too fast to show)` : '');
  }
  function paintMonitor(status) {
    const cs = cacheState, p = els.panel;
    const out = p && p.querySelector('[data-id="mp-c-mon-out"]');
    if (!out) return;
    const on = !!cs.mon;
    const q = sel => p.querySelector(`[data-id="${sel}"]`);
    q('mp-c-mon-start').hidden = on;
    q('mp-c-mon-stop').hidden = !on;
    q('mp-c-mon-pause').hidden = !on;
    q('mp-c-mon-pause').textContent = cs.monPaused ? 'Resume' : 'Pause';
    q('mp-c-mon-min').disabled = on;
    if (status) cs.monStatus = status;
    paintMonitorHint();
    const atBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 30;
    out.innerHTML = cs.monLines.length ? cs.monLines.map(e => {
      const t = new Date(e.time * 1000);
      const ts = t.toLocaleTimeString([], { hour12: false }) + '.' + String(t.getMilliseconds()).padStart(3, '0');
      return `<span class="dim">${ts} db${e.db}</span> <span class="a-36">${esc(e.cmd)}</span>${esc(e.text.slice(e.cmd.length))}`;
    }).join('\n') : `<span class="dim">${on ? 'Waiting for commands — use the site to see its cache traffic…' : 'Press Start to watch this project\'s Redis traffic live.'}</span>`;
    if (atBottom) out.scrollTop = out.scrollHeight;
  }

  /* ════════════════════════════════════════════════════════════════
     WATCH — what the site does while it's used, live (internal/watch):
     requests from Caddy's access log, Laravel's log files, and — with
     Mullion's PHP prepend loaded for this site — dumps, queries, jobs,
     cache, events, views, HTTP calls and a per-request timeline. The
     stream (/api/watch/stream, SSE) runs while this project is mounted
     and its watcher is on, so exceptions can flag the tab strip.
     ════════════════════════════════════════════════════════════════ */
  const W_CAP = { query: 3000, request: 1500, log: 1500, def: 1000 };
  const W_SUBS = [
    ['requests', 'Requests', 'l1'], ['timeline', 'Timeline', 'deep'], ['queries', 'Queries', 'deep'],
    ['jobs', 'Jobs', 'deep'], ['cache', 'Cache', 'deep'], ['events', 'Events', 'deep'], ['views', 'Views', 'deep'],
    ['http', 'HTTP', 'deep'], ['logs', 'Logs', 'l1'], ['exceptions', 'Exceptions', 'l1'], ['dumps', 'Dumps', 'dumps'], ['mail', 'Mail', 'l1'],
  ];
  function watchFresh() {
    return {
      info: null, err: '', es: null, status: '', paused: false, held: [], sub: prefGet('watch.sub', 'requests'),
      reqs: [], phpReqs: new Map(), byRid: new Map(), lists: {}, analysis: new Map(),
      fileExc: [], phpExc: [], logs: [], unseen: 0, open: new Set(), paintT: 0, mail: null, mailT: 0,
      f: { status: '', method: '', q: '', noStatic: prefGet('watch.noStatic', true), issuesOnly: false, level: '' },
      showSettings: false, mailAll: false,
      // EXPLAIN: results per query shape + bindings (ex), auto-explain
      // badges per shape (exNorm), shapes already queued (exAuto).
      qSeq: 0, ex: new Map(), exNorm: new Map(), exAuto: new Set(), exQueue: [], exBusy: false,
      autoExplain: !!prefGet('watch.autoExplain', false),
    };
  }
  const watchOn = () => !!(info && info.watch && info.watch.enabled);
  function wList(k) { return watchState.lists[k] || (watchState.lists[k] = []); }
  function wPush(arr, it, cap) {
    arr.push(it);
    if (arr.length > cap * 1.2) arr.splice(0, arr.length - cap);
  }

  function startWatchStream() {
    const ws = watchState;
    stopWatchStream();
    if (!watchOn() || destroyed) return;
    const es = new EventSource('/api/watch/stream?t=' + encodeURIComponent(TOKEN) + '&site=' + encodeURIComponent(cur));
    ws.es = es;
    ws.status = 'connecting…';
    es.addEventListener('start', ev => {
      if (ws.es !== es) return;
      // A reconnect replays the backlog: start from a clean slate.
      const keep = { info: ws.info, sub: ws.sub, f: ws.f, paused: ws.paused, showSettings: ws.showSettings, mail: ws.mail, mailAll: ws.mailAll, open: ws.open,
        qSeq: ws.qSeq, ex: ws.ex, exNorm: ws.exNorm, exAuto: ws.exAuto, exQueue: ws.exQueue, exBusy: ws.exBusy, autoExplain: ws.autoExplain };
      Object.assign(ws, watchFresh(), keep, { es });
      ws.status = 'live';
      // Only the Watch tab owns the panel (and only once its settings loaded).
      if (tab === 'watch' && ws.info) wPaint();
    });
    es.addEventListener('backlog', ev => { if (ws.es === es) wIngest(JSON.parse(ev.data), true); });
    es.addEventListener('items', ev => {
      if (ws.es !== es) return;
      const items = JSON.parse(ev.data);
      if (ws.paused) { ws.held.push(...items); if (ws.held.length > 5000) ws.held.splice(0, ws.held.length - 5000); wPaintHead(); return; }
      wIngest(items, false);
    });
    // Another tab (or `mullion watch clear`) cleared: everything, or some views.
    es.addEventListener('cleared', ev => {
      if (ws.es !== es) return;
      let d = {};
      try { d = JSON.parse(ev.data) || {}; } catch {}
      const before = (ws.info && ws.info.cleared) || {};
      const after = Object.assign({}, d.types || {});
      if (d.at) after[''] = d.at;
      if (ws.info) ws.info.cleared = after;
      if (d.all) { wClearLocal(); return; }
      const types = Object.keys(after).filter(k => k && after[k] > (before[k] || 0));
      if (types.length) wDropTypes(types);
    });
    es.onerror = () => {
      if (ws.es !== es) return;
      ws.status = es.readyState === 2 ? 'disconnected' : 'reconnecting…';
      wPaintHead();
    };
  }
  function stopWatchStream() {
    if (!watchState) return;
    if (watchState.es) { try { watchState.es.close(); } catch {} }
    watchState.es = null;
    clearTimeout(watchState.paintT);
    clearInterval(watchState.mailT);
    watchState.mailT = 0;
  }

  // wIngest files items into their lists; live exceptions flag the tab.
  function wIngest(items, isBacklog) {
    const ws = watchState;
    let exc = 0;
    for (const it of items) {
      const t = it.type;
      if (it.rid && t !== 'analysis' && !(t === 'request' && it.src === 'caddy')) {
        let a = ws.byRid.get(it.rid);
        if (!a) { a = []; ws.byRid.set(it.rid, a); if (ws.byRid.size > 1500) ws.byRid.delete(ws.byRid.keys().next().value); }
        if (a.length < 3000) a.push(it);
      }
      if (t === 'request') {
        if (it.src === 'caddy') wPush(ws.reqs, it, W_CAP.request);
        else { ws.phpReqs.set(it.rid, it); wPush(wList('units'), it, W_CAP.def); }
      } else if (t === 'command') wPush(wList('units'), it, W_CAP.def);
      else if (t === 'analysis') { ws.analysis.set(it.rid, it); if (ws.analysis.size > 1500) ws.analysis.delete(ws.analysis.keys().next().value); }
      else if (t === 'exception') {
        if (it.src === 'file') { wPush(ws.fileExc, it, W_CAP.def); wPush(ws.logs, it, W_CAP.log); } else wPush(ws.phpExc, it, W_CAP.def);
        exc++;
      } else if (t === 'log') { if (it.src === 'file') wPush(ws.logs, it, W_CAP.log); }
      else if (t === 'job') {
        wPush(wList('jobs'), it, W_CAP.def);
        if (it.op === 'processed' || it.op === 'failed') wPush(wList('units'), it, W_CAP.def);
      } else if (t === 'query') { it._q = ++ws.qSeq; wPush(wList('queries'), it, W_CAP.query); wAutoExplain(it); }
      else if (t === 'cache') wPush(wList('cache'), it, W_CAP.def);
      else if (t === 'event') wPush(wList('events'), it, W_CAP.def);
      else if (t === 'view') wPush(wList('views'), it, W_CAP.def);
      else if (t === 'http') wPush(wList('http'), it, W_CAP.def);
      else if (t === 'dump') wPush(wList('dumps'), it, W_CAP.def);
      else if (t === 'mail') wPush(wList('mails'), it, W_CAP.def);
    }
    if (exc && !isBacklog) {
      const looking = tab === 'watch' && ws.sub === 'exceptions' && visible();
      if (!looking) {
        ws.unseen += exc;
        window.dispatchEvent(new CustomEvent('mullion:watch-exception', { detail: { site: cur, count: ws.unseen } }));
      }
    }
    wUpdateTabBadge();
    wSchedule();
  }
  function wUpdateTabBadge() {
    const el = root && root.querySelector('[data-id="mp-watch-n"]');
    if (!el || !watchState) return;
    el.hidden = !watchState.unseen;
    el.textContent = watchState.unseen;
    el.classList.toggle('bad', !!watchState.unseen);
  }
  function wSeen() {
    if (!watchState || !watchState.unseen) return;
    watchState.unseen = 0;
    wUpdateTabBadge();
    window.dispatchEvent(new CustomEvent('mullion:watch-seen', { detail: { site: cur } }));
  }
  function wSchedule() {
    const ws = watchState;
    if (tab !== 'watch' || ws.paintT) return;
    ws.paintT = setTimeout(() => {
      ws.paintT = 0;
      if (tab !== 'watch' || destroyed) return;
      // Don't swap the rows out from under a press or a text selection:
      // the click would land on a row that no longer exists and the
      // selection would vanish. Try again a little later.
      if ((wPressing && Date.now() - wPressing < 5000) || wSelecting()) { wSchedule(); return; }
      wPaintSubs();
      wPaintBody(false, true);
    }, 250);
  }
  let wPressing = 0; // when a press inside the list started (0: none)
  let wHover = false; // the pointer is over the list: new rows mustn't push what's under it
  function wSelecting() {
    const sel = window.getSelection && window.getSelection();
    const body = els.panel && els.panel.querySelector('[data-id="mp-w-body"]');
    return !!(sel && !sel.isCollapsed && body && sel.anchorNode && body.contains(sel.anchorNode));
  }
  function wClearLocal() {
    const ws = watchState;
    const keep = { info: ws.info, sub: ws.sub, f: ws.f, paused: ws.paused, es: ws.es, status: ws.status, showSettings: ws.showSettings, mail: ws.mail, mailAll: ws.mailAll };
    Object.assign(ws, watchFresh(), keep);
    wSeen();
    if (tab === 'watch' && ws.info) wPaint();
  }
  // W_CLEAR is what "Clear this view" clears: item types, or type/src.
  const W_CLEAR = {
    requests: ['request'], timeline: ['request/php', 'command', 'job'], queries: ['query', 'analysis'], jobs: ['job'],
    cache: ['cache'], events: ['event'], views: ['view'], http: ['http'], logs: ['log', 'exception/file'],
    exceptions: ['exception'], dumps: ['dump'], mail: ['mail'],
  };
  // wDropTypes forgets the items of some kinds (the panel's copy; the
  // server hides them from the next backlog).
  function wDropTypes(types) {
    const ws = watchState;
    const hit = it => types.includes(it.type) || types.includes(it.type + '/' + (it.src || ''));
    const keep = arr => arr.filter(it => !hit(it));
    for (const k of Object.keys(ws.lists)) ws.lists[k] = keep(ws.lists[k]);
    ws.reqs = keep(ws.reqs);
    for (const [k, v] of ws.phpReqs) if (hit(v)) ws.phpReqs.delete(k);
    if (types.includes('analysis')) ws.analysis.clear();
    ws.fileExc = keep(ws.fileExc);
    ws.phpExc = keep(ws.phpExc);
    ws.logs = keep(ws.logs);
    ws.held = keep(ws.held);
    for (const [k, v] of ws.byRid) { const n = keep(v); if (n.length) ws.byRid.set(k, n); else ws.byRid.delete(k); }
    if (types.includes('exception') || types.includes('exception/file')) wSeen();
    if (tab === 'watch' && ws.info) { wPaintSubs(); wPaintBody(); }
  }
  // wClearMenu: Clear this view / Clear everything / …and laravel.log.
  function wClearMenu(anchor) {
    const ws = watchState;
    const sub = (W_SUBS.find(s => s[0] === ws.sub) || [, 'this view'])[1];
    const items = [
      { icon: ICON.x, label: `Clear this view (${sub})`, run: () => wClear({ types: W_CLEAR[ws.sub] || [] }) },
      { icon: ICON.trash, label: 'Clear everything', right: fmtBytes(ws.info.dataBytes || 0), run: () => wClear({}) },
    ];
    if (ws.info.laravel || ws.info.php) {
      items.push('-', { icon: ICON.file, label: 'Clear everything + empty storage/logs…', danger: true, run: async () => {
        const ok = await confirmBox({
          title: 'Empty the project\'s log files?',
          message: `This edits files in the project: every <span class="mono">storage/logs/*.log</span> in <span class="mono">${esc(info.path)}</span> (laravel.log included) is emptied — their contents are gone for good. The watcher's history is cleared too.`,
          confirmText: 'Empty logs', danger: true,
        });
        if (ok) wClear({ logs: true });
      } });
    }
    if (G('openMenu')) { G('openMenu')(anchor, items); return; }
    // No shared menu (shouldn't happen): fall back to clearing everything.
    wClear({});
  }
  async function wClear(opts) {
    const ws = watchState;
    let r;
    try { r = await busy('Clearing…', () => call('/api/project/watch/clear', Object.assign({ site: cur }, opts))); }
    catch (e) { say('Error: ' + e.message); return; }
    if (r && r.info) { ws.info = r.info; info.watch = r.info.settings; }
    if (opts.types && opts.types.length) { wDropTypes(opts.types); wPaintHeadSize(); say('Cleared'); return; }
    wClearLocal();
    say(opts.logs ? `Cleared — emptied ${r && r.emptied ? r.emptied.length : 0} log file${r && r.emptied && r.emptied.length === 1 ? '' : 's'}` : 'Cleared');
  }
  function wPaintHeadSize() {
    const el = els.panel && els.panel.querySelector('[data-id="mp-w-size"]');
    if (el && watchState.info) el.textContent = fmtBytes(watchState.info.dataBytes || 0);
  }

  /* ── tab rendering ── */
  async function renderWatch() {
    const g = gen, ws = watchState;
    if (!ws.info) els.panel.innerHTML = `<section class="card"><div class="mp-loading"><div class="spin"></div>Reading the watcher's settings…</div></section>`;
    try { ws.info = await call('/api/project/watch/info', { site: cur }); ws.err = ''; } catch (e) { ws.err = e.message; }
    if (!live(g) || tab !== 'watch') return;
    if (ws.info) info.watch = ws.info.settings;
    if (watchOn() && !ws.es) startWatchStream();
    wPaint();
    if (ws.sub === 'exceptions') wSeen();
  }

  function wSubsFor() {
    const st = (watchState.info && watchState.info.settings) || {};
    return W_SUBS.filter(([, , lvl]) => lvl === 'l1' || (lvl === 'deep' && st.deep) || (lvl === 'dumps' && st.dumps));
  }
  function wCount(k) {
    const ws = watchState;
    switch (k) {
      case 'requests': return ws.reqs.length || ws.phpReqs.size;
      case 'timeline': return wList('units').length;
      case 'logs': return ws.logs.length;
      case 'exceptions': return wExceptionGroups().length;
      case 'mail': return ws.mail ? wMailShown().length : 0;
      default: return wList(k).length;
    }
  }

  function wPaint() {
    const ws = watchState, inf = ws.info;
    const keepY = window.scrollY; // before the panel is swapped (and the page maybe shortened)
    if (!inf) {
      els.panel.innerHTML = `<section class="card"><div class="card-body"><div class="mp-empty"><b>Couldn't read the watcher's settings</b>${esc(ws.err)}</div></div></section>`;
      return;
    }
    const st = inf.settings;
    const lvl = (id, label, on, dis, hint) => `<label class="mp-w-lvl${on ? ' on' : ''}${dis ? ' dis' : ''}" title="${esc(hint)}"><input type="checkbox" data-wlvl="${id}" ${on ? 'checked' : ''} ${dis ? 'disabled' : ''}>${label}</label>`;
    let notes = '';
    if (inf.note) notes += `<div class="mp-warn">${esc(inf.note)}</div>`;
    if (st.enabled && st.deep && !inf.laravel) notes += `<div class="mp-note">The deep watcher hooks into Laravel; this doesn't look like a Laravel app, so only requests, logs and dumps show.</div>`;
    if (st.enabled && inf.prepend && inf.workers) notes += `<div class="mp-sub" style="margin-bottom:10px">Queue workers started by Mullion load the watcher too (they were restarted when you turned it on). Artisan commands from the Commands tab do as well; the Terminal tab doesn't.</div>`;
    els.panel.innerHTML = `
      <section class="card mp-w">
        <div class="card-head">${ICON.eye}<h2>Watch</h2><span class="hint" data-id="mp-w-status"></span><span class="spacer"></span>
          <label class="toggle" title="watch this site live — data stays on this computer"><span class="switch"><input type="checkbox" data-id="mp-w-on" ${st.enabled ? 'checked' : ''}><i></i></span>${st.enabled ? 'On' : 'Off'}</label>
        </div>
        <div class="card-body">
          <div class="row mp-w-levels">
            ${lvl('l1', 'Requests &amp; logs', st.enabled, true, 'Caddy\'s access log and storage/logs — nothing changes in the project')}
            ${lvl('dumps', 'Dumps', st.dumps, !st.enabled || !inf.php, 'dump() and dd() show here — Mullion loads a small PHP file before this site\'s code (auto_prepend_file, this site only)')}
            ${lvl('deep', 'Deep <span class="mp-sub">queries, jobs, cache…</span>', st.deep, !st.enabled || !inf.php, 'Laravel queries, N+1, jobs, cache, events, views, HTTP calls and a per-request timeline — through the same prepend, no Composer package')}
            <span class="spacer"></span>
            ${st.enabled ? `<button class="sm" data-id="mp-w-pause">${ws.paused ? 'Resume' : 'Pause'}</button>
              <span class="mp-w-clear"><span class="mp-w-disk mono" data-id="mp-w-size" title="what the watcher keeps on disk for this site (${esc(inf.dir || '')})">${fmtBytes(inf.dataBytes || 0)}</span>
              <button class="sm mp-btn-ic" data-id="mp-w-clear" aria-haspopup="menu" title="clear this view, everything, or the project's log files">${ICON.trash}Clear${ICON.chev}</button></span>` : ''}
            <button class="sm mp-btn-ic${ws.showSettings ? ' active' : ''}" data-id="mp-w-set">${ICON.gear}Settings</button>
          </div>
          <div data-id="mp-w-settings"></div>
          ${notes}
        </div>
      </section>
      ${st.enabled ? `<div class="mp-w-subs" role="tablist" data-id="mp-w-subs"></div><section class="card"><div class="card-body" data-id="mp-w-body"></div></section>`
        : `<section class="card"><div class="card-body"><div class="mp-empty"><b>The watcher is off</b>Turn it on to see this site's requests, logs and exceptions as you use it${inf.php ? ' — add Dumps and Deep for dump(), queries, jobs, cache and a per-request timeline' : ''}. Everything stays on this computer.</div></div></section>`}`;
    const q = s => els.panel.querySelector(`[data-id="${s}"]`);
    q('mp-w-on').onchange = e => wSave({ enabled: e.target.checked });
    els.panel.querySelectorAll('[data-wlvl]').forEach(c => c.onchange = () => wSave({ [c.dataset.wlvl]: c.checked }));
    q('mp-w-set').onclick = () => { ws.showSettings = !ws.showSettings; wPaint(); };
    if (q('mp-w-pause')) q('mp-w-pause').onclick = () => {
      ws.paused = !ws.paused;
      if (!ws.paused && ws.held.length) { const h = ws.held; ws.held = []; wIngest(h, false); }
      wPaint();
    };
    if (q('mp-w-clear')) q('mp-w-clear').onclick = e => wClearMenu(e.currentTarget);
    const wb = q('mp-w-body');
    if (wb) {
      wHover = false;
      wb.addEventListener('pointerenter', () => { wHover = true; });
      wb.addEventListener('pointerleave', () => { wHover = false; });
      wb.addEventListener('pointerdown', () => {
        wPressing = Date.now();
        const up = () => { wPressing = 0; window.removeEventListener('pointerup', up, true); window.removeEventListener('pointercancel', up, true); };
        window.addEventListener('pointerup', up, true);
        window.addEventListener('pointercancel', up, true);
      });
    }
    if (ws.showSettings) wPaintSettings();
    wPaintHead();
    if (st.enabled) { wPaintSubs(); wPaintBody(false, false, keepY); }
  }

  function wPaintHead() {
    const ws = watchState, h = tab === 'watch' && els.panel && els.panel.querySelector('[data-id="mp-w-status"]');
    if (!h || !ws.info) return;
    const st = ws.info.settings;
    if (!st.enabled) { h.textContent = 'live requests, logs, dumps and queries — off by default'; return; }
    h.innerHTML = `<span class="mp-dot ${ws.es && ws.status === 'live' ? 'on' : ws.status === 'disconnected' ? 'bad' : 'sleep'}" style="display:inline-block;vertical-align:-1px;margin-right:6px"></span>${esc(ws.paused ? 'paused' + (ws.held.length ? ` · ${ws.held.length} waiting` : '') : ws.status || 'idle')}`;
  }

  function wPaintSettings() {
    const ws = watchState, st = ws.info.settings, box = els.panel.querySelector('[data-id="mp-w-settings"]');
    if (!box) return;
    const eds = ws.info.editors || [];
    const edName = { vscode: 'VS Code', cursor: 'Cursor', phpstorm: 'PhpStorm', zed: 'Zed', sublime: 'Sublime Text' };
    box.innerHTML = `<div class="mp-w-settings">
      <label>Slow request <span class="mp-sub">ms</span><input type="number" min="1" max="600000" data-ws="slowRequestMs" value="${st.slowRequestMs}"></label>
      <label>Slow query <span class="mp-sub">ms</span><input type="number" min="1" max="600000" data-ws="slowQueryMs" value="${st.slowQueryMs}"></label>
      <label>N+1 after <span class="mp-sub">repeats</span><input type="number" min="2" max="1000" data-ws="nPlusOne" value="${st.nPlusOne}"></label>
      <label>Open files in<select data-ws="editor"><option value="">Auto${eds.length ? ' (' + esc(edName[eds[0]] || eds[0]) + ')' : ''}</option>${['vscode', 'cursor', 'phpstorm', 'zed', 'sublime'].map(e => `<option value="${e}" ${st.editor === e ? 'selected' : ''}>${edName[e]}${eds.includes(e) ? '' : ' (not found)'}</option>`).join('')}</select></label>
      <label class="toggle" title="dump() also prints in the page, as without Mullion"><span class="switch"><input type="checkbox" data-ws="dumpsInPage" ${st.dumpsInPage ? 'checked' : ''}><i></i></span>Show dumps in the page too</label>
      <button class="sm primary" data-id="mp-w-save">Save</button>
    </div>`;
    box.querySelector('[data-id="mp-w-save"]').onclick = () => {
      const v = {};
      box.querySelectorAll('[data-ws]').forEach(i => { v[i.dataset.ws] = i.type === 'checkbox' ? i.checked : i.type === 'number' ? (parseInt(i.value, 10) || 0) : i.value; });
      wSave(v, 'Saved');
    };
  }

  async function wSave(change, done) {
    const ws = watchState, st = ws.info.settings;
    const next = Object.assign({}, st, change);
    if (!next.enabled) { next.dumps = next.dumps && false; next.deep = next.deep && false; }
    if (change.enabled && !st.enabled && ws.info.php) { next.dumps = true; next.deep = ws.info.laravel; }
    const needsReload = (st.enabled && (st.dumps || st.deep)) !== (next.enabled && (next.dumps || next.deep));
    try {
      ws.info = await busy(needsReload ? 'Updating the site\'s PHP settings…' : 'Saving…', () => call('/api/project/watch/set', {
        site: cur, watch: {
          enabled: next.enabled, dumps: next.dumps, dumpsInPage: next.dumpsInPage, deep: next.deep, editor: next.editor || '',
          slowRequestMs: next.slowRequestMs, slowQueryMs: next.slowQueryMs, nPlusOne: next.nPlusOne,
        },
      }));
    } catch (e) { say('Error: ' + e.message); wPaint(); return; }
    info.watch = ws.info.settings;
    if (done) say(done);
    if (watchOn()) { if (!ws.es || needsReload) startWatchStream(); } else { stopWatchStream(); wClearLocal(); }
    wPaint();
  }

  function wPaintSubs() {
    const ws = watchState, box = els.panel.querySelector('[data-id="mp-w-subs"]');
    if (!box) return;
    const subs = wSubsFor();
    if (!subs.some(s => s[0] === ws.sub)) ws.sub = 'requests';
    const hadFocus = document.activeElement && box.contains(document.activeElement) ? document.activeElement.dataset.wsub : null;
    box.innerHTML = subs.map(([k, label]) => {
      const n = wCount(k);
      const bad = k === 'exceptions' && n;
      return `<button data-wsub="${k}" class="${ws.sub === k ? 'active' : ''}">${label}${n ? `<span class="n${bad ? ' bad' : ''}">${fmtNum(n)}</span>` : ''}</button>`;
    }).join('');
    box.querySelectorAll('[data-wsub]').forEach(b => b.onclick = () => {
      ws.sub = b.dataset.wsub;
      prefSet('watch.sub', ws.sub);
      if (ws.sub === 'exceptions') wSeen();
      if (ws.sub === 'mail') wLoadMail();
      wPaintSubs();
      wPaintBody(true);
    });
    if (hadFocus) { const b = box.querySelector(`[data-wsub="${hadFocus}"]`); if (b) b.focus({ preventScroll: true }); }
  }

  // wPaintBody redraws the active sub-tab, keeping the list's scroll
  // position, the page's scroll position and any open search box's focus.
  // reset starts the list at the top (another sub-tab or filter); stream
  // marks a repaint for new events, which are added above what the user
  // is looking at — a scrolled list is shifted by what was added so the
  // rows in view stay put. keepY is the page scroll to hold (wPaint reads
  // it before swapping the whole panel).
  function wPaintBody(reset, stream, keepY) {
    const ws = watchState, body = els.panel.querySelector('[data-id="mp-w-body"]');
    if (!body) return;
    const wy = keepY != null ? keepY : window.scrollY;
    const focused = document.activeElement && body.contains(document.activeElement) ? document.activeElement.dataset.wf : null;
    const caret = focused && document.activeElement.selectionStart;
    const list = body.querySelector('.mp-w-list');
    const scroll = !reset && list ? list.scrollTop : 0;
    const oldH = list ? list.scrollHeight : 0;
    // The row at the top of the list's view (and how far into it), to
    // keep it there when new rows arrive above it.
    const keepRows = stream && list && (scroll > 2 || wHover);
    let anchor = null;
    if (keepRows) {
      const lt = list.getBoundingClientRect().top;
      for (const r of list.querySelectorAll('[data-wopen]')) {
        const top = r.getBoundingClientRect().top - lt;
        if (top + r.offsetHeight > 0) { anchor = { key: r.dataset.wopen, top }; break; }
      }
    }
    const fn = { requests: wRequests, timeline: wTimeline, queries: wQueries, jobs: wJobs, cache: wCache, events: wEvents, views: wViews, http: wHttp, logs: wLogs, exceptions: wExceptions, dumps: wDumps, mail: wMail }[ws.sub] || wRequests;
    body.style.minHeight = '';
    body.innerHTML = fn();
    const nl = body.querySelector('.mp-w-list');
    if (nl) {
      nl.scrollTop = keepRows ? scroll + Math.max(0, nl.scrollHeight - oldH) : scroll;
      // Rows with keys: line the same row up exactly (old rows may also
      // have dropped off the bottom, which the height difference misses).
      const ar = anchor && [...nl.querySelectorAll('[data-wopen]')].find(r => r.dataset.wopen === anchor.key);
      if (ar) nl.scrollTop += ar.getBoundingClientRect().top - nl.getBoundingClientRect().top - anchor.top;
    }
    // A shorter body would pull the page up (the browser clamps the
    // scroll to the new height) — hold the old height instead.
    if (window.scrollY < wy - 1) {
      body.style.minHeight = (body.offsetHeight + wy - window.scrollY) + 'px';
      window.scrollTo(window.scrollX, wy);
    }
    // preventScroll: refocusing the search box must never scroll the
    // page back up to it.
    if (focused) { const el = body.querySelector(`[data-wf="${focused}"]`); if (el) { el.focus({ preventScroll: true }); try { el.setSelectionRange(caret, caret); } catch {} } }
    wBindBody(body);
  }

  /* ── helpers ── */
  const wTime = t => { const d = new Date(t * 1000); return d.toLocaleTimeString([], { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0'); };
  const wMs = ms => ms == null ? '—' : ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : (ms >= 10 ? Math.round(ms) : (+ms).toFixed(ms >= 1 ? 1 : 2)) + ' ms';
  const wStatus = s => `<span class="mp-w-st s${String(s || 0)[0]}">${esc(s || '—')}</span>`;
  const wLoc = (file, line) => file ? `<a class="mp-w-loc" data-open="${esc(file)}" data-line="${line || 0}" title="open in your editor">${esc(shortPath(file))}${line ? ':' + line : ''}</a>` : '';
  function shortPath(f) {
    const base = (info.path || '').replace(/\\/g, '/').replace(/\/$/, '') + '/';
    const n = String(f).replace(/\\/g, '/');
    return n.startsWith(base) ? n.slice(base.length) : n;
  }
  const newestFirst = (arr, max) => arr.slice(-(max || 300)).reverse();
  const wEmpty = (title, text) => `<div class="mp-empty"><b>${title}</b>${text}</div>`;
  function wSearch(placeholder, extra) {
    return `<div class="row" style="gap:8px; margin-bottom:12px">
      <div class="mp-search">${ICON.search}<input type="text" data-wf="q" placeholder="${placeholder}" value="${esc(watchState.f.q)}" spellcheck="false"></div>${extra || ''}</div>`;
  }
  const wMatch = (s) => !watchState.f.q || String(s).toLowerCase().includes(watchState.f.q.toLowerCase());

  const SQL_KW = /\b(select|from|where|and|or|not|insert|into|values|update|set|delete|join|left|right|inner|outer|on|group|by|order|having|limit|offset|as|in|is|null|distinct|count|exists|union|all|case|when|then|else|end|asc|desc|like|between|returning|with|create|table|alter|drop|index)\b/gi;
  function highlightSQL(sql) {
    // Tokenise first so keywords inside strings/identifiers stay plain.
    const parts = String(sql).split(/('(?:[^'\\]|\\.|'')*'|"[^"]*"|`[^`]*`)/);
    return parts.map((p, i) => {
      if (i % 2) return p[0] === "'" ? `<span class="sql-s">${esc(p)}</span>` : `<span class="sql-i">${esc(p)}</span>`;
      return esc(p).replace(SQL_KW, m => `<span class="sql-k">${m}</span>`).replace(/\b(\d+(?:\.\d+)?)\b/g, '<span class="sql-n">$1</span>').replace(/\?/g, '<span class="sql-p">?</span>');
    }).join('');
  }
  function wBindings(b) {
    if (!b || !b.length) return '';
    return `<div class="mp-w-bind">${b.map((v, i) => `<span><span class="mp-sub">${i + 1}</span> ${esc(v === null ? 'null' : typeof v === 'string' ? JSON.stringify(v) : String(v))}</span>`).join('')}</div>`;
  }
  function wQueryFlags(q) {
    const a = watchState.analysis.get(q.rid);
    const st = watchState.info.settings, flags = [];
    if (q.ms >= st.slowQueryMs) flags.push('<span class="mp-w-badge slow">slow</span>');
    if (a && a.issues) {
      for (const is of a.issues) {
        if (is.kind === 'n+1' && is.sql === q.norm && is.file === (q.file || '') && is.line === (q.line || 0)) flags.push(`<span class="mp-w-badge n1" title="the same query ran ${is.count}× from here in one request">N+1 ×${is.count}</span>`);
        else if (is.kind === 'duplicate' && is.sql === q.sql && is.count > 1 && !flags.some(f => f.includes('dup'))) flags.push(`<span class="mp-w-badge dup" title="identical query and bindings ran ${is.count}× in one request">dup ×${is.count}</span>`);
      }
    }
    return flags.join('');
  }

  /* ── Requests ── */
  function wReqRows() {
    const ws = watchState, f = ws.f;
    // Caddy rows, plus PHP-recorded requests Caddy's log doesn't have.
    const rows = ws.reqs.slice();
    if (!rows.length) for (const r of ws.phpReqs.values()) rows.push(Object.assign({ path: String(r.uri || '').split('?')[0] }, r));
    return rows.filter(r => {
      if (f.noStatic && r.static) return false;
      if (f.status && String(r.status)[0] !== f.status) return false;
      if (f.method && r.method !== f.method) return false;
      return wMatch(r.uri || r.path);
    });
  }
  function wPhpReqFor(r) {
    const ws = watchState;
    if (r.rid && ws.phpReqs.has(r.rid)) return ws.phpReqs.get(r.rid);
    if (r.src === 'php') return r;
    return null;
  }
  function wRequests() {
    const ws = watchState, f = ws.f, st = ws.info.settings;
    const rows = newestFirst(wReqRows());
    const methods = ['', 'GET', 'POST', 'PUT', 'PATCH', 'DELETE'];
    const filters = `<div class="mp-seg">${[['', 'All'], ['2', '2xx'], ['3', '3xx'], ['4', '4xx'], ['5', '5xx']].map(([v, l]) => `<button data-wfs="${v}" class="${f.status === v ? 'active' : ''}">${l}</button>`).join('')}</div>
      <select data-wf="method" class="sm">${methods.map(m => `<option value="${m}" ${f.method === m ? 'selected' : ''}>${m || 'Any method'}</option>`).join('')}</select>
      <label class="toggle" title="hide CSS, JS, images, fonts…"><span class="switch"><input type="checkbox" data-wf="noStatic" ${f.noStatic ? 'checked' : ''}><i></i></span>No assets</label>`;
    if (!rows.length) return wSearch('Filter by path…', filters) + wEmpty('No requests yet', 'Open the site — requests appear here as they happen.');
    return wSearch('Filter by path…', filters) + `<div class="mp-w-list">` + rows.map(r => {
      const key = 'r:' + (r.rid || r.t + r.uri);
      const php = wPhpReqFor(r), a = r.rid && ws.analysis.get(r.rid);
      const open = ws.open.has(key);
      const slow = r.ms >= st.slowRequestMs;
      const marks = [];
      if (a) marks.push(`<span class="mp-w-badge q" title="${a.queries} queries, ${wMs(a.ms)}">${a.queries} q</span>`);
      if (a && a.issues.some(i => i.kind === 'n+1')) marks.push('<span class="mp-w-badge n1">N+1</span>');
      if (r.rid && (ws.byRid.get(r.rid) || []).some(e => e.type === 'exception')) marks.push('<span class="mp-w-badge exc">exception</span>');
      return `<div class="mp-w-row${slow ? ' slow' : ''}${open ? ' open' : ''}" data-wopen="${esc(key)}">
        <span class="mp-w-time">${wTime(r.t)}</span><span class="mp-w-m">${esc(r.method)}</span>${wStatus(r.status)}
        <span class="mp-w-main mono" title="${esc(r.uri)}">${esc(r.uri || r.path)}</span>${marks.join('')}
        <span class="mp-w-ms${slow ? ' slow' : ''}">${wMs(r.ms)}</span><span class="mp-w-size">${r.size != null ? fmtBytes(r.size) : ''}</span></div>
        ${open ? wRequestDetail(r, php, a) : ''}`;
    }).join('') + `</div>`;
  }
  function wKV(obj) {
    const keys = Object.keys(obj || {}).sort();
    if (!keys.length) return '<span class="mp-sub">none</span>';
    return `<div class="mp-w-kv">${keys.map(k => `<div class="k mono">${esc(k)}</div><div class="mono">${esc([].concat(obj[k]).join(', '))}</div>`).join('')}</div>`;
  }
  function wRequestDetail(r, php, a) {
    let html = '<div class="mp-w-detail">';
    if (php && php.route) html += `<div class="mp-sub" style="margin-bottom:8px">Route <span class="mono">${esc(php.route.uri || '')}</span>${php.route.name ? ' · <span class="mono">' + esc(php.route.name) + '</span>' : ''} · <span class="mono">${esc(php.route.action || '')}</span>${php.memory ? ' · ' + fmtBytes(php.memory) + ' peak' : ''}</div>`;
    if (php && php.fatal) html += `<div class="mp-warn">${esc(php.fatal)}</div>`;
    if (a && a.issues.length) html += `<div class="mp-w-issues">${a.issues.map(i => `<div><span class="mp-w-badge ${i.kind === 'n+1' ? 'n1' : 'dup'}">${i.kind === 'n+1' ? 'N+1' : 'duplicate'} ×${i.count}</span> <code class="sql">${highlightSQL(i.sql)}</code> ${wLoc(i.file, i.line)}</div>`).join('')}</div>`;
    if (r.rid && watchState.byRid.has(r.rid)) html += `<h4>Timeline</h4>` + wTimelineOf(r.rid, php || r);
    if (r.query) html += `<h4>Query</h4>${wKV(r.query)}`;
    if (r.headers) html += `<h4>Request headers</h4>${wKV(r.headers)}`;
    if (r.respHeaders) html += `<h4>Response headers</h4>${wKV(r.respHeaders)}`;
    if (!r.rid && watchState.info.settings.deep && r.src === 'caddy' && !r.static) html += `<div class="mp-sub" style="margin-top:8px">${watchState.info.requestId ? 'PHP recorded nothing for this request.' : 'Mullion\'s Caddy is older than 2.8, so request rows can\'t be linked to their timeline — see the Timeline tab.'}</div>`;
    return html + '</div>';
  }

  /* ── Timeline ── */
  function wUnitLabel(u) {
    if (u.type === 'request') return `<span class="mp-w-m">${esc(u.method)}</span>${wStatus(u.status)}<span class="mp-w-main mono">${esc(u.uri)}</span>`;
    if (u.type === 'job') return `<span class="mp-w-badge ${u.op === 'failed' ? 'exc' : 'q'}">job ${esc(u.op)}</span><span class="mp-w-main mono">${esc(u.name)}</span>`;
    return `<span class="mp-w-badge q">artisan</span><span class="mp-w-main mono">${esc(u.command)}</span>`;
  }
  function wTimeline() {
    const ws = watchState;
    const units = newestFirst(wList('units').filter(u => wMatch(u.uri || u.name || u.command)));
    if (!units.length) return wSearch('Filter…') + wEmpty('Nothing recorded yet', 'Each request, queued job and artisan command gets a timeline of its queries, cache, events, views, logs and HTTP calls.');
    return wSearch('Filter by path, job or command…') + `<div class="mp-w-list">` + units.map(u => {
      const key = 'u:' + u.rid + u.t;
      const a = ws.analysis.get(u.rid), open = ws.open.has(key);
      const n = (ws.byRid.get(u.rid) || []).length;
      return `<div class="mp-w-row${open ? ' open' : ''}" data-wopen="${esc(key)}"><span class="mp-w-time">${wTime(u.t)}</span>${wUnitLabel(u)}
        ${a ? `<span class="mp-w-badge q">${a.queries} q · ${wMs(a.ms)}</span>` : ''}${a && a.issues.some(i => i.kind === 'n+1') ? '<span class="mp-w-badge n1">N+1</span>' : ''}
        <span class="mp-sub">${n} events</span><span class="mp-w-ms">${wMs(u.ms)}</span></div>${open ? `<div class="mp-w-detail">${wTimelineOf(u.rid, u)}</div>` : ''}`;
    }).join('') + '</div>';
  }
  function wTimelineOf(rid, unit) {
    const evs = (watchState.byRid.get(rid) || []).filter(e => e.type !== 'request' && e.type !== 'command');
    if (!evs.length) return '<div class="mp-sub">No events recorded for this one.</div>';
    const start = unit && unit.ms ? unit.t - unit.ms / 1000 : evs[0].t;
    const total = Math.max(1, unit && unit.ms ? unit.ms : (evs[evs.length - 1].t - start) * 1000);
    return `<div class="mp-w-tl">${evs.slice(0, 600).map(e => {
      const off = Math.max(0, (e.t - start) * 1000);
      const pct = Math.min(100, off / total * 100);
      return `<div class="mp-w-tle t-${esc(e.type)}"><span class="mp-w-off">+${wMs(off)}</span><span class="mp-w-bar"><i style="left:${pct.toFixed(1)}%"></i></span><span class="mp-w-kind">${esc(e.type === 'job' ? 'job ' + e.op : e.type)}</span><span class="mp-w-what">${wEventText(e)}</span></div>`;
    }).join('')}${evs.length > 600 ? `<div class="mp-sub">…and ${evs.length - 600} more</div>` : ''}</div>`;
  }
  function wEventText(e) {
    switch (e.type) {
      case 'query': return `<code class="sql">${highlightSQL(e.sql)}</code> <span class="mp-sub">${wMs(e.ms)}</span> ${wQueryFlags(e)}${wExplainBits(e)} ${wLoc(e.file, e.line)}`;
      case 'cache': return `<span class="mp-w-badge c-${esc(e.op)}">${esc(e.op)}</span> <span class="mono">${esc(e.key)}</span>`;
      case 'view': return `<span class="mono">${esc(e.name)}</span> ${wLoc(e.file, 0)}`;
      case 'event': return `<span class="mono">${esc(e.name)}</span>`;
      case 'http': return `<span class="mp-w-m">${esc(e.method)}</span> ${wStatus(e.status)} <span class="mono">${esc(e.url)}</span> <span class="mp-sub">${wMs(e.ms)}</span>${e.error ? ` <span class="mp-w-err">${esc(e.error)}</span>` : ''}`;
      case 'log': return `<span class="mp-w-lv lv-${esc(e.level)}">${esc(e.level)}</span> ${esc(e.message)}`;
      case 'exception': return `<span class="mp-w-err">${esc(e.class)}</span> ${esc(e.message)} ${wLoc(e.file, e.line)}`;
      case 'dump': return `<span class="mono">${esc(String(e.text).split('\n')[0])}${String(e.text).includes('\n') ? ' …' : ''}</span> ${wLoc(e.file, e.line)}`;
      case 'job': return `<span class="mono">${esc(e.name)}</span>${e.ms != null ? ` <span class="mp-sub">${wMs(e.ms)}</span>` : ''}${e.error ? ` <span class="mp-w-err">${esc(e.error)}</span>` : ''}`;
      case 'mail': return `<span class="mono">${esc(e.subject || '')}</span> <span class="mp-sub">to ${esc((e.to || []).join(', '))}</span>`;
      case 'dropped': return `<span class="mp-sub">${e.count} more events weren't recorded (limit per request)</span>`;
    }
    return '';
  }

  /* ── Queries ── */
  function wQueries() {
    const ws = watchState, f = ws.f;
    let qs = wList('queries').filter(q => wMatch(q.sql));
    if (f.issuesOnly) qs = qs.filter(q => wQueryFlags(q) !== '');
    qs = newestFirst(qs, 400);
    const extra = `<label class="toggle"><span class="switch"><input type="checkbox" data-wf="issuesOnly" ${f.issuesOnly ? 'checked' : ''}><i></i></span>Only slow / N+1 / duplicates</label>
      <label class="toggle" title="Run EXPLAIN once for each distinct slow query and badge its rows with what's wrong (full scan, filesort, no index…). Only reads plans — it never runs the queries."><span class="switch"><input type="checkbox" data-wf="autoExplain" ${ws.autoExplain ? 'checked' : ''}><i></i></span>Auto-explain slow queries</label>`;
    if (!qs.length) return wSearch('Filter SQL…', extra) + wEmpty('No queries' + (f.q || f.issuesOnly ? ' match' : ' yet'), 'Database queries show here with their time, the line that ran them, and N+1 / duplicate / slow flags.');
    return wSearch('Filter SQL…', extra) + `<div class="mp-w-list">` + qs.map(q => `<div class="mp-w-q">
      <div class="mp-w-qh"><span class="mp-w-time">${wTime(q.t)}</span><span class="mp-w-ms${q.ms >= ws.info.settings.slowQueryMs ? ' slow' : ''}">${wMs(q.ms)}</span>${wQueryFlags(q)}${wExplainBits(q)}<span class="spacer"></span><span class="mp-sub">${esc(q.conn || '')}</span>${wLoc(q.file, q.line)}</div>
      <code class="sql">${highlightSQL(q.sql)}</code>${wBindings(q.bindings)}</div>`).join('') + '</div>';
  }


  /* ── EXPLAIN (internal/watch/explain) ──
     Every query row gets an Explain button (prominent on slow / N+1 /
     duplicate ones). The server runs the database's plain EXPLAIN
     through the site's PHP and diagnoses it; Analyze runs the query
     (SELECT only, in a read-only transaction). Results are cached for the
     session per query shape + bindings + connection. */
  const W_EXPLAINABLE = /^[\s(]*(select|with|update|delete)\b|^[\s(]*(insert|replace)\b[\s\S]*\bselect\b/i;
  const wExplainKey = (q, analyze) => (analyze ? 'A\u0000' : '') + (q.norm || q.sql) + '\u0000' + JSON.stringify(q.bindings || []) + '\u0000' + (q.conn || '');
  function wFindQuery(id) {
    const ws = watchState;
    for (const q of wList('queries')) if (q._q === id) return q;
    for (const evs of ws.byRid.values()) for (const e of evs) if (e._q === id) return e;
    return null;
  }
  function wNPlus1Of(q) {
    const a = watchState.analysis.get(q.rid);
    const is = a && a.issues && a.issues.find(i => i.kind === 'n+1' && i.sql === q.norm && i.file === (q.file || '') && i.line === (q.line || 0));
    return is ? is.count : 0;
  }
  function wExplainBits(q) {
    if (!q._q || !W_EXPLAINABLE.test(q.sql || '')) return '';
    const ws = watchState, st = ws.info.settings;
    const hot = q.ms >= st.slowQueryMs || wNPlus1Of(q) > 0 || wQueryFlags(q).includes('dup');
    let badges = '';
    const done = ws.ex.get(wExplainKey(q, false));
    const res = done && done.res ? done.res : null;
    const auto = !res && ws.exNorm.get(q.norm);
    const list = res ? res.badges : auto && auto.badges;
    if (list) badges = list.length ? list.map(b => `<span class="mp-w-badge xb" title="from EXPLAIN — click Explain for why and how to fix it">${esc(b)}</span>`).join('') : '<span class="mp-w-badge ok" title="EXPLAIN found nothing slow in the plan">plan ok</span>';
    else if (auto && auto.err) badges = `<span class="mp-w-badge" title="${esc(auto.err)}">explain failed</span>`;
    else if (auto && auto.pending) badges = '<span class="mp-w-badge">explaining…</span>';
    return `${badges}<button class="sm mp-w-xbtn${hot ? ' hot' : ''}" data-explain="${q._q}" title="Show the query plan — why it's slow and how to fix it">Explain</button>`;
  }
  function wExplainFetch(q, analyze) {
    const ws = watchState, key = wExplainKey(q, analyze);
    const have = ws.ex.get(key);
    if (have && (have.res || have.p)) return have.p || Promise.resolve(have.res);
    const site = cur;
    const p = call('/api/project/watch/explain', { site, sql: q.sql, bindings: q.bindings || [], conn: q.conn || '', analyze: !!analyze, nPlusOne: wNPlus1Of(q) })
      .then(res => { if (watchState === ws) ws.ex.set(key, { res }); return res; })
      .catch(err => { ws.ex.delete(key); throw err; });
    ws.ex.set(key, { p });
    if (ws.ex.size > 400) ws.ex.delete(ws.ex.keys().next().value);
    return p;
  }
  // wAutoExplain queues one explain per distinct slow query shape.
  function wAutoExplain(q) {
    const ws = watchState;
    if (!ws.autoExplain || !ws.info || !ws.info.settings || q.ms < ws.info.settings.slowQueryMs) return;
    if (!q.norm || ws.exAuto.has(q.norm) || !W_EXPLAINABLE.test(q.sql || '')) return;
    ws.exAuto.add(q.norm);
    ws.exNorm.set(q.norm, { pending: true });
    ws.exQueue.push(q);
    wAutoPump();
  }
  async function wAutoPump() {
    const ws = watchState;
    if (ws.exBusy) return;
    ws.exBusy = true;
    try {
      while (ws.exQueue.length && ws.autoExplain && watchState === ws && !destroyed) {
        const q = ws.exQueue.shift();
        try { const res = await wExplainFetch(q, false); ws.exNorm.set(q.norm, { badges: res.badges || [] }); }
        catch (e) { ws.exNorm.set(q.norm, { err: e.message }); }
        wSchedule();
      }
    } finally { ws.exBusy = false; }
  }
  // Findings use `code` spans in their text.
  const wMd = s => esc(s).replace(/`([^`]+)`/g, '<code>$1</code>');
  function wInlineSQL(q) {
    // A display copy with the bindings filled in (the server's copy,
    // quoted by the driver, replaces it once the explain is back).
    let i = 0;
    const b = q.bindings || [];
    return String(q.sql).replace(/('(?:[^'\\]|\\.|'')*'|"[^"]*"|`[^`]*`)|\?/g, (m, quoted) => {
      if (quoted) return m;
      const v = b[i++];
      if (v === undefined) return '?';
      if (v === null) return 'NULL';
      if (typeof v === 'number' || typeof v === 'boolean') return String(v === true ? 1 : v === false ? 0 : v);
      return "'" + String(v).replace(/'/g, "''") + "'";
    });
  }
  function wExplainOpen(q) {
    const ws = watchState;
    const stateOf = { analyze: false, raw: false, confirm: false };
    mpModal({ title: 'Explain', wide: true }, (body, close, card) => {
      card.classList.add('mp-x-card');
      const paint = (res, err, loading) => {
        let html = '';
        if (loading) html = `<div class="mp-x-load"><span class="mp-spin"></span>${stateOf.analyze ? 'Running the query to measure it…' : 'Reading the plan…'}</div>`;
        else if (err) html = `<div class="mp-warn">${esc(err)}</div>`;
        else html = wExplainHTML(res, stateOf);
        const canAnalyze = res && res.canAnalyze && !res.plan.analyzed;
        body.innerHTML = `<code class="sql mp-x-sql">${highlightSQL(res ? res.boundSql || q.sql : wInlineSQL(q))}</code>${html}
          <div class="mp-x-actions">
            <button class="sm" data-x="copy">Copy SQL with bindings</button>
            ${canAnalyze ? (stateOf.confirm
              ? `<span class="mp-x-confirm">Analyze <b>runs</b> this query on your database (in a read-only transaction that is rolled back) to measure real row counts and time.</span><button class="sm danger" data-x="analyze-go">Run it</button><button class="sm" data-x="analyze-no">Cancel</button>`
              : '<button class="sm" data-x="analyze" title="Runs the query to measure real rows and time (SELECT only)">Analyze (runs the query)</button>') : ''}
            ${res && res.plan && res.plan.raw ? `<button class="sm" data-x="raw">${stateOf.raw ? 'Hide' : 'Show'} raw ${esc((res.plan.rawKind || 'json').toUpperCase())}</button>` : ''}
            <span class="spacer"></span><button class="sm primary" data-x="close">Close</button>
          </div>`;
        const $ = k => body.querySelector(`[data-x="${k}"]`);
        $('close').onclick = () => close(null);
        $('copy').onclick = async () => say(await copyText(res ? res.boundSql || wInlineSQL(q) : wInlineSQL(q)) ? 'SQL copied' : 'Could not copy');
        if ($('raw')) $('raw').onclick = () => { stateOf.raw = !stateOf.raw; paint(res); };
        if ($('analyze')) $('analyze').onclick = () => { stateOf.confirm = true; paint(res); };
        if ($('analyze-no')) $('analyze-no').onclick = () => { stateOf.confirm = false; paint(res); };
        if ($('analyze-go')) $('analyze-go').onclick = () => { stateOf.confirm = false; stateOf.analyze = true; load(); };
        body.querySelectorAll('[data-xcopy]').forEach(b => b.onclick = async () => {
          const v = b.closest('[data-xval]');
          say(await copyText(v ? v.dataset.xval : '') ? 'Copied' : 'Could not copy');
        });
      };
      const load = () => {
        paint(null, '', true);
        wExplainFetch(q, stateOf.analyze).then(res => { paint(res); if (!stateOf.analyze) wSchedule(); }).catch(e => paint(null, e.message));
      };
      load();
    });
  }
  const W_SEV = { high: 'High', medium: 'Medium', low: 'Low', info: 'Info' };
  function wExplainHTML(res, st) {
    let h = `<div class="mp-sub mp-x-meta">${esc(res.conn)} · ${esc(res.engine)} ${esc(res.server || '')}${res.via ? ' · ' + esc(res.via) : ''}${res.plan.analyzed ? ' · <span class="mp-w-badge slow">analyzed — the query ran' + (res.plan.timeMs ? ' (' + wMs(res.plan.timeMs) + ')' : '') + '</span>' : ''}</div>`;
    for (const n of res.notes || []) h += `<div class="mp-note mp-x-note">${wMd(n)}</div>`;
    h += '<h4>Findings</h4>';
    if (!res.findings.length) h += '<div class="mp-x-f sev-ok"><div class="t"><span class="mp-x-sev">OK</span><b>Nothing in the plan looks slow</b></div><div class="why">The database finds the rows through an index and doesn\'t sort or scan more than it needs. If the query is still slow, look at how often it runs (N+1) or how many rows it returns.</div></div>';
    for (const f of res.findings) {
      h += `<div class="mp-x-f sev-${esc(f.severity)}"><div class="t"><span class="mp-x-sev">${W_SEV[f.severity] || esc(f.severity)}</span><b>${wMd(f.title)}</b></div>
        <div class="why">${wMd(f.why)}</div>${f.fix ? `<div class="fix"><span class="lbl">Fix</span> ${wMd(f.fix)}</div>` : ''}
        ${f.code ? `<div class="mp-x-code" data-xval="${esc(f.code)}"><pre>${esc(f.code)}</pre><button class="sm" data-xcopy>Copy</button></div>` : ''}</div>`;
    }
    if (res.suggestions.length) {
      h += '<h4>Suggested indexes</h4>';
      for (const s of res.suggestions) {
        h += `<div class="mp-x-sugg"><div class="mp-sub">${s.source === 'server' ? '<span class="mp-w-badge q">SQL Server\'s own hint</span> ' : ''}${wMd(s.reason)}${s.extends ? ` It makes <code>${esc(s.extends)}</code> redundant.` : ''}</div>
          <div class="mp-x-code" data-xval="${esc(s.sql)}"><pre>${esc(s.sql)}</pre><button class="sm" data-xcopy>Copy SQL</button></div>
          <div class="mp-x-code" data-xval="${esc(s.migration)}"><pre>${esc(s.migration)}</pre><button class="sm" data-xcopy>Copy migration</button></div></div>`;
      }
    }
    h += `<h4>Plan${res.plan.cost ? ` <span class="mp-sub">cost ${esc(res.plan.cost.toFixed(1))}</span>` : ''}</h4>`;
    const tab = res.plan.tabular || [];
    if (tab.length) {
      const cols = ['id', 'select_type', 'table', 'type', 'possible_keys', 'key', 'rows', 'filtered', 'Extra'];
      h += `<div class="mp-x-tab"><table><thead><tr>${cols.map(c => `<th>${c}</th>`).join('')}</tr></thead><tbody>${tab.map(r => `<tr>${cols.map(c => {
        const v = r[c] == null ? 'NULL' : String(r[c]);
        const bad = (c === 'type' && v === 'ALL') || (c === 'key' && v === 'NULL' && r.table) || (c === 'Extra' && /filesort|temporary/i.test(v));
        return `<td class="${bad ? 'bad' : ''}${v === 'NULL' ? ' nul' : ''}">${esc(v)}</td>`;
      }).join('')}</tr>`).join('')}</tbody></table></div>`;
    }
    const tree = tab.length ? (res.plan.nodes || []).filter(n => n.op === 'EXPLAIN ANALYZE').flatMap(n => n.children || []) : (res.plan.nodes || []);
    if (tree.length) h += `<div class="mp-x-tree">${wPlanTree(tree, 0)}</div>`;
    const tables = Object.keys(res.tables || {});
    if (tables.length) {
      h += '<h4>Indexes</h4><div class="mp-x-idx">';
      for (const t of tables) {
        const ti = res.tables[t];
        h += `<div><b class="mono">${esc(t)}</b> <span class="mp-sub">${ti.rows != null ? '~' + fmtNum(ti.rows) + ' rows' : ''}</span>${ti.error ? ` <span class="mp-w-err">${esc(ti.error)}</span>` : ''}
          ${(ti.indexes || []).length ? `<ul>${ti.indexes.map(ix => `<li><span class="mono">${esc(ix.name)}</span> <span class="mono mp-sub">(${esc((ix.columns || []).join(', '))}${ix.include && ix.include.length ? ') INCLUDE (' + esc(ix.include.join(', ')) : ''})</span>${ix.primary ? ' <span class="mp-w-badge">primary</span>' : ix.unique ? ' <span class="mp-w-badge">unique</span>' : ''}</li>`).join('')}</ul>` : '<div class="mp-sub">no indexes</div>'}</div>`;
      }
      h += '</div>';
    }
    if (st.raw) {
      let raw = res.plan.raw || '';
      if (res.plan.rawKind === 'json') { try { raw = JSON.stringify(JSON.parse(raw), null, 2); } catch {} }
      h += `<h4>Raw plan</h4><pre class="mp-term mp-x-raw">${esc(raw)}</pre>`;
      if (res.plan.rawAnalyze) h += `<h4>EXPLAIN ANALYZE</h4><pre class="mp-term mp-x-raw">${esc(res.plan.rawAnalyze)}</pre>`;
    }
    return h;
  }
  function wPlanTree(nodes, depth) {
    return nodes.map(n => {
      const flags = [];
      if (n.fullScan) flags.push('<span class="mp-w-badge n1">full scan</span>');
      if (n.indexScan) flags.push('<span class="mp-w-badge slow">index scan</span>');
      if (n.filesort) flags.push('<span class="mp-w-badge slow">sort</span>');
      if (n.diskSort || n.diskHash) flags.push('<span class="mp-w-badge n1">on disk</span>');
      if (n.temporary) flags.push('<span class="mp-w-badge slow">temporary</span>');
      if (n.dependent) flags.push('<span class="mp-w-badge slow">per row</span>');
      if (n.lookup) flags.push('<span class="mp-w-badge">lookup</span>');
      if (n.autoIndex) flags.push('<span class="mp-w-badge n1">auto index</span>');
      const nums = [];
      if (n.rows) nums.push('rows ≈ ' + fmtNum(n.rows));
      if (n.actualRows >= 0) nums.push('actual ' + fmtNum(n.actualRows) + (n.loops > 1 ? ' × ' + fmtNum(n.loops) + ' loops' : ''));
      if (n.removed) nums.push(fmtNum(n.removed) + ' removed by filter');
      if (n.cost) nums.push('cost ' + n.cost.toFixed(1));
      if (n.timeMs) nums.push(wMs(n.timeMs));
      const on = n.table && !String(n.op).includes(n.table) ? ` on <b class="mono">${esc(n.table)}</b>${n.alias ? ' <span class="mp-sub">' + esc(n.alias) + '</span>' : ''}` : '';
      const ix = n.index && !String(n.op).includes(n.index) ? ` using <span class="mono">${esc(n.index)}</span>` : '';
      return `<div class="mp-x-node" style="--d:${depth}"><div class="op"><span class="mono">${esc(n.op)}</span>${on}${ix} ${flags.join('')}<span class="mp-sub mono">${esc(nums.join(' · '))}</span></div>${n.detail ? `<div class="mp-sub mono det">${esc(n.detail)}</div>` : ''}</div>` + (n.children && n.children.length ? wPlanTree(n.children, depth + 1) : '');
    }).join('');
  }

  /* ── simple lists ── */
  function wSimple(key, ph, emptyTitle, emptyText, row, text) {
    const items = newestFirst(wList(key).filter(e => wMatch(text(e))));
    if (!items.length) return wSearch(ph) + wEmpty(emptyTitle, emptyText);
    return wSearch(ph) + `<div class="mp-w-list">` + items.map(e => `<div class="mp-w-row"><span class="mp-w-time">${wTime(e.t)}</span>${row(e)}</div>`).join('') + '</div>';
  }
  const wJobs = () => wSimple('jobs', 'Filter jobs…', 'No jobs yet', 'Jobs dispatched by the site, and processed by queue workers Mullion runs (Workers tab), show here.',
    e => `<span class="mp-w-badge ${e.op === 'failed' ? 'exc' : e.op === 'processed' ? 'ok' : 'q'}">${esc(e.op)}</span><span class="mp-w-main mono">${esc(e.name)}</span><span class="mp-sub">${esc(e.conn || '')}${e.queue ? ' · ' + esc(e.queue) : ''}${e.attempts ? ' · try ' + e.attempts : ''}</span>${e.error ? `<span class="mp-w-err">${esc(e.error)}</span>` : ''}<span class="mp-w-ms">${e.ms != null ? wMs(e.ms) : ''}</span>`,
    e => e.name);
  const wCache = () => wSimple('cache', 'Filter keys…', 'No cache activity yet', 'Cache hits, misses, writes and forgets show here.',
    e => `<span class="mp-w-badge c-${esc(e.op)}">${esc(e.op)}</span><span class="mp-w-main mono">${esc(e.key)}</span><span class="mp-sub">${esc(e.store || '')}${e.ttl ? ' · ttl ' + e.ttl + 's' : ''}</span>`, e => e.key);
  const wEvents = () => wSimple('events', 'Filter events…', 'No app events yet', 'Events your app dispatches show here (the framework\'s own are left out).',
    e => `<span class="mp-w-main mono">${esc(e.name)}</span>`, e => e.name);
  const wViews = () => wSimple('views', 'Filter views…', 'No views rendered yet', 'Every Blade view the site renders, with its file.',
    e => `<span class="mp-w-main mono">${esc(e.name)}</span>${wLoc(e.file, 0)}`, e => e.name);
  const wHttp = () => wSimple('http', 'Filter URLs…', 'No outgoing HTTP calls yet', 'Calls made with Laravel\'s Http client show here.',
    e => `<span class="mp-w-m">${esc(e.method)}</span>${wStatus(e.status)}<span class="mp-w-main mono">${esc(e.url)}</span>${e.error ? `<span class="mp-w-err">${esc(e.error)}</span>` : ''}<span class="mp-w-ms">${wMs(e.ms)}</span>`, e => e.url);

  /* ── Logs & Exceptions ── */
  function wLogs() {
    const ws = watchState, f = ws.f;
    const levels = ['', 'debug', 'info', 'notice', 'warning', 'error', 'critical', 'alert', 'emergency'];
    const extra = `<select data-wf="level" class="sm">${levels.map(l => `<option value="${l}" ${f.level === l ? 'selected' : ''}>${l || 'Any level'}</option>`).join('')}</select>`;
    const rank = l => Math.max(0, levels.indexOf(l));
    const items = newestFirst(ws.logs.filter(e => (!f.level || rank(e.level) >= rank(f.level)) && wMatch(e.message + ' ' + (e.class || ''))), 400);
    if (!items.length) return wSearch('Filter messages…', extra) + wEmpty('No log entries' + (f.q || f.level ? ' match' : ' yet'), 'New lines in <span class="mono">storage/logs/*.log</span> show here as they are written.');
    return wSearch('Filter messages…', extra) + `<div class="mp-w-list">` + items.map(e => {
      // Keyed by the entry itself, not its position: new lines arrive on
      // top, and an index would hand the open state to another row.
      const key = 'l:' + e.t + (e.logFile || '') + (e.level || '') + String(e.message || '').slice(0, 60);
      const open = ws.open.has(key);
      return `<div class="mp-w-row log${open ? ' open' : ''}" data-wopen="${esc(key)}"><span class="mp-w-time">${wTime(e.t)}</span><span class="mp-w-lv lv-${esc(e.level)}">${esc(e.level)}</span>
        <span class="mp-w-main">${e.class ? `<span class="mp-w-err">${esc(e.class)}</span> ` : ''}${esc(String(e.message).split('\n')[0])}</span><span class="mp-sub">${esc(e.logFile || '')}</span></div>
        ${open ? `<div class="mp-w-detail">${e.class ? wTrace(e) : `<pre class="mp-term short">${esc(e.message)}</pre>`}</div>` : ''}`;
    }).join('') + '</div>';
  }
  function wTrace(e) {
    const frames = (e.trace || []).map(fr => `<div class="mp-w-frame${/[\\/]vendor[\\/]/.test(fr.file || '') ? ' vendor' : ''}">${fr.file ? wLoc(fr.file, fr.line) : '<span class="mp-sub">[internal]</span>'} <span class="mono mp-sub">${esc(fr.call)}</span></div>`).join('');
    return `<div class="mp-w-exc"><b class="mp-w-err">${esc(e.class)}</b> ${esc(e.message)}<div style="margin:6px 0 8px">${wLoc(e.file, e.line)}</div>
      ${frames ? `<label class="toggle" style="margin-bottom:6px"><span class="switch"><input type="checkbox" data-wvendor ${watchState.f.vendor ? 'checked' : ''}><i></i></span>Show vendor frames</label><div class="mp-w-frames${watchState.f.vendor ? ' all' : ''}">${frames}</div>` : ''}</div>`;
  }
  function wExceptionGroups() {
    const ws = watchState;
    const src = ws.fileExc.length ? ws.fileExc : ws.phpExc;
    const groups = new Map();
    for (const e of src) {
      const k = e.class + '@' + shortPath(e.file || '') + ':' + e.line;
      const g = groups.get(k);
      if (g) { g.count++; g.last = e; } else groups.set(k, { key: k, count: 1, first: e, last: e });
    }
    return [...groups.values()].sort((a, b) => b.last.t - a.last.t);
  }
  function wExceptions() {
    const ws = watchState;
    const gs = wExceptionGroups().filter(g => wMatch(g.last.class + ' ' + g.last.message));
    if (!gs.length) return wSearch('Filter exceptions…') + wEmpty('No exceptions', 'Exceptions your app reports show here, grouped — identical ones counted together.');
    return wSearch('Filter exceptions…') + `<div class="mp-w-list">` + gs.map(g => {
      const key = 'x:' + g.key, open = ws.open.has(key), e = g.last;
      return `<div class="mp-w-row${open ? ' open' : ''}" data-wopen="${esc(key)}"><span class="mp-w-time">${wTime(e.t)}</span>${g.count > 1 ? `<span class="mp-w-badge exc">×${g.count}</span>` : ''}
        <span class="mp-w-main"><span class="mp-w-err">${esc(e.class)}</span> ${esc(String(e.message).split('\n')[0])}</span>${wLoc(e.file, e.line)}</div>
        ${open ? `<div class="mp-w-detail">${wTrace(e)}</div>` : ''}`;
    }).join('') + '</div>';
  }

  /* ── Dumps ── */
  function wDumps() {
    const items = newestFirst(wList('dumps').filter(e => wMatch(e.text)), 200);
    if (!items.length) return wSearch('Filter dumps…') + wEmpty('No dumps yet', `Call <span class="mono">dump($x)</span> or <span class="mono">dd($x)</span> anywhere in the app — it shows here${watchState.info.settings.dumpsInPage ? ' and in the page' : ' instead of in the page'}.`);
    return wSearch('Filter dumps…') + `<div class="mp-w-list">` + items.map(e => `<div class="mp-w-dump"><div class="mp-w-qh"><span class="mp-w-time">${wTime(e.t)}</span>${e.label ? `<b class="mono">${esc(e.label)}</b>` : ''}<span class="spacer"></span>${wLoc(e.file, e.line)}</div><pre class="mp-term short">${esc(e.text)}</pre></div>`).join('') + '</div>';
  }

  /* ── Mail ── */
  async function wLoadMail() {
    const ws = watchState, g = gen;
    if (!ws.mailT) ws.mailT = setInterval(() => { if (tab === 'watch' && ws.sub === 'mail' && visible()) wLoadMail(); }, 5000);
    try {
      const m = await call('/api/project/watch/mail', { site: cur });
      if (!live(g)) return;
      ws.mail = m;
    } catch (e) { ws.mail = { error: e.message, messages: [] }; }
    if (tab === 'watch' && ws.sub === 'mail') { wPaintSubs(); wPaintBody(); }
  }
  function wMailShown() {
    const ws = watchState, m = ws.mail;
    if (!m) return [];
    const sent = wList('mails');
    const subjects = new Set(sent.map(x => (x.subject || '') + '|' + (x.to || []).join(',').toLowerCase()));
    const cl = (ws.info && ws.info.cleared) || {};
    const since = Math.max(cl[''] || 0, cl.mail || 0);
    return (m.messages || []).filter(x => {
      if (since && Date.parse(x.Created) / 1000 < since) return false;
      const viaEvent = subjects.has((x.Subject || '') + '|' + (x.To || []).map(a => a.Address).join(',').toLowerCase());
      x._match = x.match || (viaEvent ? 'event' : '');
      return ws.mailAll || x._match;
    });
  }
  function wMail() {
    const ws = watchState, m = ws.mail;
    if (!m) { wLoadMail(); return `<div class="mp-loading"><div class="spin"></div>Reading Mail…</div>`; }
    if (!m.installed) return wEmpty('Mail isn\'t set up', 'Install Mail (Mailpit) on the Mail page to catch the emails this site sends.');
    if (m.error) return wEmpty('Mail isn\'t running', esc(m.error));
    const shown = wMailShown().filter(x => wMatch(x.Subject + ' ' + (x.From ? x.From.Address : '')));
    const extra = `<label class="toggle" title="all recent messages, not only this project's"><span class="switch"><input type="checkbox" data-wf="mailAll" ${ws.mailAll ? 'checked' : ''}><i></i></span>All recent</label><button class="sm mp-btn-ic" data-id="mp-w-mailpage">${ICON.open}Mail page</button>`;
    const how = m.from ? `sent from <span class="mono">${esc(m.from)}</span> (MAIL_FROM_ADDRESS)` : 'matching the mails the deep watcher saw this site send';
    if (!shown.length) return wSearch('Filter by subject or sender…', extra) + wEmpty('No emails' + (ws.mailAll ? '' : ' from this project'), `Messages Mailpit caught ${how} show here.`);
    return wSearch('Filter by subject or sender…', extra) + `<div class="mp-sub" style="margin-bottom:8px">Messages ${how}.</div><div class="mp-w-list">` + shown.map(x => `<div class="mp-w-row" data-wmail="${esc(x.ID)}" title="open on the Mail page">
      <span class="mp-w-time">${esc(new Date(x.Created).toLocaleTimeString([], { hour12: false }))}</span>${x.Read ? '' : '<span class="mp-w-badge q">new</span>'}
      <span class="mp-w-main"><b>${esc(x.Subject || '(no subject)')}</b> <span class="mp-sub">${esc((x.To || []).map(a => a.Address).join(', '))}</span></span>
      <span class="mp-sub">${esc(x.From ? x.From.Address : '')}</span></div>`).join('') + '</div>';
  }

  function wBindBody(body) {
    const ws = watchState;
    const q = body.querySelector('[data-wf="q"]');
    let qt = 0;
    if (q) q.oninput = e => { clearTimeout(qt); qt = setTimeout(() => { ws.f.q = e.target.value; wPaintBody(); }, 200); };
    body.querySelectorAll('[data-wfs]').forEach(b => b.onclick = () => { ws.f.status = b.dataset.wfs; wPaintBody(true); });
    const m = body.querySelector('[data-wf="method"]');
    if (m) m.onchange = () => { ws.f.method = m.value; wPaintBody(true); };
    const lv = body.querySelector('[data-wf="level"]');
    if (lv) lv.onchange = () => { ws.f.level = lv.value; wPaintBody(true); };
    const ns = body.querySelector('[data-wf="noStatic"]');
    if (ns) ns.onchange = () => { ws.f.noStatic = ns.checked; prefSet('watch.noStatic', ns.checked); wPaintBody(true); };
    const io = body.querySelector('[data-wf="issuesOnly"]');
    if (io) io.onchange = () => { ws.f.issuesOnly = io.checked; wPaintBody(true); };
    const ae = body.querySelector('[data-wf="autoExplain"]');
    if (ae) ae.onchange = () => {
      ws.autoExplain = ae.checked;
      prefSet('watch.autoExplain', ae.checked);
      if (ae.checked) for (const q of wList('queries')) wAutoExplain(q);
      wPaintBody();
    };
    body.querySelectorAll('[data-explain]').forEach(b => b.onclick = e => {
      e.preventDefault();
      e.stopPropagation();
      const q = wFindQuery(+b.dataset.explain);
      if (q) wExplainOpen(q); else say('That query is no longer in the list');
    });
    const ma = body.querySelector('[data-wf="mailAll"]');
    if (ma) ma.onchange = () => { ws.mailAll = ma.checked; wPaintSubs(); wPaintBody(true); };
    const mp = body.querySelector('[data-id="mp-w-mailpage"]');
    if (mp) mp.onclick = () => { location.hash = 'mail'; };
    body.querySelectorAll('[data-wmail]').forEach(r => r.onclick = () => {
      const id = r.dataset.wmail;
      location.hash = 'mail';
      setTimeout(() => { if (typeof window.openMailMessage === 'function') window.openMailMessage(id); }, 150);
    });
    body.querySelectorAll('[data-wvendor]').forEach(c => c.onchange = () => { ws.f.vendor = c.checked; wPaintBody(); });
    body.querySelectorAll('[data-wopen]').forEach(r => r.onclick = e => {
      if (e.target.closest('a, button, input, label, select')) return;
      const k = r.dataset.wopen;
      if (ws.open.has(k)) ws.open.delete(k); else ws.open.add(k);
      wPaintBody();
    });
    body.querySelectorAll('[data-open]').forEach(a => a.onclick = e => {
      e.preventDefault();
      e.stopPropagation();
      call('/api/project/watch/open', { site: cur, file: a.dataset.open, line: +a.dataset.line || 0 })
        .then(r => say('Opened in ' + ({ vscode: 'VS Code', cursor: 'Cursor', phpstorm: 'PhpStorm', zed: 'Zed', sublime: 'Sublime Text', default: 'the default app' }[r.editor] || r.editor)))
        .catch(err => say('Error: ' + err.message));
    });
  }

  /* ════════════════════════════════════════════════════════════════
     SETTINGS
     ════════════════════════════════════════════════════════════════ */
  function renderSettings() {
    const rows = [];
    if (isPHP()) {
      rows.push(`<div class="k">PHP version<small>for this site only</small></div>
        <div><select data-id="mp-set-php" class="mono">${['<option value="">Global (' + esc(info.globalPhp || '—') + ')</option>']
          .concat(info.phpInstalled.map(v => `<option value="${esc(v)}" ${info.phpPinned === v ? 'selected' : ''}>PHP ${esc(v)}</option>`)).join('')}</select>
          <span class="mp-sub">${info.phpPinned ? 'Pinned — it keeps this version when the global one changes.' : 'Follows the global version.'}</span></div>`);
    }
    if (isNode()) {
      rows.push(`<div class="k">Node version<small>runs the dev server & scripts</small></div>
        <div><select data-id="mp-set-node" class="mono">${['<option value="">.nvmrc / default (' + esc(info.globalNode || '—') + ')</option>']
          .concat(info.nodeInstalled.map(v => `<option value="${esc(v)}" ${info.nodePinned === v ? 'selected' : ''}>Node ${esc(v)}</option>`)).join('')}</select>
          <span class="mp-sub">${info.node ? 'Currently ' + esc(info.node) + '.' : ''}</span></div>`);
      rows.push(`<div class="k">Serve mode<small>what ${esc(info.host)} shows</small></div>
        <div><div class="mp-seg"><button data-mode="dev" class="${info.mode !== 'build' ? 'active' : ''}">Dev server</button><button data-mode="build" class="${info.mode === 'build' ? 'active' : ''}">Production build</button></div>
          <span class="mp-sub">${info.mode === 'build' ? 'Serving <span class="mono">' + esc(info.buildDir || 'dist') + '/</span> as static files.' : 'Hot reload, wakes on demand and sleeps when idle.'}</span></div>`);
      if (info.mode !== 'build') {
        rows.push(`<div class="k">Dev server</div>
          <div>${info.devRunning ? `<span class="mp-status"><span class="mp-dot on"></span>running on <span class="mono">:${info.devRunning}</span></span>
            <button class="sm" data-dev="restart">Restart</button><button class="sm" data-dev="stop">Stop</button>`
            : `<span class="mp-status"><span class="mp-dot ${info.devPaused ? '' : 'sleep'}"></span>${info.devPaused ? 'stopped' : 'sleeping — wakes when you open the site'}</span><button class="sm primary" data-dev="start">Start</button>`}</div>`);
        const idle = info.idle || { mode: 'global', noTabMin: 2, withTabMin: 10, globalNoTabMin: 2, globalWithTabMin: 10 };
        const globalText = idle.globalNever ? 'never sleep' : `${idle.globalNoTabMin} / ${idle.globalWithTabMin} min`;
        rows.push(`<div class="k">Sleep when idle<small>no tab open / tab open</small></div>
          <div><select data-id="mp-set-idle">
              <option value="global" ${idle.mode === 'global' ? 'selected' : ''}>Use global (${esc(globalText)})</option>
              <option value="custom" ${idle.mode === 'custom' ? 'selected' : ''}>Custom</option>
              <option value="never" ${idle.mode === 'never' ? 'selected' : ''}>Never sleep</option>
            </select>
            <span data-id="mp-idle-custom" style="display:${idle.mode === 'custom' ? 'inline-flex' : 'none'}; gap:8px; align-items:center">
              <input type="number" data-id="mp-idle-notab" class="mono" min="1" max="1440" value="${idle.noTabMin}" style="width:80px" title="minutes idle with no tab open">
              <input type="number" data-id="mp-idle-withtab" class="mono" min="1" max="1440" value="${idle.withTabMin}" style="width:80px" title="minutes idle with a tab open">
              <span class="mp-sub">min</span>
              <button class="sm" data-id="mp-idle-save">Save</button>
            </span></div>`);
      }
    }
    rows.push(`<div class="k">HTTPS<small>local certificate</small></div>
      <div><label class="toggle"><span class="switch"><input type="checkbox" data-id="mp-set-secure" ${info.secure ? 'checked' : ''}><i></i></span>${info.secure ? 'On' : 'Off'}</label></div>`);
    rows.push(`<div class="k">Name<small>the domain is &lt;name&gt;.${esc(info.tld)}</small></div>
      <div><span class="mono">${esc(info.name)}</span><button class="sm" data-id="mp-set-rename">Rename…</button></div>`);
    rows.push(`<div class="k">Folder</div>
      <div style="flex-wrap:nowrap"><span class="mono muted" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0">${esc(info.path)}</span>
        <button class="sm" data-id="mp-set-reveal" style="flex:none">${info.windows ? 'Show in Explorer' : 'Show in Finder'}</button></div>`);
    els.panel.innerHTML = `
      <section class="card">
        <div class="card-head">${ICON.gear}<h2>Settings</h2></div>
        <div class="card-body"><div class="mp-kv">${rows.join('')}</div></div>
      </section>
      <section class="card mp-danger">
        <div class="card-head">${ICON.alert}<h2>Danger zone</h2></div>
        <div class="card-body"><div class="row">
          <div style="flex:1; min-width:220px"><b>Unlink this site</b><div class="mp-sub">Mullion stops serving ${esc(info.host)} and stops its workers. Your project files are untouched.</div></div>
          <button class="danger" data-id="mp-set-unlink">Unlink ${esc(info.name)}</button>
        </div></div>
      </section>`;
    const p = els.panel;
    const php = p.querySelector('[data-id="mp-set-php"]');
    if (php) php.onchange = () => action('Switching PHP version…', '/api/sites/isolate', { name: cur, version: php.value }, 'PHP version updated');
    const node = p.querySelector('[data-id="mp-set-node"]');
    if (node) node.onchange = () => action('Switching Node version…', '/api/sites/node', { name: cur, version: node.value }, 'Node version updated');
    p.querySelectorAll('[data-mode]').forEach(b => b.onclick = () => {
      if ((info.mode === 'build') === (b.dataset.mode === 'build')) return;
      action(b.dataset.mode === 'build' ? 'Switching to the production build… (builds first if needed — can take a minute)' : 'Switching to the dev server…',
        '/api/sites/mode', { name: cur, mode: b.dataset.mode }, 'Done');
    });
    p.querySelectorAll('[data-dev]').forEach(b => b.onclick = async () => {
      const v = b.dataset.dev;
      if (v === 'stop' && !(await confirmBox({ title: 'Stop dev server?', message: 'Opening the site later wakes it again.', confirmText: 'Stop' }))) return;
      action({ start: 'Starting the dev server… (first start can take a minute)', stop: 'Stopping the dev server…', restart: 'Restarting the dev server…' }[v],
        '/api/dev/' + v, { name: cur }, { start: 'Started', stop: 'Stopped', restart: 'Restarted' }[v]);
    });
    const idleSel = p.querySelector('[data-id="mp-set-idle"]');
    if (idleSel) {
      const custom = p.querySelector('[data-id="mp-idle-custom"]');
      const saveIdle = () => {
        const mode = idleSel.value;
        const noTabMin = parseInt(p.querySelector('[data-id="mp-idle-notab"]').value, 10) || 0;
        const withTabMin = parseInt(p.querySelector('[data-id="mp-idle-withtab"]').value, 10) || 0;
        if (mode === 'custom' && !([noTabMin, withTabMin].every(v => v >= 1 && v <= 1440))) { say('Use 1 to 1440 minutes'); return; }
        action('Saving…', '/api/sites/idle', { name: cur, mode, noTabMin, withTabMin },
          mode === 'never' ? 'This dev server never sleeps now' : mode === 'custom' ? 'Idle limits saved' : 'Follows the global setting');
      };
      idleSel.onchange = () => {
        custom.style.display = idleSel.value === 'custom' ? 'inline-flex' : 'none';
        if (idleSel.value !== 'custom') saveIdle();
      };
      p.querySelector('[data-id="mp-idle-save"]').onclick = saveIdle;
    }
    p.querySelector('[data-id="mp-set-secure"]').onchange = e => setSecure(e.target.checked);
    p.querySelector('[data-id="mp-set-reveal"]').onclick = () => call('/api/open-path', { path: info.path }).catch(e => say('Error: ' + e.message));
    p.querySelector('[data-id="mp-set-rename"]').onclick = rename;
    p.querySelector('[data-id="mp-set-unlink"]').onclick = unlink;
  }

  async function rename() {
    const from = cur;
    const to = await promptBox({
      title: 'Rename site',
      message: `New name for <span class="mono">${esc(from)}</span> — the domain becomes &lt;name&gt;.${esc(info.tld || 'test')}.`,
      input: { value: from, placeholder: 'site-name' },
      confirmText: 'Rename',
    });
    if (!to || to.trim() === from) return;
    try {
      await busy('Renaming ' + from + ' → ' + to + '…', () => call('/api/sites/rename', { name: from, newName: to.trim() }));
    } catch (e) { say('Error: ' + e.message); return; }
    refreshPanel();
    // The server slugifies the name; find what it became.
    const next = to.trim().toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, ''); // config.Slugify
    const fav = favsOf(from);
    if (fav.length) prefSet('fav:' + next, fav);
    say('Renamed to ' + next);
    window.dispatchEvent(new CustomEvent('mullion:project-renamed', { detail: { from, to: next } }));
    if (!destroyed && cur === from) { cur = next; load(); }
  }

  async function unlink() {
    const name = cur, kind = info.kind;
    const ok = await confirmBox({
      title: 'Unlink ' + esc(name) + '?',
      message: `Mullion stops serving <span class="mono">${esc(info.host)}</span>${info.workers ? ' and removes its workers' : ''}. Your project files are untouched — you can link the folder again any time.`,
      confirmText: 'Unlink', danger: true,
    });
    if (!ok) return;
    try { await busy('Unlinking…', () => call('/api/sites/unlink', { name })); }
    catch (e) { say('Error: ' + e.message); return; }
    say('Unlinked ' + name);
    destroy();
    refreshPanel();
    window.dispatchEvent(new CustomEvent('mullion:project-unlinked', { detail: { name, kind } }));
  }

  const api = {
    get site() { return cur; },
    unmount: destroy,
    destroy,
    // reload() re-reads the site (after a change made elsewhere).
    reload: () => { if (!destroyed) reloadInfo(); },
    // showTab(name) switches tabs (queued until the page has loaded).
    // Names: commands, workers, packages, git, cache, watch, terminal, domains, settings.
    showTab: name => { if (info) showTab(name); else pendingTab = name; },
    get tab() { return tab; },
    // fit() refits the embedded terminal (call after showing the container).
    fit: () => { checkVisible(); fitTerminal(); },
  };
  load();
  return api;
  }

  /* ── registry ──────────────────────────────────────────────────── */
  const instances = [];
  function forget(inst) {
    const i = instances.indexOf(inst);
    if (i >= 0) instances.splice(i, 1);
  }

  window.MullionProject = {
    // mount(el, site) → instance {site, unmount(), reload(), showTab(name), fit()}.
    // Any number may be alive at once, each in its own container;
    // mounting into a container that already holds one replaces it.
    mount(el, siteName) {
      if (!el || !siteName) return null;
      for (const inst of instances.filter(x => x._el === el)) inst.unmount();
      const inst = createInstance(el, siteName);
      Object.defineProperty(inst, '_el', { value: el });
      instances.push(inst);
      return inst;
    },
    // unmount(instanceOrEl?) — no argument: the most recently mounted one.
    unmount(target) {
      let inst = null;
      if (!target) inst = instances[instances.length - 1] || null;
      else if (instances.includes(target)) inst = target;
      else inst = instances.find(x => x._el === target) || null;
      if (inst) inst.unmount();
    },
    // current() → the most recently mounted instance's site (null if none).
    current: () => (instances.length ? instances[instances.length - 1].site : null),
    instances: () => instances.slice(),
    // reload() — no argument: every instance (after a change made elsewhere).
    reload(site) { instances.forEach(x => { if (!site || x.site === site) x.reload(); }); },
  };
})();
