/* The download matrix, on both pages.

   The tutorial the server serves asks that instance which client files it has published.
   This site has no instance, so it asks GitHub for the releases the pipelines publish:
   installer-release.yml on a pkgcache-v* tag, and main-artifacts.yml on every merge to
   main. Those are the two channels shown, and they are the only two that exist.

   Progressive enhancement, strictly. The mount already contains a link to the releases
   page, and that markup is only put away once a real answer has arrived — a failed
   fetch, a rate limit or a reader with scripting off all land on it rather than on an
   empty panel. It is a link to the index rather than eight direct
   /releases/latest/download/ links because those resolve only once a release exists,
   and a page that promises a file and returns 404 is worse than one that points. */
(function () {
  "use strict";

  var REPO = "aabdlwahab/PKGCache";
  var RELEASES_API = "https://api.github.com/repos/" + REPO + "/releases?per_page=12";
  var RELEASES_PAGE = "https://github.com/" + REPO + "/releases";
  var ROLLING_TAG = "main-latest";
  var CACHE_KEY = "pkgcache_releases_v1";
  var CACHE_MS = 10 * 60 * 1000;

  var mounts = [].slice.call(document.querySelectorAll('[data-role="downloads"]'));
  if (!mounts.length || !window.fetch) return;

  /* ---- what each file is ------------------------------------------------ */
  /* Ordered, but not order-dependent: the alternations are exact, so
     pkgcache-app-linux-amd64 cannot fall into the pkgcache group by arriving first. */
  var GROUPS = [
    {
      id: "installers",
      title: "Installers",
      blurb: "What most people want. The .deb pair installs the daemon and the desktop " +
        "half together, and the .pkg carries a universal binary.",
      test: function (name) { return /(\.deb|\.pkg|-setup\.exe)$/.test(name); },
    },
    {
      id: "pkgcache",
      title: "pkgcache",
      blurb: "The cache and the CLI: one static binary, no dependencies. This is what " +
        "install.sh fetches and what a CI runner wants.",
      test: function (name) { return /^pkgcache-(linux|darwin|windows)-/.test(name); },
    },
    {
      id: "app",
      title: "Desktop app",
      blurb: "The window and the status-bar item. Already inside the three installers — " +
        "take it on its own only if you install by hand.",
      test: function (name) { return /^pkgcache-app-/.test(name); },
    },
    {
      id: "shim",
      title: "Docker shim",
      blurb: "pkgcache-docker: drives docker with every build pointed at the cache. " +
        "Needed on its own for crate prepare --runtime pkgcache-docker.",
      test: function (name) { return /^pkgcache-docker-/.test(name); },
    },
    {
      id: "bridge",
      title: "Bridge",
      blurb: "Reaches a team cache over a verified loopback bridge, so nothing has to " +
        "install a CA on the machine. The unprivileged way onto a pkgreg server.",
      test: function (name) { return /^pkgreg-bridge-/.test(name); },
    },
    {
      id: "server",
      title: "pkgreg server",
      blurb: "The other half of the product: the cache other machines point at. One " +
        "static binary, console and all six ecosystems inside it.",
      test: function (name) { return /^pkgreg-(linux|darwin|windows)-/.test(name); },
    },
    {
      id: "scripts",
      title: "Install scripts",
      blurb: "install.sh for Linux and macOS, install.ps1 for Windows. Both take " +
        "--server and a CA fingerprint to set the machine up as they install.",
      test: function (name) { return /^install\.(sh|ps1)$/.test(name); },
    },
    {
      id: "sums",
      title: "Checksums",
      blurb: "Verify before you run. SHA256SUMS covers the whole release; the per-tool " +
        "files are the format pkgreg publish-client reads.",
      test: function (name) { return /SHA256SUMS$/.test(name); },
    },
  ];

  var OS_NAMES = { linux: "Linux", darwin: "macOS", windows: "Windows" };

  /* The platform a file is for, read from its own name. Three grammars, because the
     packaging tools each impose one: dpkg wants name_version_arch.deb, pkgbuild and
     NSIS put the version in the middle, and our own binaries are name-os-arch. */
  function platformOf(name) {
    var deb = /_(amd64|arm64)\.deb$/.exec(name);
    if (deb) return { os: "linux", arch: deb[1], label: "Linux " + deb[1] };
    if (/\.pkg$/.test(name)) return { os: "darwin", arch: "", label: "macOS universal" };
    if (/-setup\.exe$/.test(name)) return { os: "windows", arch: "amd64", label: "Windows amd64" };
    var bare = /-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$/.exec(name);
    if (bare) return { os: bare[1], arch: bare[2], label: OS_NAMES[bare[1]] + " " + bare[2] };
    if (/^install\.ps1$/.test(name)) return { os: "windows", arch: "", label: "Windows" };
    if (/^install\.sh$/.test(name)) return { os: "", arch: "", label: "Linux and macOS" };
    return { os: "", arch: "", label: "any" };
  }

  /* Which OS the reader is on. Only the OS: architecture is not reliably readable
     without the async high-entropy client-hints dance, and a wrong arch marked
     "yours" is worse than no mark at all. */
  function readerOS() {
    var hint = navigator.userAgentData && navigator.userAgentData.platform;
    var value = String(hint || navigator.platform || navigator.userAgent || "").toLowerCase();
    if (value.indexOf("win") !== -1) return "windows";
    if (value.indexOf("mac") !== -1 || value.indexOf("darwin") !== -1) return "darwin";
    if (value.indexOf("linux") !== -1 || value.indexOf("android") !== -1) return "linux";
    return "";
  }
  var mine = readerOS();

  function fileSize(bytes) {
    var value = Number(bytes || 0);
    if (value >= 1048576) return (value / 1048576).toFixed(1) + " MB";
    if (value >= 1024) return Math.round(value / 1024) + " KB";
    return value + " B";
  }

  function element(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  /* ---- rendering -------------------------------------------------------- */

  function renderGroup(group, assets) {
    var section = element("div", "dl-group");
    section.appendChild(element("h4", null, group.title));
    section.appendChild(element("p", null, group.blurb));

    var table = element("table", "dl-table");
    var head = element("thead");
    var headRow = element("tr");
    ["File", "For", "Size"].forEach(function (label, index) {
      var cell = element("th", index === 2 ? "dl-bytes" : null, label);
      headRow.appendChild(cell);
    });
    head.appendChild(headRow);
    table.appendChild(head);

    var body = element("tbody");
    assets.forEach(function (asset) {
      var platform = platformOf(asset.name);
      var row = element("tr");
      if (platform.os && platform.os === mine) row.className = "is-yours";

      var file = element("td", "dl-file");
      var link = element("a", null, asset.name);
      link.href = asset.browser_download_url;
      link.setAttribute("download", asset.name);
      file.appendChild(link);
      row.appendChild(file);

      row.appendChild(element("td", "dl-plat", platform.label));
      row.appendChild(element("td", "dl-bytes", fileSize(asset.size)));
      body.appendChild(row);
    });
    table.appendChild(body);
    section.appendChild(table);
    return section;
  }

  function renderChannel(release) {
    var wrap = element("div");

    var note = element("p", "note");
    if (release.prerelease) {
      note.appendChild(document.createTextNode(
        "Built from main. Verify against "));
    } else {
      note.appendChild(document.createTextNode(
        "A fixed version. Verify what you download against "));
    }
    note.appendChild(element("code", null, "SHA256SUMS"));
    note.appendChild(document.createTextNode(" with "));
    note.appendChild(element("code", null, "sha256sum -c SHA256SUMS"));
    note.appendChild(document.createTextNode(", or "));
    note.appendChild(element("code", null, "shasum -a 256 -c"));
    note.appendChild(document.createTextNode(" on macOS. Released "));
    note.appendChild(element("code", null, (release.published_at || "").slice(0, 10)));
    note.appendChild(document.createTextNode("."));
    wrap.appendChild(note);

    var assets = release.assets || [];
    var taken = {};
    GROUPS.forEach(function (group) {
      var rows = assets.filter(function (asset) {
        if (taken[asset.name]) return false;
        if (!group.test(asset.name)) return false;
        taken[asset.name] = true;
        return true;
      });
      if (rows.length) wrap.appendChild(renderGroup(group, rows));
    });

    /* Anything the grammar above does not know about still gets a row. A release that
       grows a file this file has never heard of should show it, not hide it. */
    var rest = assets.filter(function (asset) { return !taken[asset.name]; });
    if (rest.length) {
      wrap.appendChild(renderGroup(
        { title: "Also in this release", blurb: "" }, rest));
    }
    return wrap;
  }

  function renderMount(mount, releases) {
    var stable = null;
    var rolling = null;
    releases.forEach(function (release) {
      if (release.draft) return;
      if (!release.prerelease && !stable) stable = release;
      if (release.tag_name === ROLLING_TAG && !rolling) rolling = release;
    });

    var channels = [];
    if (stable) {
      channels.push({ id: "stable", label: stable.tag_name, tag: "tagged", release: stable });
    }
    if (rolling) {
      channels.push({ id: "rolling", label: "Latest from main", tag: "newest", release: rolling });
    }
    if (!channels.length) return null;

    var body = element("div");
    var tabs = null;

    function show(index) {
      body.textContent = "";
      body.appendChild(renderChannel(channels[index].release));
    }

    if (channels.length > 1) {
      tabs = element("div", "dl-channels");
      var group = "dl-channel-" + Math.random().toString(36).slice(2, 8);
      channels.forEach(function (channel, index) {
        var label = element("label", "dl-channel-tab" + (index === 0 ? " is-on" : ""));
        var input = document.createElement("input");
        input.type = "radio";
        input.name = group;
        input.value = channel.id;
        if (index === 0) input.checked = true;
        input.addEventListener("change", function () {
          if (!input.checked) return;
          [].forEach.call(tabs.querySelectorAll(".dl-channel-tab"), function (tab) {
            tab.classList.remove("is-on");
          });
          label.classList.add("is-on");
          show(index);
        });
        label.appendChild(input);
        label.appendChild(element("b", null, channel.label));
        label.appendChild(element("span", "dl-tag", channel.tag));
        tabs.appendChild(label);
      });
    }

    show(0);
    return { tabs: tabs, body: body, count: (channels[0].release.assets || []).length };
  }

  function setState(text, className) {
    [].forEach.call(document.querySelectorAll('[data-role="downloads-state"]'), function (node) {
      node.textContent = text;
      var live = node.parentNode;
      if (!live) return;
      live.classList.remove("ready", "empty");
      if (className) live.classList.add(className);
    });
  }

  /* ---- the fetch, cached for the visit ---------------------------------- */
  /* Unauthenticated GitHub allows 60 requests an hour per address. A reader who opens
     the landing page and then the tutorial should cost one, so the answer is kept in
     sessionStorage — long enough to cross the site, short enough that a release
     published while somebody reads is not hidden from them for the day. */
  function cached() {
    try {
      var raw = sessionStorage.getItem(CACHE_KEY);
      if (!raw) return null;
      var held = JSON.parse(raw);
      if (!held || Date.now() - held.at > CACHE_MS) return null;
      return held.releases;
    } catch (error) {
      return null;
    }
  }

  function remember(releases) {
    try {
      sessionStorage.setItem(CACHE_KEY, JSON.stringify({ at: Date.now(), releases: releases }));
    } catch (error) {
      /* Storage is full or blocked: one more request next page is not a problem. */
    }
  }

  function paint(releases) {
    if (!releases.length) {
      /* True on the day this site first goes up: the pipelines publish, but nothing has
         been published yet. Say that, rather than showing an empty table and letting the
         reader wonder which of us is broken. */
      setState("nothing published yet", "empty");
      return;
    }
    var painted = 0;
    mounts.forEach(function (mount) {
      var rendered = renderMount(mount, releases);
      if (!rendered) return;
      var fallback = mount.querySelector('[data-role="downloads-static"]');
      if (fallback) fallback.hidden = true;
      if (rendered.tabs) mount.appendChild(rendered.tabs);
      mount.appendChild(rendered.body);
      painted = rendered.count;
    });
    if (painted) setState(painted + " files", "ready");
  }

  setState("checking GitHub…", null);

  var held = cached();
  if (held) {
    paint(held);
    return;
  }

  fetch(RELEASES_API, { headers: { Accept: "application/vnd.github+json" } })
    .then(function (response) { return response.ok ? response.json() : null; })
    .then(function (releases) {
      if (!releases || !releases.length) {
        setState(releases ? "nothing published yet" : "GitHub did not answer", "empty");
        return;
      }
      remember(releases);
      paint(releases);
    })
    .catch(function () {
      /* Offline, rate-limited, or a browser blocking the request. The static table is
         still on the page; all that is missing is the version-stamped installers. */
      setState("showing fixed links — GitHub is not reachable", "empty");
    });

  /* Exposed for the "open the releases page" links to stay in one place. */
  [].forEach.call(document.querySelectorAll('[data-role="releases-link"]'), function (link) {
    link.href = RELEASES_PAGE;
  });
})();
