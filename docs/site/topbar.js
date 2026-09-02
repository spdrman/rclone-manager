/* rclone-manager docs shared header, used on every page.
 *
 * Three responsibilities:
 *   1. Scroll-collapse animation on the home page (.topbar.is-hero).
 *      Drives --p1 / --p2 / --p3 and toggles .collapsed when fully shrunk.
 *      Inner pages don't ship .is-hero, so this becomes a no-op for them.
 *   2. Burger drawer toggle (.menu-open on the header).
 *   3. Light/dark theme toggle, which lives inside the drawer.
 *
 * The theme choice is remembered in localStorage under "theme". With
 * nothing stored, the OS preference decides, and a later OS change is
 * followed until the reader picks a side explicitly.
 */
(function () {
  'use strict';

  // ---------------------------------------------------------------------------
  // Theme toggle
  // ---------------------------------------------------------------------------

  function initTheme() {
    var btn = document.getElementById('themeToggle');
    var glyph = btn && btn.querySelector('.theme-glyph');
    var label = btn && btn.querySelector('.theme-label');

    function applyTheme(theme) {
      document.documentElement.setAttribute('data-theme', theme);
      if (glyph) glyph.textContent = theme === 'dark' ? '☀' : '☾';
      if (label) label.textContent = theme === 'dark' ? 'Light mode' : 'Dark mode';
      if (btn) btn.setAttribute('aria-checked', theme === 'dark' ? 'true' : 'false');
      try { localStorage.setItem('theme', theme); } catch (_) { /* private mode */ }
    }

    var saved = null;
    try { saved = localStorage.getItem('theme'); } catch (_) { /* ignore */ }

    if (saved === 'light' || saved === 'dark') {
      applyTheme(saved);
    } else if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) {
      applyTheme('dark');
    } else {
      applyTheme('light');
    }

    if (btn) {
      btn.addEventListener('click', function () {
        var current = document.documentElement.getAttribute('data-theme');
        applyTheme(current === 'dark' ? 'light' : 'dark');
      });
    }

    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: dark)');
      var listener = function (e) {
        var pinned = null;
        try { pinned = localStorage.getItem('theme'); } catch (_) { /* ignore */ }
        if (!pinned) applyTheme(e.matches ? 'dark' : 'light');
      };
      if (mq.addEventListener) mq.addEventListener('change', listener);
      else if (mq.addListener) mq.addListener(listener);  // Safari < 14
    }
  }

  // ---------------------------------------------------------------------------
  // Burger drawer
  // ---------------------------------------------------------------------------

  function initDrawer() {
    var bar = document.getElementById('topbar');
    var burger = document.getElementById('topbarBurger');
    var menu = document.getElementById('topbar-menu');
    if (!bar || !burger || !menu) return;

    function setOpen(open) {
      bar.classList.toggle('menu-open', !!open);
      burger.setAttribute('aria-expanded', open ? 'true' : 'false');
      menu.setAttribute('aria-hidden', open ? 'false' : 'true');
    }

    burger.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      setOpen(!bar.classList.contains('menu-open'));
    });

    // Tap a link → close so the navigation feels snappy.
    Array.prototype.forEach.call(menu.querySelectorAll('a'), function (a) {
      a.addEventListener('click', function () { setOpen(false); });
    });

    // Click anywhere outside the drawer or burger closes it.
    document.addEventListener('click', function (e) {
      if (!bar.classList.contains('menu-open')) return;
      if (bar.contains(e.target)) return;
      setOpen(false);
    });

    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && bar.classList.contains('menu-open')) {
        setOpen(false);
        burger.focus();
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Scroll-collapse animation (home page only)
  // ---------------------------------------------------------------------------

  function initHeroCollapse() {
    var bar = document.getElementById('topbar');
    if (!bar || !bar.classList.contains('is-hero')) return;

    function clamp01(x) { return Math.max(0, Math.min(1, x)); }

    function update() {
      var vh = window.innerHeight || document.documentElement.clientHeight;
      var y = window.scrollY || document.documentElement.scrollTop;
      var stage = function (start, end) {
        return clamp01((y - vh * start) / (vh * (end - start)));
      };
      bar.style.setProperty('--p1', stage(0, 0.02));
      bar.style.setProperty('--p2', stage(0.02, 0.04));
      var p3 = stage(0.04, 0.06);
      bar.style.setProperty('--p3', p3);
      bar.classList.toggle('collapsed', p3 >= 1);
    }

    window.addEventListener('scroll', update, { passive: true });
    window.addEventListener('resize', update);
    update();
  }

  // ---------------------------------------------------------------------------
  // Init — run *now* if DOM is already parsed; otherwise wait for it. Don't
  // gate on DOMContentLoaded alone, because <script defer> runs after parse
  // but before DCL fires, and we want the burger to work the moment the
  // user can see it.
  // ---------------------------------------------------------------------------

  function start() {
    initTheme();
    initDrawer();
    initHeroCollapse();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
})();
