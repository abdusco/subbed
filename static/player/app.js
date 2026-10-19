import { resolveLanguage, translate } from "./i18n.js";
import { cuesAt, languageFromLocation, parseSubtitles, renderCues, urlFromLocation, videoIdFromUrl } from "./subtitles.js";

/** @typedef {import("./subtitles.js").Cue} Cue */
/** @typedef {{ id: string, language: string, label: string, cues: Cue[] }} Track */
/** @typedef {"system" | "light" | "dark"} ThemePreference */
/** @typedef {{ key: string, params?: Record<string, string | number> }} Message */

const STORAGE_KEYS = {
    theme: "subbed-player-theme",
    language: "subbed-player-language",
    size: "subbed-player-subtitle-size",
    subtitleLanguage: "subbed-player-subtitle-language",
};
const SIZE_STEPS = [0.75, 0.9, 1, 1.15, 1.3, 1.5, 1.75];
const DEFAULT_SIZE_INDEX = 2;
const DELAY_STEP_SECONDS = 0.5;
const DELAY_LIMIT_SECONDS = 30;
const SYNC_INTERVAL_MS = 100;
// how many sync ticks pass between checks that YouTube's own captions are still off (every 0.5 s)
const CAPTION_CHECK_EVERY_TICKS = 5;
const OFF = "off";

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

const SKIP_SECONDS = 10;
const FRAME_SECONDS = 1 / 30;
const VOLUME_STEP = 5;

// The YouTube player and timers live outside Alpine so they are never wrapped in reactive proxies.
/** @type {any} */
let player = null;
let syncTimer = 0;
/** true while library subtitles are shown instead of YouTube's own captions */
let suppressYoutubeCaptions = false;
/** @type {Cue[]} */
let shownCues = [];
/** @type {Promise<void> | null} */
let youtubeApi = null;

/**
 * Load the YouTube IFrame API once.
 * @returns {Promise<void>}
 */
function loadYoutubeApi() {
    // @ts-ignore YT is a global from the YouTube script
    if (window.YT?.Player) return Promise.resolve();
    youtubeApi ??= new Promise((resolve, reject) => {
        // @ts-ignore
        window.onYouTubeIframeAPIReady = () => resolve();
        const script = document.createElement("script");
        script.src = "https://www.youtube.com/iframe_api";
        script.async = true;
        script.onerror = () => {
            youtubeApi = null;
            reject(new Error("YouTube API failed to load"));
        };
        document.head.appendChild(script);
    });
    return youtubeApi;
}

/**
 * Turn off YouTube's built-in captions while the library subtitles are in use. Safe to call repeatedly.
 * setOption switches off a caption track that is already showing; unloadModule stops the module from loading more.
 */
function hideYoutubeCaptions() {
    if (!suppressYoutubeCaptions || !player) return;
    try {
        if (player.getOptions?.().includes("captions")) player.setOption("captions", "track", {});
        player.unloadModule("captions");
        player.unloadModule("cc");
    } catch {
        // the module may not exist yet
    }
}

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

