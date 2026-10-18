/**
 * @typedef {{ id: number, video_id: number, language: string, type: string }} Subtitle
 * @typedef {{ id: number, original_url: string, title: string, subtitles: Subtitle[] }} Video
 */

const VIDEO_ID_PATTERN = /^[A-Za-z0-9_-]{11}$/;
// "00:01:02,345 --> 00:01:04,000" (SRT) or "01:02.345 --> 01:04.000" (VTT, hours optional)
const TIMELINE_PATTERN = /(\d{1,2}:)?\d{2}:\d{2}[,.]\d{3}\s*-->\s*(\d{1,2}:)?\d{2}:\d{2}[,.]\d{3}/;
const LANGUAGE_PATTERN = /^[a-z]{2,3}(-[a-z0-9]{2,8})*$/;

/**
 * Extract the YouTube video id from a watch/short/embed URL or a bare 11-character id.
 * @param {string} input
 * @returns {string | null}
 */
export function videoIdFromUrl(input) {
    const text = String(input ?? "").trim();
    if (VIDEO_ID_PATTERN.test(text)) return text;
    let url;
    try {
        url = new URL(text);
    } catch {
        return null;
    }
    const host = url.hostname.replace(/^www\.|^m\./, "");
    let id = null;
    if (host === "youtu.be") {
        id = url.pathname.split("/")[1];
    } else if (host === "youtube.com" || host === "music.youtube.com") {
        const [, kind, value] = url.pathname.split("/");
        id = url.searchParams.get("v") ?? (["embed", "shorts", "live"].includes(kind) ? value : null);
    }
    return id && VIDEO_ID_PATTERN.test(id) ? id : null;
}

/**
 * Turn what the user typed into the URL stored on the server (a bare id becomes a watch URL).
 * @param {string} input
 * @returns {string | null} null if no video id can be found
 */
export function normalizeVideoUrl(input) {
    const id = videoIdFromUrl(input);
    if (!id) return null;
    const text = input.trim();
    return VIDEO_ID_PATTERN.test(text) ? `https://www.youtube.com/watch?v=${id}` : text;
}

/**
 * @param {string} id YouTube video id
 * @returns {string}
 */
export function thumbnailUrl(id) {
    return `https://i.ytimg.com/vi/${id}/mqdefault.jpg`;
}

/**
 * Lowercase and validate a BCP-47-like language code such as "en" or "pt-br".
 * @param {string} input
 * @returns {string | null} null if the code is invalid
 */
export function normalizeLanguage(input) {
    const code = String(input ?? "")
        .trim()
        .toLowerCase()
        .replace(/_/g, "-");
    return LANGUAGE_PATTERN.test(code) ? code : null;
}

/**
 * @param {string} name file name
 * @returns {"srt" | "vtt" | null}
 */
export function subtitleTypeFromName(name) {
    const match = /\.(srt|vtt)$/i.exec(name ?? "");
    return match ? /** @type {"srt" | "vtt"} */ (match[1].toLowerCase()) : null;
}

/**
 * Guess whether pasted text is subtitle content, and in which format.
 * @param {string} text
 * @returns {"srt" | "vtt" | null}
 */
export function sniffSubtitleText(text) {
    const trimmed = String(text ?? "")
        .replace(/^﻿/, "")
        .trim();
    if (!TIMELINE_PATTERN.test(trimmed)) return null;
    return trimmed.startsWith("WEBVTT") ? "vtt" : "srt";
}

/**
 * Find a language code hidden in a file name, e.g. "movie.tr.srt" or "movie_en-US.vtt".
 * @param {string} name file name
 * @returns {string | null}
 */
export function languageFromName(name) {
    const base = String(name ?? "").replace(/\.(srt|vtt)$/i, "");
    // a separator, then a 2-3 letter code and an optional region, at the very end of the name
    const match = /[._\s-]([a-z]{2,3}(?:[-_][A-Za-z0-9]{2,4})?)$/i.exec(base);
    return match ? normalizeLanguage(match[1]) : null;
}

/**
 * Case-insensitive match on a video's title, URL and subtitle languages.
 * @param {Video} video
 * @param {string} query
 * @returns {boolean}
 */
export function matchesQuery(video, query) {
    const needle = query.trim().toLowerCase();
    if (!needle) return true;
    const haystack = [video.title, video.original_url, ...video.subtitles.map((subtitle) => subtitle.language)];
    return haystack.some((value) => value.toLowerCase().includes(needle));
}
