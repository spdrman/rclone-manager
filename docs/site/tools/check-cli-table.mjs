// Holds this site's command table to the binary's own dispatch table.
//
// Run it from the repository root:
//
//     node docs/site/tools/check-cli-table.mjs
//
// Exit 0 and one summary line when the two agree. Exit 1 and a list of
// what is wrong when they do not.
//
// # Why this exists
//
// The README's copy of the command table is checked against
// core/cmd/backup-manager/main.go on every run of the gate, and it sits
// beside a machine-checked region for exactly that reason. This site's
// copy was a hand transcription with a disclaimer under it saying so, and
// the first review of that transcription found two errors in it. A
// disclaimer is not a check: it tells a reader which of two documents to
// believe and does nothing to stop the wrong one being written.
//
// So this checks the two things a transcription gets wrong. Every command
// the binary dispatches has a row on cli.html, so a new verb cannot land
// with the page silently short of it. And every `rbm <word>` written in a
// code block anywhere on the site names a command the binary actually
// has, which is the half that catches a typo, a verb that was renamed,
// and a command somebody remembered rather than looked up.
//
// # What it does not check
//
// What a command DOES. The one-line descriptions on cli.html are prose
// and no test can decide them; two of the three defects the review found
// were of that kind (an exit code and a default path) and this would not
// have caught either. It catches the shape, which is the part that can be
// caught, and the PR that added it says as much rather than letting a
// green check stand for more than it proves.
//
// It is also not wired into the repository's gate, because the gate's
// script is not this branch's to edit. Adding it there is one line.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const SITE = resolve(HERE, "..");
const REPO = resolve(SITE, "../..");
const MAIN_GO = resolve(REPO, "core/cmd/backup-manager/main.go");
const TABLE_PAGE = resolve(SITE, "cli.html");

/** The dispatch table, read out of the map literal that is the single
 *  source of truth for what this binary answers to. Anchored on a tab and
 *  a quoted key followed by a cmd* function, which is the shape every
 *  entry has and which no other line in that file has. */
function dispatched() {
  const src = readFileSync(MAIN_GO, "utf8");
  const names = [...src.matchAll(/^\t"([a-z-]+)":\s+cmd[A-Za-z]+,$/gm)].map((m) => m[1]);
  if (names.length < 10) {
    throw new Error(
      "found only " + names.length + " commands in " + MAIN_GO + ", which means the map's shape " +
        "changed and this reader is now measuring nothing. Fix the pattern rather than the count."
    );
  }
  return new Set(names);
}

/** Every command the site's table has a row for. A row's first column is
 *  `<code>rbm NAME ...</code>`, and NAME is the top-level command however
 *  many subcommand words follow it. */
function tabled() {
  const html = readFileSync(TABLE_PAGE, "utf8");
  const body = html.slice(html.indexOf('<h2 id="table"'));
  return new Set([...body.matchAll(/<td><code>rbm ([a-z-]+)/g)].map((m) => m[1]));
}

/** Every command invoked anywhere on the site, with the page it is on, so
 *  a failure says where to go. Deliberately across all pages and not only
 *  the table: a verb invented in a sentence is the same defect as a verb
 *  missing from the table, one paragraph over.
 *
 *  Only inside <code>, which is the difference between an invocation and
 *  a sentence. "The rbm command line" is English and names nothing;
 *  <code>rbm medium list</code> is a claim that `medium` exists. Matching
 *  running text instead read the page title as a command called
 *  `command`, which is the checker being wrong about the site rather than
 *  the other way round. */
function mentioned() {
  const found = new Map();
  for (const file of readdirSync(SITE).filter((f) => f.endsWith(".html"))) {
    const html = readFileSync(resolve(SITE, file), "utf8");
    for (const block of html.matchAll(/<code[^>]*>([\s\S]*?)<\/code>/g)) {
      // Tags inside a highlighted block, and entities, are not part of
      // the command line an operator would type.
      const text = block[1].replace(/<[^>]+>/g, "").replace(/&[a-z]+;/g, " ");
      for (const m of text.matchAll(/\brbm ([a-z][a-z-]*)/g)) {
        const name = m[1];
        if (!found.has(name)) found.set(name, new Set());
        found.get(name).add(file);
      }
    }
  }
  return found;
}

const commands = dispatched();
const rows = tabled();
const prose = mentioned();

const problems = [];

for (const name of [...commands].sort()) {
  if (!rows.has(name)) {
    problems.push("cli.html has no row for `rbm " + name + "`, which the binary dispatches");
  }
}
for (const name of [...rows].sort()) {
  if (!commands.has(name)) {
    problems.push("cli.html has a row for `rbm " + name + "`, which the binary does not dispatch");
  }
}
for (const [name, files] of [...prose.entries()].sort()) {
  if (!commands.has(name)) {
    problems.push(
      "`rbm " + name + "` is written on " + [...files].sort().join(", ") + " and is not a command"
    );
  }
}

if (problems.length > 0) {
  console.error("docs/site's command table disagrees with " + MAIN_GO + ":\n");
  for (const p of problems) console.error("  " + p);
  console.error(
    "\nThe binary's dispatch table is right and this site is a transcription of it.\n" +
      "Fix docs/site/cli.html, not core/cmd/backup-manager/main.go."
  );
  process.exit(1);
}

console.log(
  "docs/site/cli.html covers all " + commands.size + " dispatched commands, and the " +
    prose.size + " invoked in code blocks across the site all exist"
);
