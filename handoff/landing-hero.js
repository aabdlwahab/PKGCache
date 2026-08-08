/* =====================================================================
   pkgcache — hero scene engine ("membrane" redesign)

   CSP-safe: no inline styles in markup, no eval, no external requests.
   The only thing this file writes to `style` is the `transform` of the
   moving dots (and the measured x/y of a few transient markers) — every
   other visual state is a CSS class defined in landing-hero.css.

   Contract it expects in the DOM (see landing-hero.html):
     .scene                          the band
     .scene .rails line.rail         6 client lanes then 6 upstream lanes,
                                     in the same order as the client chips
     .scene .clients [data-node="c-<eco>"]
     .scene .ups     [data-node="u-<eco>"]
     .scene .wall  ·  .wall #cas  ·  .wall .wall-ckpt
     .scene .flow                    dots are appended here (behind the wall)
     .scene .scene-status .label
     #typed                          the typed command line
     #kpi-hitrate                    optional readout in the copy column
   ===================================================================== */
(function () {
  "use strict";

  /* ---------------- tunables ---------------- */
  var CFG = {
    tiles: 22,             // CAS tiles in the wall
    litPerCheckpoint: 8,   // fresh blobs before a checkpoint commits
    hitRate: 0.70,         // ~70% of requests are hits
    sfChance: 0.14,        // chance a spawn is a single-flight pair
    maxLive: 4,            // requests in flight
    spawnMin: 940, spawnJitter: 860,
    legIn: 680,            // client → wall
    legHit: 240,           // wall → client (a hit is visibly ~3× faster)
    legThrough: 200,       // wall left face → right face
    legOut: 620,           // wall → upstream
    legStream: 900,        // upstream → wall (the one fetch)
    legServe: 300,         // wall → client after a miss
    offlineEvery: 24000,   // air-gap beat cadence
    offlineFor: 9000,      // how long upstreams stay dark
    startVersion: 13
  };

  var scene = document.querySelector(".scene");
  if (!scene) return;

  var flow    = scene.querySelector(".flow");
  var wall    = scene.querySelector(".wall");
  var casEl   = scene.querySelector("#cas");
  var ckptEl  = scene.querySelector(".wall-ckpt");
  var statusT = scene.querySelector(".scene-status .label");
  var kpiHit  = document.querySelector("#kpi-hitrate");
  var reduce  = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  /* ecosystems, in the order the client chips appear */
  var ECOS = [].map.call(scene.querySelectorAll(".clients [data-node]"), function (n) {
    return n.getAttribute("data-node").slice(2);
  });

  /* ---------------- CAS tiles ---------------- */
  var tiles = [];
  for (var i = 0; i < CFG.tiles; i++) {
    var t = document.createElement("div");
    t.className = "cas-tile";
    casEl.appendChild(t);
    tiles.push(t);
  }

  /* ---------------- reduced motion / no WAAPI: static end-state ------- */
  if (reduce || !flow || typeof flow.animate !== "function") {
    for (var s0 = 0; s0 < 14; s0++) tiles[s0].className = "cas-tile committed";
    for (var s1 = 14; s1 < 17; s1++) tiles[s1].className = "cas-tile lit";
    if (ckptEl) ckptEl.innerHTML = "ckpt<br>v" + CFG.startVersion;
    if (kpiHit) kpiHit.textContent = "93%";
    var tc = document.querySelector("#typed");
    if (tc) tc.textContent = "docker pull cache.internal:8443/library/nginx:1.27";
    return;
  }

  /* ---------------- geometry (zoom / transform safe) ------------------ */
  /* getBoundingClientRect is in *screen* px: if any ancestor is scaled
     (page zoom, a design canvas, a CSS transform) those numbers do not match
     the CSS px a transform animates in. Everything below is divided by the
     measured scale factor, so the dots stay on their rails at any zoom.     */
  var pos = {}, wallL = 0, wallR = 0, gapX = 0;

  function measure() {
    var s = scene.getBoundingClientRect();
    var k = scene.offsetWidth ? s.width / scene.offsetWidth : 1;
    if (!k) k = 1;
    var W = scene.offsetWidth || s.width;
    var H = scene.offsetHeight || s.height;
    var un = function (v) { return v / k; };

    [].forEach.call(scene.querySelectorAll("[data-node]"), function (n) {
      var r = n.getBoundingClientRect();
      var id = n.getAttribute("data-node");
      pos[id] = {
        /* dots leave from / arrive at the chip EDGE, never its label */
        x: id.charAt(0) === "u" ? un(r.left - s.left) - 9 : un(r.right - s.left) + 9,
        y: un(r.top - s.top + r.height / 2)
      };
    });

    var w = wall.getBoundingClientRect();
    wallL = un(w.left - s.left);
    wallR = un(w.right - s.left);
    var g = scene.querySelector(".gapline");
    gapX = g ? un(g.getBoundingClientRect().left - s.left) : wallR + 120;

    /* rails are drawn from the measurement, so they can never drift out of
       register with the chips or with the dot paths at any scene size */
    var lines = scene.querySelectorAll(".rails line.rail");
    if (!lines.length || !H) return;
    var pct = function (v) { return ((v / W) * 100).toFixed(2); };
    var x1c = pct(pos["c-" + ECOS[0]].x - 1), x2c = pct(wallL - 2);
    var x1u = pct(wallR + 2), x2u = pct(pos["u-" + ECOS[0]].x + 1);
    ECOS.forEach(function (eco, i) {
      var p = pos["c-" + eco];
      if (!p) return;
      var y = ((p.y / H) * 100).toFixed(2);
      set(lines[i], x1c, x2c, y);
      set(lines[i + ECOS.length], x1u, x2u, y);
    });
    function set(L, x1, x2, y) {
      if (!L) return;
      L.setAttribute("x1", x1); L.setAttribute("x2", x2);
      L.setAttribute("y1", y);  L.setAttribute("y2", y);
    }
  }
  measure();
  if ("ResizeObserver" in window) new ResizeObserver(measure).observe(scene);
  else window.addEventListener("resize", measure);

  /* ---------------- small helpers ---------------- */
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
    node.classList.add(cls);
    setTimeout(function () { node.classList.remove(cls); }, ms);
  }
  function nodeAt(id) { return scene.querySelector('[data-node="' + id + '"]'); }
  function drop(node, delay) {
    setTimeout(function () {
      node.classList.add("out");
      setTimeout(function () { node.remove(); }, 450);
    }, delay);
  }

  /* ---------------- store state ---------------- */
  var state = { hits: 0, misses: 0, live: 0, lit: 0, version: CFG.startVersion, offline: false };

  function readout() {
    var total = state.hits + state.misses;
    if (kpiHit && total >= 4) kpiHit.textContent = Math.round((state.hits / total) * 100) + "%";
  }
  function pingTile() {
    var on = tiles.filter(function (t) { return t.className !== "cas-tile"; });
    if (!on.length) return;
    var t = on[(Math.random() * on.length) | 0];
    t.classList.remove("ping"); void t.offsetWidth; t.classList.add("ping");
  }
  function lightTile() {
    var empty = tiles.filter(function (t) { return t.className === "cas-tile"; });
    if (!empty.length) return;
    empty[0].classList.add("lit");
    state.lit++;
    if (state.lit >= CFG.litPerCheckpoint) checkpoint();
  }
  function checkpoint() {
    mark(wall, "commit", 1000);
    state.version++;
    if (ckptEl) ckptEl.innerHTML = "ckpt<br>v" + state.version;
    tiles.forEach(function (t) {
      if (t.classList.contains("lit")) { t.classList.remove("lit"); t.classList.add("committed"); }
    });
    state.lit = 0;
    var full = tiles.filter(function (t) { return t.classList.contains("committed"); }).length;
    if (full >= tiles.length - 2) {
      setTimeout(function () { tiles.forEach(function (t) { t.className = "cas-tile"; }); }, 1600);
    }
  }

  /* ---------------- one request ---------------- */
  /* twin != null  →  this fetch is shared: two clients, one upstream stream. */
  function request(eco, twin) {
    var from = pos["c-" + eco];
    if (!from) return;
    state.live++;
    var face = { x: wallL - 4, y: from.y };
    var d = dot("e-" + eco);
    var isHit = !twin && Math.random() < CFG.hitRate;

    move(d, from, face, CFG.legIn, "cubic-bezier(.45,0,.55,1)").then(function () {
      if (isHit) return served(d, eco, face, from);
      state.misses++;

      /* the wall opens for this lane only */
      var slit = el("slit", flow);
      slit.style.left = wallL + "px";
      slit.style.width = (wallR - wallL) + "px";
      slit.style.transform = "translateY(" + (from.y - 1) + "px)";
      requestAnimationFrame(function () { slit.classList.add("on"); });
      setTimeout(function () { slit.classList.remove("on"); setTimeout(function () { slit.remove(); }, 300); }, 700);

      if (state.offline) return failsAtSeam(d, eco, face, from);

      /* one fetch, upstream */
      var upNode = nodeAt("u-" + eco);
      if (upNode) mark(upNode, "busy", 1400);
      var out = { x: wallR + 4, y: from.y };
      return move(d, face, out, CFG.legThrough)
        .then(function () { return move(d, out, pos["u-" + eco], CFG.legOut, "cubic-bezier(.45,0,.55,1)"); })
        .then(function () {
          d.remove();
          var stream = dot("e-" + eco + " rd-stream");
          return move(stream, pos["u-" + eco], out, CFG.legStream, "cubic-bezier(.4,0,.6,1)").then(function () {
            stream.remove();
            lightTile();
            var targets = twin ? [eco, twin] : [eco];
            return Promise.all(targets.map(function (id, n) {
              var r = dot("rd-hit");
              var start = { x: wallL - 4, y: pos["c-" + id].y };
              return move(r, start, pos["c-" + id], CFG.legServe + n * 60, "cubic-bezier(.1,.9,.2,1)")
                .then(function () {
                  var c = nodeAt("c-" + id);
                  if (c) mark(c, "served", 900);
                  r.remove();
                });
            })).then(function () { state.live--; readout(); });
          });
        });
    });

    /* a hit: the near face flashes, "0 ms", and the dot comes straight back */
    function served(reqDot, id, at, home) {
      state.hits++;
      pingTile();

      var f = el("face face-hit", flow);
      f.style.left = (wallL - 2) + "px";
      f.style.top = (at.y - 9) + "px";
      drop(f, 160);

      var ms = el("tag tag-ms", flow);
      ms.textContent = "0 ms";
      ms.style.left = (wallL - 12) + "px";
      ms.style.top = at.y + "px";
      ms.style.transform = "translate(-100%,-50%)";
      requestAnimationFrame(function () { ms.classList.add("on"); });
      setTimeout(function () { ms.classList.remove("on"); setTimeout(function () { ms.remove(); }, 320); }, 620);

      var r = dot("rd-hit");
      return move(r, at, home, CFG.legHit, "cubic-bezier(.1,.9,.2,1)").then(function () {
        var c = nodeAt("c-" + id);
        if (c) mark(c, "served", 900);
        r.remove(); reqDot.remove(); state.live--; readout();
      });
    }

    /* OFFLINE=1: a miss dies at the seam, hits keep being served */
    function failsAtSeam(reqDot, id, at, home) {
      var seal = el("sealed", flow);
      seal.style.left = gapX + "px";
      seal.style.top = (at.y - 9) + "px";
      drop(seal, 500);
      var stop = { x: gapX - 8, y: at.y };
      return move(reqDot, at, stop, 380).then(function () {
        var r = dot("rd-fail");
        return move(r, stop, home, 420, "cubic-bezier(.2,.7,.3,1)").then(function () {
          var c = nodeAt("c-" + id);
          if (c) mark(c, "failed", 900);
          r.remove(); reqDot.remove(); state.live--; readout();
        });
      });
    }
  }

  /* two clients ask for the same artifact: one upstream fetch serves both */
  function singleFlight() {
    var a = ECOS[(Math.random() * ECOS.length) | 0];
    var b = a, guard = 0;
    while (b === a && guard++ < 8) b = ECOS[(Math.random() * ECOS.length) | 0];

    var tag = el("tag tag-sf", flow);
    tag.textContent = "single-flight · 2 clients, 1 fetch";
    tag.style.left = (wallL - 10) + "px";
    tag.style.top = ((pos["c-" + a].y + pos["c-" + b].y) / 2) + "px";
    tag.style.transform = "translate(-100%,-50%)";
    requestAnimationFrame(function () { tag.classList.add("on"); });
    setTimeout(function () { tag.classList.remove("on"); setTimeout(function () { tag.remove(); }, 400); }, 2800);

    request(a, b);

    /* the follower rides to the wall and waits on the leader's stream */
    var d2 = dot("e-" + b);
    move(d2, pos["c-" + b], { x: wallL - 10, y: pos["c-" + b].y }, 720, "cubic-bezier(.45,0,.4,1)")
      .then(function () {
        return d2.animate([{ opacity: 1 }, { opacity: 0.3 }, { opacity: 1 }],
          { duration: 640, iterations: 2 }).finished;
      })
      .then(function () { d2.remove(); });
  }

  /* ---------------- the air-gap beat ---------------- */
  function setOffline(on) {
    state.offline = on;
    scene.classList.toggle("offline", on);
    if (statusT) statusT.textContent = on ? "offline=1 · hits still serve, misses fail" : "online";
  }
  (function beat() {
    setTimeout(function () {
      setOffline(true);
      setTimeout(function () { setOffline(false); beat(); }, CFG.offlineFor);
    }, CFG.offlineEvery);
  })();

  /* ---------------- scripted intro, then ambient ---------------- */
  setTimeout(function () { request(ECOS[0], null); }, 600);          // a hit
  setTimeout(function () { request(ECOS[2], null); }, 1900);         // a miss → upstream
  setTimeout(function () { singleFlight(); }, 3600);                 // the signature behaviour
  setTimeout(function loop() {
    if (!document.hidden && state.live < CFG.maxLive) {
      if (Math.random() < CFG.sfChance) singleFlight();
      else request(ECOS[(Math.random() * ECOS.length) | 0], null);
    }
    setTimeout(loop, CFG.spawnMin + Math.random() * CFG.spawnJitter);
  }, 7200);

  /* ---------------- the typed command line (unchanged behaviour) ------- */
  (function typed() {
    var target = document.querySelector("#typed");
    if (!target) return;
    var cmds = [
      "docker pull cache.internal:8443/library/nginx:1.27",
      "uv pip install -i https://cache.internal:8443/pypi/simple numpy",
      "pkgcache checkpoint && pkgcache export --to /media/usb",
      "OFFLINE=1 pkgcache serve   # air-gapped host"
    ];
    var ci = 0;
    (function run() {
      var cmd = cmds[ci++ % cmds.length], i = 0;
      (function type() {
        target.textContent = cmd.slice(0, ++i);
        if (i < cmd.length) return setTimeout(type, 26 + Math.random() * 34);
        setTimeout(function () {
          var j = cmd.length;
          (function del() {
            j -= 2;
            target.textContent = cmd.slice(0, Math.max(0, j));
            if (j > 0) setTimeout(del, 14); else setTimeout(run, 400);
          })();
        }, 2600);
      })();
    })();
  })();
})();
