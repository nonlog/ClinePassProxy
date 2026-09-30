"use strict";

const test = require("node:test");
const assert = require("node:assert");
const fmt = require("./format.js");

test("scaled renders the official token magnitudes", () => {
  assert.equal(fmt.scaled(827430000), "827.43M");
  assert.equal(fmt.scaled(819200000), "819.20M");
  assert.equal(fmt.scaled(1500), "1.50K");
  assert.equal(fmt.scaled(42), "42");
  assert.equal(fmt.scaled(null), "—");
});

test("percent and ratio keep missing values missing", () => {
  assert.equal(fmt.percent(1.04, 1), "1.0%");
  assert.equal(fmt.percent(undefined), "—");
  assert.equal(fmt.ratio(0.5), "50.00%");
  assert.equal(fmt.ratio(null), "—");
});

test("ms formats each duration band", () => {
  assert.equal(fmt.ms(0), "0ms");
  assert.equal(fmt.ms(820), "820ms");
  assert.equal(fmt.ms(12400), "12.4s");
  assert.equal(fmt.ms(185000), "3m05s");
  assert.equal(fmt.ms(-1), "—");
});

test("countdown renders days, hours and seconds", () => {
  assert.equal(fmt.countdown(3 * 3600 + 50 * 60), "3h50m");
  assert.equal(fmt.countdown(90), "1m30s");
  assert.equal(fmt.countdown(30), "30s");
  assert.equal(fmt.countdown(0), "—");
});

test("goDuration reads Go's duration strings", () => {
  assert.equal(fmt.goDuration("3h50m0s"), 3 * 3600 + 50 * 60);
  assert.equal(fmt.goDuration("1h30m12.5s"), 5412.5);
  assert.equal(fmt.goDuration("250ms"), 0.25);
  assert.equal(fmt.goDuration("2m"), 120);
  assert.equal(fmt.goDuration("0s"), 0);
  assert.equal(fmt.goDuration(""), null);
  assert.equal(fmt.goDuration("soon"), null);
  assert.equal(fmt.goDuration("1h remaining"), null);
});

test("money keeps four decimals for micro-USD scale balances", () => {
  assert.equal(fmt.money(12.5), "$12.5000");
  assert.equal(fmt.money(undefined), "—");
});
