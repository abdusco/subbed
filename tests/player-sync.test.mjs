import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

import { cuesAt, renderCues } from "../static/player/subtitles.js";

test("subtitles follow iframe seeks while paused and buffering", async () => {
    let createPlayer;
    let events;
    let sync;
    let time = 2;
    const source = await readFile(new URL("../static/player/app.js", import.meta.url), "utf8");
    vm.runInNewContext(source.replace(/^import .*;\n/gm, ""), {
        cuesAt,
        renderCues,
        document: {
            fullscreenEnabled: false,
            addEventListener: (_name, callback) => callback(),
        },
        Alpine: { data: (_name, factory) => { createPlayer = factory; } },
        location: { origin: "https://example.com" },
        window: { setInterval: (callback) => { sync = callback; return 1; } },
        clearInterval: () => { sync = null; },
        YT: {
            Player: function (_id, options) {
                events = options.events;
                this.getCurrentTime = () => time;
            },
        },
    });

    const app = createPlayer();
    app.$refs = { mount: { innerHTML: "" } };
    app.tracks = [{ id: "1", cues: [
        { start: 1, end: 4, lines: ["First"] },
        { start: 10, end: 14, lines: ["Second"] },
    ] }];
    app.trackId = "1";
    const ready = app.startPlayer("video", false);
    events.onReady();
    await ready;
    assert.equal(app.cueHtml, '<div class="cue">First</div>');

    for (const scenario of [
        { state: 2, time: 12, text: "Second" },
        { state: 2, time: 3, text: "First" },
        { state: 3, time: 11, text: "Second" },
        { state: 1, time: 6, text: "" },
    ]) {
        events.onStateChange({ data: scenario.state });
        // The iframe reports the seek destination after the state event.
        time = scenario.time;
        assert.equal(typeof sync, "function");
        sync();
        assert.equal(app.cueHtml, scenario.text ? `<div class="cue">${scenario.text}</div>` : "");
    }

    app.disposePlayer();
    assert.equal(sync, null);
    assert.equal(app.cueHtml, "");
});

test("bracket shortcuts seek to cue starts in the selected track", async () => {
    let createPlayer;
    let time;
    let destination;
    const source = await readFile(new URL("../static/player/app.js", import.meta.url), "utf8");
    vm.runInNewContext(source.replace(/^import .*;\n/gm, ""), {
        cuesAt,
        renderCues,
        document: {
            fullscreenEnabled: false,
            addEventListener: (_name, callback) => callback(),
        },
        Alpine: { data: (_name, factory) => { createPlayer = factory; } },
        location: { origin: "https://example.com" },
        HTMLInputElement: class {},
        HTMLTextAreaElement: class {},
        YT: {
            Player: function () {
                this.getCurrentTime = () => time;
                this.seekTo = (seconds, allowSeekAhead) => {
                    assert.equal(allowSeekAhead, true);
                    destination = seconds;
                    time = seconds;
                };
            },
        },
    });

    const app = createPlayer();
    app.$refs = { mount: { innerHTML: "" } };
    app.tracks = [{ id: "1", cues: [
        { start: 1, end: 4, lines: ["First"] },
        { start: 10, end: 14, lines: ["Second"] },
        { start: 20, end: 24, lines: ["Third"] },
    ] }];
    app.startPlayer("video", false);

    for (const scenario of [
        { key: "]", time: 0, expected: 1 },
        { key: "]", time: 1, expected: 10 },
        { key: "[", time: 12, expected: 10 },
        { key: "[", time: 10, expected: 1 },
        { key: "[", time: 10.05, expected: 1 },
        { key: "]", time: 6, expected: 10 },
        { key: "[", time: 6, expected: 1 },
        { key: "[", time: 1, expected: undefined },
        { key: "]", time: 22, expected: undefined },
        { key: "]", time: 0, trackId: "off", expected: undefined },
        { key: "]", time: 0, ctrlKey: true, expected: undefined },
    ]) {
        time = scenario.time;
        destination = undefined;
        app.trackId = scenario.trackId ?? "1";
        let prevented = false;
        app.onKeydown({
            key: scenario.key,
            ctrlKey: scenario.ctrlKey,
            preventDefault: () => { prevented = true; },
        });
        assert.equal(destination, scenario.expected, JSON.stringify(scenario));
        assert.equal(prevented, scenario.expected !== undefined, JSON.stringify(scenario));
    }
});
