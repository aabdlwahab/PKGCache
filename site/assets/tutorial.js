/* pkgcache setup tutorial — progressive enhancement only. The page is complete
   without JavaScript; this file adds the address field, copy buttons, theme switching,
   and restrained scroll effects. The download matrix lives in downloads.js.

   Forked from go/internal/web/dist/tutorial.js with everything that talked to a running
   instance taken out: that page is served by a cache and could ask it for its own
   address, its CA fingerprint and its published client files. This one is a static site
   on GitHub Pages, so the address is asked of the reader, the fingerprint belongs to
   whoever runs the cache, and the files come from the GitHub Releases API — see
   downloads.js. What is left below — copy buttons, the rails, the parallax — never
   needed a server and is unchanged. */
(function () {
  "use strict";

  var root = document.documentElement;

  /* ---- "your cache address" --------------------------------------------- */
  /* The server's copy of this page had a script beside it that asked the instance
     serving the page for its own coordinates, and rewrote every example address in the
     document to the reader's real cache before they copied anything. That is a good
     trick and it is unavailable here, because nothing is serving this page but a CDN.
     So the reader supplies the address and the same substitution happens.

     Original text is kept per node rather than substituted in place, so typing into the
     field re-renders from the original every time. Rewriting the live value instead
     would compound: "cache.internal:8443" typed over an already-substituted document
     has nothing left to match, and the field would appear to stop working after the
     first keystroke. */
  var PLACEHOLDER = "cache.example.com:8443";
  var STORAGE_KEY = "pkgcache_site_address";

  var field = document.querySelector('[data-role="cache-address"]');
  var fieldCount = document.querySelector('[data-role="cache-address-count"]');
  var sites = [];

  if (field && document.createTreeWalker) {
    var walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT, null);
    for (var node = walker.nextNode(); node; node = walker.nextNode()) {
      if (node.nodeValue.indexOf(PLACEHOLDER) !== -1) {
        sites.push({ node: node, text: node.nodeValue });
      }
    }
  }

  /* A reader will paste whatever they were given: an origin with a scheme, a bare
     host:port, something with a trailing slash. All three name the same cache, and the
     examples need the bare authority because half of them are image references, where a
     scheme is not merely redundant but wrong. */
  function authority(value) {
    return String(value || "")
      .trim()
      .replace(/^[a-z][a-z0-9+.-]*:\/\//i, "")
      .replace(/\/+$/, "")
      .split("/")[0];
  }

  function applyAddress(value) {
    var host = authority(value);
    for (var i = 0; i < sites.length; i++) {
      sites[i].node.nodeValue = host
        ? sites[i].text.split(PLACEHOLDER).join(host)
        : sites[i].text;
    }
    if (fieldCount) {
      if (!host) {
        fieldCount.textContent = sites.length + " examples use the placeholder";
        fieldCount.classList.remove("is-live");
      } else {
        fieldCount.textContent = sites.length + " examples now say " + host;
        fieldCount.classList.add("is-live");
      }
    }
  }

  if (field) {
    try {
      var stored = localStorage.getItem(STORAGE_KEY);
      if (stored) field.value = stored;
    } catch (error) {
      /* Private mode or blocked storage: the placeholder stands on its own. */
    }
    applyAddress(field.value);
    field.addEventListener("input", function () {
      applyAddress(field.value);
      try {
        if (authority(field.value)) localStorage.setItem(STORAGE_KEY, field.value.trim());
        else localStorage.removeItem(STORAGE_KEY);
      } catch (error) {
        /* Nothing to persist to; the substitution still holds for this visit. */
      }
    });
  }

  /* ---- theme ----------------------------------------------------------- */
  var theme = document.getElementById("theme");
  if (theme) {
    theme.addEventListener("click", function () {
      var next = root.dataset.theme === "light" ? "dark" : "light";
      root.dataset.theme = next;
      try { localStorage.setItem("pcc_theme", next); } catch (error) { /* no storage */ }
    });
  }

  /* ---- copy buttons ---------------------------------------------------- */
  function selectionCopy(text) {
    var copyField = document.createElement("textarea");
    copyField.value = text;
    copyField.setAttribute("readonly", "");
    copyField.style.position = "fixed";
    copyField.style.left = "-9999px";
    copyField.style.top = "0";
    document.body.appendChild(copyField);
    var ok = false;
    try {
      copyField.select();
      copyField.setSelectionRange(0, copyField.value.length);
      ok = document.execCommand("copy");
    } catch (error) {
      ok = false;
    }
    document.body.removeChild(copyField);
    return ok;
  }

  [].forEach.call(document.querySelectorAll(".cp"), function (button) {
    var command = button.parentNode.querySelector(".term-cmd");
    if (!command) return;
    button.addEventListener("click", function () {
      var text = command.textContent;
      var reset = function (label, className, delay) {
        button.textContent = label;
        button.classList.toggle("ok", className === "ok");
        setTimeout(function () {
          button.classList.remove("ok");
          button.textContent = "copy";
        }, delay);
      };
      var fallback = function () {
        if (selectionCopy(text)) reset("copied", "ok", 1400);
        else reset("select + copy", "", 2200);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text)
          .then(function () { reset("copied", "ok", 1400); })
          .catch(fallback);
      } else {
        fallback();
      }
    });
  });

  /* ---- scroll progress, fast path and parallax ------------------------- */
  /* One rAF-throttled scroll handler drives the progress beam, the TL;DR rail and
     the parallax drift. Parallax elements carry data-px (a speed); we translate them
     by their distance from the viewport centre. Only element.style.transform /
     .width are written — both CSSOM mutations. */
  var progress = document.querySelector(".progress");
  var pxLayers = [].slice.call(document.querySelectorAll("[data-px]"));
  var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var queued = false;

  /* The TL;DR rails, one per platform column.
     Each beam fills to wherever the reader has got to and each step marker lights as
     it crosses the same line, so a column reports progress through its own setup
     rather than decorating it. Written for many tracks rather than one because the
     section became three columns; a single-track version would have lit the first
     column and left the other two dead. Scroll-linked, so with reduced motion they are
     set finished once below and never touched again. */
  var READING_LINE = 0.62; // fraction of the viewport height a step counts as reached
  var fpTracks = [].slice.call(document.querySelectorAll(".fp-track")).map(function (track) {
    var steps = [].slice.call(track.querySelectorAll(".fp-step"));
    return {
      track: track,
      beam: track.querySelector(".fp-beam"),
      fill: track.querySelector(".fp-beam i"),
      steps: steps,
      last: steps.length ? steps[steps.length - 1].querySelector(".fp-mark") : null,
      card: track.closest ? track.closest(".fp-card") : null,
    };
  });

  /* The beam stops at the middle of the last marker rather than at the foot of the
     track, so "full" and "every step lit" are the same moment. The track keeps growing
     past that point — the last step still has a command and a note under it — and a
     beam that ran to the bottom would trail off into empty space. */
  function fastPathExtent(rail) {
    if (!rail.beam) return null;
    var box = rail.track.getBoundingClientRect();
    var top = box.top + 6;
    var end = rail.last ? rail.last.getBoundingClientRect() : null;
    var bottom = end ? end.top + end.height / 2 : box.bottom - 6;
    var span = Math.max(0, bottom - top);
    rail.beam.style.height = span.toFixed(1) + "px";
    return { top: top, span: span };
  }

  function fastPathFrame(vh) {
    var line = vh * READING_LINE;
    for (var t = 0; t < fpTracks.length; t++) {
      var rail = fpTracks[t];
      for (var s = 0; s < rail.steps.length; s++) {
        rail.steps[s].classList.toggle("is-lit",
          rail.steps[s].getBoundingClientRect().top <= line);
      }
      var extent = fastPathExtent(rail);
      if (!extent || !rail.fill) continue;
      var filled = extent.span > 0 ? (line - extent.top) / extent.span : 0;
      rail.fill.style.transform =
        "scaleY(" + Math.max(0, Math.min(1, filled)).toFixed(3) + ")";
      /* A column is "being read" while its rail is part-filled. Once it is full the
         highlight lets go, so the warmth follows the reader down the page rather than
         accumulating behind them. */
      if (rail.card) {
        rail.card.classList.toggle("is-active", filled > 0 && filled < 1);
      }
    }
  }

  /* The platform columns arrive with a blur that resolves as they rise. Deliberately
     the only entrance animation on the page: everything below is instructions someone
     is scanning for a command, and content that starts invisible costs them more than
     it gives. These three blocks are the first thing seen and settle long before
     anyone could be reading them.

     One-shot, via IntersectionObserver, and unobserved once played — a block that
     re-animates every time it scrolls back into view is a block nobody can read. */
  var arriving = [].slice.call(document.querySelectorAll(".fp-card, .fp-download"));
  if (!reduce && window.IntersectionObserver && arriving.length) {
    var watcher = new IntersectionObserver(function (entries) {
      entries.forEach(function (entry) {
        if (!entry.isIntersecting) return;
        entry.target.classList.add("is-in");
        watcher.unobserve(entry.target);
      });
    }, { rootMargin: "0px 0px -12% 0px", threshold: 0.15 });
    arriving.forEach(function (block) { watcher.observe(block); });
  } else {
    // No observer, or the reader asked for less motion: show them finished.
    arriving.forEach(function (block) { block.classList.add("is-in"); });
  }

  /* Reduced motion: the rails are scroll-linked by nature, so they are stood finished
     once and never follow the reader. */
  if (reduce) {
    fpTracks.forEach(function (rail) {
      fastPathExtent(rail);
      if (rail.fill) rail.fill.style.transform = "none";
      rail.steps.forEach(function (step) { step.classList.add("is-lit"); });
    });
  }

  function onScrollFrame() {
    queued = false;
    var vh = window.innerHeight;
    if (progress) {
      var height = root.scrollHeight - vh;
      var value = height > 0 ? window.scrollY / height : 0;
      progress.style.width = (value * 100).toFixed(2) + "%";
    }
    if (reduce) return;
    fastPathFrame(vh);
    for (var i = 0; i < pxLayers.length; i++) {
      var layer = pxLayers[i];
      var rect = layer.getBoundingClientRect();
      if (rect.bottom < -300 || rect.top > vh + 300) continue; // off-screen: skip
      var center = (rect.top + rect.height / 2 - vh / 2) / vh; // ~ -1 (below) .. 1 (above)
      var speed = parseFloat(layer.getAttribute("data-px")) || 0;
      layer.style.transform = "translate3d(0," + (center * speed * -60).toFixed(1) + "px,0)";
    }
  }

  function requestScrollFrame() {
    if (queued) return;
    queued = true;
    requestAnimationFrame(onScrollFrame);
  }
  window.addEventListener("scroll", requestScrollFrame, { passive: true });
  window.addEventListener("resize", requestScrollFrame, { passive: true });
  onScrollFrame();
})();
