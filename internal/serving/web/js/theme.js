/* Theme preference: system / light / dark.

   The resolved theme is written to <html data-theme>, and the preference to
   <html data-theme-pref>. Resolving before the first paint happens in the
   inline bootstrap in index.html; this file keeps it correct afterwards. */
(function (root) {
  "use strict";

  var STORAGE_KEY = "clinepassproxy-theme";
  var media = root.matchMedia ? root.matchMedia("(prefers-color-scheme: dark)") : null;
  var observers = [];

  function stored() {
    try {
      var value = localStorage.getItem(STORAGE_KEY) || "";
      return value === "light" || value === "dark" || value === "system" ? value : "system";
    } catch (error) {
      return "system";
    }
  }

  // hostTheme reads CPAMP's own theme when the console is embedded in it. Any
  // cross-origin or missing host simply yields "", which leaves the OS
  // preference in charge.
  function hostTheme() {
    try {
      var frame = root.parent;
      if (!frame || frame === root) return "";
      var host = frame.document && frame.document.documentElement;
      if (!host) return "";
      var attr = host.getAttribute("data-theme") || host.getAttribute("data-bs-theme") || "";
      if (attr === "light" || attr === "dark") return attr;
      return host.classList && host.classList.contains("dark") ? "dark" : "";
    } catch (error) {
      return "";
    }
  }

  function resolve(preference) {
    if (preference === "light" || preference === "dark") return preference;
    var host = hostTheme();
    if (host) return host;
    return media && media.matches ? "dark" : "light";
  }

  function apply(preference) {
    var pref = preference === "light" || preference === "dark" ? preference : "system";
    var element = document.documentElement;
    element.dataset.themePref = pref;
    element.dataset.theme = resolve(pref);
    notify(pref);
  }

  function set(preference) {
    try {
      localStorage.setItem(STORAGE_KEY, preference);
    } catch (error) {
      /* Private mode: keep the in-memory choice for this page. */
    }
    apply(preference);
  }

  function notify(preference) {
    observers.forEach(function (handler) {
      try {
        handler(preference);
      } catch (error) {
        /* A broken listener must not break theming. */
      }
    });
  }

  function onChange(handler) {
    observers.push(handler);
    handler(stored());
  }

  function watchHost() {
    try {
      var frame = root.parent;
      if (!frame || frame === root) return;
      var host = frame.document && frame.document.documentElement;
      if (!host || !frame.MutationObserver) return;
      new frame.MutationObserver(function () {
        if (stored() === "system") apply("system");
      }).observe(host, { attributes: true, attributeFilter: ["data-theme", "data-bs-theme", "class"] });
    } catch (error) {
      /* Cross-origin frame: the OS preference alone is used. */
    }
  }

  if (media) {
    var listener = function () {
      if (stored() === "system") apply("system");
    };
    if (media.addEventListener) media.addEventListener("change", listener);
    else if (media.addListener) media.addListener(listener);
  }

  function start() {
    watchHost();
    // Re-resolve once the DOM is up: a host theme applied after our bootstrap
    // script still wins while the preference is "system".
    apply(stored());
    window.addEventListener("storage", function (event) {
      if (event.key === STORAGE_KEY) apply(stored());
    });
  }

  var API = { start: start, apply: apply, set: set, get: stored, hostTheme: hostTheme, onChange: onChange, key: STORAGE_KEY };
  root.CPPTheme = API;
  if (typeof module === "object" && module.exports) module.exports = API;
})(typeof globalThis !== "undefined" ? globalThis : this);
