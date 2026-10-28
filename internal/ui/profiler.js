/* The project page's Performance → Profiler sub-tab.
 *
 * window.MullionProfilerPanel(container, site) renders into container
 * and returns { site, reload(), unmount() }; calling it again on the same
 * container replaces the previous instance. project.js mounts it when the
 * Profiler sub-tab opens. Backed by /api/project/profiler/* (profiler.go):
 *
 *   - turning SPX on for the site (it may be built first — a job), and
 *     the profiling mode: browser-triggered, the next N requests,
 *     requests matching a path, or every request;
 *   - the site's stored profiles (time, request, wall time, memory,
 *     calls), delete / clear all;
 *   - one profile: a zoomable, searchable flame graph (canvas, drawn
 *     top-down: callers above callees) and the top functions by self
 *     and inclusive time, plus a link to SPX's own report viewer.
 *
 * It uses the panel's globals when present (api(), toast(),
 * confirmModal()) and the design tokens of index.html; profiler.css
 * ships beside it.
 */
(function () {
  'use strict';
  if (window.MullionProfilerPanel) return;

  if (!document.querySelector('link[href="/profiler.css"]')) {
    const l = document.createElement('link');
    l.rel = 'stylesheet';
    l.href = '/profiler.css';
    document.head.appendChild(l);
  }

  /* ── plumbing ───────────────────────────────────────────────────── */
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
  const confirmBox = opts => (G('confirmModal') ? G('confirmModal')(opts) : Promise.resolve(window.confirm(opts.message || opts.title || 'Sure?')));
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const sleep = ms => new Promise(r => setTimeout(r, ms));

  async function waitJob(id, onStatus) {
    for (;;) {
      await sleep(700);
      const st = await call('/api/job', { id });
      if (onStatus && (st.status || st.log)) onStatus(st.status || st.log);
      if (st.done) {
        if (st.error) throw new Error(st.error);
        return st.result;
      }
    }
  }

  /* ── formatting ─────────────────────────────────────────────────── */
  function fmtNs(ns) {
    ns = Number(ns) || 0;
    if (ns >= 1e9) return (ns / 1e9).toFixed(ns >= 1e10 ? 1 : 2) + ' s';
    if (ns >= 1e6) return (ns / 1e6).toFixed(ns >= 1e8 ? 0 : ns >= 1e7 ? 1 : 2) + ' ms';
    if (ns >= 1e3) return (ns / 1e3).toFixed(ns >= 1e5 ? 0 : 1) + ' µs';
    return Math.round(ns) + ' ns';
  }
  function fmtBytes(b) {
    b = Number(b) || 0;
    const neg = b < 0; b = Math.abs(b);
    const u = ['B', 'KB', 'MB', 'GB'];
    let i = 0;
    while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
    return (neg ? '−' : '') + (i ? b.toFixed(b >= 100 ? 0 : 1) : Math.round(b)) + ' ' + u[i];
  }
  const fmtInt = n => (Number(n) || 0).toLocaleString();
  function fmtAgo(unix) {
    const s = Math.max(0, Date.now() / 1000 - unix);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return new Date(unix * 1000).toLocaleDateString();
  }
  const fmtTime = unix => new Date(unix * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  const pct = (v, total) => (total > 0 ? (v / total * 100) : 0).toFixed(1) + '%';

  const MODES = [
    { id: 'browser', label: 'When I ask', hint: 'Only requests you trigger: SPX\'s control panel switches profiling on for your browser session, or open a URL with the profile link. Nothing else is slowed down.' },
    { id: 'next', label: 'Next requests', hint: 'Profile the next N requests to the site (optionally only matching paths), then stop.' },
    { id: 'path', label: 'Matching path', hint: 'Profile every request whose path matches — a prefix like /api or a glob like /orders/*/edit.' },
    { id: 'all', label: 'Every request', hint: 'Profile every request. Each one writes a report and runs slower; old reports are trimmed to the limits below.' },
  ];

  /* ── the panel ──────────────────────────────────────────────────── */
  const instances = new WeakMap();

  function MullionProfilerPanel(container, site) {
    const prev = instances.get(container);
    if (prev) prev.unmount();

    let alive = true;
    let info = null;
    let reports = [];
    let open = null;          // { meta, profile }
    let openId = '';
    let busyMsg = '';
    let listTimer = 0;
    let flame = null;
    const form = { mode: 'browser', count: 10, path: '', builtins: false };

    container.classList.add('mpf-root');
    container.innerHTML = `<div class="mpf">
      <section class="card mpf-head" data-id="head"><div class="card-body"><div class="mpf-loading">Loading the profiler…</div></div></section>
      <section class="card" data-id="list-card" hidden>
        <div class="card-head"><h2>Profiles</h2><span class="hint" data-id="list-hint"></span>
          <div class="mpf-head-actions"><button class="sm" data-id="refresh" title="Look for new profiles">Refresh</button><button data-id="clear" class="sm danger">Clear all</button></div></div>
        <div class="card-body mpf-list" data-id="list"></div>
      </section>
      <section class="card" data-id="view-card" hidden></section>
    </div>`;
    const q = id => container.querySelector(`[data-id="${id}"]`);

    const shortName = name => {
      const p = info && info.path ? info.path.replace(/\/+$/, '') + '/' : '';
      return p ? name.split(p).join('') : name;
    };

    /* header: status + settings */
    function renderHead() {
      const box = q('head');
      if (!info) return;
      if (!info.supported) {
        box.innerHTML = `<div class="card-head"><h2>Profiler</h2><span class="badge off">Not available</span></div>
          <div class="card-body"><p class="mpf-note">${esc(info.note || 'The profiler is not available on this platform yet.')}</p>
          <p class="muted">Mullion uses <a href="https://github.com/NoiseByNorthwest/php-spx" target="_blank" rel="noopener">SPX</a>, which supports macOS and Linux only.</p></div>`;
        return;
      }
      const s = info.settings;
      if (s && !busyMsg) {
        form.mode = s.mode || 'browser';
        form.count = s.count || form.count;
        form.path = s.path || '';
        form.builtins = !!s.builtins;
      }
      const mode = MODES.find(m => m.id === form.mode) || MODES[0];
      const gate = info.gate;
      let state = '';
      if (info.enabled) {
        if (s.mode === 'next' && gate) state = `<span class="badge on">Profiling · ${fmtInt(gate.captured)} of ${fmtInt(s.count)} captured</span>`;
        else if (s.mode === 'path' && gate) state = `<span class="badge on">Profiling ${esc(s.path)} · ${fmtInt(gate.captured)} captured</span>`;
        else if (s.mode === 'all') state = '<span class="badge warnb">Profiling every request</span>';
        else state = '<span class="badge on">On · when you ask</span>';
      } else state = '<span class="badge off">Off</span>';

      const links = info.enabled ? `<div class="mpf-links">
          <a class="button" href="${esc(info.controlUrl)}" target="_blank" rel="noopener" title="SPX's control panel on the site: switch profiling on for this browser session, see its own report list">Open SPX control panel</a>
          <a class="button" href="${esc(info.profileUrl)}" target="_blank" rel="noopener" title="Opens the home page with ?SPX_ENABLED=1 — add the same query to any URL of the site to profile it once">Profile the home page once</a>
          <span class="muted">Profile any page once: add <code>?SPX_ENABLED=1&amp;SPX_KEY=${esc(s.key)}</code> to its URL.</span>
        </div>` : '';
      const wired = info.enabled && !info.wired
        ? '<p class="mpf-warn">Caddy is not passing this site\'s profiler settings to PHP yet — restart Mullion\'s services (<code>mullion start</code>) and try again.</p>' : '';
      const finished = info.finished ? '<p class="mpf-ok">All requested profiles were captured — switched back to “when I ask”.</p>' : '';
      box.innerHTML = `<div class="card-head"><h2>Profiler</h2>${state}
          <span class="hint">SPX ${esc(info.spxVersion)} · PHP ${esc(info.php || '?')}${info.spxLoaded ? '' : ' · not built yet'}</span></div>
        <div class="card-body">
          ${finished}${wired}
          <div class="mpf-modes" role="radiogroup" aria-label="What to profile">${MODES.map(m =>
            `<button role="radio" aria-checked="${m.id === form.mode}" class="${m.id === form.mode ? 'active' : ''}" data-mode="${m.id}">${esc(m.label)}</button>`).join('')}</div>
          <p class="muted mpf-mode-hint">${esc(mode.hint)}</p>
          <div class="mpf-opts">
            <label ${form.mode === 'next' ? '' : 'hidden'}>Requests <input type="number" min="1" max="10000" data-id="count" value="${esc(form.count)}"></label>
            <label ${form.mode === 'next' || form.mode === 'path' ? '' : 'hidden'}>Path <input type="text" data-id="path" placeholder="${form.mode === 'path' ? '/api or /orders/*' : 'any (optional)'}" value="${esc(form.path)}" spellcheck="false"></label>
            <label class="mpf-check" title="Also record PHP's internal functions (strlen, PDO::execute…) — more detail, bigger reports"><input type="checkbox" data-id="builtins" ${form.builtins ? 'checked' : ''}> Include PHP built-ins</label>
          </div>
          <div class="mpf-actions">
            <button class="primary" data-id="apply">${info.enabled ? 'Apply' : (info.spxLoaded ? 'Turn on' : 'Build SPX & turn on')}</button>
            ${info.enabled ? '<button data-id="off">Turn off</button>' : ''}
            <span class="muted" data-id="busy">${esc(busyMsg)}</span>
          </div>
          ${links}
          ${info.spxLoaded ? '' : '<p class="muted">The first time, Mullion builds SPX for this PHP version from source (about a minute; needs the Xcode Command Line Tools). Disabled, it costs nothing.</p>'}
        </div>`;
      box.querySelectorAll('[data-mode]').forEach(b => {
        b.onclick = () => { form.mode = b.dataset.mode; keepForm(); renderHead(); };
      });
      const count = q('count'), path = q('path'), bi = q('builtins');
      if (count) count.oninput = () => { form.count = parseInt(count.value, 10) || 0; };
      if (path) path.oninput = () => { form.path = path.value; };
      if (bi) bi.onchange = () => { form.builtins = bi.checked; };
      q('apply').onclick = () => setProfiler(true);
      if (q('off')) q('off').onclick = () => setProfiler(false);
    }
    // Keep typed values when re-rendering for a mode switch.
    function keepForm() {
      if (!info || !info.settings) return;
      info = Object.assign({}, info, { settings: Object.assign({}, info.settings, { mode: form.mode, count: form.count, path: form.path, builtins: form.builtins }) });
    }

    async function setProfiler(on) {
      if (busyMsg) return;
      busyMsg = on ? 'Working…' : 'Turning off…';
      renderHead();
      try {
        const settings = { mode: form.mode, count: form.count, path: form.path.trim(), builtins: form.builtins };
        const r = await call('/api/project/profiler/set', { site, on, settings });
        if (r.build) { busyMsg = `Building SPX ${r.spxVersion}… (about a minute)`; renderHead(); }
        info = await waitJob(r.job, msg => { busyMsg = msg; const b = q('busy'); if (b) b.textContent = msg; });
        busyMsg = '';
        say(on ? 'Profiler on for ' + site : 'Profiler off for ' + site);
      } catch (e) {
        busyMsg = '';
        say('Profiler: ' + e.message);
        await loadInfo();
      }
      if (alive) { renderHead(); renderList(); }
    }

    /* profiles list */
    function renderList() {
      const card = q('list-card');
      if (!info || !info.supported) { card.hidden = true; return; }
      card.hidden = false;
      const lim = info.limits || {};
      q('list-hint').textContent = `${fmtInt(reports.length)} kept · ${fmtBytes(reports.reduce((t, r) => t + (r.bytes || 0), 0))} · newest ${fmtInt(lim.maxReports)} / ${fmtBytes(lim.maxBytes)} max`;
      q('clear').disabled = !reports.length;
      const list = q('list');
      if (!reports.length) {
        list.innerHTML = `<div class="empty">${info.enabled ? 'No profiles yet — make a request that gets profiled, it shows up here.' : 'No profiles. Turn the profiler on to record some.'}</div>`;
        return;
      }
      const maxWall = Math.max(...reports.map(r => r.wallUs || 0), 1);
      list.innerHTML = `<table class="mpf-table"><thead><tr>
          <th>When</th><th>Request</th><th class="num">Wall time</th><th class="num">Memory</th><th class="num">Calls</th><th></th></tr></thead>
        <tbody>${reports.map(r => {
          const req = r.cli ? `<span class="badge mono">CLI</span> ${esc(r.command)}` : `<span class="mpf-method">${esc(r.method)}</span> ${esc(r.uri)}`;
          return `<tr data-rid="${esc(r.id)}" class="${r.id === openId ? 'sel' : ''}">
            <td title="${esc(new Date(r.time * 1000).toLocaleString())}">${esc(fmtTime(r.time))}<div class="muted">${esc(fmtAgo(r.time))}</div></td>
            <td class="mpf-req">${req}</td>
            <td class="num"><div class="mpf-bar"><i style="width:${(r.wallUs / maxWall * 100).toFixed(1)}%"></i></div>${esc(fmtNs(r.wallUs * 1000))}</td>
            <td class="num">${esc(fmtBytes(r.peakMemory))}</td>
            <td class="num">${esc(fmtInt(r.calls))}</td>
            <td class="mpf-rowact"><button data-del="${esc(r.id)}" title="Delete this profile" aria-label="Delete">✕</button></td></tr>`;
        }).join('')}</tbody></table>`;
      list.querySelectorAll('tr[data-rid]').forEach(tr => {
        tr.onclick = e => { if (!e.target.closest('[data-del]')) openReport(tr.dataset.rid); };
      });
      list.querySelectorAll('[data-del]').forEach(b => {
        b.onclick = async () => {
          try {
            await call('/api/project/profiler/delete', { site, id: b.dataset.del });
            if (b.dataset.del === openId) closeReport();
            await loadList();
          } catch (e) { say('Error: ' + e.message); }
        };
      });
    }

    async function loadInfo() {
      try { info = await call('/api/project/profiler/info', { site }); } catch (e) {
        q('head').innerHTML = `<div class="card-body"><p class="mpf-warn">${esc(e.message)}</p></div>`;
        info = null;
      }
    }
    async function loadList() {
      if (!info || !info.supported) return;
      try { reports = await call('/api/project/profiler/list', { site }) || []; } catch { reports = []; }
      if (alive) renderList();
    }
    async function reload() {
      await loadInfo();
      if (!alive) return;
      renderHead();
      await loadList();
    }

    q('refresh').onclick = () => reload();
    q('clear').onclick = async () => {
      const ok = await confirmBox({ title: 'Clear all profiles?', message: `Deletes the ${reports.length} stored profile(s) of ${site}.`, confirm: 'Clear all', danger: true });
      if (!ok) return;
      try {
        await call('/api/project/profiler/delete', { site, id: '' });
        closeReport();
        await loadList();
      } catch (e) { say('Error: ' + e.message); }
    };

    // Look for new profiles while the panel is visible and profiling.
    function schedule() {
      clearTimeout(listTimer);
      listTimer = setTimeout(async () => {
        if (!alive) return;
        if (info && info.enabled && container.isConnected && !document.hidden && container.offsetParent !== null) {
          const before = reports.length ? reports[0].id : '';
          await loadList();
          if (info.settings && info.settings.mode === 'next' && reports.length && reports[0].id !== before) {
            await loadInfo(); renderHead();
          }
        }
        schedule();
      }, 4000);
    }

    /* one profile */
    function closeReport() {
      open = null; openId = '';
      if (flame) { flame.destroy(); flame = null; }
      q('view-card').hidden = true;
      q('view-card').innerHTML = '';
      renderList();
    }

    async function openReport(id) {
      openId = id;
      renderList();
      const card = q('view-card');
      card.hidden = false;
      card.innerHTML = '<div class="card-body"><div class="mpf-loading">Reading the profile…</div></div>';
      try {
        open = await call('/api/project/profiler/show', { site, id });
      } catch (e) {
        card.innerHTML = `<div class="card-body"><p class="mpf-warn">${esc(e.message)}</p></div>`;
        return;
      }
      if (!alive || openId !== id) return;
      renderReport();
      card.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }

    function renderReport() {
      const card = q('view-card');
      const m = open.meta, p = open.profile;
      const req = m.cli ? esc(m.command) : `${esc(m.method)} ${esc(m.uri)}`;
      const spxUrl = info && info.reportUrl ? info.reportUrl + encodeURIComponent(m.id) : '';
      card.innerHTML = `<div class="card-head"><h2 class="mpf-title" title="${esc(req)}">${req}</h2>
          <span class="hint">${esc(new Date(m.time * 1000).toLocaleString())}</span>
          <div class="mpf-head-actions">${spxUrl ? `<a class="button" href="${esc(spxUrl)}" target="_blank" rel="noopener" title="SPX's own viewer: timeline, flat profile, more metrics">Open in SPX UI</a>` : ''}<button class="sm" data-id="close">Close</button></div></div>
        <div class="card-body">
          <div class="mpf-stats">
            <div><b>${esc(fmtNs(m.wallUs * 1000))}</b><span>wall time</span></div>
            <div><b>${esc(fmtBytes(m.peakMemory))}</b><span>peak memory</span></div>
            <div><b>${esc(fmtInt(m.calls))}</b><span>function calls</span></div>
            <div><b>${esc(fmtInt(m.functions))}</b><span>functions</span></div>
          </div>
          <div class="mpf-tools">
            <input type="search" data-id="search" placeholder="Search functions, classes, files…" spellcheck="false">
            <span class="muted" data-id="match"></span>
            <span class="mpf-crumbs" data-id="crumbs"></span>
            <button class="sm" data-id="reset" hidden>Reset zoom</button>
          </div>
          <div class="mpf-flame" data-id="flame"><canvas></canvas><div class="mpf-tip" data-id="tip" hidden></div></div>
          <p class="muted mpf-legend">Each bar is a function, as wide as its share of the request; callers are above their callees. Click a bar to zoom in, click the top bar (or Reset) to zoom out.${p.pruned ? ` ${fmtInt(p.pruned)} frames under 0.02% of the time are left out.` : ''}</p>
          <h3 class="mpf-sub">Top functions</h3>
          <div class="mpf-top" data-id="top"></div>
        </div>`;
      q('close').onclick = closeReport;
      flame = makeFlame(q('flame'), p.root, {
        name: shortName,
        onZoom: (node, path) => {
          const crumbs = q('crumbs');
          crumbs.innerHTML = path.length > 1 ? path.map((n, i) => `<a href="#" data-crumb="${i}">${esc(i ? leaf(shortName(n.n)) : 'all')}</a>`).join(' › ') : '';
          crumbs.querySelectorAll('[data-crumb]').forEach(a => {
            a.onclick = e => { e.preventDefault(); flame.zoom(path[+a.dataset.crumb]); };
          });
          q('reset').hidden = path.length <= 1;
        },
      });
      q('reset').onclick = () => flame.zoom(null);
      const search = q('search');
      let topSort = 'self';
      const renderTop = () => {
        const term = search.value.trim().toLowerCase();
        const rows = (p.funcs || []).filter(f => !term || shortName(f.name).toLowerCase().includes(term))
          .sort((a, b) => topSort === 'calls' ? b.calls - a.calls : topSort === 'inclusive' ? b.inclusive - a.inclusive : b.self - a.self)
          .slice(0, 60);
        const total = p.root.v || 1;
        const th = (k, label) => `<th class="num sortable ${topSort === k ? 'sorted' : ''}" data-sort="${k}">${label}</th>`;
        q('top').innerHTML = rows.length ? `<table class="mpf-table"><thead><tr><th>Function</th>${th('self', 'Self')}${th('inclusive', 'Inclusive')}${th('calls', 'Calls')}<th class="num">Memory</th></tr></thead>
          <tbody>${rows.map(f => `<tr data-fn="${esc(f.name)}"><td class="mpf-fn" title="${esc(f.name)}">${esc(shortName(f.name))}</td>
            <td class="num"><div class="mpf-bar"><i style="width:${(f.self / total * 100).toFixed(1)}%"></i></div>${esc(fmtNs(f.self))} <span class="muted">${pct(f.self, total)}</span></td>
            <td class="num">${esc(fmtNs(f.inclusive))} <span class="muted">${pct(f.inclusive, total)}</span></td>
            <td class="num">${esc(fmtInt(f.calls))}</td>
            <td class="num">${esc(fmtBytes(f.memory))}</td></tr>`).join('')}</tbody></table>` : '<div class="empty">No function matches.</div>';
        q('top').querySelectorAll('[data-sort]').forEach(h => { h.onclick = () => { topSort = h.dataset.sort; renderTop(); }; });
        q('top').querySelectorAll('tr[data-fn]').forEach(tr => {
          tr.onclick = () => { search.value = shortName(tr.dataset.fn); onSearch(); };
        });
      };
      const onSearch = () => {
        const term = search.value.trim();
        const r = flame.search(term);
        q('match').textContent = term ? (r.frames ? `${fmtInt(r.frames)} frames · ${pct(r.total, p.root.v)} of the time` : 'no match') : '';
        renderTop();
      };
      search.oninput = onSearch;
      renderTop();
    }

    const leaf = name => {
      const s = name.split('/').pop();
      return s.length > 40 ? s.slice(0, 38) + '…' : s;
    };

    reload().then(schedule);

    const inst = {
      site,
      reload,
      unmount() {
        alive = false;
        clearTimeout(listTimer);
        if (flame) flame.destroy();
        container.innerHTML = '';
        container.classList.remove('mpf-root');
        instances.delete(container);
      },
    };
    instances.set(container, inst);
    return inst;
  }

  /* ── flame graph (canvas, icicle layout) ────────────────────────── */
  function makeFlame(box, root, opts) {
    const canvas = box.querySelector('canvas');
    const tip = box.querySelector('[data-id="tip"]');
    const ctx = canvas.getContext('2d');
    const ROW = 20;
    let zoomRoot = root;
    let zoomPath = [root];
    let rects = [];
    let hover = null;
    let term = '';
    let width = 0;

    // Parent links, once.
    (function link(n, parent) {
      n._p = parent;
      if (n.ch) for (const c of n.ch) link(c, n);
    })(root, null);

    function maxDepth(n) {
      let d = 0;
      const walk = (x, depth) => {
        if (depth > d) d = depth;
        if (x.ch) for (const c of x.ch) walk(c, depth + 1);
      };
      walk(n, 0);
      return d;
    }

    function css(name, def) {
      const v = getComputedStyle(box).getPropertyValue(name).trim();
      return v || def;
    }
    // Dark when the box's own background is (the panel's theme, whatever
    // set it).
    function isDark() {
      const m = getComputedStyle(box).backgroundColor.match(/\d+(\.\d+)?/g);
      if (!m || m.length < 3) return document.documentElement.getAttribute('data-theme') === 'dark';
      const [r, g, b] = m.map(Number);
      return (0.2126 * r + 0.7152 * g + 0.0722 * b) < 128;
    }
    function hash(s) {
      let h = 2166136261;
      for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
      return h >>> 0;
    }
    // Warm palette by name (stable across zooms), vendor code cooler.
    function color(n, dark) {
      const name = n.n || '';
      const h = hash(name);
      const vendor = name.includes('/vendor/') || /^(Illuminate|Symfony|Composer|Laravel|Psr|GuzzleHttp|Monolog|Carbon|Doctrine)\\/.test(name);
      const hue = vendor ? 200 + (h % 40) : 8 + (h % 42);
      const sat = vendor ? 32 + (h % 18) : 70 + (h % 20);
      const light = dark ? (vendor ? 54 : 56) + ((h >> 8) % 8) : (vendor ? 72 : 62) + ((h >> 8) % 12);
      return `hsl(${hue} ${sat}% ${light}%)`;
    }

    function layout() {
      rects = [];
      const total = zoomRoot.v || 1;
      const walk = (n, x, depth) => {
        const w = n.v / total * width;
        if (w < 0.6) return;
        rects.push({ n, x, y: depth * ROW, w });
        let cx = x;
        if (n.ch) for (const c of n.ch) { walk(c, cx, depth + 1); cx += c.v / total * width; }
      };
      // Ancestors of the zoomed frame stay visible as full-width bars.
      const anc = [];
      for (let a = zoomRoot._p; a; a = a._p) anc.unshift(a);
      anc.forEach((a, i) => rects.push({ n: a, x: 0, y: i * ROW, w: width, anc: true }));
      walk(zoomRoot, 0, anc.length);
    }

    function draw() {
      width = box.clientWidth;
      if (!width) return;
      const depth = (zoomPath.length - 1) + maxDepth(zoomRoot) + 1;
      const height = Math.max(ROW * 3, Math.min(depth * ROW, 4000));
      const dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      canvas.style.width = width + 'px';
      canvas.style.height = height + 'px';
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, width, height);
      layout();
      const dark = isDark();
      const text = dark ? '#0e121a' : '#1a1f2b';
      const hitFill = css('--accent', '#ff6b4a');
      const dim = dark ? 'rgba(255,255,255,.06)' : 'rgba(22,28,45,.07)';
      const low = term.toLowerCase();
      ctx.font = '11px ' + (css('--mono', 'ui-monospace, monospace'));
      ctx.textBaseline = 'middle';
      for (const r of rects) {
        const name = r.n.n || '';
        const match = low && opts.name(name).toLowerCase().includes(low);
        ctx.fillStyle = r.anc ? dim : (low && !match ? dim : (match ? hitFill : color(r.n, dark)));
        ctx.fillRect(r.x + 0.5, r.y + 0.5, Math.max(r.w - 1, 0.5), ROW - 1);
        if (r === hover) {
          ctx.strokeStyle = dark ? '#fff' : '#1a1f2b';
          ctx.lineWidth = 1;
          ctx.strokeRect(r.x + 1, r.y + 1, Math.max(r.w - 2, 0.5), ROW - 2);
        }
        if (r.w > 34) {
          const label = (r.n === root ? 'all' : opts.name(name)) + '  ' + fmtNs(r.n.v);
          ctx.fillStyle = r.anc || (low && !match) ? css('--muted', '#6b7385') : text;
          ctx.save();
          ctx.beginPath();
          ctx.rect(r.x + 4, r.y, r.w - 8, ROW);
          ctx.clip();
          ctx.fillText(label, r.x + 5, r.y + ROW / 2 + 0.5);
          ctx.restore();
        }
      }
    }

    function hit(ev) {
      const b = canvas.getBoundingClientRect();
      const x = ev.clientX - b.left, y = ev.clientY - b.top;
      for (let i = rects.length - 1; i >= 0; i--) {
        const r = rects[i];
        if (x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + ROW) return r;
      }
      return null;
    }

    function showTip(r, ev) {
      if (!r) { tip.hidden = true; return; }
      const n = r.n, total = root.v || 1;
      tip.innerHTML = `<b>${esc(n === root ? 'all' : opts.name(n.n))}</b>
        <div>${esc(fmtNs(n.v))} · ${pct(n.v, total)} of the request</div>
        <div>self ${esc(fmtNs(n.s))} · ${esc(fmtInt(n.c))} call${n.c === 1 ? '' : 's'}</div>`;
      tip.hidden = false;
      const b = box.getBoundingClientRect();
      const top = box.scrollTop;
      let x = ev.clientX - b.left + 14, y = ev.clientY - b.top + 14;
      const tw = tip.offsetWidth, th = tip.offsetHeight;
      if (x + tw > b.width - 4) x = Math.max(4, ev.clientX - b.left - tw - 10);
      if (y + th > box.clientHeight - 4) y = Math.max(4, ev.clientY - b.top - th - 10);
      y += top;
      tip.style.left = x + 'px';
      tip.style.top = y + 'px';
    }

    function zoom(n) {
      zoomRoot = n || root;
      zoomPath = [];
      for (let a = zoomRoot; a; a = a._p) zoomPath.unshift(a);
      hover = null;
      tip.hidden = true;
      draw();
      if (opts.onZoom) opts.onZoom(zoomRoot, zoomPath);
    }

    canvas.onmousemove = ev => {
      const r = hit(ev);
      if (r !== hover) { hover = r; draw(); }
      canvas.style.cursor = r ? 'pointer' : 'default';
      showTip(r, ev);
    };
    canvas.onmouseleave = () => { hover = null; tip.hidden = true; draw(); };
    canvas.onclick = ev => {
      const r = hit(ev);
      if (!r) return;
      zoom(r.n === zoomRoot ? zoomRoot._p : r.n);
    };

    let raf = 0;
    const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(() => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(draw);
    }) : null;
    if (ro) ro.observe(box);
    const themeObs = new MutationObserver(() => draw());
    themeObs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

    zoom(null);
    return {
      zoom,
      search(t) {
        term = t || '';
        draw();
        if (!term) return { frames: 0, total: 0 };
        // Time covered by matching frames, without counting a match
        // nested inside another match twice.
        const low = term.toLowerCase();
        let frames = 0, total = 0;
        const walk = (n, inMatch) => {
          const m = n !== root && opts.name(n.n || '').toLowerCase().includes(low);
          if (m) { frames++; if (!inMatch) total += n.v; }
          if (n.ch) for (const c of n.ch) walk(c, inMatch || m);
        };
        walk(root, false);
        return { frames, total };
      },
      destroy() {
        if (ro) ro.disconnect();
        themeObs.disconnect();
        cancelAnimationFrame(raf);
      },
    };
  }

  window.MullionProfilerPanel = MullionProfilerPanel;
})();
