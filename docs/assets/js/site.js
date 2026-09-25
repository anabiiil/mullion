// Mullion website: theme toggle, copy buttons, tabs, heading anchors,
// "on this page" TOC, mobile sidebar, CLI filter. No dependencies.
(function () {
  'use strict';
  var root = document.documentElement;
  function store(k, v) { try { if (v === undefined) return localStorage.getItem(k); localStorage.setItem(k, v); } catch (e) { return null; } }

  // Theme: explicit choice wins, else the OS preference (set early in <head>).
  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  function current() { return root.getAttribute('data-theme') || (mq && mq.matches ? 'dark' : 'light'); }
  function label(btn) { btn.setAttribute('aria-label', current() === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'); }
  document.querySelectorAll('.theme-btn').forEach(function (btn) {
    label(btn);
    btn.addEventListener('click', function () {
      var next = current() === 'dark' ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      store('mullion-theme', next);
      document.querySelectorAll('.theme-btn').forEach(label);
    });
  });
  if (mq && mq.addEventListener) mq.addEventListener('change', function () {
    if (!store('mullion-theme')) { root.removeAttribute('data-theme'); root.setAttribute('data-theme', mq.matches ? 'dark' : 'light'); }
  });

  // Copy buttons on code blocks (prompt markers and comments are skipped).
  function copyText(text, btn) {
    function done() { btn.textContent = 'Copied'; btn.classList.add('done'); setTimeout(function () { btn.textContent = 'Copy'; btn.classList.remove('done'); }, 1400); }
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, function () {});
    else {
      var ta = document.createElement('textarea'); ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); done(); } catch (e) {} document.body.removeChild(ta);
    }
  }
  document.querySelectorAll('pre').forEach(function (pre) {
    if (pre.hasAttribute('data-nocopy')) return;
    var btn = document.createElement('button');
    btn.type = 'button'; btn.className = 'copy'; btn.textContent = 'Copy'; btn.setAttribute('aria-label', 'Copy code to clipboard');
    btn.addEventListener('click', function () {
      var clone = pre.cloneNode(true);
      clone.querySelectorAll('.copy, .p, .c').forEach(function (n) { n.remove(); });
      var text = clone.textContent.split('\n').map(function (l) { return l.replace(/\s+$/, ''); }).join('\n').trim();
      copyText(text, btn);
    });
    pre.appendChild(btn);
  });

  // Tabs (install instructions). Remembers the last OS picked.
  document.querySelectorAll('[data-tabs]').forEach(function (group) {
    var tabs = group.querySelectorAll('[role="tab"]');
    function select(tab, save) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
        document.getElementById(t.getAttribute('aria-controls')).hidden = !on;
      });
      if (save) store('mullion-os', tab.getAttribute('data-os'));
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(t, true); });
      t.addEventListener('keydown', function (e) {
        var d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
        if (!d) return;
        var n = tabs[(i + d + tabs.length) % tabs.length]; n.focus(); select(n, true);
      });
    });
    var saved = store('mullion-os');
    var guess = saved || (/Win/.test(navigator.platform || navigator.userAgent) ? 'windows' : 'macos');
    var pick = group.querySelector('[data-os="' + guess + '"]');
    if (pick) select(pick, false);
  });

  // Heading anchors + "On this page".
  var content = document.querySelector('.content');
  var toc = document.querySelector('.toc ul');
  if (content) {
    var heads = content.querySelectorAll('h2[id], h3[id]');
    heads.forEach(function (h) {
      var a = document.createElement('a');
      a.className = 'anchor'; a.href = '#' + h.id; a.textContent = '#';
      a.setAttribute('aria-label', 'Link to this section');
      h.appendChild(a);
      if (toc && !(h.tagName === 'H3' && h.closest('.cmd-block'))) {
        var li = document.createElement('li'); li.className = h.tagName.toLowerCase();
        var l = document.createElement('a'); l.href = '#' + h.id; l.textContent = h.firstChild.textContent.replace(/^mullion /, '');
        li.appendChild(l); toc.appendChild(li);
      }
    });
    if (toc && !toc.children.length) toc.closest('.toc').style.visibility = 'hidden';
    if (toc && 'IntersectionObserver' in window) {
      var links = toc.querySelectorAll('a');
      var io = new IntersectionObserver(function (entries) {
        entries.forEach(function (en) {
          if (!en.isIntersecting) return;
          links.forEach(function (l) { l.classList.toggle('active', l.getAttribute('href') === '#' + en.target.id); });
        });
      }, { rootMargin: '-80px 0px -70% 0px' });
      heads.forEach(function (h) { if (!(h.tagName === 'H3' && h.closest('.cmd-block'))) io.observe(h); });
    }
  }

  // Mobile sidebar.
  var menu = document.querySelector('.menu-btn');
  if (menu) {
    menu.addEventListener('click', function () {
      var open = document.body.classList.toggle('nav-open');
      menu.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && document.body.classList.contains('nav-open')) menu.click(); });
    document.addEventListener('click', function (e) {
      if (document.body.classList.contains('nav-open') && !e.target.closest('.sidebar') && !e.target.closest('.menu-btn')) menu.click();
    });
  }

  // CLI reference filter.
  var filter = document.getElementById('cli-filter');
  if (filter) {
    filter.addEventListener('input', function () {
      var q = filter.value.trim().toLowerCase();
      document.querySelectorAll('.cmd-block').forEach(function (b) {
        b.hidden = q && b.textContent.toLowerCase().indexOf(q) < 0;
      });
      document.querySelectorAll('.cmd-group').forEach(function (g) {
        g.hidden = q && !g.querySelector('.cmd-block:not([hidden])');
      });
    });
  }
})();
