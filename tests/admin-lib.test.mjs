import assert from "node:assert/strict";
import test from "node:test";

import { resolveLanguage, translate } from "../static/admin/i18n.js";
import {
    GENERATION_LANGUAGES,
    knownGenerationLanguages,
    languageFromName,
    matchesQuery,
    normalizeLanguage,
    normalizeVideoUrl,
    sniffSubtitleText,
    subtitleTypeFromName,
    videoIdFromUrl,
} from "../static/admin/lib.js";

test("videoIdFromUrl handles common YouTube URL shapes", () => {
    const id = "dQw4w9WgXcQ";
    for (const input of [
        `https://www.youtube.com/watch?v=${id}&t=10s`,
        `https://youtu.be/${id}?si=x`,
        `https://m.youtube.com/watch?v=${id}`,
        `https://www.youtube.com/embed/${id}`,
        `https://www.youtube.com/shorts/${id}`,
        id,
    ]) {
        assert.equal(videoIdFromUrl(input), id, input);
    }
    for (const input of ["", "hello", "https://example.com/watch?v=dQw4w9WgXcQ", "https://youtube.com/watch?v=short"]) {
        assert.equal(videoIdFromUrl(input), null, input);
    }
});

test("normalizeVideoUrl expands bare ids and keeps full URLs", () => {
    assert.equal(normalizeVideoUrl("dQw4w9WgXcQ"), "https://www.youtube.com/watch?v=dQw4w9WgXcQ");
    assert.equal(normalizeVideoUrl("  https://youtu.be/dQw4w9WgXcQ "), "https://youtu.be/dQw4w9WgXcQ");
    assert.equal(normalizeVideoUrl("nope"), null);
});

test("normalizeLanguage", () => {
    assert.equal(normalizeLanguage(" TR "), "tr");
    assert.equal(normalizeLanguage("pt_BR"), "pt-br");
    for (const bad of ["", "e", "english", "en us", "1a"]) assert.equal(normalizeLanguage(bad), null, bad);
});

test("subtitle type, language and text sniffing", () => {
    assert.equal(subtitleTypeFromName("a.SRT"), "srt");
    assert.equal(subtitleTypeFromName("a.vtt"), "vtt");
    assert.equal(subtitleTypeFromName("a.txt"), null);
    assert.equal(languageFromName("My Movie.tr.srt"), "tr");
    assert.equal(languageFromName("movie_en-US.vtt"), "en-us");
    assert.equal(languageFromName("movie.srt"), null);
    assert.equal(sniffSubtitleText("1\n00:00:01,000 --> 00:00:02,500\nHi"), "srt");
    assert.equal(sniffSubtitleText("WEBVTT\n\n00:01.000 --> 00:02.500\nHi"), "vtt");
    assert.equal(sniffSubtitleText("just some text"), null);
});

test("matchesQuery looks at title, url and languages", () => {
    const video = {
        id: 1,
        title: "Çok Güzel",
        original_url: "https://youtu.be/abc",
        subtitles: [{ id: 1, video_id: 1, language: "tr", type: "srt" }],
    };
    assert.ok(matchesQuery(video, ""));
    assert.ok(matchesQuery(video, "güzel"));
    assert.ok(matchesQuery(video, "YOUTU.BE"));
    assert.ok(matchesQuery(video, "tr"));
    assert.ok(!matchesQuery(video, "xyz"));
});

test("generation languages are valid subtitle codes, and a remembered choice is cleaned up", () => {
    const codes = GENERATION_LANGUAGES.map((language) => language.code);
    assert.equal(new Set(codes).size, codes.length);
    for (const code of codes) assert.equal(normalizeLanguage(code), code);

    assert.deepEqual(knownGenerationLanguages(["tr", "xx", "de", "tr"]), ["de", "tr"]);
    assert.deepEqual(knownGenerationLanguages("tr"), []);
    assert.deepEqual(knownGenerationLanguages(null), []);
});

test("i18n: language resolution, plurals and fallbacks", () => {
    assert.equal(resolveLanguage("tr", ["en-US"]), "tr");
    assert.equal(resolveLanguage("auto", ["de-DE", "tr-TR"]), "tr");
    assert.equal(resolveLanguage(null, ["de"]), "en");
    assert.equal(translate("en", "videos", { n: 1 }), "1 video");
    assert.equal(translate("en", "videos", { n: 2 }), "2 videos");
    assert.equal(translate("tr", "videos", { n: 2 }), "2 video");
    assert.equal(translate("tr", "confirm.video", { label: "X" }), "“X” ve tüm altyazıları silinsin mi?");
    assert.equal(translate("en", "missing.key"), "missing.key");
});
