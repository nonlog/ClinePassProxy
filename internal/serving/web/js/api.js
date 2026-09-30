/* Management API client.

   Every call goes through api(): it always shows a failure, never returns a
   half-parsed body, and never swallows an HTTP error. Inside CPAMP a shim
   rewrites these same-origin /api/... requests onto the CPA connector, so the
   path must stay a literal starting with "/api/". */
(function (root) {
  "use strict";

  function ApiError(message, status) {
    var error = new Error(message);
    error.name = "ApiError";
    error.status = status;
    return error;
  }

  var bannerText = "";
  var bannerKind = "";

  function banner(message, kind) {
    bannerText = message || "";
    bannerKind = kind || "";
    var element = document.getElementById("banner");
    if (!element) return;
    if (!bannerText) {
      element.textContent = "";
      element.className = "banner hidden";
      return;
    }
    element.textContent = bannerText;
    element.className = "banner" + (bannerKind === "bad" ? " bad" : "");
  }

  async function api(path, options) {
    var init = Object.assign({ headers: {} }, options || {});
    if (init.body !== undefined && !init.headers["Content-Type"]) {
      init.headers["Content-Type"] = "application/json";
    }
    var response;
    try {
      response = await fetch(path, init);
    } catch (error) {
      banner("无法连接到 ClinePassProxy：" + error.message, "bad");
      throw ApiError("网络错误：" + error.message, 0);
    }
    var text = await response.text();
    var payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch (error) {
        payload = null;
      }
    }
    if (!response.ok) {
      var message = payload && payload.error ? payload.error : "HTTP " + response.status;
      if (response.status === 401) {
        banner("管理认证失败，请用管理令牌登录后再试。", "bad");
      } else if (response.status >= 500) {
        banner("ClinePassProxy 返回 " + response.status + "：" + message, "bad");
      }
      throw ApiError(message, response.status);
    }
    if (payload === null && text) {
      throw ApiError("响应不是合法 JSON", response.status);
    }
    return payload;
  }

  // busy disables a button while an async action runs and restores its label,
  // so a slow refresh never looks like a dead button.
  async function busy(button, label, action) {
    if (!button) return action();
    var original = button.textContent;
    button.disabled = true;
    if (label) button.textContent = label;
    try {
      return await action();
    } finally {
      button.disabled = false;
      button.textContent = original;
    }
  }

  function query(params) {
    var parts = [];
    Object.keys(params || {}).forEach(function (key) {
      var value = params[key];
      if (value === undefined || value === null || value === "") return;
      parts.push(encodeURIComponent(key) + "=" + encodeURIComponent(value));
    });
    return parts.length ? "?" + parts.join("&") : "";
  }

  var API = { api: api, busy: busy, banner: banner, query: query, ApiError: ApiError };
  root.CPPApi = API;
  if (typeof module === "object" && module.exports) module.exports = API;
})(typeof globalThis !== "undefined" ? globalThis : this);
