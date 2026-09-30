"use strict";

/* Theme coverage is a static property of the CSS, so check it without a
   browser: a token defined for light but missing from dark silently keeps the
   light value, which is exactly the "background swapped, text did not" bug. */

const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const WEB = path.join(__dirname, "..");
// Comments mention selectors by name; strip them so the block lookup below
// cannot lock onto a comment instead of the rule.
const theme = fs.readFileSync(path.join(WEB, "theme.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
const app = fs.readFileSync(path.join(WEB, "app.css"), "utf8");
const index = fs.readFileSync(path.join(WEB, "index.html"), "utf8");

function tokens(block) {
  const found = new Set();
  for (const match of block.matchAll(/(--[a-z0-9-]+)\s*:/g)) found.add(match[1]);
  return found;
}

function blockAfter(selector) {
  const start = theme.indexOf(selector);
  assert.notEqual(start, -1, selector + " is missing from theme.css");
  const open = theme.indexOf("{", start);
  const close = theme.indexOf("}", open);
  return theme.slice(open + 1, close);
}

// Fonts and radii are theme-independent; everything else must exist in both.
const SHARED_OK = new Set(["--mono", "--sans", "--radius", "--radius-sm"]);

test("every design token exists in both themes", () => {
  const light = tokens(blockAfter(":root"));
  const dark = tokens(blockAfter('[data-theme="dark"]'));
  const missingInDark = [...light].filter((name) => !dark.has(name) && !SHARED_OK.has(name));
  const missingInLight = [...dark].filter((name) => !light.has(name) && !SHARED_OK.has(name));
  assert.deepEqual(missingInDark, [], "tokens missing from the dark theme");
  assert.deepEqual(missingInLight, [], "tokens missing from the light theme");
});

test("both themes declare a color-scheme", () => {
  assert.match(blockAfter(":root"), /color-scheme:\s*light/);
  assert.match(blockAfter('[data-theme="dark"]'), /color-scheme:\s*dark/);
});

test("page styles use tokens instead of hardcoded colors", () => {
  const hardcoded = [...app.matchAll(/#[0-9a-fA-F]{3,8}\b/g)].map((match) => match[0]);
  assert.deepEqual(hardcoded, [], "app.css must not hardcode colors");
  const rgba = [...app.matchAll(/rgba?\(/g)].map((match) => match[0]);
  assert.deepEqual(rgba, [], "app.css must not hardcode colors");
});

test("the theme is resolved before any CSS is applied", () => {
  const bootstrap = index.indexOf("dataset.theme");
  const cssMarker = index.indexOf("<!--assets:css-->");
  assert.notEqual(bootstrap, -1, "index.html has no theme bootstrap");
  assert.notEqual(cssMarker, -1, "index.html has no asset marker");
  assert.ok(bootstrap < cssMarker, "the theme must be applied before the stylesheets");
});
