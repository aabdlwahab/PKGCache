/* pkgcache landing page — progressive enhancement only. With JS off the page is
   fully readable; nothing here gates content. No inline handlers.

   Forked verbatim from go/internal/web/dist/landing.js. It is driven entirely by class
   names and data attributes — #story, .act, data-act, .rv, .progress — so the client
   page keeps the server page's stage by keeping that skeleton, and this file needed no
   edit at all. */
(function () {
  "use strict";
  var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  /* ---- theme toggle (shares the console's pcc_theme key) ---------------- */
  var root = document.documentElement;
  var toggle = document.getElementById("theme");
  if (toggle) {
    toggle.addEventListener("click", function () {
      var next = root.dataset.theme === "light" ? "dark" : "light";
      root.dataset.theme = next;
      try { localStorage.setItem("pcc_theme", next); } catch (e) { /* no storage */ }
    });
  }

  /* ---- scroll progress beam -------------------------------------------- */
  var bar = document.querySelector(".progress");
  if (bar) {
    var onScroll = function () {
      var h = document.documentElement.scrollHeight - window.innerHeight;
      var p = h > 0 ? window.scrollY / h : 0;
      bar.style.width = (p * 100).toFixed(2) + "%";
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
  }

  /* ---- reveal on scroll ------------------------------------------------- */
  var revealed = document.querySelectorAll(".rv");
  if (reduce || !("IntersectionObserver" in window)) {
    revealed.forEach(function (el) { el.classList.add("in"); });
  } else {
    var io = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (en) {
          if (en.isIntersecting) { en.target.classList.add("in"); io.unobserve(en.target); }
        });
      },
      { rootMargin: "0px 0px -12% 0px", threshold: 0.12 }
    );
    revealed.forEach(function (el) { io.observe(el); });
  }

})();
/* =====================================================================
   The stage: one pinned frame, four scroll-scrubbed beats.

   Everything the frame shows is a pure function of (beat, t) — which beat and
   how far the reader is through it — so scrolling back up plays the story
   backwards exactly. Ambient traffic (the moving dots) is the one
   non-deterministic part, and it is gated on the frame being on screen.

   CSP-safe: writes `data-act` (the beat index) + the numeric custom properties
   --peel / --peel-inv / --p, and `transform` on moving parts. Nothing else.
   ===================================================================== */
