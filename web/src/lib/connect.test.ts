/// <reference types="node" />
// pnpm test: node --test (Node strips the types; no test framework needed).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { connectCommand, hubUrl, maskToken, shq } from "./connect.ts";

/** The shells a Mac pastes into (zsh is its default); those this machine has. */
const shells = ["sh", "bash", "zsh"].filter((sh) => {
  try {
    execFileSync(sh, ["-c", "true"]);
    return true;
  } catch {
    return false;
  }
});

const tricky = ["plain", "", "it's", "a'b'c", "$HOME `id` $(id) \\n", 'say "hi"', "a b\tc", "x&y;z|w>v<u", "!event", "~/x*?[a]", "ünï €"];

test("shq: the shell reads back exactly the word", () => {
  for (const sh of shells)
    for (const s of tricky) {
      const out = execFileSync(sh, ["-c", `printf '%s' ${shq(s)}`], { encoding: "utf8" });
      assert.equal(out, s, `${sh}: ${s}`);
    }
});

test("hubUrl: the origin without trailing slashes", () => {
  assert.equal(hubUrl("https://mlx.example.com/"), "https://mlx.example.com");
  assert.equal(hubUrl(" http://localhost:8080// "), "http://localhost:8080");
});

test("connectCommand: the contract's shape (docs/architecture.md)", () => {
  assert.equal(
    connectCommand("https://hub.example.com", "tok123"),
    [
      "mkdir -p ~/.config/mlxlab/bin \\",
      "  && curl -fsSL -H 'Authorization: Bearer tok123' -o ~/.config/mlxlab/bin/mlxlab 'https://hub.example.com/tools/mlxlab/'\"$(uname -s)-$(uname -m)\" \\",
      "  && chmod +x ~/.config/mlxlab/bin/mlxlab \\",
      "  && MLXLAB_WORKER_TOKEN='tok123' ~/.config/mlxlab/bin/mlxlab worker --hub 'https://hub.example.com'",
    ].join("\n"),
  );
});

// Runs the command in sh with HOME in a temporary folder and a fake curl that writes a program printing what it got:
// every word arrives intact, whatever the token and the address hold.
test("connectCommand: runs in sh, bash and zsh with special characters in the token and the hub address", () => {
  const cases: [string, string][] = [
    ["https://hub.example.com/", "mlxw_abc-DEF_123"],
    ["http://host:8080/a&b?c=$x", "t'o\"k $(id) `id` ;|&><*?"],
    ["https://exa'mple.com", "a\\b\nc"],
  ];
  for (const sh of shells) {
    for (const [origin, token] of cases) {
      const home = mkdtempSync(join(tmpdir(), "mlxlab-connect-"));
      try {
        const bin = join(home, "fakebin");
        execFileSync("mkdir", ["-p", bin]);
        // curl -fsSL -H <header> -o <file> <url>: records the header and the url, writes a program that prints its args
        writeFileSync(
          join(bin, "curl"),
          `#!/bin/sh\nprintf '%s' "$3" > "$HOME/header"\nprintf '%s' "$6" > "$HOME/url"\nprintf '#!/bin/sh\\nprintf "%%s|" "$MLXLAB_WORKER_TOKEN" "$@" > "$HOME/ran"\\n' > "$5"\n`,
        );
        writeFileSync(join(bin, "uname"), `#!/bin/sh\n[ "$1" = -s ] && echo Darwin || echo arm64\n`);
        chmodSync(join(bin, "curl"), 0o755);
        chmodSync(join(bin, "uname"), 0o755);
        execFileSync(sh, ["-c", connectCommand(origin, token)], { env: { HOME: home, PATH: `${bin}:/usr/bin:/bin` } });
        const read = (f: string) => execFileSync("cat", [join(home, f)], { encoding: "utf8" });
        const hub = hubUrl(origin);
        assert.equal(read("header"), `Authorization: Bearer ${token}`);
        assert.equal(read("url"), `${hub}/tools/mlxlab/Darwin-arm64`);
        assert.equal(read("ran"), `${token}|worker|--hub|${hub}|`);
      } finally {
        rmSync(home, { recursive: true, force: true });
      }
    }
  }
});

test("maskToken: the token never on screen", () => {
  for (const token of ["mlxw_0123456789abcdef", "it's-a-long-token-x"]) {
    const shown = maskToken(connectCommand("https://hub.example.com", token), token);
    assert.ok(!shown.includes(token.slice(5)), shown);
    assert.ok(shown.includes("••••"));
  }
});
