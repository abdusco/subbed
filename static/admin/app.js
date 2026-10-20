import { resolveLanguage, translate } from "./i18n.js";
import {
    GENERATION_LANGUAGES,
    isJobActive,
    isLogComplete,
    knownGenerationLanguages,
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
/**
 * @typedef {{
 *     id: number, video_id: number, video_title: string, kind: "transcribe" | "translate",
 *     status: "queued" | "running" | "done" | "failed" | "canceled", progress: number, stage: string,
 *     languages: string[], source_language: string, error: string, created_at: number
 * }} Job
 */
/** @typedef {{ id: number, at: number, level: "info" | "error", message: string }} JobLogLine */
/** @typedef {{ id: number, kind: "success" | "error", text: string }} Toast */
/** @typedef {"system" | "light" | "dark"} ThemePreference */

const STORAGE_KEYS = {
    theme: "subbed-admin-theme",
    language: "subbed-admin-language",
    lastSubtitleLanguage: "subbed-admin-last-subtitle-language",
    lastTargets: "subbed-admin-last-generation-targets",
};
const REFRESH_MIN_INTERVAL_MS = 5000;
/** the admin API's collection for each kind of thing that can be deleted */
const DELETE_PATHS = { video: "videos", subtitle: "subtitles", job: "jobs" };
const JOB_POLL_INTERVAL_MS = 2000;
const JOBS_SHOWN = 5;
/** a log scrolled to within this many pixels of its end keeps following new lines */
const LOG_FOLLOW_SLACK_PX = 24;
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

        /** titleEdited: the user typed a title, so a fetched one must not overwrite it */
        videoForm: { url: "", title: "", titleEdited: false, titleLoading: false, busy: false },
        /** counts title lookups so a slow, outdated answer is ignored */
        titleRequest: 0,
        subtitleForm: {
            videoId: "",
            /** true when the dialog was opened from a video card, so the video can't be changed */
            fixedVideo: false,
            language: "",
            /** @type {File | null} */
            file: null,
            type: /** @type {"srt" | "vtt"} */ ("srt"),
            pasted: false,
            busy: false,
            dragging: false,
        },
        editForm: { id: 0, url: "", original: "", title: "", busy: false },
        confirmation: { kind: /** @type {"video" | "subtitle" | "job"} */ ("video"), id: 0, label: "", busy: false },
        JOBS_SHOWN,
        isJobActive,
        /** the jobs that are being canceled right now */
        jobBusy: /** @type {Record<number, boolean>} */ ({}),
        /** @type {Job[]} */
        jobs: [],
        jobsExpanded: false,
        /** the ids of the jobs whose log is open */
        openJobs: /** @type {Record<number, boolean>} */ ({}),
        /** what has been read of each job's log; complete once the job is over and every line is in */
        jobLogs: /** @type {Record<number, { lines: JobLogLine[], complete: boolean }>} */ ({}),
        jobPollTimer: 0,
        generationLanguages: GENERATION_LANGUAGES,
        transcribeForm: {
            videoId: 0,
            title: "",
            /** @type {string | null} */
            thumbnail: null,
            /** the spoken language; empty lets the model detect it */
            source: "",
            busy: false,
        },
        translateForm: {
            videoId: 0,
            title: "",
            /** @type {string | null} */
            thumbnail: null,
            /** the languages the video has a subtitle in */
            available: /** @type {string[]} */ ([]),
            /** the subtitle to translate */
            source: "",
            /** @type {string[]} */
            targets: [],
            busy: false,
        },
        generation: {
            failed: false,
            /** the model field; savedModel is what the server has, empty meaning its default */
            model: "",
            savedModel: "",
            modelDefault: "",
            translateModel: "",
            savedTranslateModel: "",
            translateModelDefault: "",
            cookiesText: "",
            cookieCount: 0,
            /** @type {string[]} */
            cookieDomains: [],
            ytdlpInstalled: false,
            ytdlpVersion: "",
            /** the action in progress, so only one runs at a time */
            busy: /** @type {"" | "model" | "cookies" | "ytdlp"} */ (""),
        },

        // ---- lifecycle ----

        init() {
            if (!["system", "light", "dark"].includes(this.themePreference)) this.themePreference = "system";
            applyTheme(this.themePreference);
            matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => applyTheme(this.themePreference));
            matchMedia("(display-mode: standalone)").addEventListener("change", (event) => (this.installed = event.matches));
            this.$watch("lang", (language) => (document.documentElement.lang = language));
            this.$watch("videoUrlId", (id) => this.suggestTitle(id));
            // a subtitle can't be translated into its own language
            this.$watch("translateForm.source", (code) => {
                this.translateForm.targets = this.translateForm.targets.filter((target) => target !== code);
            });
            document.documentElement.lang = this.lang;

            window.addEventListener("online", () => {
                this.online = true;
                this.scheduleRefresh({ force: true });
            });
            window.addEventListener("offline", () => (this.online = false));
            document.addEventListener("visibilitychange", () => {
                if (document.visibilityState !== "visible") return;
                this.scheduleRefresh();
                this.pollJobs();
            });
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

            this.$refs.toasts.showPopover?.();
            this.registerServiceWorker();
            this.loadVideos();
            this.loadJobs();
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
         * Call the admin API. Network failures and non-2xx answers become Errors with a readable message.
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
            if (response.status === 401) {
                // the session expired or was never there
                location.replace("/login");
                throw new Error(this.t("error.signedOut"));
            }
            if (!response.ok) {
                // the API answers errors as { "error": "message" }
                const body = await response.json().catch(() => null);
                const detail = typeof body?.error === "string" ? body.error : "";
                throw new Error(detail || this.t("error.http", { status: response.status }));
            }
            return response.json();
        },

        async signOut() {
            await fetch("/api/logout", { method: "POST" }).catch(() => {});
            location.replace("/login");
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
            // top-layer elements stack in the order they were opened; reopen so toasts sit above any open dialog
            this.$refs.toasts.hidePopover?.();
            this.$refs.toasts.showPopover?.();
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

        /**
         * Fill in the title from YouTube (via our oEmbed proxy) for the video in the URL field.
         * Failures are silent: the user can always type a title.
         * @param {string | null} id the video id, or null when the URL is empty or invalid
         */
        async suggestTitle(id) {
            const request = ++this.titleRequest;
            if (this.videoForm.titleEdited) return;
            if (!id) {
                // the URL was cleared or broken: drop a title we filled in ourselves
                this.videoForm.title = "";
                this.videoForm.titleLoading = false;
                return;
            }
            this.videoForm.titleLoading = true;
            try {
                const watchUrl = encodeURIComponent(`https://www.youtube.com/watch?v=${id}`);
                const { title } = await this.api(`/api/admin/oembed?url=${watchUrl}`);
                if (request === this.titleRequest && !this.videoForm.titleEdited) this.videoForm.title = title;
            } catch {
                // keep whatever the field holds
            } finally {
                if (request === this.titleRequest) this.videoForm.titleLoading = false;
            }
        },

        /** @param {Event} event */
        onTitleInput(event) {
            // clearing the field hands control back to the automatic title
            this.videoForm.titleEdited = /** @type {HTMLInputElement} */ (event.target).value.trim() !== "";
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
                this.videoForm.titleEdited = false;
                this.$refs.addDialog.close();
                this.toast("success", this.t("video.added"));
                await this.loadVideos();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.videoForm.busy = false;
            }
        },

        // ---- edit video ----

        /** Save is only useful when the title is not empty and differs from the stored one. */
        get canSaveEdit() {
            const title = this.editForm.title.trim();
            return title !== "" && title !== this.editForm.original && !this.editForm.busy;
        },

        /** @param {Video} video */
        openEdit(video) {
            this.editForm = { id: video.id, url: video.original_url, original: video.title, title: video.title, busy: false };
            this.$refs.editDialog.showModal();
            this.$nextTick(() => this.$refs.editDialog.querySelector("input")?.select());
        },

        async submitEdit() {
            if (!this.canSaveEdit) return;
            this.editForm.busy = true;
            try {
                await this.api(`/api/admin/videos/${this.editForm.id}`, {
                    method: "PATCH",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ title: this.editForm.title.trim() }),
                });
                this.$refs.editDialog.close();
                this.toast("success", this.t("video.updated"));
                await this.loadVideos();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.editForm.busy = false;
            }
        },

        // ---- upload subtitle ----

        get languageInvalid() {
            return this.subtitleForm.language.trim() !== "" && !normalizeLanguage(this.subtitleForm.language);
        },

        get selectedVideo() {
            return this.videos.find((video) => String(video.id) === this.subtitleForm.videoId) ?? null;
        },

        /** The video is known (opened from a card, or it's the only video), so no picker is needed. */
        get videoLocked() {
            return Boolean(this.selectedVideo) && (this.subtitleForm.fixedVideo || this.videos.length === 1);
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
            if (!this.$refs.uploadDialog.open) {
                // a fresh dialog starts from the card's video, or from nothing; a file dropped into an open one keeps its video
                this.subtitleForm.fixedVideo = videoId !== undefined;
                this.subtitleForm.videoId = videoId !== undefined ? String(videoId) : "";
            }
            if (!this.subtitleForm.videoId && this.videos.length === 1) this.subtitleForm.videoId = String(this.videos[0].id);
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
                // a pasted file that isn't a subtitle (an image, say) is none of our business
                if (!subtitleTypeFromName(file.name)) return;
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
         * @param {"video" | "subtitle" | "job"} kind
         * @param {{ id: number }} item
         * @param {string} label shown in the confirmation text
         */
        askDelete(kind, item, label) {
            // a job that is over is only a log entry, so it goes without asking
            if (kind === "job" && !isJobActive(/** @type {Job} */ (item))) {
                this.deleteItem(kind, item.id).catch((error) => this.toast("error", error.message));
                return;
            }
            this.confirmation = { kind, id: item.id, label, busy: false };
            this.$refs.confirmDialog.showModal();
        },

        async confirmDelete() {
            this.confirmation.busy = true;
            try {
                await this.deleteItem(this.confirmation.kind, this.confirmation.id);
                this.$refs.confirmDialog.close();
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.confirmation.busy = false;
            }
        },

        /**
         * @param {"video" | "subtitle" | "job"} kind
         * @param {number} id
         */
        async deleteItem(kind, id) {
            await this.api(`/api/admin/${DELETE_PATHS[kind]}/${id}`, { method: "DELETE" });
            this.toast("success", this.t(`${kind}.deleted`));
            if (kind === "job") {
                delete this.openJobs[id];
                delete this.jobLogs[id];
                await this.loadJobs();
            } else {
                await this.loadVideos();
            }
        },

        // ---- transcribe and translate ----

        /**
         * @param {string} code
         * @returns {string} the language's name and code, e.g. "Türkçe (tr)"
         */
        languageLabel(code) {
            const known = GENERATION_LANGUAGES.find((language) => language.code === code);
            return known ? `${known.name} (${code})` : code.toUpperCase();
        },

        /** @returns {string[]} the languages that were picked for the last translation */
        rememberedTargets() {
            try {
                return knownGenerationLanguages(JSON.parse(storage.get(STORAGE_KEYS.lastTargets) ?? "[]"));
            } catch {
                // a damaged value is the same as none
                return [];
            }
        },

        /**
         * Queue a job for a video and show it in the jobs list.
         * @param {"transcribe" | "translate"} kind
         * @param {{ videoId: number, busy: boolean }} form
         * @param {HTMLDialogElement} dialog
         * @param {Record<string, unknown>} body
         * @returns {Promise<boolean>} whether the job was queued
         */
        async queueJob(kind, form, dialog, body) {
            form.busy = true;
            try {
                await this.api(`/api/admin/videos/${form.videoId}/${kind}`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(body),
                });
                dialog.close();
                this.toast("success", this.t("generation.started"));
                await this.loadJobs();
                return true;
            } catch (error) {
                this.toast("error", error.message);
                return false;
            } finally {
                form.busy = false;
            }
        },

        /** @param {Video} video */
        openTranscribe(video) {
            this.transcribeForm = { videoId: video.id, title: video.title, thumbnail: this.thumbnail(video), source: "", busy: false };
            this.$refs.transcribeDialog.showModal();
        },

        async submitTranscribe() {
            if (this.transcribeForm.busy) return;
            await this.queueJob("transcribe", this.transcribeForm, this.$refs.transcribeDialog, {
                source_language: this.transcribeForm.source,
            });
        },

        get canTranslate() {
            const { busy, source, targets } = this.translateForm;
            return !busy && source !== "" && targets.length > 0;
        },

        /** @param {Video} video */
        openTranslate(video) {
            const available = video.subtitles.map((subtitle) => subtitle.language);
            const source = available.includes("en") ? "en" : (available[0] ?? "");
            this.translateForm = {
                videoId: video.id,
                title: video.title,
                thumbnail: this.thumbnail(video),
                available,
                source,
                targets: this.rememberedTargets().filter((code) => code !== source),
                busy: false,
            };
            this.$refs.translateDialog.showModal();
        },

        async submitTranslate() {
            if (!this.canTranslate) return;
            const { source } = this.translateForm;
            const targets = this.translateForm.targets.filter((code) => code !== source);
            const queued = await this.queueJob("translate", this.translateForm, this.$refs.translateDialog, {
                source_language: source,
                languages: targets,
            });
            if (queued) storage.set(STORAGE_KEYS.lastTargets, JSON.stringify(targets));
        },

        // ---- generation jobs and their logs ----

        get visibleJobs() {
            return this.jobsExpanded ? this.jobs : this.jobs.slice(0, JOBS_SHOWN);
        },

        /** @param {Job} job */
        jobDate(job) {
            return new Date(job.created_at * 1000);
        },

        /** @param {Job} job */
        formatJobTime(job) {
            return this.jobDate(job).toLocaleString(this.lang, { dateStyle: "medium", timeStyle: "short" });
        },

        /** @param {JobLogLine} line */
        formatLogTime(line) {
            return new Date(line.at).toLocaleTimeString(this.lang, { hour12: false });
        },

        /**
         * @param {Job} job
         * @returns {string} what a running job is doing and how far it is, e.g. "Transcribing chunk 2/5 · 48%"
         */
        jobProgressText(job) {
            return job.stage ? `${job.stage} · ${job.progress}%` : `${job.progress}%`;
        },

        /** @param {Job} job */
        jobSummary(job) {
            if (job.kind !== "translate") {
                return this.t("jobs.kind.transcribe", {
                    language: job.source_language ? job.source_language.toUpperCase() : this.t("jobs.autoDetected"),
                });
            }
            return this.t("jobs.kind.translate", {
                source: job.source_language.toUpperCase(),
                languages: job.languages.map((code) => code.toUpperCase()).join(", "),
            });
        },

        /**
         * Reload the job list. A job that just finished adds subtitles, so the library is reloaded too.
         */
        async loadJobs() {
            try {
                const before = new Map(this.jobs.map((job) => [job.id, job]));
                this.jobs = await this.api("/api/admin/jobs");
                if (this.jobs.some((job) => !isJobActive(job) && before.has(job.id) && isJobActive(before.get(job.id)))) {
                    this.loadVideos();
                }
            } catch {
                // the next poll tries again
            }
            this.schedulePoll();
        },

        /**
         * Read the log lines that came after the last one seen.
         * @param {number} id
         */
        async loadJobLogs(id) {
            if (!this.jobLogs[id]) this.jobLogs[id] = { lines: [], complete: false };
            // read it back: the stored object is wrapped by Alpine, and only the wrapper triggers updates
            const log = this.jobLogs[id];
            const after = log.lines.at(-1)?.id ?? 0;
            const { job, logs } = await this.api(`/api/admin/jobs/${id}/logs?after=${after}`);
            const element = document.getElementById(`job-log-${id}`);
            const following = !element || element.scrollHeight - element.scrollTop - element.clientHeight <= LOG_FOLLOW_SLACK_PX;
            log.lines.push(...logs);
            log.complete = isLogComplete(job, logs.length);
            const listed = this.jobs.find((candidate) => candidate.id === id);
            if (listed) Object.assign(listed, job);
            if (following && logs.length) this.$nextTick(() => element && (element.scrollTop = element.scrollHeight));
        },

        /**
         * Poll only for what someone can see changing: the jobs list while a job is going, and the log of an open
         * job until it is complete. Nothing runs while the page is hidden.
         */
        schedulePoll() {
            clearTimeout(this.jobPollTimer);
            if (document.visibilityState !== "visible" || (!this.jobs.some(isJobActive) && !this.openIncompleteJobs().length)) return;
            this.jobPollTimer = window.setTimeout(() => this.pollJobs(), JOB_POLL_INTERVAL_MS);
        },

        openIncompleteJobs() {
            return this.jobs.filter((job) => this.openJobs[job.id] && !this.jobLogs[job.id]?.complete);
        },

        async pollJobs() {
            clearTimeout(this.jobPollTimer);
            if (document.visibilityState !== "visible") return;
            await Promise.all([this.loadJobs(), ...this.openIncompleteJobs().map((job) => this.loadJobLogs(job.id).catch(() => {}))]);
        },

        /**
         * Stop a job that is queued or running. The answer comes once it has ended.
         * @param {Job} job
         */
        async cancelJob(job) {
            this.jobBusy[job.id] = true;
            try {
                await this.api(`/api/admin/jobs/${job.id}/cancel`, { method: "POST" });
                this.toast("success", this.t("jobs.canceledToast"));
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                delete this.jobBusy[job.id];
                await this.pollJobs();
                // a canceled job's log has its last line now
                if (this.openJobs[job.id]) await this.loadJobLogs(job.id).catch(() => {});
            }
        },

        /**
         * Start reading a job's log when it is opened.
         * @param {Job} job
         * @param {Event} event
         */
        async onJobToggle(job, event) {
            const open = /** @type {HTMLDetailsElement} */ (event.target).open;
            this.openJobs[job.id] = open;
            if (!open) return;
            if (!this.jobLogs[job.id]?.complete) await this.loadJobLogs(job.id).catch((error) => this.toast("error", error.message));
            this.schedulePoll();
        },

        // ---- subtitle generation settings ----

        get canSaveModel() {
            return (
                this.generation.busy === "" &&
                (this.generation.model.trim() !== this.generation.savedModel ||
                    this.generation.translateModel.trim() !== this.generation.savedTranslateModel)
            );
        },

        get cookieSummary() {
            const { cookieCount, cookieDomains } = this.generation;
            if (!cookieCount) return this.t("generation.cookiesNone");
            return this.t("generation.cookiesStored", { n: cookieCount, domains: cookieDomains.join(", ") });
        },

        /** @param {{ transcribe_model: string, transcribe_model_default: string, translate_model: string, translate_model_default: string }} settings */
        applySettings(settings) {
            this.generation.model = settings.transcribe_model;
            this.generation.savedModel = settings.transcribe_model;
            this.generation.modelDefault = settings.transcribe_model_default;
            this.generation.translateModel = settings.translate_model;
            this.generation.savedTranslateModel = settings.translate_model;
            this.generation.translateModelDefault = settings.translate_model_default;
        },

        /** @param {{ count: number, domains: string[] }} summary */
        applyCookies(summary) {
            this.generation.cookieCount = summary.count;
            this.generation.cookieDomains = summary.domains;
        },

        /** @param {{ installed: boolean, version: string }} status */
        applyYtDlp(status) {
            this.generation.ytdlpInstalled = status.installed;
            this.generation.ytdlpVersion = status.version;
        },

        async openGeneration() {
            this.$refs.settings.hidePopover?.();
            this.$refs.generationDialog.showModal();
            this.generation.failed = false;
            try {
                // Populate each section immediately; checking the yt-dlp binary can take a while.
                await Promise.all([
                    this.api("/api/admin/settings").then((settings) => this.applySettings(settings)),
                    this.api("/api/admin/cookies").then((cookies) => this.applyCookies(cookies)),
                    this.api("/api/admin/yt-dlp")
                        .catch(() => ({ installed: false, version: "" }))
                        .then((ytdlp) => this.applyYtDlp(ytdlp)),
                ]);
            } catch (error) {
                this.generation.failed = true;
                this.toast("error", error.message);
            }
        },

        /**
         * Run one of the dialog's actions, showing its error as a toast.
         * @param {"model" | "cookies" | "ytdlp"} action
         * @param {() => Promise<string>} run returns the success message
         */
        async runGenerationAction(action, run) {
            this.generation.busy = action;
            try {
                this.toast("success", await run());
            } catch (error) {
                this.toast("error", error.message);
            } finally {
                this.generation.busy = "";
            }
        },

        saveModel() {
            if (!this.canSaveModel) return;
            return this.runGenerationAction("model", async () => {
                this.applySettings(
                    await this.api("/api/admin/settings", {
                        method: "PUT",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({
                            transcribe_model: this.generation.model.trim(),
                            translate_model: this.generation.translateModel.trim(),
                        }),
                    }),
                );
                return this.t("generation.modelSaved");
            });
        },

        saveCookies() {
            if (!this.generation.cookiesText.trim() || this.generation.busy !== "") return;
            return this.runGenerationAction("cookies", async () => {
                this.applyCookies(
                    await this.api("/api/admin/cookies", {
                        method: "PUT",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ cookies: this.generation.cookiesText }),
                    }),
                );
                // the cookies are secrets: don't keep them in the page once they are saved
                this.generation.cookiesText = "";
                return this.t("generation.cookiesSaved");
            });
        },

        clearCookies() {
            return this.runGenerationAction("cookies", async () => {
                this.applyCookies(await this.api("/api/admin/cookies", { method: "DELETE" }));
                return this.t("generation.cookiesCleared");
            });
        },

        updateYtDlp() {
            return this.runGenerationAction("ytdlp", async () => {
                this.applyYtDlp(await this.api("/api/admin/yt-dlp/update", { method: "POST" }));
                return this.t("generation.ytdlpUpdated");
            });
        },

        // ---- misc ----

        /** @param {MouseEvent} event close a <dialog> when its backdrop is clicked */
        closeOnBackdrop(event) {
            if (event.target === event.currentTarget) /** @type {HTMLDialogElement} */ (event.currentTarget).close();
        },
    }));
});
