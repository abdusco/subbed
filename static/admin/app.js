import { resolveLanguage, translate } from "./i18n.js";
import {
    languageFromName,
    matchesQuery,
    normalizeLanguage,
    normalizeVideoUrl,
    sniffSubtitleText,
    subtitleTypeFromName,
    thumbnailUrl,
    videoIdFromUrl,
} from "./lib.js";

/** @typedef {import("./lib.js").Video} Video */
/** @typedef {import("./lib.js").Subtitle} Subtitle */
/** @typedef {{ id: number, kind: "success" | "error", text: string }} Toast */
/** @typedef {"system" | "light" | "dark"} ThemePreference */

const STORAGE_KEYS = {
    theme: "subbed-admin-theme",
    language: "subbed-admin-language",
    lastSubtitleLanguage: "subbed-admin-last-subtitle-language",
};
const REFRESH_MIN_INTERVAL_MS = 5000;
const TOAST_DURATION_MS = { success: 3500, error: 6000 };
const SERVICE_WORKER_URL = "/admin-sw.js";
const SERVICE_WORKER_SCOPE = "/admin";

/**
 * localStorage that never throws (private windows, blocked storage).
 */
const storage = {
    /**
     * @param {string} key
     * @returns {string | null}
     */
    get(key) {
        try {
            return localStorage.getItem(key);
        } catch {
            return null;
        }
    },
    /**
     * @param {string} key
     * @param {string | null} value null removes the key
     */
    set(key, value) {
        try {
            if (value === null) localStorage.removeItem(key);
            else localStorage.setItem(key, value);
        } catch {
            // preferences simply won't persist
        }
    },
};

/**
 * Apply the theme to <html> and keep the browser UI color in sync with the page background.
 * @param {ThemePreference} theme
 */
function applyTheme(theme) {
    const root = document.documentElement;
    if (theme === "system") delete root.dataset.theme;
    else root.dataset.theme = theme;
    requestAnimationFrame(() => {
        const [first, ...rest] = document.querySelectorAll('meta[name="theme-color"]');
        rest.forEach((meta) => meta.remove());
        first?.removeAttribute("media");
        first?.setAttribute("content", getComputedStyle(document.body).backgroundColor);
    });
}

/**
 * True when the focused element takes typed text, so a paste there should stay a normal paste.
 * @param {EventTarget | null} target
 * @returns {boolean}
 */
function isTextEntry(target) {
    return target instanceof HTMLElement && (target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName));
}

