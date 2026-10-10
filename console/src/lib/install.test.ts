/// <reference types="node" />
// pnpm test: node --test (Node strips the types; no test framework needed).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { guessOS, installCommand, maskToken, psq, restartCommand, serverUrl, shq, UNIX_BIN, unixInstallCommand, windowsInstallCommand } from "./install.ts";

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

test("psq: quotes are doubled, nothing else changes", () => {
  assert.equal(psq("plain"), "'plain'");
  assert.equal(psq("it's"), "'it''s'");
  assert.equal(psq("$env:X `n"), "'$env:X `n'");
  assert.equal(psq("a’b"), "'a’’b'");
});

test("serverUrl: trims and drops trailing slashes", () => {
  assert.equal(serverUrl(" https://ai.example.com/// "), "https://ai.example.com");
  assert.equal(serverUrl("http://10.0.0.2:8080"), "http://10.0.0.2:8080");
});

test("unix: downloads for this machine's uname, then starts the worker with the token in its environment", () => {
  const c = unixInstallCommand("https://ai.example.com/", "tok");
  assert.equal(
    c,
    [
      "mkdir -p ~/.config/mlxlab/bin",
      `&& curl -fsSL -H 'Authorization: Bearer tok' -o ~/.config/mlxlab/bin/ai-connector 'https://ai.example.com/tools/mlxlab/'"$(uname -s)-$(uname -m)"`,
      "&& chmod +x ~/.config/mlxlab/bin/ai-connector",
      "&& MLXLAB_WORKER_TOKEN='tok' ~/.config/mlxlab/bin/ai-connector worker --hub 'https://ai.example.com'",
    ].join(" \\\n  "),
  );
  assert.equal(installCommand("unix", "https://ai.example.com", "tok"), unixInstallCommand("https://ai.example.com", "tok"));
});

test("unix: the pasted command runs in sh, bash and zsh (fake curl and HOME)", () => {
  for (const sh of shells) {
    const dir = mkdtempSync(join(tmpdir(), "console-install-"));
    try {
      const bin = join(dir, "fakebin");
      mkdirSync(bin);
      // curl writes a program that prints its environment's token and its arguments
      writeFileSync(
        join(bin, "curl"),
        `#!/bin/sh\nout=""; url=""; hdr=""\nwhile [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; -H) hdr="$2"; shift 2;; -*) shift;; *) url="$1"; shift;; esac; done\nprintf '%s\\n%s\\n' "$url" "$hdr" > "$HOME/curl.log"\nprintf '#!/bin/sh\\nprintf "%%s|%%s" "$MLXLAB_WORKER_TOKEN" "$*" > "$HOME/run.log"\\n' > "$out"\n`,
      );
      chmodSync(join(bin, "curl"), 0o755);
      const token = "aic_it's $(id)";
      execFileSync(sh, ["-c", unixInstallCommand("https://ai.example.com", token)], { env: { PATH: `${bin}:/usr/bin:/bin`, HOME: dir } });
      const [url, hdr] = readFileSync(join(dir, "curl.log"), "utf8").split("\n");
      assert.match(url, /^https:\/\/ai\.example\.com\/tools\/mlxlab\/[A-Za-z]+-[A-Za-z0-9_]+$/);
      assert.equal(hdr, `Authorization: Bearer ${token}`);
      assert.equal(readFileSync(join(dir, "run.log"), "utf8"), `${token}|worker --hub https://ai.example.com`);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

test("windows: one script block that downloads windows-amd64 and starts the worker", () => {
  const c = windowsInstallCommand("https://ai.example.com/", "to'k");
  assert.ok(c.startsWith("& {\n") && c.endsWith("\n}"));
  assert.ok(c.includes("$ErrorActionPreference = 'Stop'"));
  assert.ok(c.includes("Join-Path $env:LOCALAPPDATA 'ai-connector'"));
  assert.ok(c.includes("-Headers @{ Authorization = 'Bearer to''k' }"));
  assert.ok(c.includes("-Uri 'https://ai.example.com/tools/mlxlab/windows-amd64'"));
  assert.ok(c.includes("$env:MLXLAB_WORKER_TOKEN = 'to''k'"));
  assert.ok(c.includes("& $exe worker --hub 'https://ai.example.com'"));
  assert.equal(installCommand("windows", "https://ai.example.com/", "to'k"), c);
});

test("restart and OS guess", () => {
  assert.equal(restartCommand("unix"), `${UNIX_BIN} worker`);
  assert.equal(restartCommand("windows"), `& "$env:LOCALAPPDATA\\ai-connector\\ai-connector.exe" worker`);
  assert.equal(guessOS("Mozilla/5.0 (Windows NT 10.0; Win64; x64)"), "windows");
  assert.equal(guessOS("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"), "unix");
});

test("maskToken hides the token in both shells' quoting", () => {
  const token = "abcdefghijklmnop'q";
  for (const c of [unixInstallCommand("https://x", token), windowsInstallCommand("https://x", token)]) {
    const m = maskToken(c, token);
    assert.ok(!m.includes("efghijkl"), m);
    assert.ok(m.includes("abcd••••••••••••"));
  }
  assert.equal(maskToken("text", ""), "text");
});
