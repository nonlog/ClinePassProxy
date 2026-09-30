/* Small DOM builders. No framework: every page renders plain elements, so the
   only shared code worth keeping is the handful of pieces used everywhere. */
(function (root) {
  "use strict";

  var fmt = root.CPPFormat;

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (key) {
        var value = attrs[key];
        if (value === undefined || value === null || value === false) return;
        if (key === "class") node.className = value;
        else if (key === "text") node.textContent = value;
        else if (key === "html") node.innerHTML = value;
        else if (key === "dataset") Object.assign(node.dataset, value);
        else if (key.slice(0, 2) === "on") node.addEventListener(key.slice(2), value);
        else if (key === "value") node.value = value;
        else if (key === "checked" || key === "disabled" || key === "selected") node[key] = Boolean(value);
        else node.setAttribute(key, value);
      });
    }
    append(node, children);
    return node;
  }

  function append(node, children) {
    if (children === undefined || children === null || children === false) return;
    if (Array.isArray(children)) {
      children.forEach(function (child) { append(node, child); });
      return;
    }
    if (children instanceof Node) node.appendChild(children);
    else node.appendChild(document.createTextNode(String(children)));
  }

  function fragment(children) {
    var node = document.createDocumentFragment();
    append(node, children);
    return node;
  }

  function section(title, note, actions, children) {
    return el("section", { class: "section" }, [
      el("div", { class: "section-head" }, [
        el("h2", { text: title }),
        note ? el("span", { class: "muted", text: note }) : null,
        actions ? el("div", { class: "btn-row" }, actions) : null
      ]),
      children
    ]);
  }

  function card(title, note, actions, body) {
    return el("div", { class: "card" }, [
      title
        ? el("div", { class: "card-head" }, [
            el("h3", { text: title }),
            note ? el("span", { class: "muted", text: note }) : null,
            actions ? el("div", { class: "btn-row" }, actions) : null
          ])
        : null,
      el("div", { class: "card-body" }, body)
    ]);
  }

  function stat(label, value, unit, sub) {
    return el("div", { class: "stat" }, [
      el("div", { class: "label" }, label),
      el("div", { class: "value" }, [String(value), unit ? el("small", { text: unit }) : null]),
      sub ? el("div", { class: "sub" }, sub) : null
    ]);
  }

  function stats(items) {
    return el("div", { class: "stats" }, items);
  }

  function badge(text, kind) {
    return el("span", { class: "badge" + (kind ? " " + kind : ""), text: text });
  }

  function progress(percent, extraClass) {
    var value = fmt.number(percent);
    var width = value === null ? 0 : Math.max(0, Math.min(100, value));
    var kind = extraClass === undefined ? root.CPPQuota.tint(value) : extraClass;
    return el("div", { class: "progress" + (kind ? " " + kind : "") },
      el("i", { style: "width:" + width.toFixed(2) + "%" }));
  }

  // table renders columns from rows; each column is {label, render(row)}.
  function table(columns, rows, options) {
    var settings = options || {};
    if (!rows || !rows.length) {
      return el("div", { class: "empty" }, [
        el("strong", { text: settings.emptyTitle || "没有数据" }),
        el("span", { text: settings.emptyDetail || "" })
      ]);
    }
    var head = el("tr", {}, columns.map(function (column) {
      return el("th", { class: column.num ? "num" : "" }, column.label);
    }));
    var body = rows.map(function (row) {
      var tr = el("tr", {
        class: settings.onRowClick ? "clickable" : "",
        onclick: settings.onRowClick ? function () { settings.onRowClick(row); } : null
      });
      columns.forEach(function (column) {
        var cell = el("td", { class: column.num ? "num" : "" });
        append(cell, column.render ? column.render(row) : "");
        tr.appendChild(cell);
      });
      return tr;
    });
    return el("div", { class: "table-wrap" }, el("table", {}, [
      el("thead", {}, head),
      el("tbody", {}, body)
    ]));
  }

  function kv(pairs) {
    var node = el("dl", { class: "kv" });
    pairs.forEach(function (pair) {
      if (pair[1] === undefined || pair[1] === null || pair[1] === "") return;
      node.appendChild(el("dt", { text: pair[0] }));
      var dd = el("dd");
      append(dd, pair[1]);
      node.appendChild(dd);
    });
    return node;
  }

  function loading(lines) {
    var body = el("div", { class: "stack" });
    var count = lines || 4;
    for (var i = 0; i < count; i++) {
      body.appendChild(el("div", { class: "skeleton", style: "width:" + (100 - i * 8) + "%" }));
    }
    return el("div", { class: "card pad" }, body);
  }

  function empty(title, detail) {
    return el("div", { class: "empty" }, [
      el("strong", { text: title }),
      detail ? el("span", { text: detail }) : null
    ]);
  }

  function errorBox(message, retry) {
    return el("div", { class: "card pad" }, el("div", { class: "stack" }, [
      el("div", { class: "bad-text", text: message }),
      retry ? el("div", { class: "btn-row" }, el("button", { class: "btn small", onclick: retry }, "重试")) : null
    ]));
  }

  function toast(message, kind) {
    var host = document.getElementById("toasts");
    if (!host) return;
    var node = el("div", { class: "toast" + (kind ? " " + kind : ""), text: message });
    host.appendChild(node);
    var life = kind === "bad" ? 9000 : 4500;
    setTimeout(function () {
      node.remove();
    }, life);
  }

  // modal opens a dialog and returns a close function. Only one is open at a
  // time; Escape and a backdrop click both close it.
  function modal(options) {
    var settings = options || {};
    var host = document.getElementById("modal-root");
    var backdrop = el("div", { class: "modal-backdrop" });
    var dialog = el("div", { class: "modal" + (settings.narrow ? " narrow" : "") });
    var close = function () {
      document.removeEventListener("keydown", onKey);
      backdrop.remove();
    };
    var onKey = function (event) {
      if (event.key === "Escape") close();
    };
    backdrop.addEventListener("click", function (event) {
      if (event.target === backdrop) close();
    });
    document.addEventListener("keydown", onKey);
    dialog.appendChild(el("div", { class: "modal-head" }, [
      el("h2", { text: settings.title || "" }),
      el("button", { class: "btn small", onclick: close, type: "button" }, "关闭")
    ]));
    dialog.appendChild(el("div", { class: "modal-body" }, settings.body));
    if (settings.actions) {
      dialog.appendChild(el("div", { class: "modal-foot" }, settings.actions(close)));
    }
    backdrop.appendChild(dialog);
    host.replaceChildren(backdrop);
    return close;
  }

  function confirmAction(message) {
    return window.confirm(message);
  }

  // sparkline draws the 31-day official token series without a chart library.
  function sparkline(values, label) {
    var list = (values || []).map(function (value) { return fmt.number(value) || 0; });
    if (list.length < 2) return null;
    var max = Math.max.apply(null, list);
    if (max <= 0) return null;
    var width = 240;
    var height = 28;
    var step = width / (list.length - 1);
    var points = list.map(function (value, index) {
      var x = (index * step).toFixed(1);
      var y = (height - (value / max) * (height - 2) - 1).toFixed(1);
      return x + "," + y;
    }).join(" ");
    var svg = '<svg class="sparkline" viewBox="0 0 ' + width + " " + height + '" preserveAspectRatio="none">' +
      '<polyline fill="none" stroke="currentColor" stroke-width="1.5" points="' + points + '"/></svg>';
    return el("div", {}, [
      el("div", { class: "hint", text: label || "" }),
      el("div", { html: svg })
    ]);
  }

  var API = {
    el: el,
    fragment: fragment,
    section: section,
    card: card,
    stat: stat,
    stats: stats,
    badge: badge,
    progress: progress,
    table: table,
    kv: kv,
    loading: loading,
    empty: empty,
    errorBox: errorBox,
    toast: toast,
    modal: modal,
    confirm: confirmAction,
    sparkline: sparkline
  };

  root.CPPUI = API;
  if (typeof module === "object" && module.exports) module.exports = API;
})(typeof globalThis !== "undefined" ? globalThis : this);
