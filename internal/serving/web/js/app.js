/* Router and shell wiring. #view is replaced by one page renderer at a time;
   every render either shows the page or an error box, never a blank screen. */
(function (root) {
  "use strict";

  var theme = root.CPPTheme;
  var pages = root.CPPPages;
  var ui = root.CPPUI;
  var client = root.CPPApi;
  var fmt = root.CPPFormat;
  var el = ui.el;

  var PAGES = {
    dashboard: { title: "概览", subtitle: "服务状态、流量与最近请求", render: pages.dashboard },
    credentials: { title: "凭据", subtitle: "Cline 凭据池、代理与官方额度", render: pages.credentials },
    models: { title: "模型", subtitle: "模型别名、Provider 与真实测试", render: pages.models },
    requests: { title: "请求", subtitle: "请求级诊断与筛选", render: pages.requests },
    usage: { title: "用量", subtitle: "官方 Cline 额度与本机观测分开显示", render: pages.usage },
    settings: { title: "设置", subtitle: "上游、认证与主题", render: pages.settings }
  };

  var current = "dashboard";
  var rendering = false;
  var pending = false;

  function go(page) {
    if (!PAGES[page]) page = "dashboard";
    if (location.hash !== "#" + page) {
      location.hash = page;
      return; // hashchange triggers the render
    }
    render();
  }

  function setActive(page) {
    document.querySelectorAll("#nav button").forEach(function (button) {
      button.classList.toggle("active", button.dataset.page === page);
    });
    var meta = PAGES[page];
    document.getElementById("page-title").textContent = meta.title;
    document.getElementById("page-subtitle").textContent = meta.subtitle;
    document.title = meta.title + " · ClinePassProxy";
  }

  async function render() {
    if (rendering) { pending = true; return; }
    rendering = true;
    var page = current;
    setActive(page);
    var view = document.getElementById("view");
    view.replaceChildren(ui.loading(6));
    try {
      var content = await PAGES[page].render();
      if (current === page) view.replaceChildren(content);
      document.getElementById("updated-at").textContent = "更新于 " + fmt.clock(new Date().toISOString());
    } catch (error) {
      if (current === page) {
        view.replaceChildren(el("div", { class: "stack" }, [
          ui.errorBox("加载失败：" + error.message, function () { render(); }),
          el("p", { class: "hint", text: "如果这是管理认证失败，请用管理令牌重新登录。" })
        ]));
      }
    } finally {
      rendering = false;
      if (pending) {
        pending = false;
        render();
      }
    }
  }

  function syncThemeSelects(preference) {
    document.querySelectorAll("#theme-select, #theme-select-inline").forEach(function (select) {
      select.value = preference;
    });
  }

  function wireTheme() {
    document.addEventListener("change", function (event) {
      var target = event.target;
      if (!target || (target.id !== "theme-select" && target.id !== "theme-select-inline")) return;
      theme.set(target.value);
      ui.toast("主题已切换为「" + (target.value === "system" ? "跟随系统" : target.value === "dark" ? "深色" : "浅色") + "」", "ok");
    });
    theme.onChange(syncThemeSelects);
  }

  async function loadIdentity() {
    try {
      var version = await client.api("/api/version");
      document.getElementById("brand-version").textContent = version.version + (version.commit ? " · " + String(version.commit).slice(0, 8) : "");
      document.getElementById("commit-label").textContent = version.commit || "";
    } catch (error) {
      document.getElementById("brand-version").textContent = "版本未知";
    }
    try {
      var status = await client.api("/api/status");
      var pill = document.getElementById("ready-pill");
      pill.textContent = status.ready ? "Ready" : "未就绪";
      pill.className = "pill " + (status.ready ? "ok" : "bad");
      pill.title = status.ready ? "推理入口已开放" : (status.ready_error || "等待凭据");
    } catch (error) {
      var readyPill = document.getElementById("ready-pill");
      readyPill.textContent = "状态不可用";
      readyPill.className = "pill bad";
    }
  }

  function boot() {
    theme.start();
    wireTheme();
    document.querySelectorAll("#nav button").forEach(function (button) {
      button.addEventListener("click", function () { go(button.dataset.page); });
    });
    var refresh = document.getElementById("refresh-button");
    refresh.addEventListener("click", function () {
      client.busy(refresh, "刷新中…", async function () {
        await loadIdentity();
        await render();
        ui.toast("已刷新", "ok");
      });
    });
    window.addEventListener("hashchange", function () {
      var page = location.hash.replace("#", "");
      current = PAGES[page] ? page : "dashboard";
      render();
    });
    current = PAGES[location.hash.replace("#", "")] ? location.hash.replace("#", "") : "dashboard";
    loadIdentity();
    render();
  }

  var API = { boot: boot, go: go, render: render, pages: PAGES, current: function () { return current; } };
  root.CPPApp = API;

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})(typeof globalThis !== "undefined" ? globalThis : this);
