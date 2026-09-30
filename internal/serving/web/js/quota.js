/* Shapes the official Cline account snapshot into what the Usage page draws.
   Pure functions so node --test can check the quota maths without a browser. */
(function (root) {
  "use strict";

  var fmt = root.CPPFormat || (typeof require === "function" ? require("./format.js") : null);

  var ORDER = { five_hour: 0, weekly: 1, monthly: 2 };

  function windowKey(type) {
    var value = String(type === undefined || type === null ? "" : type).trim().toLowerCase();
    switch (value) {
      case "five_hour": case "five_hours": case "5_hour": case "5h": return "five_hour";
      case "weekly": case "week": return "weekly";
      case "monthly": case "month": return "monthly";
      default: return value;
    }
  }

  function windowSort(a, b) {
    var left = ORDER[windowKey(a)] === undefined ? 9 : ORDER[windowKey(a)];
    var right = ORDER[windowKey(b)] === undefined ? 9 : ORDER[windowKey(b)];
    if (left !== right) return left - right;
    return String(a).localeCompare(String(b));
  }

  // limits returns the rolling quota windows in a stable order. A window
  // without a usable percent keeps percent = null so the UI can print "—"
  // rather than a fabricated 0%.
  function limits(snapshot) {
    var list = (snapshot && snapshot.limits) || [];
    var out = [];
    for (var i = 0; i < list.length; i++) {
      var item = list[i] || {};
      var type = windowKey(item.type);
      if (!type) continue;
      out.push({
        type: type,
        label: item.label || item.type || type,
        percent: fmt.number(item.percent_used),
        remaining: fmt.number(item.percent_used) === null ? null : 100 - fmt.number(item.percent_used),
        resetsAt: item.resets_at || "",
        resetsIn: item.resets_in || ""
      });
    }
    out.sort(function (a, b) { return windowSort(a.type, b.type); });
    return out;
  }

  // resetText prefers the absolute reset timestamp: resets_in is computed when
  // the snapshot was taken and goes stale, while resetsAt stays correct.
  function resetText(window, nowMs) {
    var now = fmt.number(nowMs) === null ? Date.now() : nowMs;
    if (window && window.resetsAt) {
      var at = new Date(window.resetsAt);
      if (!isNaN(at.getTime())) {
        var seconds = (at.getTime() - now) / 1000;
        if (seconds <= 0) return "即将重置";
        return fmt.countdown(seconds);
      }
    }
    var parsed = fmt.goDuration(window && window.resetsIn);
    if (parsed === null || parsed <= 0) return fmt.DASH;
    return fmt.countdown(parsed);
  }

  function resetAtText(window) {
    if (!window || !window.resetsAt) return fmt.DASH;
    return fmt.date(window.resetsAt);
  }

  function tint(percent) {
    var n = fmt.number(percent);
    if (n === null) return "";
    if (n >= 90) return "bad";
    if (n >= 70) return "warn";
    return "ok";
  }

  // tokens normalises the official 31-day totals. `hasTotals` is false when the
  // daily endpoint answered with nothing, so the caller renders "—".
  function tokens(snapshot) {
    var raw = (snapshot && snapshot.tokens) || {};
    var input = fmt.number(raw.input_tokens);
    var output = fmt.number(raw.output_tokens);
    var total = fmt.number(raw.total_tokens);
    if (total === null && (input !== null || output !== null)) {
      total = (input || 0) + (output || 0);
    }
    var cost = fmt.number(raw.cost_usd);
    return {
      from: raw.from_date || "",
      to: raw.to_date || "",
      input: input,
      output: output,
      total: total,
      cost: cost !== null && cost > 0 ? cost : null,
      balance: fmt.number(raw.balance_usd),
      billingItems: fmt.number(raw.requests),
      series: raw.series || [],
      hasTotals: total !== null && total > 0
    };
  }

  function find(snapshots, credentialId) {
    var list = snapshots || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i] && list[i].credential_id === credentialId) return list[i];
    }
    return null;
  }

  // status explains one snapshot in one word for a table cell.
  function status(snapshot) {
    if (!snapshot) return { text: "未查询", kind: "" };
    if (snapshot.rejected) return { text: "凭据被拒", kind: "bad" };
    if (snapshot.available) {
      if (snapshot.tokens_error) return { text: "汇总失败", kind: "warn" };
      return { text: "正常", kind: "ok" };
    }
    return { text: "不可用", kind: "warn" };
  }

  var API = {
    windowKey: windowKey,
    windowSort: windowSort,
    limits: limits,
    resetText: resetText,
    resetAtText: resetAtText,
    tint: tint,
    tokens: tokens,
    find: find,
    status: status
  };

  root.CPPQuota = API;
  if (typeof module === "object" && module.exports) module.exports = API;
})(typeof globalThis !== "undefined" ? globalThis : this);
