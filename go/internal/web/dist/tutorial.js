/* Client setup tutorial — progressive enhancement only. The page is complete
   without JavaScript; this file adds instance downloads, the current fingerprint,
   copy buttons, theme switching, and restrained scroll effects. */
(function () {
  "use strict";

  var root = document.documentElement;

  /* Every command on this page is rewritten in place rather than by assigning
     textContent. coords.js holds references to these exact text nodes so it can swap
     the example address for this instance's own, and replacing a node would leave it
     writing to a detached one — the substitution would silently stop happening
     depending on which fetch resolved first. */
  function rewriteCommands(replace) {
    [].forEach.call(document.querySelectorAll(".term-cmd"), function (command) {
      for (var node = command.firstChild; node; node = node.nextSibling) {
        if (node.nodeType === 3) node.nodeValue = replace(node.nodeValue);
      }
    });
  }

  var project = "global";
  try {
    var requestedProject = new URLSearchParams(window.location.search).get("project") || "";
    if (/^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/.test(requestedProject)) {
      project = requestedProject;
    }
  } catch (error) {
    /* Older browsers use the global project, which is also the static fallback. */
  }
  [].forEach.call(document.querySelectorAll('[data-role="project"]'), function (node) {
    node.textContent = project;
  });
  /* A non-global project prefixes ecosystem paths as well as naming itself in the
     start command, so both forms are rewritten together. */
  if (project !== "global") {
    rewriteCommands(function (text) {
      return text
        .split("-project global")
        .join("-project " + project)
        .split("/projects/global/")
        .join("/projects/" + project + "/")
        .split("/global/")
        .join("/" + project + "/")
        .split("/dockerhub/")
        .join("/" + project + "/dockerhub/");
    });
  }

  /* ---- instance downloads --------------------------------------------- */
  var downloads = document.querySelector('[data-role="downloads"]');
  var downloadState = document.querySelector(".download-live");
  /* The TL;DR's first step. It offers only the builds for the machine asking, since
     the whole point of that section is not making the reader choose; the full matrix
     stays in the walkthrough below, and the static markup already links to it for
     anyone whose platform we cannot name. */
  var quick = document.querySelector('[data-role="downloads-quick"]');

  function fileSize(value) {
    var bytes = Number(value || 0);
    if (bytes >= 1048576) return (bytes / 1048576).toFixed(1) + " MB";
    return Math.max(1, Math.round(bytes / 1024)) + " KB";
  }

  function platformLabel(item) {
    var labels = { linux: "Linux", darwin: "macOS", windows: "Windows" };
    return (labels[item.os] || item.os) + " " + item.arch;
  }

  function currentOS() {
    var value = String(navigator.platform || navigator.userAgent || "").toLowerCase();
    if (value.indexOf("win") !== -1) return "windows";
    if (value.indexOf("mac") !== -1) return "darwin";
    if (value.indexOf("linux") !== -1) return "linux";
    return "";
  }

  function setDownloadState(label, className) {
    if (!downloadState) return;
    downloadState.lastChild.nodeValue = label;
    downloadState.classList.remove("ready", "empty");
    if (className) downloadState.classList.add(className);
  }

  function downloadFailure(message, command) {
    if (!downloads) return;
    downloads.textContent = "";
    var note = document.createElement("p");
    note.className = "note";
    note.textContent = message;
    if (command) {
      var code = document.createElement("code");
      code.textContent = command;
      note.appendChild(document.createTextNode(" "));
      note.appendChild(code);
    }
    downloads.appendChild(note);
    setDownloadState("not published", "empty");
  }

  function renderQuick(rows) {
    if (!quick || !rows.length) return;
    quick.textContent = "";
    rows.forEach(function (item) {
      var link = document.createElement("a");
      link.className = "btn btn-primary dl-btn";
      link.href = "/api/v1/downloads/" + encodeURIComponent(item.name);
      link.download = item.name;
      if (item.sha256) link.title = "SHA-256 " + item.sha256;
      var label = document.createElement("b");
      label.textContent = "Download for " + platformLabel(item);
      var size = document.createElement("span");
      size.className = "dl-size";
      size.textContent = fileSize(item.bytes);
      link.appendChild(label);
      link.appendChild(size);
      quick.appendChild(link);
    });
  }

  if (downloads && window.fetch) {
    fetch("/api/v1/downloads", { credentials: "same-origin" })
      .then(function (response) { return response.ok ? response.json() : null; })
      .then(function (data) {
        if (!data) {
          downloadFailure("Could not ask this cache which client files are available.");
          return;
        }
        var rows = (data.downloads || []).filter(function (item) {
          return item.tool === "client";
        });
        if (!rows.length) {
          /* Name the command rather than saying "ask the operator": on most instances
             the reader either is the operator or is about to message them, and either
             way one sentence they can paste ends this faster. */
          downloadFailure(
            "No client is published on this instance yet. On the cache host, the " +
              "operator runs",
            "pkgreg publish-client",
          );
          return;
        }

        downloads.textContent = "";
        var list = document.createElement("div");
        list.className = "dl-row";
        var os = currentOS();
        rows.forEach(function (item) {
          var cell = document.createElement("div");
          var link = document.createElement("a");
          link.className = "btn btn-ghost dl-btn";
          if (item.os === os) link.classList.add("recommended");
          link.href = "/api/v1/downloads/" + encodeURIComponent(item.name);
          link.download = item.name;
          if (item.sha256) link.title = "SHA-256 " + item.sha256;

          var label = document.createElement("b");
          label.textContent = platformLabel(item);
          var size = document.createElement("span");
          size.className = "dl-size";
          size.textContent = fileSize(item.bytes);
          link.appendChild(label);
          link.appendChild(size);
          cell.appendChild(link);

          if (item.sha256) {
            var hash = document.createElement("code");
            hash.className = "dl-hash";
            hash.textContent = "sha256 " + item.sha256;
            cell.appendChild(hash);
          }
          list.appendChild(cell);
        });
        downloads.appendChild(list);
        setDownloadState(rows.length + " files ready", "ready");
        renderQuick(rows.filter(function (item) { return item.os === os; }));
      })
      .catch(function () {
        downloadFailure("Could not reach this cache to list client downloads.");
      });
  }

  /* ---- fingerprint ----------------------------------------------------- */
  /* Taken from the public coordinates response that coords.js already fetched. An
     earlier version asked the project endpoints route, which requires a login: on any
     instance with authentication switched on, this page left the literal
     PASTE_FINGERPRINT in the command it told people to copy. */
  var fingerprint = document.querySelector('[data-role="fingerprint"]');
  var noTLS = document.querySelector('[data-role="no-tls"]');
  if (window.pkgregCoordinates) {
    window.pkgregCoordinates
      .then(function (data) {
        if (!data) return;
        var value = data.ca_sha256;
        if (fingerprint && value) {
          fingerprint.textContent = value;
          fingerprint.classList.add("is-live");
          rewriteCommands(function (text) {
            return text.split("PASTE_FINGERPRINT").join(value);
          });
        }
        /* A non-https scheme means the server has no certificate pair. coords.js will
           have rewritten every example address to http://, and the client rejects those,
           so the reader needs to know the blocker is upstream of them. */
        if (noTLS && data.scheme && data.scheme !== "https") {
          noTLS.hidden = false;
        }
      })
      .catch(function () {
        /* Offline or a file:// preview. The static wording already stands on its own. */
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
    var field = document.createElement("textarea");
    field.value = text;
    field.setAttribute("readonly", "");
    field.style.position = "fixed";
    field.style.left = "-9999px";
    field.style.top = "0";
    document.body.appendChild(field);
    var ok = false;
    try {
      field.select();
      field.setSelectionRange(0, field.value.length);
      ok = document.execCommand("copy");
    } catch (error) {
      ok = false;
    }
    document.body.removeChild(field);
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
     .width are written — both CSSOM mutations, which the strict CSP allows (the
     landing's scroll-scrubbed story relies on the same). */
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
    arriving.forEach(function (node) { watcher.observe(node); });
  } else {
    // No observer, or the reader asked for less motion: show them finished.
    arriving.forEach(function (node) { node.classList.add("is-in"); });
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
      var el = pxLayers[i];
      var rect = el.getBoundingClientRect();
      if (rect.bottom < -300 || rect.top > vh + 300) continue; // off-screen: skip
      var center = (rect.top + rect.height / 2 - vh / 2) / vh; // ~ -1 (below) .. 1 (above)
      var speed = parseFloat(el.getAttribute("data-px")) || 0;
      el.style.transform = "translate3d(0," + (center * speed * -60).toFixed(1) + "px,0)";
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