(function () {
  "use strict";

  var stage = document.getElementById("story");
  if (!stage) return;
  var deck = stage.querySelector(".deck");
  var sc = stage.querySelector(".stage-scene");
  if (!deck || !sc) return;

  var flow    = sc.querySelector(".flow");
  var wall    = sc.querySelector(".wall");
  var casEl   = sc.querySelector(".wall .cas");
  var casOff  = sc.querySelector(".cas-off");
  var ckptEl  = sc.querySelector(".wall-ckpt");
  var upsEl   = sc.querySelector(".ups");
  var hostOff = sc.querySelector(".host-off");
  var shuttle = sc.querySelector(".shuttle");
  var statusT = sc.querySelector(".scene-status .label");
  var offChips = [].slice.call(sc.querySelectorAll(".off-clients .fnode"));
  var ggNodes  = [].slice.call(stage.querySelectorAll(".gg-node"));
  var ticks    = [].slice.call(stage.querySelectorAll(".deck-rail .tick"));
  var actEls   = [].slice.call(stage.querySelectorAll(".act"));
  var deckTitle = stage.querySelector(".deck-title");
  var hudV = stage.querySelector(".hud-v");
  var hudK = stage.querySelector(".hud-k");

  var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var waapi = flow && typeof flow.animate === "function";

  var CFG = {
    tiles: 24, offTiles: 12,
    committedAt3: 18,      // tiles a full beat-3 scrub commits
    startVersion: 12,
    legIn: 620, legHit: 230, legThrough: 190, legOut: 560, legStream: 820, legServe: 280,
    legDirect: 1500, legBack: 1300,
    spawn1: 1150, spawn2: 1000, spawn3: 1450, spawn4: 1100,
    maxLive: 3
  };

  /* the four beats, in order: the label shown on the deck + its readout */
  var BEAT = [
    { title: "the same download, again",                 k: "already on this disk" },
    { title: "one cache, on this machine",               k: "served from disk" },
    { title: "name exactly what this project holds",     k: "checkpointed" },
    { title: "cached packages, ready with no network",   k: "offline cache" }
  ];

  var ECOS = [].map.call(sc.querySelectorAll(".clients [data-node]"), function (n) {
    return n.getAttribute("data-node").slice(2);
  });

  /* ---------------- tiles ---------------- */
  function buildTiles(host, n) {
    var out = [];
    if (!host) return out;
    for (var i = 0; i < n; i++) {
      var t = document.createElement("div");
      t.className = "cas-tile";
      host.appendChild(t);
      out.push(t);
    }
    return out;
  }
  var tiles = buildTiles(casEl, CFG.tiles);
  var offTiles = buildTiles(casOff, CFG.offTiles);

  /* ---------------- geometry (zoom / transform safe) ------------------- */
  var pos = {}, upX = {}, wallL = 0, wallR = 0, gapX = 0, hostL = 0, sceneW = 0, sceneH = 0;
  var upsHidden = false;

  function measure() {
    var s = sc.getBoundingClientRect();
    var k = sc.offsetWidth ? s.width / sc.offsetWidth : 1;
    if (!k) k = 1;
    var un = function (v) { return v / k; };
    sceneW = sc.offsetWidth || s.width;
    sceneH = sc.offsetHeight || s.height;
    upsHidden = !upsEl || window.getComputedStyle(upsEl).display === "none";

    [].forEach.call(sc.querySelectorAll("[data-node]"), function (n) {
      var r = n.getBoundingClientRect();
      var id = n.getAttribute("data-node");
      pos[id] = {
        x: id.charAt(0) === "u" ? un(r.left - s.left) - 9 : un(r.right - s.left) + 9,
        y: un(r.top - s.top + r.height / 2)
      };
    });

    var w = wall.getBoundingClientRect();
    wallL = un(w.left - s.left);
    wallR = un(w.right - s.left);
    var g = sc.querySelector(".gapline");
    gapX = g ? un(g.getBoundingClientRect().left - s.left) : wallR + 100;
    hostL = hostOff ? un(hostOff.getBoundingClientRect().left - s.left) : sceneW - 40;

    /* where "upstream" is for each lane — the chip when it is shown, else a
       point out beyond the seam, so beat 1 still reads on a phone */
    ECOS.forEach(function (eco) {
      var u = pos["u-" + eco], c = pos["c-" + eco];
      upX[eco] = { x: upsHidden || !u ? sceneW * 0.9 : u.x, y: c ? c.y : 0 };
    });
    rails();
  }

  /* rails: client → (upstream | wall) lerped by the peel, plus the far legs */
  function rails() {
    var lines = sc.querySelectorAll(".rails line.rail");
    if (!lines.length || !sceneH || !ECOS.length) return;
    var pct = function (v) { return ((v / sceneW) * 100).toFixed(2); };
    var near = pct(pos["c-" + ECOS[0]].x - 1);
    var far = wallL - 2, direct = upX[ECOS[0]].x + 1;
    var end = pct(direct + (far - direct) * peel);
    var x1u = pct(wallR + 2), x2u = pct(direct);
    ECOS.forEach(function (eco, i) {
      var p = pos["c-" + eco];
      if (!p) return;
      var y = ((p.y / sceneH) * 100).toFixed(2);
      set(lines[i], near, end, y);
      set(lines[i + ECOS.length], x1u, x2u, y);
    });
    function set(L, x1, x2, y) {
      if (!L) return;
      L.setAttribute("x1", x1); L.setAttribute("x2", x2);
      L.setAttribute("y1", y);  L.setAttribute("y2", y);
    }
  }

  /* ---------------- helpers (same vocabulary as the hero scene) -------- */
  function el(cls, parent) {
    var d = document.createElement("div");
    d.className = cls;
    (parent || flow).appendChild(d);
    return d;
  }
  function dot(cls) { return el("req-dot " + cls, flow); }
  function move(node, from, to, dur, ease) {
    node.style.transform = "translate(" + from.x + "px," + from.y + "px)";
    return node.animate(
      [{ transform: "translate(" + from.x + "px," + from.y + "px)" },
       { transform: "translate(" + to.x + "px," + to.y + "px)" }],
      { duration: dur, easing: ease || "cubic-bezier(.4,0,.2,1)", fill: "forwards" }
    ).finished;
  }
  function mark(node, cls, ms) {
    if (!node) return;
    node.classList.add(cls);
    setTimeout(function () { node.classList.remove(cls); }, ms);
  }
  function nodeAt(id) { return sc.querySelector('[data-node="' + id + '"]'); }
  function drop(node, delay) {
    setTimeout(function () {
      node.classList.add("out");
      setTimeout(function () { node.remove(); }, 450);
    }, delay);
  }
  function clamp01(v) { return v < 0 ? 0 : v > 1 ? 1 : v; }
  function pick(list) { return list[(Math.random() * list.length) | 0]; }
  /* how much room there is between the client chips and the wall: the floating
     labels are right-aligned on the wall, so anything wider than this would
     land on a chip (which happens on a phone, where the wall sits at 46%) */
  function corridor() {
    var c = pos["c-" + ECOS[0]];
    return c ? wallL - c.x : 0;
  }
  function tileState(list, on, cls) {
    for (var i = 0; i < list.length; i++) {
      list[i].classList.toggle(cls, i < on);
      if (i < on && cls === "committed") list[i].classList.remove("lit");
    }
  }
  function ping(list) {
    var on = list.filter(function (t) { return t.className !== "cas-tile"; });
    if (!on.length) return;
    var t = pick(on);
    t.classList.remove("ping"); void t.offsetWidth; t.classList.add("ping");
  }

  /* ---------------- scrub state ---------------- */
  var act = 1, t = 0, peel = 1, live = 0, hits = 0, misses = 0, lit = 0;

  function render() {
    var i = act - 1;
    peel = act > 1 ? 1 : clamp01((t - 0.45) / 0.5);

    stage.dataset.act = act;
    deck.style.setProperty("--peel", peel.toFixed(3));
    deck.style.setProperty("--peel-inv", (1 - peel).toFixed(3));
    stage.style.setProperty("--p", ((i + t) / BEAT.length).toFixed(4));
    /* progress through this beat — the copy drifts against it, so reading a
       beat is never a still frame even while the words stay put */
    stage.style.setProperty("--t", t.toFixed(3));

    if (deckTitle) deckTitle.textContent = BEAT[i].title;
    if (hudK) hudK.textContent = BEAT[i].k;

    /* the copy cross-fades between beats rather than scrolling past */
    actEls.forEach(function (a, n) {
      a.classList.toggle("is-current", n === i);
      a.classList.toggle("is-past", n < i);
    });
    ticks.forEach(function (tk, n) {
      tk.classList.toggle("on", n === i);
      tk.classList.toggle("done", n < i);
    });

    /* --- beats 3/4: the commit graph and what the store has committed - */
    var commits = act < 3 ? 0 : act > 3 ? ggNodes.length : (t < 0.18 ? 0 : t < 0.46 ? 1 : t < 0.74 ? 2 : 3);
    ggNodes.forEach(function (n, x) { n.classList.toggle("on", x < commits); });
    if (ckptEl) ckptEl.innerHTML = "saved<br>v" + (CFG.startVersion + commits);
    if (act >= 3) {
      var done = act > 3 ? CFG.committedAt3 : Math.round(clamp01(t * 1.15) * CFG.committedAt3);
      tileState(tiles, done, "committed");
    } else {
      tileState(tiles, 0, "committed");
    }

    /* --- beat 4: the shuttle crossing, then the import, then service -- */
    var off = act === 4;
    sc.classList.toggle("offline", off);
    if (shuttle) {
      /* out of the online host, across the seam, into the far host — then it
         hands off to the import, so it never parks on the seam label */
      var cross = off ? clamp01((t - 0.10) / 0.40) : 0;
      var from = wallR + 14, to = hostL + 14;
      var sx = from + (to - from) * cross;
      var sy = sceneH * 0.30 - 11;
      shuttle.style.transform = "translate(" + sx.toFixed(1) + "px," + sy.toFixed(1) + "px)";
      shuttle.classList.toggle("landed", off && t > 0.52);
    }
    var imported = off ? Math.round(clamp01((t - 0.50) / 0.22) * CFG.offTiles) : 0;
    tileState(offTiles, imported, "committed");

    if (statusT) {
      statusT.textContent =
        act === 1 ? "no cache · every request goes upstream"
        : act === 4 ? (t < 0.5 ? "offline · the pack is in transit"
                     : t < 0.72 ? "offline · importing the pack"
                     : "offline · serving from disk")
        : "connected";
    }

    /* --- the readout --- */
    if (hudV) {
      if (act === 1) hudV.textContent = t < 0.34 ? "again" : t < 0.68 ? "again ×2" : "again ×3";
      else if (act === 2) hudV.textContent = "local";
      else if (act === 3) hudV.textContent = "v" + (CFG.startVersion + commits);
      else hudV.textContent = t < 0.72 ? "loading" : "ready";
    }
  }

  /* ---------------- scroll → (beat, t) ----------------
     The frame is pinned for the whole section, so nothing scrolls past: the
     distance travelled inside the section is mapped onto one screen per beat
     plus a hold at the end. Must match `.stage-track { height: 460vh }`. */
  var BEATS = 4, HOLD = 0.6, HEADER = 56;
  var track = stage.querySelector(".stage-track");
  var queued = false;
  function onScroll() {
    if (queued) return;
    queued = true;
    requestAnimationFrame(function () {
      queued = false;
      var h = track ? track.offsetHeight : 0;
      var span = h ? h / (BEATS + HOLD) : window.innerHeight;
      var idx = (HEADER - stage.getBoundingClientRect().top) / span;
      var a = Math.min(BEATS, Math.max(1, Math.floor(idx) + 1));
      var changed = a !== act;
      act = a;
      t = clamp01(idx - (a - 1));
      render();
      if (changed || act === 1) rails();
    });
  }

  /* ---------------- links into the story ----------------
     The beats are pinned, so their ids have no scroll position of their own:
     map #how / #airgap / … onto the point in the track where that beat plays. */
  function beatTop(i) {
    var h = track ? track.offsetHeight : 0;
    var span = h ? h / (BEATS + HOLD) : window.innerHeight;
    return Math.round(stage.offsetTop - HEADER + (i + 0.35) * span);
  }
  function jump(id, smooth) {
    for (var i = 0; i < actEls.length; i++) {
      if (actEls[i].id !== id) continue;
      /* "instant" on load: `behavior: auto` would defer to the page's
         scroll-behavior: smooth and race the browser's own hash jump */
      window.scrollTo({ top: beatTop(i), behavior: smooth ? "smooth" : "instant" });
      return true;
    }
    return false;
  }
  [].forEach.call(document.querySelectorAll('a[href^="#"]'), function (a) {
    a.addEventListener("click", function (e) {
      if (jump(a.getAttribute("href").slice(1), true)) e.preventDefault();
    });
  });
  /* …and the same for a hash typed into the bar or arrived at via history */
  window.addEventListener("hashchange", function () {
    jump(window.location.hash.slice(1), false);
  });
  /* a deep link (…/#airgap) has to beat the browser's own fragment scroll,
     which lands on the top of the pinned frame — so re-apply it once things
     have settled. A programmatic scroll cancels the one already in flight. */
  if (window.location.hash) {
    var deep = window.location.hash.slice(1);
    var land = function () { jump(deep, false); };
    land();
    window.addEventListener("load", land);
    setTimeout(land, 400);
  }

  /* ---------------- one request (beats 2–4) ---------------- */
  function request(eco, twin) {
    var from = pos["c-" + eco];
    if (!from) return;
    live++;
    var face = { x: wallL - 4, y: from.y };
    var d = dot("e-" + eco);
    var isHit = !twin && Math.random() < (act === 4 ? 0.72 : 0.66);

    move(d, from, face, CFG.legIn, "cubic-bezier(.45,0,.55,1)").then(function () {
      if (isHit) return served(d, eco, face, from);
      misses++;

      var slit = el("slit", flow);
      slit.style.left = wallL + "px";
      slit.style.width = (wallR - wallL) + "px";
      slit.style.transform = "translateY(" + (from.y - 1) + "px)";
      requestAnimationFrame(function () { slit.classList.add("on"); });
      setTimeout(function () { slit.classList.remove("on"); setTimeout(function () { slit.remove(); }, 300); }, 700);

      if (act === 4) return failsAtSeam(d, eco, face, from);

      var upNode = nodeAt("u-" + eco);
      mark(upNode, "busy", 1300);
      var out = { x: wallR + 4, y: from.y };
      return move(d, face, out, CFG.legThrough)
        .then(function () { return move(d, out, upX[eco], CFG.legOut, "cubic-bezier(.45,0,.55,1)"); })
        .then(function () {
          d.remove();
          var stream = dot("e-" + eco + " rd-stream");
          return move(stream, upX[eco], out, CFG.legStream, "cubic-bezier(.4,0,.6,1)").then(function () {
            stream.remove();
            lightTile();
            var targets = twin ? [eco, twin] : [eco];
            return Promise.all(targets.map(function (id, n) {
              var r = dot("rd-hit");
              return move(r, { x: wallL - 4, y: pos["c-" + id].y }, pos["c-" + id],
                          CFG.legServe + n * 60, "cubic-bezier(.1,.9,.2,1)")
                .then(function () { mark(nodeAt("c-" + id), "served", 850); r.remove(); });
            })).then(function () { live--; });
          });
        });
    });

    function served(reqDot, id, at, home) {
      hits++;
      ping(tiles);
      var f = el("face face-hit", flow);
      f.style.left = (wallL - 2) + "px";
      f.style.top = (at.y - 9) + "px";
      drop(f, 160);

      if (corridor() > 62) {
        var ms = el("tag tag-ms", flow);
        ms.textContent = "local hit";
        ms.style.left = (wallL - 12) + "px";
        ms.style.top = at.y + "px";
        ms.style.transform = "translate(-100%,-50%)";
        requestAnimationFrame(function () { ms.classList.add("on"); });
        setTimeout(function () { ms.classList.remove("on"); setTimeout(function () { ms.remove(); }, 320); }, 600);
      }

      var r = dot("rd-hit");
      return move(r, at, home, CFG.legHit, "cubic-bezier(.1,.9,.2,1)").then(function () {
        mark(nodeAt("c-" + id), "served", 850);
        r.remove(); reqDot.remove(); live--;
      });
    }

    function failsAtSeam(reqDot, id, at, home) {
      var seal = el("sealed", flow);
      seal.style.left = gapX + "px";
      seal.style.top = (at.y - 9) + "px";
      drop(seal, 500);
      var stop = { x: gapX - 8, y: at.y };
      return move(reqDot, at, stop, 360).then(function () {
        var r = dot("rd-fail");
        return move(r, stop, home, 400, "cubic-bezier(.2,.7,.3,1)").then(function () {
          mark(nodeAt("c-" + id), "failed", 850);
          r.remove(); reqDot.remove(); live--;
        });
      });
    }
  }

  function lightTile() {
    var empty = tiles.filter(function (x) { return x.className === "cas-tile"; });
    if (!empty.length) return;
    empty[0].classList.add("lit");
    lit++;
  }

  /* two clients, one fetch — beat 2's signature */
  function singleFlight() {
    var a = pick(ECOS), b = a, guard = 0;
    while (b === a && guard++ < 8) b = pick(ECOS);
    if (corridor() > 130) {
      var tag = el("tag tag-sf", flow);
      tag.textContent = "two builds asking · one download";
      tag.style.left = (wallL - 10) + "px";
      tag.style.top = ((pos["c-" + a].y + pos["c-" + b].y) / 2) + "px";
      tag.style.transform = "translate(-100%,-50%)";
      requestAnimationFrame(function () { tag.classList.add("on"); });
      setTimeout(function () { tag.classList.remove("on"); setTimeout(function () { tag.remove(); }, 400); }, 2600);
    }

    request(a, b);
    var d2 = dot("e-" + b);
    move(d2, pos["c-" + b], { x: wallL - 10, y: pos["c-" + b].y }, 700, "cubic-bezier(.45,0,.4,1)")
      .then(function () {
        return d2.animate([{ opacity: 1 }, { opacity: 0.3 }, { opacity: 1 }],
          { duration: 620, iterations: 2 }).finished;
      })
      .then(function () { d2.remove(); });
  }

  /* ---------------- beat 1: no cache — the whole way, every time ------- */
  var direct = 0;
  function directFetch(eco) {
    var home = pos["c-" + eco], target = upX[eco];
    if (!home || !target) return;
    live++;
    var d = dot("e-" + eco);
    var upNode = nodeAt("u-" + eco);
    mark(upNode, "busy", CFG.legDirect + 400);
    var throttled = ++direct % 4 === 0;

    move(d, home, target, CFG.legDirect, "cubic-bezier(.4,0,.6,1)").then(function () {
      if (throttled) {                       /* the rate limit, at the worst moment */
        d.remove();
        var tag = el("tag tag-bad", flow);
        tag.textContent = "blocked · too many requests";
        tag.style.left = (target.x - 12) + "px";
        tag.style.top = target.y + "px";
        tag.style.transform = "translate(-100%,-50%)";
        requestAnimationFrame(function () { tag.classList.add("on"); });
        setTimeout(function () { tag.classList.remove("on"); setTimeout(function () { tag.remove(); }, 350); }, 1500);
        mark(upNode, "failed", 1200);
        var f = dot("rd-fail");
        return move(f, target, home, 700, "cubic-bezier(.2,.7,.3,1)").then(function () {
          mark(nodeAt("c-" + eco), "failed", 900);
          f.remove(); live--;
        });
      }
      d.remove();
      var stream = dot("e-" + eco + " rd-stream");
      return move(stream, target, home, CFG.legBack, "cubic-bezier(.4,0,.6,1)").then(function () {
        stream.remove();
        mark(nodeAt("c-" + eco), "busy", 800);
        live--;
      });
    });
  }

  /* ---------------- beat 4: the imported cache serving its own host --- */
  var offN = 0;
  function offlineServe() {
    if (!offChips.length) return;
    var chip = offChips[offN++ % offChips.length];
    var failing = offN % 5 === 0;
    mark(chip, failing ? "failed" : "served", 900);
    if (!failing) ping(offTiles);
  }

  /* ---------------- ambient traffic, gated on the deck being visible --- */
  var onScreen = false;
  if ("IntersectionObserver" in window) {
    new IntersectionObserver(function (e) { onScreen = e[0].isIntersecting; },
      { rootMargin: "10% 0px" }).observe(stage);
  } else {
    onScreen = true;
  }

  function ambient() {
    var wait = CFG.spawn2;
    if (!reduce && waapi && onScreen && !document.hidden && live < CFG.maxLive) {
      if (act === 1) { directFetch(pick(ECOS)); wait = CFG.spawn1; }
      else if (act === 2) {
        if (Math.random() < 0.18) singleFlight(); else request(pick(ECOS), null);
        wait = CFG.spawn2;
      } else if (act === 3) { request(pick(ECOS), null); wait = CFG.spawn3; }
      else {
        if (t > 0.72 && Math.random() < 0.6) offlineServe();
        else request(pick(ECOS), null);
        wait = CFG.spawn4;
      }
    }
    setTimeout(ambient, wait + Math.random() * 500);
  }

  /* ---------------- go ---------------- */
  measure();
  if ("ResizeObserver" in window) new ResizeObserver(measure).observe(sc);
  else window.addEventListener("resize", measure);
  window.addEventListener("resize", measure);
  window.addEventListener("scroll", onScroll, { passive: true });
  onScroll();
  render();

  if (reduce || !waapi) {
    /* no ambient traffic: show the store as a filled, checkpointed cache so
       every beat still reads as a finished state while the reader scrubs */
    for (var q = CFG.committedAt3; q < CFG.committedAt3 + 3 && q < tiles.length; q++) {
      tiles[q].classList.add("lit");
    }
  } else {
    setTimeout(ambient, 900);
  }
})();