document.addEventListener("alpine:init", () => {
    // @ts-ignore Alpine is a global from the vendored script
    Alpine.data("player", () => ({
        inputUrl: "",
        loading: false,
        /** @type {Message | null} */
        error: null,
        videoId: "",
        title: "",
        /** @type {Track[]} */
        tracks: [],
        trackId: OFF,
        cueHtml: "",
        sizeIndex: Math.min(Math.max(Number(storage.get(STORAGE_KEYS.size) ?? DEFAULT_SIZE_INDEX), 0), SIZE_STEPS.length - 1),
        delay: 0,
        fullscreen: false,
        canFullscreen: Boolean(document.fullscreenEnabled),
        themePreference: /** @type {ThemePreference} */ (storage.get(STORAGE_KEYS.theme) ?? "system"),
        languagePreference: storage.get(STORAGE_KEYS.language) ?? "auto",

        // ---- lifecycle ----

        init() {
            if (!["system", "light", "dark"].includes(this.themePreference)) this.themePreference = "system";
            if (!Number.isInteger(this.sizeIndex)) this.sizeIndex = DEFAULT_SIZE_INDEX;
            applyTheme(this.themePreference);
            matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => applyTheme(this.themePreference));
            this.$watch("lang", (language) => (document.documentElement.lang = language));
            document.documentElement.lang = this.lang;

            document.addEventListener("fullscreenchange", () => {
                this.fullscreen = Boolean(document.fullscreenElement);
                // a phone held upright wastes most of the screen on a 16:9 video; the lock only works in fullscreen
                if (this.fullscreen) screen.orientation?.lock?.("landscape").catch(() => {});
                else screen.orientation?.unlock?.();
            });
            window.addEventListener("popstate", () => this.route());
            window.addEventListener("paste", (event) => this.onPaste(event));
            window.addEventListener("pagehide", () => this.stopSync());
            window.addEventListener("keydown", (event) => this.onKeydown(event));

            this.route();
        },

        /** Load whatever video the address bar names, or show the start page. */
        route() {
            const url = urlFromLocation(location.pathname + location.search);
            if (url) this.loadVideo(url, { updateHistory: false, language: languageFromLocation(location.pathname + location.search) });
            else this.reset();
        },

        reset() {
            this.disposePlayer();
            this.videoId = "";
            this.title = "";
            this.tracks = [];
            this.trackId = OFF;
            this.error = null;
            this.loading = false;
            document.title = "subbed";
        },

        // ---- i18n and theme ----

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

        get errorText() {
            return this.error ? this.t(this.error.key, this.error.params) : "";
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

        // ---- loading a video ----

        /** @param {SubmitEvent} event */
        submit(event) {
            event.preventDefault();
            this.loadVideo(this.inputUrl.trim(), { updateHistory: true });
        },

        /** @param {ClipboardEvent} event */
        onPaste(event) {
            const target = event.target;
            if (target instanceof HTMLElement && (target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName)))
                return;
            const text = event.clipboardData?.getData("text/plain") ?? "";
            if (!videoIdFromUrl(text)) return;
            event.preventDefault();
            this.loadVideo(text.trim(), { updateHistory: true });
        },

        /**
         * Fetch the library entry for a video, then start the player.
         * @param {string} raw a YouTube URL or video id
         * @param {{ updateHistory: boolean, language?: string }} options language is the subtitle language a link asked for
         */
        async loadVideo(raw, { updateHistory, language = "" }) {
            const id = videoIdFromUrl(raw);
            this.error = null;
            if (!id) {
                this.disposePlayer();
                this.videoId = "";
                this.error = { key: "error.invalid" };
                return;
            }
            // the API and the address bar both use one canonical URL, whatever shape the user pasted
            const canonical = `https://www.youtube.com/watch?v=${id}`;
            if (updateHistory) history.pushState(null, "", `/${canonical}`);
            this.inputUrl = canonical;
            this.loading = true;
            this.disposePlayer();
            this.videoId = "";
            try {
                const response = await fetch(`/api/video?url=${encodeURIComponent(canonical)}`);
                if (response.status !== 404 && !response.ok) throw new HttpError(response.status);
                const data = response.ok ? await response.json() : { video: { title: "" }, subtitles: [] };
                this.title = data.video.title;
                this.tracks = this.buildTracks(data.subtitles);
                this.trackId = this.pickTrack(language);
                this.videoId = id;
                document.title = this.title ? `${this.title} – subbed` : "subbed";
                await this.$nextTick();
                await loadYoutubeApi();
                await this.startPlayer(id, this.tracks.length === 0);
            } catch (error) {
                this.videoId = "";
                this.error =
                    error instanceof HttpError ? { key: "error.server", params: { status: error.status } } : { key: "error.network" };
            } finally {
                this.loading = false;
            }
        },

        /**
         * @param {{ id: number, language: string, content: string }[]} subtitles
         * @returns {Track[]}
         */
        buildTracks(subtitles) {
            const names = new Intl.DisplayNames([this.lang], { type: "language" });
            const seen = new Map();
            return subtitles
                .map((subtitle) => ({ subtitle, cues: parseSubtitles(subtitle.content) }))
                .filter(({ cues }) => cues.length)
                .map(({ subtitle, cues }) => {
                    const count = (seen.get(subtitle.language) ?? 0) + 1;
                    seen.set(subtitle.language, count);
                    let name;
                    try {
                        name = names.of(subtitle.language) ?? subtitle.language.toUpperCase();
                    } catch {
                        name = subtitle.language.toUpperCase();
                    }
                    return {
                        id: String(subtitle.id),
                        language: subtitle.language,
                        label: count > 1 ? `${name} (${count})` : name,
                        cues,
                    };
                });
        },

        /**
         * Prefer the language the link asked for (without saving it as the preference), then the one chosen last time,
         * else the first track.
         * @param {string} [requested]
         */
        pickTrack(requested = "") {
            const asked = requested && this.tracks.find((track) => track.language.toLowerCase() === requested);
            if (asked) return asked.id;
            const saved = storage.get(STORAGE_KEYS.subtitleLanguage);
            if (saved === OFF) return OFF;
            return (this.tracks.find((track) => track.language === saved) ?? this.tracks[0])?.id ?? OFF;
        },

        // ---- YouTube player ----

        /**
         * @param {string} id
         * @param {boolean} withYoutubeCaptions turn YouTube's own captions on (no library subtitles)
         * @returns {Promise<void>}
         */
        startPlayer(id, withYoutubeCaptions) {
            return new Promise((resolve) => {
                // YT.Player replaces its target element, so every start gets a fresh one
                this.$refs.mount.innerHTML = '<div id="yt-player"></div>';
                suppressYoutubeCaptions = !withYoutubeCaptions;
                // @ts-ignore
                player = new YT.Player("yt-player", {
                    videoId: id,
                    // privacy-enhanced host: it ignores the viewer's YouTube account and the caption choices remembered
                    // for youtube.com embeds, which would otherwise switch YouTube's captions on over ours
                    host: "https://www.youtube-nocookie.com",
                    playerVars: {
                        controls: 1,
                        autoplay: 0,
                        rel: 0,
                        playsinline: 1,
                        // hint English to YouTube; it may keep it from defaulting to a dubbed audio track
                        hl: "en",
                        origin: location.origin,
                        // our own fullscreen button keeps the subtitles visible; fall back to YouTube's where it can't
                        fs: this.canFullscreen ? 0 : 1,
                        ...(withYoutubeCaptions && { cc_load_policy: 1, cc_lang_pref: this.lang }),
                    },
                    events: {
                        onReady: () => {
                            if (this.canFullscreen) blockIframeFullscreen();
                            hideYoutubeCaptions();
                            this.startSync();
                            this.tick();
                            resolve();
                        },
                        // the captions module loads lazily, so react when a module loads and on every state change
                        onApiChange: () => hideYoutubeCaptions(),
                        onStateChange: () => {
                            hideYoutubeCaptions();
                            this.tick();
                        },
                    },
                });
            });
        },

        disposePlayer() {
            this.stopSync();
            player?.destroy?.();
            player = null;
            suppressYoutubeCaptions = false;
            shownCues = [];
            this.cueHtml = "";
            if (this.$refs.mount) this.$refs.mount.innerHTML = "";
        },

        // ---- subtitle sync ----

        get track() {
            return this.tracks.find((track) => track.id === this.trackId) ?? null;
        },

        startSync() {
            this.stopSync();
            // Iframe seeks can finish without a state change, including while paused.
            let ticks = 0;
            syncTimer = window.setInterval(() => {
                this.tick();
                // a captions track can still appear later (e.g. the viewer presses CC), so keep checking
                if (++ticks % CAPTION_CHECK_EVERY_TICKS === 0) hideYoutubeCaptions();
            }, SYNC_INTERVAL_MS);
        },

        stopSync() {
            clearInterval(syncTimer);
            syncTimer = 0;
        },

        /** Show the cue for the player's current time, re-rendering only when the cue changes. */
        tick() {
            const track = this.track;
            const cues = track && player?.getCurrentTime ? cuesAt(track.cues, player.getCurrentTime() - this.delay) : [];
            if (cues.length === shownCues.length && cues.every((cue, index) => cue === shownCues[index])) return;
            shownCues = cues;
            this.cueHtml = renderCues(cues);
        },

        onTrackChange() {
            storage.set(STORAGE_KEYS.subtitleLanguage, this.track ? this.track.language : OFF);
            shownCues = [];
            this.tick();
        },

        // ---- subtitle controls ----

        get scale() {
            return SIZE_STEPS[this.sizeIndex];
        },

        /** @param {number} direction -1 or 1 */
        bumpSize(direction) {
            this.sizeIndex = Math.min(Math.max(this.sizeIndex + direction, 0), SIZE_STEPS.length - 1);
            storage.set(STORAGE_KEYS.size, String(this.sizeIndex));
        },

        /** @param {number} direction -1 shows subtitles earlier, 1 later */
        bumpDelay(direction) {
            const next = Math.round((this.delay + direction * DELAY_STEP_SECONDS) * 10) / 10;
            this.delay = Math.min(Math.max(next, -DELAY_LIMIT_SECONDS), DELAY_LIMIT_SECONDS);
            shownCues = [];
            this.tick();
        },

        resetDelay() {
            this.delay = 0;
            shownCues = [];
            this.tick();
        },

        get delayLabel() {
            return `${this.delay > 0 ? "+" : ""}${this.delay.toFixed(1)} s`;
        },

        /** @param {KeyboardEvent} event */
        onKeydown(event) {
            if (event.ctrlKey || event.metaKey || event.altKey || !player?.getCurrentTime) return;
            if (event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement) return;
            switch (event.key.toLowerCase()) {
                case "f":
                    if (!this.canFullscreen) return;
                    this.toggleFullscreen();
                    break;
                case "j":
                    player.seekTo(Math.max(0, player.getCurrentTime() - SKIP_SECONDS), true);
                    break;
                case "l":
                    player.seekTo(player.getCurrentTime() + SKIP_SECONDS, true);
                    break;
                case "k":
                    // @ts-ignore
                    if (player.getPlayerState() === YT.PlayerState.PLAYING) player.pauseVideo();
                    else player.playVideo();
                    break;
                case "+":
                case "=":
                    this.bumpSize(1);
                    break;
                case "-":
                    this.bumpSize(-1);
                    break;
                case "arrowup":
                case "arrowdown": {
                    const step = event.key === "ArrowUp" ? VOLUME_STEP : -VOLUME_STEP;
                    if (step > 0 && player.isMuted()) player.unMute();
                    player.setVolume(Math.min(100, Math.max(0, player.getVolume() + step)));
                    break;
                }
                case ",":
                case ".": {
                    // the IFrame API has no frame step: pause and seek by one frame (assumes 30 fps)
                    player.pauseVideo();
                    const step = event.key === "," ? -FRAME_SECONDS : FRAME_SECONDS;
                    player.seekTo(Math.max(0, player.getCurrentTime() + step), true);
                    break;
                }
                default:
                    return;
            }
            event.preventDefault();
            this.tick();
        },

        toggleFullscreen() {
            if (document.fullscreenElement) document.exitFullscreen();
            else this.$refs.stage.requestFullscreen?.();
        },
    }));
});

/** Stop the YouTube iframe from going fullscreen on its own (its "f" key), which would hide our subtitles. */
function blockIframeFullscreen() {
    const iframe = player?.getIframe?.();
    if (!iframe) return;
    iframe.removeAttribute("allowfullscreen");
    iframe.allow = iframe.allow
        .split(";")
        .map((feature) => feature.trim())
        .filter((feature) => feature && feature !== "fullscreen")
        .join("; ");
}

class HttpError extends Error {
    /** @param {number} status */
    constructor(status) {
        super(`HTTP ${status}`);
        this.status = status;
    }
}
