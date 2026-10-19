// Bump SHELL_VERSION whenever any file listed in SHELL_ASSETS (or the /admin page itself) changes.
const SHELL_VERSION = "26";
const CACHE_PREFIX = "subbed-admin-shell-";
const CACHE_NAME = `${CACHE_PREFIX}${SHELL_VERSION}`;
const ENTRY_URL = "/admin";
const SHELL_ASSETS = [
    "/static/admin/admin.css",
    "/static/admin/app.js",
    "/static/admin/i18n.js",
    "/static/admin/lib.js",
    "/static/alpinejs@3.x.x.min.js",
    "/static/admin.manifest.json",
    "/static/admin/icon.svg",
    "/static/admin/icon-192.png",
    "/static/admin/icon-512.png",
    "/static/admin/icon-1024.png",
    "/static/admin/icon-maskable-512.png",
    "/static/admin/apple-touch-icon.png",
];

/**
 * Fetch a shell file bypassing the HTTP cache so a new version never mixes with stale files.
 * @param {string} url
 * @returns {Promise<Response>}
 */
async function fetchFresh(url) {
    const response = await fetch(new Request(url, { cache: "reload" }));
    if (!response.ok) throw new Error(`${url} -> ${response.status}`);
    return response;
}

self.addEventListener("install", (event) => {
    event.waitUntil(
        (async () => {
            const cache = await caches.open(CACHE_NAME);
            // the static assets are required; the whole install fails (and retries later) if any is missing
            await Promise.all(SHELL_ASSETS.map(async (url) => cache.put(url, await fetchFresh(url))));
            // /admin needs a session: signed out it redirects to the login page, which must not be cached as the entry.
            // If it can't be fetched now, it is cached on the first signed-in visit
            try {
                const entry = await fetchFresh(ENTRY_URL);
                if (entry.redirected) throw new Error(`${ENTRY_URL} redirected to ${entry.url}`);
                await cache.put(ENTRY_URL, entry);
            } catch (error) {
                console.warn("Admin entry page not precached", error);
            }
            // no skipWaiting here: the page asks for it so open tabs never run new JS against old assets
        })(),
    );
});

self.addEventListener("message", (event) => {
    if (event.data?.type === "SKIP_WAITING") self.skipWaiting();
});

self.addEventListener("activate", (event) => {
    event.waitUntil(
        (async () => {
            const names = await caches.keys();
            await Promise.all(
                names.filter((name) => name.startsWith(CACHE_PREFIX) && name !== CACHE_NAME).map((name) => caches.delete(name)),
            );
            await self.clients.claim();
        })(),
    );
});

self.addEventListener("fetch", (event) => {
    const { request } = event;
    if (request.method !== "GET") return;
    const url = new URL(request.url);
    if (url.origin !== self.location.origin || url.pathname.startsWith("/api/")) return;

    if (request.mode === "navigate" && (url.pathname === ENTRY_URL || url.pathname === `${ENTRY_URL}/`)) {
        event.respondWith(serveEntry(request));
    } else if (SHELL_ASSETS.includes(url.pathname)) {
        event.respondWith(serveAsset(request, url.pathname));
    }
});

/**
 * Cache-first entry page. The browser keeps the URL it asked for (query string included).
 * @param {Request} request
 * @returns {Promise<Response>}
 */
async function serveEntry(request) {
    const cache = await caches.open(CACHE_NAME);
    const cached = await cache.match(ENTRY_URL);
    if (cached) return cached;
    const response = await fetch(request);
    // a redirect means the session is gone and this is the login page
    if (response.ok && !response.redirected) await cache.put(ENTRY_URL, response.clone());
    return response;
}

/**
 * @param {Request} request
 * @param {string} pathname
 * @returns {Promise<Response>}
 */
async function serveAsset(request, pathname) {
    const cache = await caches.open(CACHE_NAME);
    return (await cache.match(pathname)) ?? fetch(request);
}
