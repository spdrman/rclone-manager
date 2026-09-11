// The built bundle needs nothing off the machine it runs on (issue #631).
//
// This is the half of H2.5 that markup cannot answer. Reading index.html
// and finding no CDN in it proves that one file names no CDN, and the
// thing that actually broke on the NAS was a page: a link element, the
// stylesheet it pulls, the font files that stylesheet asks for, and any
// one of those four able to reach off the box. So this builds the real
// production bundle, serves it over loopback, refuses every connection
// that is not loopback at the socket, and walks the page the way a
// browser would.
//
// It is deliberately not a browser. There is no Playwright in this
// workspace and there should not be: the browser suite left for
// spdrman/backupd-tests in #158 and re-adding it here to check one
// property would undo that. What this loses is layout and what the user
// sees; what it keeps is the property under test, which is whether
// anything the page loads resolves off this host. The socket trap is what
// makes that a proof rather than an inspection, and the control below
// proves the trap fires, because a trap nobody has seen refuse anything
// is indistinguishable from no trap at all.
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync } from "node:fs";
import { readFile, readdir, stat } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { resolve, dirname, join, extname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");

// The hosts a request may resolve to and still be inside this machine.
// An empty host is a unix socket or a same-process handle, neither of
// which leaves the box.
const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost", ""]);

/** Every non-loopback connection anything in this process attempted. */
const denied = [];

const realConnect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function egressTrap(...args) {
  const options = typeof args[0] === "object" && args[0] !== null ? args[0] : { host: args[1] };
  const host = String(options.host ?? options.path ?? "");
  if (!LOOPBACK.has(host)) {
    denied.push(host);
    throw new Error(`egress denied: ${host}`);
  }
  return realConnect.apply(this, args);
};

const MIME = {
  ".html": "text/html",
  ".css": "text/css",
  ".js": "text/javascript",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
  ".txt": "text/plain"
};

let outDir = "";
let origin = "";
let server;

beforeAll(async () => {
  outDir = mkdtempSync(resolve(tmpdir(), "offline-bundle-"));

  // The local Vite binary through this process's own node, never npx:
  // npx is allowed to go and fetch a package it cannot find, and this
  // test's whole subject is a build that needs no network.
  const vite = resolve(root, "node_modules", "vite", "bin", "vite.js");
  const built = spawnSync(process.execPath, [vite, "build", "--outDir", outDir, "--emptyOutDir"], {
    cwd: root,
    encoding: "utf8"
  });
  if (built.status !== 0) {
    throw new Error(`vite build failed (${built.status}):\n${built.stdout}\n${built.stderr}`);
  }

  server = createServer((req, res) => {
    const path = decodeURIComponent(new URL(req.url, "http://127.0.0.1").pathname);
    const file = join(outDir, path === "/" ? "index.html" : path);
    stat(file)
      .then(() => readFile(file))
      .then((body) => {
        res.writeHead(200, { "content-type": MIME[extname(file)] ?? "application/octet-stream" });
        res.end(body);
      })
      .catch(() => {
        res.writeHead(404);
        res.end("not in the bundle");
      });
  });
  await new Promise((ok) => server.listen(0, "127.0.0.1", ok));
  origin = `http://127.0.0.1:${server.address().port}`;
}, 600_000);

afterAll(async () => {
  net.Socket.prototype.connect = realConnect;
  if (server) await new Promise((ok) => server.close(ok));
});

// Nothing here asserts a Content-Type, and that is measured rather than
// overlooked. The runtime image is distroless with no /etc/mime.types and
// Go's mime table has no .woff2 entry, so serve-ui's http.FileServer sniffs
// these files to application/octet-stream, and SecurityHeaders sends
// X-Content-Type-Options: nosniff over the top of that. It reads like the
// combination that would block a font, and it does not: nosniff's blocking
// covers script-like and stylesheet destinations, not fonts. Checked rather
// than reasoned about, with a real Chromium against a server forced into
// exactly that pair of headers, where document.fonts reported IBM Plex Sans
// 400, IBM Plex Sans 700 and IBM Plex Mono 600 all loaded.

/** Fetch one URL off the loopback server, failing loudly on a 404. */
async function get(url) {
  const res = await fetch(url);
  expect(res.status, `${url} is referenced by the built page and the bundle does not carry it`).toBe(200);
  return res;
}

describe("the built bundle runs on a host with no route off itself", () => {
  it("refuses a connection off this machine, so the walk below means something", async () => {
    // The positive control, and the reason this file is a proof rather
    // than a claim. Everything underneath passes trivially if the trap
    // is not installed or stops matching.
    const before = denied.length;
    await expect(fetch("https://fonts.googleapis.com/css2?family=IBM+Plex+Sans")).rejects.toThrow();
    expect(
      denied.slice(before),
      "the trap let a request to fonts.googleapis.com through, so nothing below is evidence of anything"
    ).toContain("fonts.googleapis.com");
  }, 60_000);

  it("resolves the whole page, every stylesheet and every font, without one", async () => {
    const attemptedBefore = denied.length;

    const html = await (await get(`${origin}/`)).text();

    const subresources = [];
    for (const m of html.matchAll(/(?:href|src)=["']([^"']+)["']/g)) subresources.push(m[1]);
    expect(subresources.length, "the built index.html references nothing at all, so this walk read nothing").toBeGreaterThan(1);

    const fetched = [];
    const walk = async (url) => {
      if (fetched.includes(url)) return;
      fetched.push(url);
      const res = await get(url);
      if (!(res.headers.get("content-type") ?? "").startsWith("text/css")) return;
      const css = await res.text();
      const nested = [...css.matchAll(/url\(\s*["']?([^"')]+)["']?\s*\)/g)].map((m) => m[1]);
      for (const ref of nested) {
        if (ref.startsWith("data:")) continue;
        await walk(new URL(ref, url).toString());
      }
    };
    for (const ref of subresources) {
      if (ref.startsWith("data:") || ref.startsWith("#")) continue;
      await walk(new URL(ref, `${origin}/`).toString());
    }

    // Seven faces, and the count is the point: a walk that reached the
    // stylesheet and none of its fonts would pass every assertion above.
    const fonts = fetched.filter((url) => url.endsWith(".woff2"));
    expect(fonts.length, `the walk reached ${fonts.length} woff2 files: ${fetched.join(", ")}`).toBe(7);

    expect(
      denied.slice(attemptedBefore),
      "loading the built page attempted a connection off this machine, which is the defect #631 is about"
    ).toEqual([]);
  }, 600_000);

  it("carries the font licence where a recipient of the image finds it", async () => {
    // OFL-1.1 §2 asks that each copy of the Font Software carry the
    // copyright notice and the licence. The image is a copy, and this is
    // the path inside it.
    const licence = await (await get(`${origin}/fonts/LICENSE.txt`)).text();
    for (const phrase of ["SIL OPEN FONT LICENSE Version 1.1", "IBM Corp.", 'Reserved Font Name "Plex"']) {
      expect(licence, `the shipped licence text never says ${phrase}`).toContain(phrase);
    }
  }, 60_000);

  it("names no font CDN in any file it ships", async () => {
    // The artifact-level form of the markup check in
    // src/test/vendored-fonts.test.ts. A CSS import that survived
    // bundling, or a string in the JavaScript, would be invisible to a
    // walk that only follows what the page declares.
    const files = [];
    const collect = async (dir) => {
      for (const entry of await readdir(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) await collect(path);
        else files.push(path);
      }
    };
    await collect(outDir);
    expect(files.length, "the build produced no files, so this sweep read nothing").toBeGreaterThan(3);

    for (const path of files) {
      if (path.endsWith(".woff2")) continue;
      const body = await readFile(path, "utf8");
      for (const host of ["fonts.googleapis.com", "fonts.gstatic.com"]) {
        expect(body.includes(host), `${path.slice(outDir.length)} still names ${host}`).toBe(false);
      }
    }
  }, 120_000);

  it("spends what the record says it spends", async () => {
    // The number #631 asks to be written down, checked rather than
    // asserted in prose. The image carries this payload six times over:
    // once compiled into the binary and once per bundle at /ui/bundles,
    // so the ceiling that matters is six times this one.
    //
    // 200 KB is not a target, it is the line past which the six-times
    // arithmetic stops being obviously fine against the image budget's
    // remaining headroom. Shipping the complete faces instead of the
    // Latin1 subsets would put this at about 414 KB, which is over.
    const fonts = join(outDir, "fonts");
    let bytes = 0;
    const names = await readdir(fonts);
    for (const name of names) {
      if (!name.endsWith(".woff2")) continue;
      bytes += (await stat(join(fonts, name))).size;
    }
    expect(bytes, "the bundle carries no woff2 at all").toBeGreaterThan(0);
    expect(
      bytes,
      `the vendored faces are ${bytes} bytes, and the image carries six copies of them`
    ).toBeLessThan(200_000);
  }, 60_000);
});
