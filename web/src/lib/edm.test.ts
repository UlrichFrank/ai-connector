import assert from "node:assert/strict";
import { test } from "node:test";
import { edmWords, isEDM } from "./edm.ts";

test("EDM descriptions", () => {
  for (const s of [
    "uplifting trance, 138 bpm, supersaw lead",
    "dark melodic techno, 124 BPM, A minor",
    "progressive house, euphoric, 124 bpm",
    "liquid drum and bass, 174 bpm",
    "Dubstep with a heavy drop",
    "synthwave, retro 80s, driving bass",
    "club track, four-on-the-floor kick, sidechain",
  ])
    assert.ok(isEDM(s), s);
});

test("not EDM", () => {
  for (const s of [
    "indie pop, bright acoustic guitar, warm female vocal",
    "German birthday pop anthem, acoustic guitar and piano",
    "dance pop ballad",
    "jazz club, upright bass, brushes",
    "lo-fi hip hop, mellow piano",
    "",
  ])
    assert.ok(!isEDM(s), s);
});

test("weak words need company", () => {
  assert.deepEqual(edmWords("dance music with a big drop"), ["dance", "drop"]);
});
