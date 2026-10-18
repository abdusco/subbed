/**
 * @typedef {{ start: number, end: number, lines: string[] }} Cue
 */

// "00:01:02,345", "01:02.345" or "1:02,3" -> hours optional, comma or dot before the fraction
const TIMESTAMP_PATTERN = /^(?:(\d+):)?(\d{1,2}):(\d{1,2})(?:[.,](\d{1,3}))?$/;

/**
 * @param {string} text a subtitle timestamp
 * @returns {number | null} seconds, or null if the text isn't a timestamp
 */
export function parseTimestamp(text) {
    const match = TIMESTAMP_PATTERN.exec(text.trim());
    if (!match) return null;
    const [, hours = "0", minutes, seconds, fraction = "0"] = match;
    return Number(hours) * 3600 + Number(minutes) * 60 + Number(seconds) + Number(fraction.padEnd(3, "0")) / 1000;
}

/**
 * Parse SRT (or converted VTT) content into cues, sorted by start time. Line breaks inside a cue are kept.
 * @param {string} content
 * @returns {Cue[]}
 */
export function parseSubtitles(content) {
    /** @type {Cue[]} */
    const cues = [];
    // a blank line (possibly holding spaces) separates cues
    for (const block of content
        .replace(/^﻿/, "")
        .replace(/\r\n?/g, "\n")
        .split(/\n\s*\n/)) {
        const lines = block.split("\n").map((line) => line.trim());
        const timeLine = lines.findIndex((line) => line.includes("-->"));
        if (timeLine < 0) continue;
        const [start, end] = lines[timeLine].split("-->").map((part) => parseTimestamp(part.trim().split(/\s+/)[0] ?? ""));
        const text = lines.slice(timeLine + 1).filter(Boolean);
        if (start === null || end === null || !text.length) continue;
        cues.push({ start, end, lines: text });
    }
    return cues.sort((a, b) => a.start - b.start);
}

/**
 * Find every cue showing at `time` (several when speakers overlap), in start order. Cues must be sorted by start.
 * @param {Cue[]} cues
 * @param {number} time seconds
 * @returns {Cue[]}
 */
export function cuesAt(cues, time) {
    let low = 0;
    let high = cues.length - 1;
    let candidate = -1;
    // binary search for the last cue that has started
    while (low <= high) {
        const middle = (low + high) >> 1;
        if (cues[middle].start <= time) {
            candidate = middle;
            low = middle + 1;
        } else {
            high = middle - 1;
        }
    }
    // overlapping cues are short-lived; look back a few for ones that are still showing
    /** @type {Cue[]} */
    const showing = [];
    for (let index = candidate; index >= 0 && index > candidate - 8; index--) {
        if (time <= cues[index].end) showing.unshift(cues[index]);
    }
    return showing;
}

/**
 * Turn cue lines into safe HTML: everything is escaped, then <i>/<b> tags and **bold** / *italic* / _italic_ are restored.
 * @param {string[]} lines
 * @returns {string}
 */
export function renderCue(lines) {
    return lines.map(renderLine).join("<br>");
}

// a line that starts with a dash is one speaker's turn in a dialogue
const DIALOGUE_LINE = /^[-–—]\s*/;

/**
 * Render the cues showing at once, one block per cue. Dialogue cues (every line starts with a dash) get a `dialogue`
 * class so the lines can be left-aligned and their dashes line up.
 * @param {Cue[]} cues
 * @returns {string}
 */
export function renderCues(cues) {
    return cues
        .map((cue) => {
            const dialogue = cue.lines.length > 1 && cue.lines.every((line) => DIALOGUE_LINE.test(line));
            return `<div class="cue${dialogue ? " dialogue" : ""}">${renderCue(cue.lines)}</div>`;
        })
        .join("");
}

/**
 * @param {string} line
 * @returns {string}
 */
function renderLine(line) {
    let html = line.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    // &lt;i&gt; ... &lt;/i&gt; (and b/em/strong) were real tags in the source
    html = html.replace(/&lt;(\/?)(i|em)&gt;/gi, "<$1em>").replace(/&lt;(\/?)(b|strong)&gt;/gi, "<$1strong>");
    // any other tag (font, ASS-style {\an8}) is dropped
    html = html.replace(/&lt;\/?[a-z][^&]*?&gt;/gi, "").replace(/\{\\[^}]*\}/g, "");
    html = html.replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>").replace(/__(.+?)__/g, "<strong>$1</strong>");
    // single * or _ pairs that aren't part of a longer run or inside a word (snake_case stays as is)
    html = html.replace(/(?<!\*)\*(?!\*)(.+?)(?<!\*)\*(?!\*)/g, "<em>$1</em>");
    html = html.replace(/(?<![\w_])_(?!_)(.+?)(?<![_\s])_(?![\w_])/g, "<em>$1</em>");
    return html;
}

/**
 * Extract the YouTube video id from a watch/short/embed URL or a bare 11-character id.
 * @param {string} input
 * @returns {string | null}
 */
export function videoIdFromUrl(input) {
    const text = String(input ?? "").trim();
    const idPattern = /^[A-Za-z0-9_-]{11}$/;
    if (idPattern.test(text)) return text;
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
    return id && idPattern.test(id) ? id : null;
}

/**
 * The player is addressed as /<youtube url>; browsers and proxies often collapse "https://" to "https:/".
 * @param {string} pathAndSearch location.pathname + location.search
 * @returns {string} the embedded URL, or "" if the path holds none
 */
export function urlFromLocation(pathAndSearch) {
    const rest = pathAndSearch.replace(/^\/+/, "").trim();
    return /^https?:/i.test(rest) ? rest.replace(/^(https?):\/*/i, "$1://") : "";
}

/**
 * The subtitle language a link asks for, e.g. /https://youtu.be/<id>?lang=tr. It is part of the embedded URL's query.
 * @param {string} pathAndSearch location.pathname + location.search
 * @returns {string} the lowercase language code, or "" if the link names none
 */
export function languageFromLocation(pathAndSearch) {
    try {
        return (new URL(urlFromLocation(pathAndSearch)).searchParams.get("lang") ?? "").trim().toLowerCase();
    } catch {
        return "";
    }
}
