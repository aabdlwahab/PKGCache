/* pkgcache marketing homepage — progressive enhancement only. With JS off the
   page is fully readable; nothing here gates content. No inline handlers, so the
   console's strict CSP (script-src 'self') is satisfied by loading this file. */
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

  /* ---- typed hero command ---------------------------------------------- */
  var typed = document.getElementById("typed");
  var commandLine = document.getElementById("command-line");
  if (typed && !reduce) {
    var cmds = [
      "docker pull cache:8443/dockerhub/library/python:3.12-slim",
      "pip install --index-url https://cache:8443/global/pypi/root/pypi/+simple/ torch",
      "npm install --registry https://cache:8443/global/npm/ left-pad",
      "git clone https://cache:8443/global/git/github.com/pallets/click.git",
      "python3 scripts/pkgops.py checkpoint \"added numpy 2.1 + torch 2.3\"",
    ];
    var ci = 0, chi = 0;
    var announce = function (name) {
      window.dispatchEvent(new CustomEvent("pkgcache:" + name, { detail: { index: ci } }));
    };
    var nextCommand = function () {
      ci = (ci + 1) % cmds.length;
      chi = 0;
      typed.textContent = "";
      if (commandLine) {
        commandLine.classList.remove("is-sent");
        commandLine.classList.remove("is-swapping");
      }
      announce("prepare");
      setTimeout(tick, 180);
    };
    var tick = function () {
      var cur = cmds[ci];
      if (chi < cur.length) {
        chi++;
        typed.textContent = cur.slice(0, chi);
        setTimeout(tick, 26 + Math.random() * 34);
      } else {
        if (commandLine) commandLine.classList.add("is-sent");
        announce("trace");
        setTimeout(function () {
          if (commandLine) commandLine.classList.add("is-swapping");
          setTimeout(nextCommand, 240);
        }, 3900);
      }
    };
    setTimeout(function () { announce("prepare"); tick(); }, 700);
  } else if (typed) {
    typed.textContent = "docker pull cache:8443/dockerhub/library/python:3.12-slim";
  }

  /* ---- hero scene: one clear benefit at a time ------------------------- */
  var scene = document.getElementById("simple-scene");
  var simpleMessage = document.getElementById("simple-message");
  var simpleIndex = document.getElementById("simple-index");
  var simpleKicker = document.getElementById("simple-kicker");
  var simpleHeadline = document.getElementById("simple-headline");
  var simpleCopy = document.getElementById("simple-copy");
  var simpleBadge = document.getElementById("simple-badge");
  var simplePayload = document.getElementById("simple-payload");
  var simpleSteps = document.querySelectorAll(".simple-steps span");

  if (scene) {
    var payloads = ["python:3.12-slim", "torch", "left-pad", "pallets/click.git", "new checkpoint"];
    var storyTimers = [];
    var clearStoryTimers = function () {
      storyTimers.forEach(function (timer) { clearTimeout(timer); });
      storyTimers = [];
    };
    var later = function (fn, delay) { storyTimers.push(setTimeout(fn, delay)); };
    var setBeat = function (beat, kicker, headline, copy, badge) {
      scene.classList.toggle("is-step-2", beat === 1);
      scene.classList.toggle("is-step-3", beat === 2);
      simpleSteps.forEach(function (step, index) { step.classList.toggle("is-active", index === beat); });
      if (!simpleMessage) return;
      simpleMessage.classList.add("is-changing");
      later(function () {
        if (simpleIndex) simpleIndex.textContent = "0" + (beat + 1);
        if (simpleKicker) simpleKicker.textContent = kicker;
        if (simpleHeadline) simpleHeadline.textContent = headline;
        if (simpleCopy) simpleCopy.textContent = copy;
        if (simpleBadge) simpleBadge.textContent = badge;
        simpleMessage.classList.remove("is-changing");
      }, 140);
    };
    var prepareStory = function (index) {
      clearStoryTimers();
      if (simplePayload) simplePayload.textContent = payloads[index] || payloads[0];
      if (index === 4) setBeat(0, "checkpoint", "Changes sealed.", "Versioned in git + DVC.", "VERSIONED");
      else setBeat(0, "first pull", "Fetched once.", "One upstream request. That’s it.", "UPSTREAM ×1");
    };
    var runStory = function (index) {
      clearStoryTimers();
      if (index === 4) setBeat(0, "checkpoint", "Changes sealed.", "Versioned in git + DVC.", "VERSIONED");
      else setBeat(0, "first pull", "Fetched once.", "One upstream request. That’s it.", "UPSTREAM ×1");

      later(function () {
        if (index === 4) setBeat(1, "what moves", "Only what’s new.", "A clean, auditable delta.", "DELTA ONLY");
        else setBeat(1, "every pull after", "Fast from then on.", "Every repeat request stays local.", "LOCAL HIT");
      }, 1250);

      later(function () {
        setBeat(2, "connected or not", "Air-gap ready.", "The packages are already there.", "Δ ONLY");
      }, 2600);
    };

    window.addEventListener("pkgcache:prepare", function (event) { prepareStory(event.detail.index); });
    window.addEventListener("pkgcache:trace", function (event) { runStory(event.detail.index); });
    prepareStory(0);
    if (reduce) {
      clearStoryTimers();
      scene.classList.add("is-step-3");
      simpleSteps.forEach(function (step, index) { step.classList.toggle("is-active", index === 2); });
      if (simpleMessage) simpleMessage.classList.remove("is-changing");
      if (simpleIndex) simpleIndex.textContent = "03";
      if (simpleKicker) simpleKicker.textContent = "connected or not";
      if (simpleHeadline) simpleHeadline.textContent = "Air-gap ready.";
      if (simpleCopy) simpleCopy.textContent = "The packages are already there.";
      if (simpleBadge) simpleBadge.textContent = "Δ ONLY";
    }
  }
})();
