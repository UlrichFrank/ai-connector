/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Uplink } from "../api/types.ts";
import { createBody, emptyForm, formOf, normalizeUrl, patchBody, stateLabel, validate } from "./uplinks.ts";

const uplink: Uplink = { id: "u1", name: "Home", url: "https://app.example.com/ai/uplink", enabled: true, owner: "Ada", client: "c1", created: "", state: "connected" };

test("normalizeUrl: https:// when there is no scheme; others stay", () => {
  assert.equal(normalizeUrl(" app.example.com/ai/uplink "), "https://app.example.com/ai/uplink");
  assert.equal(normalizeUrl("http://10.0.0.5:3000/x"), "http://10.0.0.5:3000/x");
  assert.equal(normalizeUrl("wss://app.example.com/x"), "wss://app.example.com/x");
  assert.equal(normalizeUrl(""), "");
});

test("validate: a new uplink needs an address and a token; editing keeps the token", () => {
  assert.deepEqual(Object.keys(validate(emptyForm, false)).sort(), ["token", "url"]);
  assert.ok(validate({ ...emptyForm, url: "ftp://app.example", token: "t" }, false).url);
  assert.ok(validate({ ...emptyForm, url: "app.example", token: "a b" }, false).token);
  assert.deepEqual(validate({ ...emptyForm, url: "app.example/ai", token: "vbp_x" }, false), {});
  assert.deepEqual(validate({ ...formOf(uplink) }, true), {});
});

test("createBody and patchBody: only what changed, a blank token kept", () => {
  assert.deepEqual(createBody({ name: " ", url: "app.example/ai", token: " t " }), { url: "https://app.example/ai", token: "t", enabled: true });
  assert.deepEqual(patchBody(uplink, formOf(uplink)), {});
  assert.deepEqual(patchBody(uplink, { ...formOf(uplink), name: "Office", token: "new" }), { name: "Office", token: "new" });
});

test("stateLabel", () => {
  assert.deepEqual(stateLabel("connected"), { text: "connected", tone: "ok" });
  assert.deepEqual(stateLabel("connected", false), { text: "disabled", tone: "off" });
  assert.deepEqual(stateLabel("error"), { text: "error", tone: "bad" });
});
