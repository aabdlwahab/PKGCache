/* Rewrite the example address in the landing page and the tutorial to the address
 * this instance actually answers on.
 *
 * Both pages are written against cache.example.com:8443 so they read sensibly as
 * documentation. But they are served BY the cache, which knows its own host and ports
 * — so leaving the reader to translate two values by hand is a choice, and the port is
 * the one people get wrong. The apt proxy makes it worse: it is a different port
 * unless single_port is on, and no browser can work that out on its own.
 *
 * Source of truth, in order:
 *   1. GET /api/v1/coordinates — the server's own view, including the proxy port and
 *      any configured public origin. Authoritative.
 *   2. window.location — right for the common single-port case. Used when the request
 *      above fails or is slow.
 *   3. the literal example — for a file:// preview, where neither exists.
 *
 * Substitution happens exactly once. An earlier draft substituted from location
 * immediately and then again from the server, which cannot work: the first pass
 * consumes the placeholders, and in single-port mode the unified and proxy addresses
 * are identical, so a second pass could not tell them apart to split them again.
 *
 * Shared by both pages so the two cannot drift.
 */
(function () {
  "use strict";

  var EXAMPLE = {
    host: "cache.example.com",
    unified: "cache.example.com:8443",
    proxy: "cache.example.com:3142",
    scheme: "https",
  };

  /* The landing page writes a shorter placeholder than the tutorial — "cache:8443"
     rather than a full domain. Only the authority forms are ever substituted: the word
     "cache" on its own is the subject of every sentence on that page and must never be
     touched. */
  var SHORT_UNIFIED = "cache:8443";
  var SHORT_PROXY = "cache:3142";

  /* How long to wait for the server before falling back to location. Same-origin, a
     couple of hundred bytes: if this ever expires, the page had bigger problems. */
  var BUDGET_MS = 1200;

  /* What location alone can tell us. In single-port mode — the default — one address
     carries the console, every ecosystem and the apt proxy, so this is already the
     whole answer. */
  function fromLocation() {
    var host = window.location.hostname;
    if (!host) return null;
    if (host.indexOf(":") !== -1 && host.charAt(0) !== "[") {
      host = "[" + host + "]"; // registry- and URL-safe IPv6 authority
    }
    var authority = window.location.host || host;
    return {
      host: host,
      unified: authority,
      proxy: authority,
      scheme: window.location.protocol === "https:" ? "https" : "http",
      // Unknown from the browser alone: leave the markup's default wording alone.
      singlePort: null,
    };
  }

  function fromServer(data) {
    if (!data || !data.unified || !data.host) return null;
    return {
      host: data.host,
      unified: data.unified,
      proxy: data.proxy || data.unified,
      scheme: data.scheme === "https" ? "https" : "http",
      singlePort: data.single_port === true,
    };
  }

  /* Port alone, for prose that names a port without a host. Reads back from the last
     colon, which is correct for a bracketed IPv6 authority too. */
  function portOf(authority) {
    var at = authority.lastIndexOf(":");
    return at === -1 ? "" : authority.slice(at + 1);
  }

  /* Applied in order, longest match first: an earlier rule consumes the text a later
     one would otherwise match again. */
  function rules(c) {
    return [
      // A scheme-qualified link follows the server's real scheme, so a cache running
      // without TLS stops advertising an https it cannot serve.
      ["https://" + EXAMPLE.unified, c.scheme + "://" + c.unified],
      // Preserve an explicitly plain-HTTP example and change only its address.
      ["http://" + EXAMPLE.unified, "http://" + c.unified],
      [EXAMPLE.proxy, c.proxy],
      [EXAMPLE.unified, c.unified],
      [EXAMPLE.host, c.host],

      // The landing page's short form. Scheme-qualified first, for the same reason.
      ["https://" + SHORT_UNIFIED, c.scheme + "://" + c.unified],
      ["http://" + SHORT_PROXY, "http://" + c.proxy],
      [SHORT_PROXY, c.proxy],
      [SHORT_UNIFIED, c.unified],

      // Bare ports, last. Prose can mention a listener without repeating the host,
      // so its port still has to be corrected. Safe only in this position: every
      // host-qualified form above has already been consumed, so nothing that reaches
      // here is part of an address that was already rewritten.
      [":" + portOf(EXAMPLE.proxy), ":" + portOf(c.proxy)],
      [":" + portOf(EXAMPLE.unified), ":" + portOf(c.unified)],
    ];
  }

  function apply(c) {
    if (!c) return;
    var pairs = rules(c);
    var swap = function (value) {
      pairs.forEach(function (pair) {
        value = value.split(pair[0]).join(pair[1]);
      });
      return value;
    };

    /* Skip a node only when no rule could possibly match it. Derived from the rules
       themselves rather than hand-maintained: an earlier version filtered on the
       string "cache", which silently skipped every bare-port reference. */
    var interesting = function (value) {
      for (var i = 0; i < pairs.length; i++) {
        if (value.indexOf(pairs[i][0]) !== -1) return true;
      }
      return false;
    };

    /* One sentence on the landing page depends on the listener layout rather than on
       any address: with single_port on — the default — the apt proxy is not a separate
       port at all, so claiming it is would contradict the very commands beside it.
       Both wordings live in the markup; this picks one. */
    if (typeof c.singlePort === "boolean") {
      [].forEach.call(document.querySelectorAll("[data-ports]"), function (el) {
        el.hidden = (el.getAttribute("data-ports") === "single") !== c.singlePort;
      });
    }

    var walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    var nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    nodes.forEach(function (node) {
      if (!interesting(node.nodeValue)) return;
      node.nodeValue = swap(node.nodeValue);
    });

    // Links carry the address in an attribute, where no text walker will find it.
    [].forEach.call(document.querySelectorAll("a[href]"), function (link) {
      var href = link.getAttribute("href");
      if (href && interesting(href)) link.setAttribute("href", swap(href));
    });
  }

  /* Start the request during head parsing, so by the time the document is ready it has
     almost always already arrived. */
  var asked = null;
  if (window.fetch && window.location.origin && window.location.origin !== "null") {
    asked = new Promise(function (resolve) {
      var done = false;
      var finish = function (value) {
        if (!done) {
          done = true;
          resolve(value);
        }
      };
      setTimeout(function () { finish(null); }, BUDGET_MS);
      fetch("/api/v1/coordinates", { credentials: "same-origin" })
        .then(function (response) { return response.ok ? response.json() : null; })
        .then(finish)
        .catch(function () { finish(null); });
    });
  }

  /* The same response carries the CA fingerprint, which the tutorial needs to fill
     into its start command. Publishing the promise rather than refetching keeps that to
     one request and makes this the only place either page asks the server where it is
     and what it presents. Resolves to the raw payload, or null when the request failed
     or the page is a file:// preview. */
  window.pkgregCoordinates = asked || Promise.resolve(null);

  function run() {
    if (!asked) {
      apply(fromLocation());
      return;
    }
    asked.then(function (data) {
      apply(fromServer(data) || fromLocation());
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", run);
  } else {
    run();
  }
})();
