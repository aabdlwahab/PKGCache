/* Entry point: authenticate, load, mount, subscribe. */

import { APIError, api } from "./api.js";
import { buildChrome, renderLogin, visibleNav } from "./chrome.js";
import * as store from "./store.js";
import { define, restrict, start } from "./router.js";

import overview from "./views/overview.js";
import cache from "./views/cache.js";
import connect from "./views/connect.js";
import sources from "./views/sources.js";
import transfer from "./views/transfer.js";
import admin from "./views/admin.js";

const root = document.getElementById("root");

define("overview", overview);
define("cache", cache);
define("connect", connect);
define("sources", sources);
define("transfer", transfer);
define("admin", admin);

async function boot() {
  document.documentElement.dataset.theme = store.state.theme;

  let me;
  try {
    me = await api.me();
  } catch (cause) {
    // 401 is the ordinary unauthenticated case, not an error worth a stack trace.
    // Its body carries whether this instance offers a guest session, which is the
    // only way the sign-in screen can know: every endpoint that could answer sits
    // behind the check that just refused us.
    if (cause instanceof APIError && cause.status === 401) {
      return renderLogin(root, undefined, {
        guestAvailable: cause.detail?.guest_available === true,
      });
    }
    return renderLogin(root, cause.message, {
      guestAvailable: cause.detail?.guest_available === true,
    });
  }

  // Order matters here, and getting it wrong is not a subtle failure.
  //
  //   1. Build the chrome, which registers every subscriber.
  //   2. Set `me` — after (1), or the identity subscriber has nobody to notify and the
  //      sign-out button never appears.
  //   3. Start the router, so a mount point exists. Loading first would let the store
  //      wake the project subscriber, which remounts the current view, before there is
  //      anywhere to mount it.
  //   4. Only then load. Views mount against empty state and fill in as data lands;
  //      that is what the region model is for.
  const { main } = buildChrome(root);
  store.set({ me });
  // After `me`, before start(): the router has to know which views this session may
  // enter before it renders the first one, or a guest deep-linked to #/admin mounts
  // it for a frame.
  const allowed = new Set(visibleNav().map(([name]) => name));
  restrict((name) => allowed.has(name), () => "overview");
  start(main);

  // The bus drives everything after this point; there is no polling loop.
  const disconnect = store.connectEvents();
  addEventListener("beforeunload", disconnect);

  try {
    await store.loadInstance();
    await store.loadProject();
  } catch (cause) {
    store.fail(cause);
  }
}

void boot();
