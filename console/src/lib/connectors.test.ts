/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Connector } from "../api/types.ts";
import { createBody, emptyForm, formOf, normalizeUrl, patchBody, stateLabel, validate } from "./connectors.ts";

const connector: Connector = { id: "c1", kind: "vboard", name: "vboard", url: "https://vboard.example.com", board: "b_1", enabled: true, state: "connected", models: { chat: "qwen" } };

test("normalizeUrl", () => {
  assert.equal(normalizeUrl(" vboard.example.com/ "), "https://vboard.example.com");
  assert.equal(normalizeUrl("http://10.0.0.5:3000/"), "http://10.0.0.5:3000");
  assert.equal(normalizeUrl(""), "");
});

test("validate: a new connector needs an address and a token (any prefix: vboard's is configurable)", () => {
  assert.deepEqual(Object.keys(validate(emptyForm, false)).sort(), ["token", "url"]);
  assert.ok(validate({ ...emptyForm, url: "v.example", token: "vbp_ x" }, false).token);
  assert.ok(validate({ ...emptyForm, name: "x", url: "ftp://v.example", token: "vbp_x" }, false).url);
  assert.deepEqual(validate({ ...emptyForm, name: "x", url: "v.example", token: "vbp_x" }, false), {});
  assert.deepEqual(validate({ ...emptyForm, url: "v.example", token: "corp_x" }, false), {});
  // editing: a blank token keeps the stored one
  assert.deepEqual(validate({ ...formOf(connector) }, true), {});
});

test("createBody", () => {
  assert.deepEqual(createBody({ ...emptyForm, name: " vb ", url: "vboard.example.com", token: " vbp_x ", embed: "e" }), {
    kind: "vboard",
    name: "vb",
    url: "https://vboard.example.com",
    token: "vbp_x",
    models: { embed: "e" },
  });
  assert.equal(createBody({ ...emptyForm, name: "a", url: "b", token: "vbp_" }).models, undefined);
  // a blank name: the server's default
  assert.equal(createBody({ ...emptyForm, url: "b", token: "vbp_" }).name, undefined);
});

test("patchBody: only what changed", () => {
  assert.deepEqual(patchBody(connector, formOf(connector)), {});
  assert.deepEqual(patchBody(connector, { ...formOf(connector), name: "new", token: "vbp_new" }), { name: "new", token: "vbp_new" });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), board: "" }), { board: "" });
  assert.deepEqual(patchBody(connector, { ...formOf(connector), classify: "laya" }), { models: { classify: "laya", embed: "", chat: "qwen" } });
  // the server's default name: a blank field is no change, clearing another name sends ""
  const dflt: Connector = { ...connector, name: "ai-connector" };
  assert.deepEqual(patchBody(dflt, { ...formOf(dflt), name: "" }), {});
  assert.deepEqual(patchBody(connector, { ...formOf(connector), name: "" }), { name: "" });
});

test("stateLabel", () => {
  assert.equal(stateLabel("connected").tone, "ok");
  assert.equal(stateLabel("connected", false).text, "disabled");
  assert.equal(stateLabel("error").tone, "bad");
  assert.equal(stateLabel("connecting").tone, "busy");
});