document.addEventListener("alpine:init", () => {
    // @ts-ignore Alpine is a global from the vendored script
    Alpine.data("admin", () => ({
        /** @type {Video[]} */
        videos: [],
        loading: true,
        loadFailed: false,
        loadError: "",
        query: "",
        lastLoadedAt: 0,
        /** @type {Promise<void> | null} */
        loadInFlight: null,
        refreshTimer: 0,

        /** @type {Toast[]} */
        toasts: [],
        toastSequence: 0,

        themePreference: /** @type {ThemePreference} */ (storage.get(STORAGE_KEYS.theme) ?? "system"),
        languagePreference: storage.get(STORAGE_KEYS.language) ?? "auto",

        online: navigator.onLine,
        installPrompt: /** @type {any} */ (null),
        installed: matchMedia("(display-mode: standalone)").matches,
        updateReady: false,
        /** @type {ServiceWorker | null} */
        waitingWorker: null,

        pageDragging: false,
        dragDepth: 0,

        videoForm: { url: "", title: "", busy: false },
        subtitleForm: {
            videoId: "",
            language: "",
            /** @type {File | null} */
            file: null,
            type: /** @type {"srt" | "vtt"} */ ("srt"),
            pasted: false,
            busy: false,
            dragging: false,
        },
        confirmation: { kind: /** @type {"video" | "subtitle"} */ ("video"), id: 0, label: "", busy: false },

        // ---- lifecycle ----

        init() {
            if (!["system", "light", "dark"].includes(this.themePreference)) this.themePreference = "system";
            applyTheme(this.themePreference);
            matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => applyTheme(this.themePreference));
            matchMedia("(display-mode: standalone)").addEventListener("change", (event) => (this.installed = event.matches));
            this.$watch("lang", (language) => (document.documentElement.lang = language));
            document.documentElement.lang = this.lang;

            window.addEventListener("online", () => {
                this.online = true;
                this.scheduleRefresh({ force: true });
            });
            window.addEventListener("offline", () => (this.online = false));
            document.addEventListener("visibilitychange", () => document.visibilityState === "visible" && this.scheduleRefresh());
            window.addEventListener("focus", () => this.scheduleRefresh());
            window.addEventListener("pageshow", (event) => event.persisted && this.scheduleRefresh());

            window.addEventListener("dragenter", (event) => this.onDragEnter(event));
            window.addEventListener("dragover", (event) => this.hasFiles(event) && event.preventDefault());
            window.addEventListener("dragleave", (event) => this.onDragLeave(event));
            window.addEventListener("drop", (event) => this.onDrop(event));
            window.addEventListener("paste", (event) => this.onPaste(event));

            window.addEventListener("beforeinstallprompt", (event) => {
                event.preventDefault();
                this.installPrompt = event;
            });
            window.addEventListener("appinstalled", () => {
                this.installPrompt = null;
                this.installed = true;
            });

            this.registerServiceWorker();
            this.loadVideos();
        },

        /**
         * Register the service worker and surface a "new version" prompt instead of swapping assets under open pages.
         */
        async registerServiceWorker() {
            if (!("serviceWorker" in navigator)) return;
            try {
                const registration = await navigator.serviceWorker.register(SERVICE_WORKER_URL, {
                    scope: SERVICE_WORKER_SCOPE,
                    updateViaCache: "none",
                });
                const watch = (/** @type {ServiceWorker} */ worker) => {
                    worker.addEventListener("statechange", () => {
                        if (worker.state === "installed" && navigator.serviceWorker.controller) this.offerUpdate(worker);
                    });
                };
                if (registration.waiting && navigator.serviceWorker.controller) this.offerUpdate(registration.waiting);
                if (registration.installing) watch(registration.installing);
                registration.addEventListener("updatefound", () => registration.installing && watch(registration.installing));
                let reloading = false;
                navigator.serviceWorker.addEventListener("controllerchange", () => {
                    if (reloading || !this.waitingWorker) return;
                    reloading = true;
                    location.reload();
                });
            } catch (error) {
                console.warn("Service worker registration failed", error);
            }
        },

        /** @param {ServiceWorker} worker */
        offerUpdate(worker) {
            this.waitingWorker = worker;
            this.updateReady = true;
        },

        applyUpdate() {
            this.waitingWorker?.postMessage({ type: "SKIP_WAITING" });
        },

        // ---- i18n, theme, install ----

        get lang() {
            return resolveLanguage(this.languagePreference, navigator.languages ?? [navigator.language]);
        },

        /**
         * @param {string} key
         * @param {Record<string, string | number>} [params]
         * @returns {string}
         */
        t(key, params) {
            return translate(this.lang, key, params);
        },

        /** @param {string} value "auto", "en" or "tr" */
        setLanguage(value) {
            this.languagePreference = value;
            storage.set(STORAGE_KEYS.language, value === "auto" ? null : value);
        },

        /** @param {ThemePreference} value */
        setTheme(value) {
            this.themePreference = value;
            storage.set(STORAGE_KEYS.theme, value === "system" ? null : value);
            applyTheme(value);
        },

        async install() {
            const prompt = this.installPrompt;
            if (!prompt) return;
            this.installPrompt = null;
            await prompt.prompt();
        },

        // ---- library ----

        get filteredVideos() {
            return this.videos.filter((video) => matchesQuery(video, this.query));
        },

        get subtitleCount() {
            return this.videos.reduce((total, video) => total + video.subtitles.length, 0);
        },

        /**
         * Call the admin API. Network failures and non-2xx answers become Errors with a translated message.
         * @param {string} path
         * @param {RequestInit} [options]
         * @returns {Promise<any>}
         */
        async api(path, options) {
            let response;
            try {
                response = await fetch(path, options);
            } catch {
                throw new Error(this.t("error.network"));
            }
            if (!response.ok) {
                const detail = (await response.text().catch(() => "")).trim();
                throw new Error(
                    detail && detail.length < 200 && !detail.startsWith("<") ? detail : this.t("error.http", { status: response.status }),
                );
            }
            return response.json();
        },

        loadVideos() {
            this.loadInFlight ??= (async () => {
                try {
                    this.videos = await this.api("/api/admin/videos");
                    this.loadFailed = false;
                    this.lastLoadedAt = Date.now();
                } catch (error) {
                    this.loadFailed = true;
                    this.loadError = error.message;
                    if (this.videos.length) this.toast("error", error.message);
                } finally {
                    this.loading = false;
                    this.loadInFlight = null;
                }
            })();
            return this.loadInFlight;
        },

        /**
         * Coalesce the visibility, focus, pageshow and online handlers into at most one reload.
         * @param {{ force?: boolean }} [options] force skips the minimum interval
         */
        scheduleRefresh({ force = false } = {}) {
            clearTimeout(this.refreshTimer);
            this.refreshTimer = window.setTimeout(() => {
                if (force || Date.now() - this.lastLoadedAt > REFRESH_MIN_INTERVAL_MS) this.loadVideos();
            }, 150);
        },

        /**
         * @param {Video} video
         * @returns {string | null}
         */
        thumbnail(video) {
            const id = videoIdFromUrl(video.original_url);
            return id ? thumbnailUrl(id) : null;
        },

        /**
         * @param {Video} video
         * @returns {string} link to the player page for this video
         */
        playerLink(video) {
            return `/${video.original_url}`;
        },

        // ---- toasts ----

        /**
         * @param {"success" | "error"} kind
         * @param {string} text
         */
        toast(kind, text) {
            const id = ++this.toastSequence;
            this.toasts.push({ id, kind, text });
            setTimeout(() => this.dismissToast(id), TOAST_DURATION_MS[kind]);
        },

        /** @param {number} id */
        dismissToast(id) {
            this.toasts = this.toasts.filter((toast) => toast.id !== id);
        },

        // ---- add video ----

        get videoUrlId() {
            return videoIdFromUrl(this.videoForm.url);
        },

        get videoUrlInvalid() {
            return this.videoForm.url.trim() !== "" && !this.videoUrlId;
        },

        get videoPreview() {
            return this.videoUrlId ? thumbnailUrl(this.videoUrlId) : null;
        },

        openAddVideo() {
            this.$refs.addDialog.showModal();
        },

        async submitVideo() {
            const url = normalizeVideoUrl(this.videoForm.url);
            if (!url) return;
            this.videoForm.busy = true;
            try {
                await this.api("/api/admin/videos", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ url, title: this.videoForm.title.trim() }),
                });
                this.videoForm.url = "";
                this.videoForm.title = "";
                this.$refs.addDialog.close();
                this.toast("success", this.t("video.added"));
                await this.loadVideos();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.videoForm.busy = false;
            }
        },

        // ---- upload subtitle ----

        get languageInvalid() {
            return this.subtitleForm.language.trim() !== "" && !normalizeLanguage(this.subtitleForm.language);
        },

        get canUpload() {
            return Boolean(
                this.subtitleForm.file &&
                this.subtitleForm.videoId &&
                normalizeLanguage(this.subtitleForm.language) &&
                !this.subtitleForm.busy,
            );
        },

        /**
         * Open the upload dialog, optionally preselecting a video and/or a file.
         * @param {{ videoId?: number | string, file?: File | null }} [options]
         */
        openUpload({ videoId, file = null } = {}) {
            if (!this.videos.length) {
                this.toast("error", this.t("subtitle.needVideo"));
                return;
            }
            if (videoId !== undefined) this.subtitleForm.videoId = String(videoId);
            else if (!this.subtitleForm.videoId && this.videos.length === 1) this.subtitleForm.videoId = String(this.videos[0].id);
            if (file) this.setSubtitleFile(file);
            if (!this.subtitleForm.language) this.subtitleForm.language = storage.get(STORAGE_KEYS.lastSubtitleLanguage) ?? "";
            if (!this.$refs.uploadDialog.open) this.$refs.uploadDialog.showModal();
        },

        /**
         * Accept a File (picked, dropped or pasted) if it is .srt or .vtt; guess the language from its name.
         * @param {File} file
         * @returns {boolean} whether the file was accepted
         */
        setSubtitleFile(file) {
            const type = subtitleTypeFromName(file.name);
            if (!type) {
                this.toast("error", this.t("subtitle.badFile"));
                return false;
            }
            this.subtitleForm.file = file;
            this.subtitleForm.type = type;
            this.subtitleForm.pasted = file.name.startsWith("pasted.");
            const guessed = languageFromName(file.name);
            if (guessed) this.subtitleForm.language = guessed;
            return true;
        },

        /** @param {Event} event */
        onFilePicked(event) {
            const input = /** @type {HTMLInputElement} */ (event.target);
            if (input.files?.[0]) this.setSubtitleFile(input.files[0]);
            input.value = "";
        },

        clearSubtitleFile() {
            this.subtitleForm.file = null;
            this.subtitleForm.pasted = false;
        },

        /** @param {DragEvent} event */
        onZoneDrop(event) {
            // the zone stops the event, so the window-level drop handler never resets the page overlay
            this.dragDepth = 0;
            this.pageDragging = false;
            this.subtitleForm.dragging = false;
            const file = event.dataTransfer?.files[0];
            if (file) this.setSubtitleFile(file);
        },

        async submitSubtitle() {
            const language = normalizeLanguage(this.subtitleForm.language);
            if (!this.canUpload || !language) return;
            const form = new FormData();
            form.append("video_id", this.subtitleForm.videoId);
            form.append("language", language);
            form.append("type", this.subtitleForm.type);
            form.append("file", /** @type {File} */ (this.subtitleForm.file));
            this.subtitleForm.busy = true;
            try {
                await this.api("/api/admin/subtitles", { method: "POST", body: form });
                storage.set(STORAGE_KEYS.lastSubtitleLanguage, language);
                this.subtitleForm.file = null;
                this.subtitleForm.pasted = false;
                this.subtitleForm.language = language;
                this.$refs.uploadDialog.close();
                this.toast("success", this.t("subtitle.uploaded"));
                await this.loadVideos();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.subtitleForm.busy = false;
            }
        },

        // ---- drop and paste anywhere ----

        /**
         * @param {DragEvent | ClipboardEvent} event
         * @returns {boolean}
         */
        hasFiles(event) {
            const data = "dataTransfer" in event ? event.dataTransfer : event.clipboardData;
            return Boolean(data && Array.from(data.types).includes("Files"));
        },

        /** @param {DragEvent} event */
        onDragEnter(event) {
            if (!this.hasFiles(event)) return;
            event.preventDefault();
            this.dragDepth++;
            this.pageDragging = true;
        },

        /** @param {DragEvent} event */
        onDragLeave(event) {
            if (!this.hasFiles(event)) return;
            // dragenter/dragleave fire for every child element; only the last leave means the pointer left the window
            this.dragDepth = Math.max(0, this.dragDepth - 1);
            if (this.dragDepth === 0) this.pageDragging = false;
        },

        /** @param {DragEvent} event */
        onDrop(event) {
            this.dragDepth = 0;
            this.pageDragging = false;
            if (!this.hasFiles(event)) return;
            event.preventDefault();
            const file = event.dataTransfer?.files[0];
            if (file) this.receiveFile(file);
        },

        /** @param {ClipboardEvent} event */
        onPaste(event) {
            const file = event.clipboardData?.files[0];
            if (file) {
                event.preventDefault();
                this.receiveFile(file);
                return;
            }
            if (isTextEntry(event.target)) return;
            const text = event.clipboardData?.getData("text/plain") ?? "";
            const type = sniffSubtitleText(text);
            if (!type) return;
            event.preventDefault();
            this.receiveFile(new File([text], `pasted.${type}`, { type: "text/plain" }));
        },

        /**
         * Entry point for a subtitle file dropped or pasted anywhere on the page.
         * @param {File} file
         */
        receiveFile(file) {
            this.openUpload({ file });
        },

        // ---- delete ----

        /**
         * @param {"video" | "subtitle"} kind
         * @param {{ id: number }} item
         * @param {string} label shown in the confirmation text
         */
        askDelete(kind, item, label) {
            this.confirmation = { kind, id: item.id, label, busy: false };
            this.$refs.confirmDialog.showModal();
        },

        async confirmDelete() {
            const { kind, id } = this.confirmation;
            this.confirmation.busy = true;
            try {
                await this.api(`/api/admin/${kind === "video" ? "videos" : "subtitles"}/${id}`, { method: "DELETE" });
                this.$refs.confirmDialog.close();
                this.toast("success", this.t(`${kind}.deleted`));
                await this.loadVideos();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.confirmation.busy = false;
            }
        },

        // ---- misc ----

        /** @param {MouseEvent} event close a <dialog> when its backdrop is clicked */
        closeOnBackdrop(event) {
            if (event.target === event.currentTarget) /** @type {HTMLDialogElement} */ (event.currentTarget).close();
        },
    }));
});
