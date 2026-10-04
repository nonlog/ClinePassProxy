/* Page renderers. Each function loads its own data through the management API
   and returns the DOM for #view. Nothing here invents values: a missing field
   renders as "—" and a failed call renders an error box with a retry. */
(function (root) {
  "use strict";

  var fmt = root.CPPFormat;
  var quota = root.CPPQuota;
  var ui = root.CPPUI;
  var client = root.CPPApi;
  var el = ui.el;

  var state = {
    dashboard: null,
    credentials: null,
    models: null,
    config: null,
    requests: null,
    usage: null,
    official: null,
    filters: { model: "", credential: "", provider: "", endpoint: "", status: "", range: "24h" },
    usageRange: "24h"
  };

  var STAGE_LABELS = {
    request_received: "收到请求",
    body_read_done: "读取请求体",
    parse_done: "解析完成",
    translate_done: "协议转换完成",
    credential_selected: "选定凭据",
    upstream_request_start: "发起上游请求",
    upstream_headers_received: "上游响应头",
    first_upstream_event: "上游首个事件",
    first_protocol_event: "首个协议事件",
    first_downstream_write: "首次写回客户端",
    first_token_write: "首个可见 token",
    stream_complete: "流结束",
    request_complete: "请求完成"
  };

  // ------------------------------------------------------------- official

  function quotaCard(limit, nowMs) {
    var percent = limit.percent;
    var remaining = limit.remaining;
    return el("div", { class: "quota" }, [
      el("div", { class: "quota-head" }, [
        el("span", { class: "name", text: limit.label }),
        el("span", { class: "pct " + (percent === null ? "" : quota.tint(percent) + "-text"), text: fmt.percent(percent, 1) })
      ]),
      el("div", { class: "row" }, [
        el("span", { class: "quota-value" }, [fmt.fixed(percent, 1), el("small", { text: "% 已用" })]),
        el("span", { class: "muted", style: "margin-left:auto", text: "剩余 " + fmt.percent(remaining, 1) })
      ]),
      ui.progress(percent),
      el("div", { class: "quota-rows" }, [
        el("div", { class: "rowline" }, [
          el("span", { text: "重置倒计时" }),
          el("span", { text: quota.resetText(limit, nowMs) })
        ]),
        el("div", { class: "rowline" }, [
          el("span", { text: "重置时间" }),
          el("span", { text: quota.resetAtText(limit) })
        ])
      ])
    ]);
  }

  function officialTokensCard(snapshot) {
    var tokens = quota.tokens(snapshot);
    var rows = [];
    if (tokens.from || tokens.to) {
      rows.push(el("div", { class: "rowline", text: [tokens.from, tokens.to].filter(Boolean).join(" ~ ") }));
    }
    rows.push(el("div", { class: "rowline" }, [
      el("span", { text: "输入 " + fmt.scaled(tokens.input) }),
      el("span", { text: "输出 " + fmt.scaled(tokens.output) })
    ]));
    rows.push(el("div", { class: "rowline" }, [
      el("span", { text: "余额 " + fmt.money(tokens.balance, 4) }),
      el("span", { text: "近 31 天成本 " + fmt.money(tokens.cost, 4) })
    ]));
    rows.push(el("div", { class: "rowline" }, [
      el("span", { text: "官方计费条目" }),
      el("span", { text: fmt.int(tokens.billingItems) })
    ]));
    var body = [
      el("div", { class: "quota-head" }, el("span", { class: "name", text: "近 31 天已用 Token（官方）" })),
      el("div", { class: "tok-value" }, [
        tokens.hasTotals ? fmt.scaled(tokens.total) : fmt.DASH,
        el("small", { text: "token" })
      ]),
      el("div", { class: "quota-rows" }, rows)
    ];
    var spark = ui.sparkline(tokens.series, "每日 token（官方，近 31 天）");
    if (spark) body.push(spark);
    return el("div", { class: "quota" }, body);
  }

  // officialSection renders one block per credential: quota windows, the
  // official 31-day totals, and whatever error upstream returned.
  function officialSection(snapshots, refreshedAt, options) {
    var settings = options || {};
    var list = snapshots || [];
    var now = Date.now();
    var children = [];
    if (!list.length) {
      children.push(ui.empty("官方用量尚未取到",
        "后台每 5 分钟读取一次 Cline 账号；也可以点“刷新官方用量”立即读取。"));
    }
    list.forEach(function (snapshot) {
      var status = quota.status(snapshot);
      var head = [
        el("h3", { text: "凭据 " + (snapshot.label || snapshot.credential_id) }),
        ui.badge(status.text, status.kind),
        snapshot.plan_name ? ui.badge(snapshot.plan_name, "info") : null,
        snapshot.plan_price ? el("span", { class: "muted", text: snapshot.plan_price }) : null,
        snapshot.account ? el("span", { class: "muted mono", text: "账号 " + snapshot.account }) : null,
        snapshot.fetched_at ? el("span", { class: "muted", text: "更新于 " + fmt.clock(snapshot.fetched_at) }) : null
      ];
      var body = [el("div", { class: "card-head" }, head)];
      if (!snapshot.available) {
        body.push(el("div", { class: "card-body" },
          el("div", { class: "bad-text", text: snapshot.error || "官方用量不可用" })));
      } else {
        var limits = quota.limits(snapshot);
        body.push(el("div", { class: "card-body" }, el("div", { class: "quota-grid" }, [
          limits.length ? limits.map(function (limit) { return quotaCard(limit, now); }) :
            ui.empty("Cline 未返回额度窗口", "usage-limits 的 limits 为空。"),
          officialTokensCard(snapshot)
        ])));
        if (snapshot.tokens_error) {
          body.push(el("div", { class: "card-body" },
            el("div", { class: "warn-text", text: "官方 31 天汇总未取到：" + snapshot.tokens_error })));
        }
      }
      children.push(el("div", { class: "card" }, body));
    });
    var actions = [
      el("button", {
        class: "btn small", type: "button",
        onclick: function (event) {
          client.busy(event.currentTarget, "刷新中…", async function () {
            try {
              var payload = await client.api("/api/official?refresh=1");
              state.official = payload;
              ui.toast("官方用量已刷新", "ok");
              if (settings.after) settings.after();
            } catch (error) {
              ui.toast("刷新官方用量失败：" + error.message, "bad");
            }
          });
        }
      }, "刷新官方用量")
    ];
    var note = refreshedAt
      ? "上次刷新 " + fmt.ago(refreshedAt) + (settings.interval ? "（每 " + Math.round(settings.interval / 60) + " 分钟自动刷新）" : "")
      : "尚未刷新过";
    return ui.section("官方 Cline 用量", note, actions, el("div", { class: "stack" }, children));
  }

  function credentialQuotaCell(view) {
    var snapshot = view.official;
    if (!snapshot) return el("span", { class: "muted", text: "未查询" });
    if (!snapshot.available) {
      return el("span", { class: "warn-text", text: snapshot.error || "不可用" });
    }
    var limits = quota.limits(snapshot);
    if (!limits.length) return el("span", { class: "muted", text: "无额度数据" });
    return el("div", { class: "stack", style: "min-width:160px;gap:5px" }, limits.map(function (limit) {
      return el("div", {}, [
        el("div", { class: "row", style: "gap:6px" }, [
          el("span", { class: "hint", text: limit.label }),
          el("span", { class: "hint", style: "margin-left:auto", text: fmt.percent(limit.percent, 1) })
        ]),
        ui.progress(limit.percent)
      ]);
    }));
  }

  function healthBadge(value) {
    switch (value) {
      case "ok": return ui.badge("健康", "ok");
      case "error": return ui.badge("异常", "bad");
      case "disabled": return ui.badge("已禁用", "");
      case "cooldown": return ui.badge("冷却中", "warn");
      default: return ui.badge("未知", "warn");
    }
  }

  function statusBadge(record) {
    if (record.status >= 200 && record.status < 300) return ui.badge(String(record.status), "ok");
    if (record.status === 0) return ui.badge("失败", "bad");
    return ui.badge(String(record.status), "bad");
  }

  function requestSummaryRow(record, onClick) {
    return { record: record, onClick: onClick };
  }

  var requestColumns = [
    { label: "时间", render: function (row) { return el("span", { class: "mono nowrap", text: fmt.clock(row.record.started_at) }); } },
    { label: "状态", render: function (row) { return statusBadge(row.record); } },
    { label: "端点", render: function (row) { return el("span", { class: "mono", text: row.record.endpoint }); } },
    { label: "模型", render: function (row) { return row.record.model; } },
    { label: "上游模型", render: function (row) { return el("span", { class: "mono", text: row.record.upstream_model || fmt.DASH }); } },
    { label: "凭据", render: function (row) { return row.record.credential_label || row.record.credential_id || fmt.DASH; } },
    { label: "Provider", render: function (row) { return row.record.provider || fmt.DASH; } },
    { label: "TTFT", num: true, render: function (row) { return fmt.ms(row.record.ttft_ms); } },
    { label: "Provider TTFT", num: true, render: function (row) { return fmt.ms(row.record.provider_ttft_ms); } },
    { label: "耗时", num: true, render: function (row) { return fmt.ms(row.record.duration_ms); } },
    { label: "Prompt", num: true, render: function (row) { return fmt.int(row.record.prompt_tokens); } },
    { label: "Cached", num: true, render: function (row) { return fmt.int(row.record.cached_tokens); } },
    { label: "Completion", num: true, render: function (row) { return fmt.int(row.record.completion_tokens); } },
    { label: "Reasoning", num: true, render: function (row) { return fmt.int(row.record.reasoning_tokens); } },
    { label: "切换", num: true, render: function (row) { return fmt.int(row.record.failover_count); } }
  ];

  function requestTable(records, emptyTitle, emptyDetail) {
    return ui.table(requestColumns, records.map(function (record) {
      return requestSummaryRow(record, function () { openRequestDetail(record.id); });
    }), {
      onRowClick: function (row) { row.onClick(); },
      emptyTitle: emptyTitle || "没有请求记录",
      emptyDetail: emptyDetail || ""
    });
  }

  // ------------------------------------------------------------- detail modal

  function openRequestDetail(id) {
    var body = ui.loading(5);
    ui.modal({ title: "请求 " + id, body: body });
    client.api("/api/requests/" + encodeURIComponent(id)).then(function (record) {
      body.replaceChildren(requestDetail(record));
    }).catch(function (error) {
      body.replaceChildren(el("div", { class: "bad-text", text: "读取请求详情失败：" + error.message }));
    });
  }

  function requestDetail(record) {
    var timingPairs = Object.keys(record.timings_ms || {}).map(function (stage) {
      return [STAGE_LABELS[stage] || stage, fmt.ms(record.timings_ms[stage])];
    });
    var attemptNodes = (record.attempts || []).map(function (attempt) {
      return el("div", { class: "attempt mono", text: attempt });
    });
    return el("div", { class: "detail-grid" }, [
      el("div", { class: "stack" }, [
        el("h3", { text: "请求" }),
        ui.kv([
          ["请求 ID", el("span", { class: "mono", text: record.id })],
          ["时间", fmt.date(record.started_at)],
          ["端点", el("span", { class: "mono", text: record.endpoint })],
          ["源协议", record.source_format],
          ["上游协议", "Chat Completions（Cline SSE）"],
          ["流式", record.stream ? "是" : "否"],
          ["客户端模型", record.model],
          ["上游模型", el("span", { class: "mono", text: record.upstream_model })],
          ["状态", statusBadge(record)],
          ["上游状态", record.upstream_status ? String(record.upstream_status) : ""],
          ["错误", record.error ? el("span", { class: "bad-text", text: record.error }) : ""]
        ])
      ]),
      el("div", { class: "stack" }, [
        el("h3", { text: "路由" }),
        ui.kv([
          ["凭据", record.credential_label ? record.credential_label + "（" + (record.credential_id || "") + "）" : ""],
          ["Provider", record.provider],
          ["亲和键", el("span", { class: "mono", text: record.affinity_key || "" })],
          ["亲和来源", record.affinity_reason],
          ["切换次数", String(record.failover_count || 0)],
          ["请求字节", fmt.int(record.request_bytes)],
          ["上游字节", fmt.int(record.upstream_bytes)],
          ["响应字节", fmt.int(record.response_bytes)]
        ]),
        attemptNodes.length ? el("div", { class: "stack" }, [el("h3", { text: "尝试记录" }), el("div", { class: "attempts" }, attemptNodes)]) : null
      ]),
      el("div", { class: "stack" }, [
        el("h3", { text: "Token 与耗时" }),
        ui.kv([
          ["Prompt tokens", fmt.int(record.prompt_tokens)],
          ["Cached tokens", fmt.int(record.cached_tokens)],
          ["Completion tokens", fmt.int(record.completion_tokens)],
          ["Reasoning tokens", fmt.int(record.reasoning_tokens)],
          ["缓存命中率", fmt.ratio(record.cache_hit_ratio)],
          ["TTFT（可见 token）", fmt.ms(record.ttft_ms)],
          ["Provider TTFT", fmt.ms(record.provider_ttft_ms)],
          ["总耗时", fmt.ms(record.duration_ms)],
          ["解码耗时", fmt.ms(record.decode_ms)],
          ["解码 TPS", fmt.fixed(record.decode_tps, 2)],
          ["端到端 TPS", fmt.fixed(record.end_to_end_tps, 2)]
        ])
      ]),
      el("div", { class: "stack" }, [
        el("h3", { text: "时间线（毫秒）" }),
        timingPairs.length ? ui.kv(timingPairs) : ui.empty("没有时间线数据", "")
      ])
    ]);
  }

  // ------------------------------------------------------------- dashboard

  async function dashboard() {
    var payload = await client.api("/api/dashboard");
    state.dashboard = payload;
    var service = payload.service || {};
    var traffic = (payload.traffic || {})["24h"] || {};
    var successRate = traffic.requests ? traffic.successes / traffic.requests : null;

    var serviceStats = [
      ui.stat("版本", service.version || fmt.DASH, "", service.commit ? "commit " + String(service.commit).slice(0, 8) : ""),
      ui.stat("运行时长", fmt.countdown(service.uptime_sec), "", service.build_time ? "构建 " + service.build_time : ""),
      ui.stat("就绪", service.ready ? "Ready" : "未就绪", "", service.ready ? "推理入口开放" : (service.ready_error || "等待凭据")),
      ui.stat("活跃请求", fmt.int(service.active_requests), "", "正在转发"),
      ui.stat("凭据", fmt.int(service.enabled_credentials) + " / " + fmt.int(service.credentials), "", Object.keys(service.health || {}).map(function (key) {
        return key + " " + service.health[key];
      }).join(" · ")),
      ui.stat("模型", fmt.int(service.enabled_models) + " / " + fmt.int(service.total_models), "", service.total_models ? "别名表" : "透传（无别名表）")
    ];

    var trafficStats = [
      ui.stat("请求数（24h）", fmt.int(traffic.requests), "", traffic.errors ? "错误 " + fmt.int(traffic.errors) : "无错误"),
      ui.stat("成功率（24h）", fmt.ratio(successRate, 1), "", "成功 " + fmt.int(traffic.successes)),
      ui.stat("平均 TTFT", fmt.ms(traffic.average_ttft_ms), "", "可见 token"),
      ui.stat("平均耗时", fmt.ms(traffic.average_duration_ms), "", "端到端"),
      ui.stat("Prompt tokens", fmt.scaled(traffic.prompt_tokens), "", "24 小时累计"),
      ui.stat("Cached tokens", fmt.scaled(traffic.cached_tokens), "", "24 小时累计"),
      ui.stat("Completion tokens", fmt.scaled(traffic.completion_tokens), "", "24 小时累计"),
      ui.stat("缓存命中率", fmt.ratio(traffic.cache_hit_ratio), "", "cached / prompt")
    ];

    var credentialRows = (payload.credentials || []).map(function (view) {
      return { view: view };
    });

    var official = payload.official || {};
    var recent = payload.requests || [];

    return ui.fragment([
      ui.section("服务", service.base_url || "", null, ui.stats(serviceStats)),
      ui.section("流量（近 24 小时）", "数据来自本机请求历史", null, ui.stats(trafficStats)),
      officialSection(official.snapshots, official.refreshed_at, { interval: official.interval_seconds, after: function () { root.CPPApp.render(); } }),
      ui.section("凭据状态", null, el("button", {
        class: "btn small", type: "button",
        onclick: function () { root.CPPApp.go("credentials"); }
      }, "打开凭据页"), ui.table([
        { label: "凭据", render: function (row) { return el("span", { class: "cell-strong", text: row.view.label || row.view.id }); } },
        { label: "启用", render: function (row) { return row.view.enabled ? ui.badge("启用", "ok") : ui.badge("禁用", ""); } },
        { label: "健康", render: function (row) { return healthBadge(row.view.health); } },
        { label: "套餐", render: function (row) { return (row.view.official && row.view.official.plan_name) || fmt.DASH; } },
        { label: "代理", render: function (row) { return row.view.proxy ? el("span", { class: "mono", text: row.view.proxy }) : el("span", { class: "muted", text: "直连" }); } },
        { label: "额度", render: function (row) { return credentialQuotaCell(row.view); } },
        { label: "最近检查", render: function (row) { return fmt.ago(row.view.health_checked_at); } },
        { label: "连续失败", num: true, render: function (row) { return fmt.int(row.view.consecutive_errors || 0); } }
      ], credentialRows, { emptyTitle: "还没有凭据", emptyDetail: "在凭据页添加一个 Cline API Key。" })),
      ui.section("最近请求", "点击任意一行查看完整诊断", null,
        requestTable(recent, "还没有请求记录", "客户端请求到达后会记录在这里。"))
    ]);
  }

  // ------------------------------------------------------------- credentials

  function credentialForm(record, done) {
    var labelInput = el("input", { value: record ? record.label || "" : "", placeholder: "例如 www" });
    var keyInput = el("input", {
      type: "password",
      placeholder: record ? "留空保持不变（" + (record.masked_key || "") + "）" : "Cline API Key"
    });
    var proxyInput = el("input", {
      value: record && record.proxy ? record.proxy : "",
      placeholder: "http://127.0.0.1:7890 或 socks5://…（留空 = 直连）"
    });
    var clearProxy = el("input", { type: "checkbox" });
    var enabledInput = el("input", { type: "checkbox", checked: record ? record.enabled !== false : true });

    var body = el("div", { class: "form-grid" }, [
      el("label", { class: "field" }, [el("span", { text: "标签" }), labelInput]),
      el("label", { class: "field" }, [el("span", { text: "Cline API Key" }), keyInput]),
      el("label", { class: "field", style: "grid-column:1/-1" }, [
        el("span", { text: "凭据专用代理" }),
        proxyInput
      ]),
      el("label", { class: "field inline" }, [clearProxy, el("span", { text: "清除已保存的代理（直连）" })]),
      el("label", { class: "field inline" }, [enabledInput, el("span", { text: "启用该凭据" })])
    ]);
    body.appendChild(el("p", { class: "hint", text: record
      ? "API Key 不会回显。留空表示保持原值；要替换就直接输入新的 Key。"
      : "API Key 会以 0600 权限保存在 data/credentials.json。" }));

    var submit = el("button", { class: "btn primary", type: "button" }, record ? "保存" : "添加");
    submit.addEventListener("click", function () {
      client.busy(submit, "保存中…", async function () {
        var payload = { label: labelInput.value.trim(), enabled: enabledInput.checked };
        if (keyInput.value.trim()) payload.api_key = keyInput.value.trim();
        if (record) {
          if (clearProxy.checked) payload.proxy_url = "";
          else if (proxyInput.value.trim()) payload.proxy_url = proxyInput.value.trim();
        } else {
          payload.proxy_url = proxyInput.value.trim();
        }
        try {
          if (record) {
            await client.api("/api/credentials/" + encodeURIComponent(record.id), {
              method: "PUT", body: JSON.stringify(payload)
            });
          } else {
            await client.api("/api/credentials", { method: "POST", body: JSON.stringify(payload) });
          }
          ui.toast(record ? "凭据已更新" : "凭据已添加", "ok");
          close();
          done();
        } catch (error) {
          ui.toast("保存失败：" + error.message, "bad");
        }
      });
    });
    var close = ui.modal({
      title: record ? "编辑凭据 " + (record.label || record.id) : "添加 Cline 凭据",
      body: body,
      actions: function (dismiss) {
        return [el("button", { class: "btn", type: "button", onclick: dismiss }, "取消"), submit];
      }
    });
  }

  function credentialAction(view, action, label, options) {
    var settings = options || {};
    return el("button", {
      class: "btn small" + (settings.kind ? " " + settings.kind : ""),
      type: "button",
      onclick: function (event) {
        var button = event.currentTarget;
        client.busy(button, settings.busy || "…", async function () {
          try {
            await action();
            if (settings.message) ui.toast(settings.message, "ok");
            root.CPPApp.render();
          } catch (error) {
            ui.toast((settings.failure || "操作失败") + "：" + error.message, "bad");
          }
        });
      }
    }, label);
  }

  async function credentials() {
    var payload = await client.api("/api/credentials");
    state.credentials = payload.credentials || [];
    var rows = state.credentials;

    return ui.fragment([
      ui.section("凭据池", "三套密钥互相独立：管理令牌 ≠ 网关 Key ≠ Cline 凭据",
        [el("button", { class: "btn primary", type: "button", onclick: function () { credentialForm(null, function () { root.CPPApp.render(); }); } }, "添加凭据"),
         el("button", { class: "btn", type: "button", onclick: function () { root.CPPApp.render(); } }, "重新加载")],
        el("div", { class: "card" }, ui.table([
          { label: "凭据", render: function (view) {
              return el("div", { class: "stack", style: "gap:2px" }, [
                el("span", { class: "cell-strong", text: view.label || view.id }),
                el("span", { class: "mono muted", text: view.masked_key || "" })
              ]);
            } },
          { label: "状态", render: function (view) {
              return el("div", { class: "row" }, [
                view.enabled ? ui.badge("启用", "ok") : ui.badge("禁用", ""),
                healthBadge(view.health)
              ]);
            } },
          { label: "套餐 / 账号", render: function (view) {
              var official = view.official;
              if (!official) return el("span", { class: "muted", text: "未查询" });
              return el("div", { class: "stack", style: "gap:2px" }, [
                el("span", { text: official.plan_name || fmt.DASH }),
                el("span", { class: "muted", text: [official.plan_price, official.account].filter(Boolean).join(" · ") || "—" })
              ]);
            } },
          { label: "额度", render: function (view) { return credentialQuotaCell(view); } },
          { label: "代理", render: function (view) { return view.proxy ? el("span", { class: "mono", text: view.proxy }) : el("span", { class: "muted", text: "直连" }); } },
          { label: "最近检查", render: function (view) {
              return el("div", { class: "stack", style: "gap:2px" }, [
                el("span", { text: fmt.ago(view.health_checked_at) }),
                view.health_error ? el("span", { class: "bad-text", text: view.health_error }) : null
              ]);
            } },
          { label: "操作", render: function (view) {
              return el("div", { class: "btn-row" }, [
                credentialAction(view, function () {
                  return client.api("/api/credentials/" + encodeURIComponent(view.id) + "/test", { method: "POST" });
                }, "测试", { busy: "测试中…", message: "凭据测试通过", failure: "凭据测试失败" }),
                credentialAction(view, function () {
                  return client.api("/api/credentials/" + encodeURIComponent(view.id) + "/refresh", { method: "POST" });
                }, "刷新额度", { busy: "刷新中…", message: "额度已刷新", failure: "刷新额度失败" }),
                credentialAction(view, function () {
                  return client.api("/api/credentials/" + encodeURIComponent(view.id), {
                    method: "PUT", body: JSON.stringify({ enabled: !view.enabled })
                  });
                }, view.enabled ? "禁用" : "启用", { message: view.enabled ? "已禁用" : "已启用", failure: "切换失败" }),
                el("button", { class: "btn small", type: "button", onclick: function () { credentialForm(view, function () { root.CPPApp.render(); }); } }, "编辑"),
                credentialAction(view, function () {
                  if (!ui.confirm("删除凭据 " + (view.label || view.id) + "？此操作不可撤销。")) {
                    return Promise.reject(new Error("已取消"));
                  }
                  return client.api("/api/credentials/" + encodeURIComponent(view.id), { method: "DELETE" });
                }, "删除", { kind: "danger", message: "已删除", failure: "删除失败" })
              ]);
            } }
        ], rows, {
          emptyTitle: "还没有凭据",
          emptyDetail: "添加一个 Cline API Key 后，推理入口才会就绪。"
        })))
    ]);
  }

  // ------------------------------------------------------------- models

  async function models() {
    var payload = await client.api("/api/models");
    state.models = payload;
    var list = payload.models || [];
    var seen = payload.seen_models || [];
    var config = await client.api("/api/config");
    state.config = config;
    if (!state.credentials) {
      var credentials = await client.api("/api/credentials");
      state.credentials = credentials.credentials || [];
    }

    function save(next, done) {
      client.api("/api/config", { method: "PUT", body: JSON.stringify({ models: next }) }).then(function () {
        ui.toast("模型别名已保存", "ok");
        done();
        root.CPPApp.render();
      }).catch(function (error) {
        ui.toast("保存失败：" + error.message, "bad");
      });
    }

    function modelForm(entry) {
      var idInput = el("input", { value: entry ? entry.id : "", placeholder: "cline-pass/claude-sonnet-4.5" });
      var upstreamInput = el("input", { value: entry ? entry.upstream_id || "" : "", placeholder: "上游模型 ID（留空 = 同名）" });
      var selectedProviders = Object.create(null);
      var providerOptions = Object.create(null);
      var providerPipeline = entry && entry.provider_pipeline || "";
      var probeGeneration = 0;
      (entry && entry.providers || []).forEach(function (name) {
        if (typeof name === "string" && name.trim()) selectedProviders[name.trim()] = true;
      });

      function providerName(item) {
        if (typeof item === "string") return item.trim();
        if (!item || typeof item !== "object") return "";
        return String(item.name || item.id || item.provider || "").trim();
      }

      function addProviderItems(items, source) {
        (Array.isArray(items) ? items : []).forEach(function (item) {
          var name = providerName(item);
          if (!name) return;
          var status = typeof item === "object" && item ? String(item.status || "").trim() : "";
          providerOptions[name] = { status: status, source: source };
        });
      }

      addProviderItems(entry && entry.observed_providers, "history");

      var providerList = el("div", { class: "stack", style: "gap:4px" });
      var providerHint = el("span", { class: "hint", "aria-live": "polite", text: "当前模型尚未探测；历史命中不代表当前可路由。" });
      var probeButton = el("button", { class: "btn small", type: "button" }, "探测 Provider");

      function statusLabel(status) {
        var labels = { available: "上游列出", ok: "上游列出", unavailable: "不可用", rate_limited: "限流", unknown: "未确认" };
        return labels[status] || status;
      }

      function renderProviderChoices() {
        providerList.replaceChildren();
        var names = Object.keys(providerOptions).concat(Object.keys(selectedProviders).filter(function (name) {
          return !providerOptions[name];
        })).sort();
        selectAllButton.disabled = !Object.keys(providerOptions).some(function (name) { return providerOptions[name].source === "probe"; });
        if (!names.length) {
          providerList.appendChild(el("span", { class: "muted", text: "尚未发现 Provider" }));
          return;
        }
        names.forEach(function (name) {
          var option = providerOptions[name] || { source: "configured" };
          var confirmed = option.source === "probe" || (option.source === "configured" && Boolean(providerPipeline));
          var checkbox = el("input", { type: "checkbox", checked: Boolean(selectedProviders[name]), disabled: !confirmed });
          checkbox.addEventListener("change", function () {
            if (checkbox.checked) selectedProviders[name] = true;
            else delete selectedProviders[name];
          });
          var status = option.source === "history" ? "历史命中 · 当前未确认" : option.source === "configured" && !providerPipeline
            ? "已保存 · 当前未确认" : option.source === "configured" ? "已确认 · " + providerPipeline : statusLabel(option.status || "available");
          providerList.appendChild(el("label", { class: "field inline" }, [
            checkbox,
            el("span", { class: "mono", text: name }),
            el("span", { class: "hint", text: " · " + status })
          ]));
        });
      }

      function invalidateProviders() {
        probeGeneration++;
        providerOptions = Object.create(null);
        providerPipeline = "";
        renderProviderChoices();
        providerHint.className = "hint";
        providerHint.textContent = "当前模型尚未探测；已选 Provider 未确认。";
      }
      idInput.addEventListener("input", invalidateProviders);
      upstreamInput.addEventListener("input", invalidateProviders);

      probeButton.addEventListener("click", function () {
        var model = idInput.value.trim();
        if (!model) { ui.toast("请先填写客户端模型 ID", "bad"); return; }
        invalidateProviders();
        var generation = probeGeneration;
        providerHint.textContent = "探测中…";
        client.busy(probeButton, "探测中…", async function () {
          try {
            var result = await client.api("/api/models/providers/probe", {
              method: "POST",
              body: JSON.stringify({
                model: model,
                upstream_model: upstreamInput.value.trim(),
                credential_id: typeof testCredential !== "undefined" ? testCredential.value : ""
              })
            });
            if (generation !== probeGeneration) return;
            if (!result || result.ok === false || result.error) throw new Error((result && result.error) || "上游没有返回可路由 Provider");
            // The probe catalog is authoritative for this exact model. Old
            // history entries must not remain selectable after the upstream
            // model or its routing pipeline changes.
            providerOptions = Object.create(null);
            addProviderItems(result.providers, "probe");
            var count = Object.keys(providerOptions).length;
            if (!count) throw new Error("上游没有返回可路由 Provider");
            Object.keys(selectedProviders).forEach(function (name) {
              if (!providerOptions[name]) delete selectedProviders[name];
            });
            providerPipeline = result.pipeline === "planner" || result.pipeline === "direct" ? result.pipeline : "";
            renderProviderChoices();
            var details = [];
            if (result && result.pipeline) details.push("通道：" + result.pipeline);
            if (result && result.actual_provider) details.push("最近命中：" + result.actual_provider);
            providerHint.className = "hint";
          providerHint.textContent = "发现 " + count + " 个 Provider" + (details.length ? " · " + details.join(" · ") : "") + "；只有本次探测列出的 Provider 会保存。";
          } catch (error) {
            if (generation !== probeGeneration) return;
            providerHint.className = "bad-text";
            providerHint.textContent = "探测失败：" + error.message;
          }
        });
      });

      var selectAllButton = el("button", { class: "btn small", type: "button", onclick: function () {
        Object.keys(providerOptions).forEach(function (name) {
          if (providerOptions[name].source === "probe") selectedProviders[name] = true;
        });
        renderProviderChoices();
      } }, "全选");
      var clearAllButton = el("button", { class: "btn small", type: "button", onclick: function () {
        selectedProviders = Object.create(null);
        renderProviderChoices();
      } }, "清空");
      renderProviderChoices();
      var disabledInput = el("input", { type: "checkbox", checked: entry ? Boolean(entry.disabled) : false });
      var submit = el("button", { class: "btn primary", type: "button" }, entry ? "保存" : "添加");
      submit.addEventListener("click", function () {
        var id = idInput.value.trim();
        if (!id) { ui.toast("模型 ID 不能为空", "bad"); return; }
        var next = list.filter(function (item) { return item.id !== (entry ? entry.id : id); }).map(function (item) {
          return { id: item.id, upstream_id: item.upstream_id, providers: item.providers || [], provider_pipeline: item.provider_pipeline || "", disabled: Boolean(item.disabled) };
        });
        next.push({
          id: id,
          upstream_id: upstreamInput.value.trim(),
          providers: Object.keys(selectedProviders).filter(function (name) {
            if (!selectedProviders[name]) return false;
            var option = providerOptions[name];
            return option && (option.source === "probe" || (option.source === "configured" && Boolean(providerPipeline)));
          }),
          provider_pipeline: providerPipeline,
          disabled: disabledInput.checked
        });
        close();
        save(next, function () {});
      });
      var close = ui.modal({
        title: entry ? "编辑模型 " + entry.id : "添加模型别名",
        narrow: true,
        body: el("div", { class: "form-grid" }, [
          el("label", { class: "field", style: "grid-column:1/-1" }, [el("span", { text: "客户端模型 ID" }), idInput]),
          el("label", { class: "field", style: "grid-column:1/-1" }, [el("span", { text: "上游模型 ID" }), upstreamInput]),
          el("div", { class: "field", style: "grid-column:1/-1" }, [
            el("span", { text: "固定/允许 Provider" }),
            el("div", { class: "btn-row" }, [probeButton, selectAllButton, clearAllButton]),
            providerList,
            providerHint
          ]),
          el("label", { class: "field inline" }, [disabledInput, el("span", { text: "禁用该别名" })])
        ]),
        actions: function (dismiss) {
          return [el("button", { class: "btn", type: "button", onclick: dismiss }, "取消"), submit];
        }
      });
    }

    var testModel = el("select", {}, (list.filter(function (entry) { return !entry.disabled; }).map(function (entry) {
      return el("option", { value: entry.id, selected: payload.default_for_test === entry.id }, entry.id);
    })));
    if (!list.length) {
      testModel.appendChild(el("option", { value: "", text: "先添加模型别名" }));
    }
    var testCredential = el("select", {}, [el("option", { value: "", text: "自动（第一个启用凭据）" })].concat(
      (state.credentials || []).map(function (view) { return el("option", { value: view.id, text: view.label || view.id }); })));
    var testResult = el("div", { class: "hint", text: "尚未测试" });
    var testButton = el("button", { class: "btn primary", type: "button" }, "发送测试请求");
    testButton.addEventListener("click", function () {
      client.busy(testButton, "测试中…", async function () {
        testResult.className = "hint";
        testResult.textContent = "请求已发出…";
        try {
          var result = await client.api("/api/models/test", {
            method: "POST",
            body: JSON.stringify({ model: testModel.value, credential_id: testCredential.value })
          });
          if (result.ok) {
            testResult.className = "ok-text";
            testResult.textContent = "成功 · " + fmt.ms(result.latency_ms) + " · 上游模型 " + (result.upstream_model || "") +
              " · 回复：" + (result.reply || "(空)");
            ui.toast("模型测试通过（" + fmt.ms(result.latency_ms) + "）", "ok");
          } else {
            testResult.className = "bad-text";
            testResult.textContent = "失败" + (result.status ? "（HTTP " + result.status + "）" : "") + "：" +
              (result.error || "上游未返回错误信息") + " · " + fmt.ms(result.latency_ms);
            ui.toast("模型测试失败：" + (result.error || result.status), "bad");
          }
        } catch (error) {
          testResult.className = "bad-text";
          testResult.textContent = "测试请求失败：" + error.message;
          ui.toast("测试请求失败：" + error.message, "bad");
        }
      });
    });

    var rows = list.map(function (entry) { return { entry: entry }; });
    return ui.fragment([
      ui.section("模型别名", payload.passthrough ? "当前没有别名表：客户端模型 ID 直接透传给 Cline" : "别名表已启用",
        [el("button", { class: "btn primary", type: "button", onclick: function () { modelForm(null); } }, "添加别名")],
        el("div", { class: "card" }, ui.table([
          { label: "客户端模型 ID", render: function (row) { return el("span", { class: "mono cell-strong", text: row.entry.id }); } },
          { label: "上游模型 ID", render: function (row) { return el("span", { class: "mono", text: row.entry.upstream_id || row.entry.id }); } },
          { label: "固定/允许 Provider", render: function (row) { return (row.entry.providers || []).join(", ") || fmt.DASH; } },
          { label: "状态", render: function (row) { return row.entry.disabled ? ui.badge("禁用", "warn") : ui.badge("启用", "ok"); } },
          { label: "凭据可用性", render: function (row) {
              var enabled = (state.credentials || []).filter(function (view) { return view.enabled; });
              return enabled.length ? ui.badge(enabled.length + " 个启用凭据", "info") : ui.badge("没有启用凭据", "bad");
            } },
          { label: "操作", render: function (row) {
              return el("div", { class: "btn-row" }, [
                el("button", { class: "btn small", type: "button", onclick: function () { modelForm(row.entry); } }, "编辑"),
                el("button", { class: "btn small", type: "button", onclick: function () {
                  testModel.value = row.entry.id;
                  window.scrollTo({ top: 0, behavior: "smooth" });
                } }, "选中测试"),
                el("button", { class: "btn small danger", type: "button", onclick: function () {
                  if (!ui.confirm("删除别名 " + row.entry.id + "？")) return;
                  save(list.filter(function (item) { return item.id !== row.entry.id; }).map(function (item) {
                    return { id: item.id, upstream_id: item.upstream_id, providers: item.providers || [], provider_pipeline: item.provider_pipeline || "", disabled: Boolean(item.disabled) };
                  }), function () {});
                } }, "删除")
              ]);
            } }
        ], rows, { emptyTitle: "没有模型别名", emptyDetail: "没有别名时，客户端模型 ID 会原样透传给 Cline。" }))),
      ui.section("模型测试", "通过 ClinePassProxy 的真实数据路径发送一次最小请求",
        null,
        ui.card(null, null, null, el("div", { class: "stack" }, [
          el("div", { class: "filters" }, [
            el("label", { class: "field" }, [el("span", { text: "模型" }), testModel]),
            el("label", { class: "field" }, [el("span", { text: "凭据" }), testCredential]),
            testButton
          ]),
          testResult,
          el("p", { class: "hint", text: "测试失败时会显示上游返回的真实错误，不会用占位文案掩盖。" })
        ]))),
      ui.section("请求历史中出现过的模型", "来自本机请求历史，可用于核对上游实际模型名", null,
        ui.card(null, null, null, seen.length
          ? el("div", { class: "row" }, seen.map(function (model) { return ui.badge(model, "plain"); }))
          : ui.empty("还没有历史模型", "")))
    ]);
  }

  // ------------------------------------------------------------- requests

  async function requests() {
    var payload = await client.api("/api/requests" + client.query({
      limit: 200,
      model: state.filters.model,
      credential: state.filters.credential,
      provider: state.filters.provider,
      endpoint: state.filters.endpoint,
      status: state.filters.status,
      range: state.filters.range
    }));
    state.requests = payload;
    var records = payload.requests || [];
    var facets = payload.facets || {};

    function facetSelect(key, label, values, current) {
      var select = el("select", {
        onchange: function (event) {
          state.filters[key] = event.target.value;
          root.CPPApp.render();
        }
      }, [el("option", { value: "", text: "全部" })].concat((values || []).map(function (value) {
        return el("option", { value: value, selected: value === current, text: value });
      })));
      return el("label", { class: "field" }, [el("span", { text: label }), select]);
    }

    var rangeSelect = el("select", {
      onchange: function (event) {
        state.filters.range = event.target.value;
        root.CPPApp.render();
      }
    }, ["1h", "24h", "7d", "31d", "all"].map(function (value) {
      return el("option", { value: value, selected: value === state.filters.range, text: value === "all" ? "全部时间" : "近 " + value });
    }));

    var filters = el("div", { class: "filters" }, [
      facetSelect("model", "模型", facets.models, state.filters.model),
      facetSelect("credential", "凭据", facets.credentials, state.filters.credential),
      facetSelect("provider", "Provider", facets.providers, state.filters.provider),
      facetSelect("endpoint", "端点", facets.endpoints, state.filters.endpoint),
      el("label", { class: "field" }, [el("span", { text: "状态" }), el("select", {
        onchange: function (event) {
          state.filters.status = event.target.value;
          root.CPPApp.render();
        }
      }, [
        el("option", { value: "", text: "全部", selected: state.filters.status === "" }),
        el("option", { value: "ok", text: "成功", selected: state.filters.status === "ok" }),
        el("option", { value: "error", text: "失败", selected: state.filters.status === "error" }),
        el("option", { value: "502", text: "HTTP 502", selected: state.filters.status === "502" }),
        el("option", { value: "429", text: "HTTP 429", selected: state.filters.status === "429" })
      ])]),
      el("label", { class: "field" }, [el("span", { text: "时间范围" }), rangeSelect]),
      el("button", { class: "btn", type: "button", onclick: function () {
        state.filters = { model: "", credential: "", provider: "", endpoint: "", status: "", range: "24h" };
        root.CPPApp.render();
      } }, "清除筛选"),
      el("span", { class: "hint", text: "共 " + records.length + " 条（最多 200 条，按时间倒序）" })
    ]);

    return ui.fragment([
      ui.section("请求诊断", "所有字段来自本机记录；点任意一行查看时间线与尝试记录", null,
        el("div", { class: "stack" }, [
          ui.card(null, null, null, filters),
          el("div", { class: "card" }, requestTable(records,
            "该筛选条件下没有请求",
            state.filters.range === "all" ? "客户端请求到达后会记录在这里。" : "换一个时间范围或清除筛选试试。"))
        ]))
    ]);
  }

  // ------------------------------------------------------------- usage

  function observedStats(summary, rangeLabel) {
    var successRate = summary.requests ? summary.successes / summary.requests : null;
    return ui.stats([
      ui.stat("请求数", fmt.int(summary.requests), "", rangeLabel + " · 失败 " + fmt.int(summary.errors)),
      ui.stat("成功率", fmt.ratio(successRate, 1), "", "成功 " + fmt.int(summary.successes)),
      ui.stat("Prompt tokens", fmt.scaled(summary.prompt_tokens), "", "本机观测"),
      ui.stat("Cached tokens", fmt.scaled(summary.cached_tokens), "", "本机观测"),
      ui.stat("Completion tokens", fmt.scaled(summary.completion_tokens), "", "本机观测"),
      ui.stat("Reasoning tokens", fmt.scaled(summary.reasoning_tokens), "", "本机观测"),
      ui.stat("缓存命中率", fmt.ratio(summary.cache_hit_ratio), "", "cached / prompt"),
      ui.stat("平均 TTFT", fmt.ms(summary.average_ttft_ms), "", "首个可见 token"),
      ui.stat("平均耗时", fmt.ms(summary.average_duration_ms), "", "端到端"),
      ui.stat("解码 TPS", fmt.fixed(summary.average_decode_tps, 2), "", "生成阶段")
    ]);
  }

  function credentialUsageTable(views) {
    return ui.table([
      { label: "凭据", render: function (row) {
          return el("div", { class: "stack", style: "gap:2px" }, [
            el("span", { class: "cell-strong", text: row.view.label || row.view.id }),
            el("span", { class: "mono muted", text: row.view.masked_key || "" })
          ]);
        } },
      { label: "健康", render: function (row) { return healthBadge(row.view.health); } },
      { label: "套餐", render: function (row) {
          var official = row.view.official;
          return official && official.plan_name ? official.plan_name : fmt.DASH;
        } },
      { label: "5 小时", render: function (row) { return windowCell(row.view, "five_hour"); } },
      { label: "本周", render: function (row) { return windowCell(row.view, "weekly"); } },
      { label: "本月", render: function (row) { return windowCell(row.view, "monthly"); } },
      { label: "余额", num: true, render: function (row) {
          var official = row.view.official;
          return official && official.available ? fmt.money(quota.tokens(official).balance, 4) : fmt.DASH;
        } },
      { label: "检查时间", render: function (row) {
          var official = row.view.official;
          return official && official.fetched_at ? fmt.date(official.fetched_at) : fmt.DASH;
        } }
    ], (views || []).map(function (view) { return { view: view }; }),
      { emptyTitle: "还没有凭据", emptyDetail: "添加凭据后这里会显示每个账号的官方额度。" });
  }

  function windowCell(view, type) {
    var snapshot = view.official;
    if (!snapshot || !snapshot.available) return el("span", { class: "muted", text: fmt.DASH });
    var limits = quota.limits(snapshot).filter(function (limit) { return limit.type === type; });
    if (!limits.length) return el("span", { class: "muted", text: fmt.DASH });
    var limit = limits[0];
    return el("div", { class: "stack", style: "gap:4px;min-width:110px" }, [
      el("span", { class: "hint", text: fmt.percent(limit.percent, 1) + " 已用 · 重置 " + quota.resetText(limit) }),
      ui.progress(limit.percent)
    ]);
  }

  async function usage() {
    var payload = await client.api("/api/usage" + client.query({ range: state.usageRange }));
    state.usage = payload;
    var summary = payload.summary || {};
    var official = payload.official || {};
    var rangeLabel = state.usageRange === "all" ? "全部时间" : "近 " + state.usageRange;

    var rangeSelect = el("select", {
      onchange: function (event) {
        state.usageRange = event.target.value;
        root.CPPApp.render();
      }
    }, ["1h", "24h", "7d", "31d", "all"].map(function (value) {
      return el("option", { value: value, selected: value === state.usageRange, text: value === "all" ? "全部时间" : "近 " + value });
    }));

    return ui.fragment([
      officialSection(official.snapshots, official.refreshed_at, {
        interval: official.interval_seconds,
        after: function () { root.CPPApp.render(); }
      }),
      ui.section("本机观测（经过 ClinePassProxy 的流量）",
        "与上面的官方数值口径不同：这里只统计经过本代理的请求",
        [el("label", { class: "field inline" }, [el("span", { text: "范围" }), rangeSelect])],
        ui.stats(observedStats(summary, rangeLabel))),
      ui.section("各凭据官方额度", "每张卡片对应一个 Cline 账号", null,
        el("div", { class: "card" }, credentialUsageTable(payload.credentials || []))),
      ui.section("窗口对比（本机）", "同一批请求在不同时间窗口下的表现", null,
        ui.card(null, null, null, ui.table([
          { label: "窗口", render: function (row) { return row.label; } },
          { label: "请求数", num: true, render: function (row) { return fmt.int(row.summary.requests); } },
          { label: "成功", num: true, render: function (row) { return fmt.int(row.summary.successes); } },
          { label: "失败", num: true, render: function (row) { return fmt.int(row.summary.errors); } },
          { label: "Prompt", num: true, render: function (row) { return fmt.scaled(row.summary.prompt_tokens); } },
          { label: "Cached", num: true, render: function (row) { return fmt.scaled(row.summary.cached_tokens); } },
          { label: "Completion", num: true, render: function (row) { return fmt.scaled(row.summary.completion_tokens); } },
          { label: "命中率", num: true, render: function (row) { return fmt.ratio(row.summary.cache_hit_ratio); } },
          { label: "平均 TTFT", num: true, render: function (row) { return fmt.ms(row.summary.average_ttft_ms); } }
        ], ["1h", "24h", "7d", "31d", "all"].map(function (key) {
          return { label: key === "all" ? "全部时间" : "近 " + key, summary: (payload.windows || {})[key] || {} };
        }), { emptyTitle: "没有数据", emptyDetail: "" }))),
      el("p", { class: "hint", text: "请求历史保留 " + fmt.int(payload.retention) + " 条；更早的记录已被截断，因此“全部时间”也只覆盖保留区内的数据。" })
    ]);
  }

  // ------------------------------------------------------------- settings

  async function settings() {
    var config = await client.api("/api/config");
    state.config = config;

    function field(label, input, hint) {
      return el("label", { class: "field" }, [
        el("span", { text: label }),
        input,
        hint ? el("span", { class: "hint", text: hint }) : null
      ]);
    }

    var baseURL = el("input", { value: config.base_url || "" });
    var timeout = el("input", { type: "number", value: config.timeout_seconds });
    var maxBytes = el("input", { type: "number", value: config.max_response_bytes });
    var retention = el("input", { type: "number", value: config.log_retention });
    var failover = el("input", { type: "number", value: config.retry_failover });
    var affinity = el("select", {}, ["session", "sticky", "round"].map(function (value) {
      return el("option", { value: value, selected: config.affinity === value, text: value });
    }));
    var affinityHeader = el("input", { value: config.affinity_header || "", placeholder: "留空 = 自动检测" });
    var bodyBytes = el("input", { type: "number", value: config.request_log_body_bytes });
    var managementUser = el("input", { value: config.management_user || "" });
    var allowUnauth = el("input", { type: "checkbox", checked: Boolean(config.allow_unauthenticated) });
    var gatewayKeys = el("textarea", {
      placeholder: config.gateway_key_count
        ? "留空保持不变。当前已配置 " + config.gateway_key_count + " 个 Key：" + (config.gateway_keys || []).join(", ")
        : "每行一个网关 Key。留空 = 关闭推理入口（除非显式允许匿名）。"
    });
    var managementToken = el("input", { type: "password", placeholder: "留空保持不变（轮换后所有客户端需重新认证）" });

    var save = el("button", { class: "btn primary", type: "button" }, "保存设置");
    save.addEventListener("click", function () {
      client.busy(save, "保存中…", async function () {
        var payload = {
          base_url: baseURL.value.trim(),
          timeout_seconds: Number(timeout.value),
          max_response_bytes: Number(maxBytes.value),
          log_retention: Number(retention.value),
          retry_failover: Number(failover.value),
          affinity: affinity.value,
          affinity_header: affinityHeader.value.trim(),
          request_log_body_bytes: Number(bodyBytes.value),
          management_user: managementUser.value.trim(),
          allow_unauthenticated: allowUnauth.checked
        };
        var keys = gatewayKeys.value.split("\n").map(function (value) { return value.trim(); }).filter(Boolean);
        if (keys.length) payload.gateway_keys = keys;
        if (managementToken.value.trim()) payload.management_token = managementToken.value.trim();
        try {
          await client.api("/api/config", { method: "PUT", body: JSON.stringify(payload) });
          ui.toast("设置已保存", "ok");
          root.CPPApp.render();
        } catch (error) {
          ui.toast("保存设置失败：" + error.message, "bad");
        }
      });
    });

    return ui.fragment([
      ui.section("外观", "主题设置保存在本机浏览器", null,
        ui.card(null, null, null, el("div", { class: "row" }, [
          el("label", { class: "field inline" }, [el("span", { text: "主题" }),
            el("select", { id: "theme-select-inline" }, [
              el("option", { value: "system", text: "跟随系统（默认）" }),
              el("option", { value: "light", text: "浅色" }),
              el("option", { value: "dark", text: "深色" })
            ])]),
          el("span", { class: "hint", text: "在 CPAMP 内嵌打开时，跟随系统会优先采用 CPAMP 的主题。" })
        ]))),
      ui.section("上游与数据面", "保存后立即生效（除管理令牌外都不需要重启）", [save],
        ui.card(null, null, null, el("div", { class: "form-grid" }, [
          field("Cline Base URL", baseURL, "默认 https://api.cline.bot/api/v1"),
          field("超时（秒）", timeout, "10 ~ 3600"),
          field("响应上限（字节）", maxBytes, "64 KiB ~ 512 MiB"),
          field("请求历史保留条数", retention, "50 ~ 100000"),
          field("失败切换次数", failover, "0 ~ 5"),
          field("会话亲和", affinity, "session = 按会话哈希，sticky = 固定凭据，round = 轮询"),
          field("亲和头", affinityHeader, "留空则自动检测，例如 x-session-id"),
          field("诊断保留的请求体字节", bodyBytes, "0 ~ 8 MiB；请求体只用于推导会话标识")
        ]))),
      ui.section("认证", "管理令牌 ≠ 网关 Key ≠ Cline 凭据", null,
        ui.card(null, null, null, el("div", { class: "form-grid" }, [
          field("管理用户名", managementUser, "用于 HTTP Basic 登录"),
          field("轮换管理令牌", managementToken, "不会回显当前令牌"),
          field("网关 Key", gatewayKeys, config.gateway_key_count
            ? "当前 " + config.gateway_key_count + " 个（已脱敏显示），留空保持不变"
            : "当前未配置：推理入口已关闭")
        ]))),
      ui.card("允许匿名推理", "危险开关", null, el("label", { class: "field inline" }, [
        allowUnauth,
        el("span", { text: "在没有网关 Key 时仍然开放 /v1/*（仅在本机或内网使用）" })
      ])),
      ui.section("诊断信息", null, null,
        ui.card(null, null, null, ui.kv([
          ["数据目录", el("span", { class: "mono", text: config.data_dir })],
          ["配置文件权限", "0600 / 目录 0700"],
          ["管理令牌", "仅服务端持有，接口从不返回"]
        ])))
    ]);
  }

  root.CPPPages = {
    state: state,
    dashboard: dashboard,
    credentials: credentials,
    models: models,
    requests: requests,
    usage: usage,
    settings: settings,
    officialSection: officialSection,
    openRequestDetail: openRequestDetail
  };
})(typeof globalThis !== "undefined" ? globalThis : this);
