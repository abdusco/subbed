import assert from "node:assert/strict";
import test from "node:test";

import {
    cuesAt,
    languageFromLocation,
    parseSubtitles,
    parseTimestamp,
    renderCue,
    renderCues,
    urlFromLocation,
    videoIdFromUrl,
} from "../static/player/subtitles.js";

test("parseTimestamp accepts SRT, VTT and short forms", () => {
    assert.equal(parseTimestamp("00:01:02,345"), 62.345);
    assert.equal(parseTimestamp("01:02.5"), 62.5);
    assert.equal(parseTimestamp("1:00:00,000"), 3600);
    assert.equal(parseTimestamp("nope"), null);
});

test("parseSubtitles keeps line breaks and handles CRLF, BOM and missing indexes", () => {
    const srt =
        "﻿1\r\n00:00:01,000 --> 00:00:02,500\r\n- Oh what?\r\n- Nah, dude.\r\n\r\n00:03,000 --> 00:04,000 align:start\r\nWorld\r\n\r\n3\r\ngarbage\r\n";
    const cues = parseSubtitles(srt);
    assert.deepEqual(cues, [
        { start: 1, end: 2.5, lines: ["- Oh what?", "- Nah, dude."] },
        { start: 3, end: 4, lines: ["World"] },
    ]);
});

test("parseSubtitles sorts cues by start time", () => {
    const cues = parseSubtitles("1\n00:00:05,000 --> 00:00:06,000\nb\n\n2\n00:00:01,000 --> 00:00:02,000\na\n");
    assert.deepEqual(
        cues.map((cue) => cue.lines[0]),
        ["a", "b"],
    );
});

test("cuesAt finds every showing cue in start order, and none in gaps", () => {
    const cues = parseSubtitles(
        "1\n00:00:01,000 --> 00:00:02,000\na\n\n2\n00:00:04,000 --> 00:00:09,000\nb\n\n3\n00:00:05,000 --> 00:00:06,000\nc\n",
    );
    const texts = (/** @type {number} */ time) => cuesAt(cues, time).map((cue) => cue.lines[0]);
    assert.deepEqual(texts(0.5), []);
    assert.deepEqual(texts(1.5), ["a"]);
    assert.deepEqual(texts(3), []);
    assert.deepEqual(texts(5.5), ["b", "c"], "overlapping speakers show together");
    assert.deepEqual(texts(8), ["b"], "a long cue stays visible after a short overlapping one ends");
    assert.deepEqual(texts(10), []);
    assert.deepEqual(cuesAt([], 1), []);
});

test("renderCues stacks overlapping cues, keeping each cue's line breaks", () => {
    const cues = parseSubtitles("1\n00:00:01,000 --> 00:00:03,000\n- Hi\n- Hello\n\n2\n00:00:02,000 --> 00:00:04,000\nWait\n");
    assert.equal(
        renderCues(cuesAt(cues, 2.5)),
        '<div class="cue dialogue">- Hi<br>- Hello</div><div class="cue">Wait</div>',
        "only the all-dashes cue is marked as dialogue",
    );
});

test("renderCue escapes HTML and restores formatting", () => {
    const hostile = renderCue(["<script>alert(1)</script><img src=x onerror=alert(1)>ok"]);
    assert.ok(!hostile.includes("<script") && !hostile.includes("<img"), hostile);
    assert.equal(renderCue(["a < b & c > d"]), "a &lt; b &amp; c &gt; d");
    assert.equal(renderCue(["<i>hi</i> <b>there</b>"]), "<em>hi</em> <strong>there</strong>");
    assert.equal(renderCue(["**bold** and *it* and _it2_"]), "<strong>bold</strong> and <em>it</em> and <em>it2</em>");
    assert.equal(renderCue(["snake_case_name stays"]), "snake_case_name stays");
    assert.equal(renderCue(["{\\an8}Top"]), "Top");
    assert.equal(renderCue(["- a", "- b"]), "- a<br>- b");
});

test("videoIdFromUrl and urlFromLocation", () => {
    assert.equal(videoIdFromUrl("https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=1"), "dQw4w9WgXcQ");
    assert.equal(videoIdFromUrl("https://youtu.be/dQw4w9WgXcQ"), "dQw4w9WgXcQ");
    assert.equal(videoIdFromUrl("dQw4w9WgXcQ"), "dQw4w9WgXcQ");
    assert.equal(videoIdFromUrl("https://example.com/watch?v=dQw4w9WgXcQ"), null);
    assert.equal(urlFromLocation("/https:/www.youtube.com/watch?v=abc"), "https://www.youtube.com/watch?v=abc");
    assert.equal(urlFromLocation("/https://youtu.be/abc"), "https://youtu.be/abc");
    assert.equal(urlFromLocation("/"), "");
    assert.equal(urlFromLocation("/something"), "");
});

test("languageFromLocation reads lang from the embedded URL's query", () => {
    assert.equal(languageFromLocation("/https://www.youtube.com/watch?v=dQw4w9WgXcQ&lang=tr"), "tr");
    assert.equal(languageFromLocation("/https:/youtu.be/dQw4w9WgXcQ?lang=PT-br"), "pt-br");
    assert.equal(languageFromLocation("/https://www.youtube.com/watch?v=dQw4w9WgXcQ"), "");
    assert.equal(languageFromLocation("/https://www.youtube.com/watch?v=dQw4w9WgXcQ&lang="), "");
    assert.equal(languageFromLocation("/"), "");
    assert.equal(videoIdFromUrl("https://www.youtube.com/watch?v=dQw4w9WgXcQ&lang=tr"), "dQw4w9WgXcQ");
});
