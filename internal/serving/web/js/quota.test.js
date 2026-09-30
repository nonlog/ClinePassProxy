"use strict";

const test = require("node:test");
const assert = require("node:assert");
const quota = require("./quota.js");

test("limits orders the rolling windows and keeps unknown types", () => {
  const rows = quota.limits({
    limits: [
      { type: "monthly", label: "本月额度", percent_used: 12 },
      { type: "five_hour", label: "5 小时滚动窗口", percent_used: 1 },
      { type: "weekly", label: "本周额度", percent_used: 40 },
      { type: "annual", percent_used: 3 }
    ]
  });
  assert.deepEqual(rows.map((row) => row.type), ["five_hour", "weekly", "monthly", "annual"]);
  assert.equal(rows[0].remaining, 99);
  assert.equal(rows[3].label, "annual");
});

test("a window without a percent stays unusable instead of becoming 0%", () => {
  const rows = quota.limits({ limits: [{ type: "weekly", percent_used: null }] });
  assert.equal(rows[0].percent, null);
  assert.equal(rows[0].remaining, null);
});

test("resetText prefers the absolute reset time over the stale countdown", () => {
  const now = Date.parse("2026-09-30T01:00:00Z");
  const limit = { resetsAt: "2026-09-30T04:50:00Z", resetsIn: "1h0m0s" };
  assert.equal(quota.resetText(limit, now), "3h50m");
});

test("resetText falls back to the reported countdown when the time is missing", () => {
  assert.equal(quota.resetText({ resetsIn: "3h50m12s" }, Date.now()), "3h50m");
  assert.equal(quota.resetText({}, Date.now()), "—");
  assert.equal(quota.resetText({ resetsAt: "not-a-date", resetsIn: "" }, Date.now()), "—");
});

test("an elapsed reset time says it is about to reset", () => {
  const now = Date.parse("2026-09-30T05:00:00Z");
  assert.equal(quota.resetText({ resetsAt: "2026-09-30T04:50:00Z" }, now), "即将重置");
});

test("tokens sums input and output when the upstream omits the total", () => {
  const tokens = quota.tokens({
    tokens: { input_tokens: 819200000, output_tokens: 8240000, balance_usd: 12.5, cost_usd: 0, requests: 15 }
  });
  assert.equal(tokens.total, 827440000);
  assert.equal(tokens.hasTotals, true);
  assert.equal(tokens.cost, null, "a zero cost is reported as unavailable, not as $0");
  assert.equal(tokens.billingItems, 15);
});

test("tokens reports missing data instead of zero", () => {
  const tokens = quota.tokens(null);
  assert.equal(tokens.total, null);
  assert.equal(tokens.hasTotals, false);
  assert.equal(tokens.billingItems, null);
});

test("tint escalates with quota pressure", () => {
  assert.equal(quota.tint(1), "ok");
  assert.equal(quota.tint(70), "warn");
  assert.equal(quota.tint(100), "bad");
  assert.equal(quota.tint(null), "");
});

test("find and status describe a credential snapshot", () => {
  const snapshots = [
    { credential_id: "a", available: true },
    { credential_id: "b", available: false, error: "boom" },
    { credential_id: "c", available: false, rejected: true }
  ];
  assert.equal(quota.find(snapshots, "b").error, "boom");
  assert.equal(quota.find(snapshots, "missing"), null);
  assert.equal(quota.status(snapshots[0]).kind, "ok");
  assert.equal(quota.status(snapshots[1]).text, "不可用");
  assert.equal(quota.status(snapshots[2]).text, "凭据被拒");
  assert.equal(quota.status(null).text, "未查询");
});
