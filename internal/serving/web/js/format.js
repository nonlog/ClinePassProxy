/* Formatting helpers. Pure functions only: the page code passes values in and
   renders the returned strings, and node --test exercises them directly. */
(function (root) {
  "use strict";

  function isNumber(value) {
    return typeof value === "number" && isFinite(value);
  }

  // number returns value when it is a usable number, otherwise null, so
  // callers can render "—" instead of NaN.
  function number(value) {
    if (isNumber(value)) return value;
    if (typeof value === "string" && value.trim() !== "") {
      var parsed = Number(value);
      if (isFinite(parsed)) return parsed;
    }
    return null;
  }

  var DASH = "—";

  function fixed(value, digits) {
    var n = number(value);
    if (n === null) return DASH;
    return n.toFixed(digits === undefined ? 2 : digits);
  }

  function int(value) {
    var n = number(value);
    if (n === null) return DASH;
    return Math.round(n).toLocaleString("en-US");
  }

  // scaled renders a large count with a K/M/B suffix: 827.43M.
  function scaled(value, digits) {
    var n = number(value);
    if (n === null) return DASH;
    var places = digits === undefined ? 2 : digits;
    var abs = Math.abs(n);
    if (abs >= 1e9) return (n / 1e9).toFixed(places) + "B";
    if (abs >= 1e6) return (n / 1e6).toFixed(places) + "M";
    if (abs >= 1e3) return (n / 1e3).toFixed(places) + "K";
    return String(Math.round(n));
  }

  function percent(value, digits) {
    var n = number(value);
    if (n === null) return DASH;
    return n.toFixed(digits === undefined ? 1 : digits) + "%";
  }

  // ratio renders a 0..1 fraction as a percentage.
  function ratio(value, digits) {
    var n = number(value);
    if (n === null) return DASH;
    return percent(n * 100, digits === undefined ? 2 : digits);
  }

  function money(value, digits) {
    var n = number(value);
    if (n === null) return DASH;
    return "$" + n.toFixed(digits === undefined ? 4 : digits);
  }

  // duration renders milliseconds compactly: 820ms, 12.4s, 3m05s.
  function ms(value) {
    var n = number(value);
    if (n === null) return DASH;
    if (n < 0) return DASH;
    if (n < 1000) return Math.round(n) + "ms";
    if (n < 60000) return (n / 1000).toFixed(1) + "s";
    var minutes = Math.floor(n / 60000);
    var seconds = Math.round((n % 60000) / 1000);
    if (seconds === 60) { minutes += 1; seconds = 0; }
    return minutes + "m" + String(seconds).padStart(2, "0") + "s";
  }

  // seconds renders a second count as 3h50m or 12m06s.
  function countdown(totalSeconds) {
    var n = number(totalSeconds);
    if (n === null || n <= 0) return DASH;
    var seconds = Math.round(n);
    var days = Math.floor(seconds / 86400);
    var hours = Math.floor((seconds % 86400) / 3600);
    var minutes = Math.floor((seconds % 3600) / 60);
    var secs = seconds % 60;
    if (days > 0) return days + "d" + hours + "h";
    if (hours > 0) return hours + "h" + String(minutes).padStart(2, "0") + "m";
    if (minutes > 0) return minutes + "m" + String(secs).padStart(2, "0") + "s";
    return secs + "s";
  }

  // goDuration parses Go's time.Duration.String() output ("3h50m12.5s").
  // Returns null for anything it cannot read, so the caller keeps its own
  // fallback instead of printing a wrong countdown.
  function goDuration(text) {
    if (typeof text !== "string") return null;
    var source = text.trim();
    if (source === "" || source === "0s") return source === "0s" ? 0 : null;
    var sign = 1;
    if (source[0] === "-") { sign = -1; source = source.slice(1); }
    else if (source[0] === "+") { source = source.slice(1); }
    var pattern = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
    var total = 0;
    var consumed = 0;
    var match;
    while ((match = pattern.exec(source)) !== null) {
      consumed += match[0].length;
      var value = parseFloat(match[1]);
      switch (match[2]) {
        case "h": total += value * 3600; break;
        case "m": total += value * 60; break;
        case "s": total += value; break;
        case "ms": total += value / 1000; break;
        case "us": case "µs": total += value / 1e6; break;
        case "ns": total += value / 1e9; break;
      }
    }
    if (consumed !== source.length) return null;
    return sign * total;
  }

  function date(value) {
    if (!value) return DASH;
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) return String(value);
    return parsed.toLocaleString("sv-SE", { hour12: false });
  }

  function clock(value) {
    if (!value) return DASH;
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) return String(value);
    return parsed.toLocaleTimeString("sv-SE", { hour12: false });
  }

  function ago(value) {
    if (!value) return DASH;
    var parsed = new Date(value);
    if (isNaN(parsed.getTime())) return String(value);
    var delta = (Date.now() - parsed.getTime()) / 1000;
    if (delta < 0) return clock(value);
    if (delta < 60) return Math.round(delta) + " 秒前";
    if (delta < 3600) return Math.round(delta / 60) + " 分钟前";
    if (delta < 86400) return Math.round(delta / 3600) + " 小时前";
    return Math.round(delta / 86400) + " 天前";
  }

  var API = {
    DASH: DASH,
    number: number,
    fixed: fixed,
    int: int,
    scaled: scaled,
    percent: percent,
    ratio: ratio,
    money: money,
    ms: ms,
    countdown: countdown,
    goDuration: goDuration,
    date: date,
    clock: clock,
    ago: ago
  };

  root.CPPFormat = API;
  if (typeof module === "object" && module.exports) module.exports = API;
})(typeof globalThis !== "undefined" ? globalThis : this);
