"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Node {
  constructor(tag) {
    this.tag = tag;
    this.children = [];
    this.attributes = {};
    this.listeners = {};
    this.value = "";
    this.className = "";
    this.checked = false;
    this.disabled = false;
  }
  set textContent(value) { this.children = []; this.text = String(value); }
  get textContent() { return (this.text || "") + this.children.map(node => node.textContent).join(""); }
  setAttribute(name, value) { this.attributes[name] = value; }
  appendChild(node) { this.children.push(node); return node; }
  replaceChildren(...nodes) { this.children = nodes; this.text = ""; }
  addEventListener(name, listener) { (this.listeners[name] ||= []).push(listener); }
  emit(name) {
    if (name === "click" && this.disabled) return;
    for (const listener of this.listeners[name] || []) listener({ target: this, currentTarget: this });
  }
}

function nodes(root, predicate) {
  return [root, ...root.children.flatMap(child => nodes(child, () => true))].filter(predicate);
}

async function modelForm(options = {}) {
  const pending = [];
  let modal;
  let saved;
  const entry = Object.assign({ id: "glm", upstream_id: "cline-pass/glm-5.3-flash", providers: ["saved"], observed_providers: ["history"] }, options.entry);
  const context = {
    Node,
    document: {
      createElement: tag => new Node(tag),
      createDocumentFragment: () => new Node("fragment"),
      createTextNode: text => { const node = new Node("text"); node.textContent = text; return node; }
    },
    CPPFormat: { DASH: "-" }, CPPQuota: {}, CPPApp: { render() {} },
    CPPApi: {
      async api(url, request) {
        if (url === "/api/models") return Object.assign({ models: [entry], providers: ["global"], observed_providers: ["other-model"] }, options.payload);
        if (url === "/api/config" && request) { saved = JSON.parse(request.body).models; return {}; }
        if (url === "/api/config") return {};
        if (url === "/api/credentials") return { credentials: [] };
        if (url === "/api/models/providers/probe") return options.probe(JSON.parse(request.body));
        throw new Error("Unexpected API: " + url);
      },
      busy(button, label, callback) { pending.push(callback()); }
    }
  };
  vm.createContext(context);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "components.js"), "utf8"), context);
  context.CPPUI.toast = () => {};
  context.CPPUI.modal = settings => {
    modal = new Node("modal");
    modal.appendChild(settings.body);
    settings.actions(() => {}).forEach(node => modal.appendChild(node));
    return () => {};
  };
  vm.runInContext(fs.readFileSync(path.join(__dirname, "pages.js"), "utf8"), context);
  const page = await context.CPPPages.models();
  nodes(page, node => node.tag === "button" && node.textContent === "编辑")[0].emit("click");
  const button = label => nodes(modal, node => node.tag === "button" && node.textContent === label)[0];
  return {
    modal, button,
    choices: () => nodes(modal, node => node.tag === "label" && nodes(node, child => child.tag === "span" && child.className === "mono").length),
    names: () => nodes(modal, node => node.tag === "span" && node.className === "mono").map(node => node.textContent),
    async click(label) { button(label).emit("click"); await Promise.all(pending.splice(0)); await Promise.resolve(); },
    saved: () => saved
  };
}

test("model form excludes global provider history and labels its own unconfirmed values", async () => {
  const form = await modelForm();
  assert.deepEqual(form.names(), ["history", "saved"]);
  assert.match(form.choices()[0].textContent, /历史命中/);
  assert.match(form.choices()[1].textContent, /未确认/);
  assert.equal(form.button("全选").disabled, true);
});

test("failed probe remains an error even with historical or saved provider rows", async () => {
  const form = await modelForm({ probe: async () => ({ ok: false, error: "HTTP 404", providers: ["untrusted"] }) });
  await form.click("探测 Provider");
  assert.match(form.modal.textContent, /探测失败：HTTP 404/);
  assert.doesNotMatch(form.modal.textContent, /发现 \d+ 个 Provider/);
  assert.deepEqual(form.names(), ["saved"]);
  assert.equal(form.button("全选").disabled, true);
});

test("HTTP probe failure and empty catalog cannot reuse historical provider counts", async () => {
  for (const probe of [async () => { throw new Error("HTTP 404"); }, async () => ({ ok: true, providers: [] })]) {
    const form = await modelForm({ probe });
    await form.click("探测 Provider");
    assert.match(form.modal.textContent, /探测失败：/);
    assert.doesNotMatch(form.modal.textContent, /发现 \d+ 个 Provider/);
    assert.deepEqual(form.names(), ["saved"]);
    assert.equal(form.button("全选").disabled, true);
  }
});

test("successful probes replace catalog and discard unconfirmed saved selections", async () => {
  const results = [
    { ok: true, pipeline: "planner", providers: [{ name: "runware", status: "available" }] },
    { ok: true, pipeline: "planner", providers: [{ name: "alibaba", status: "available" }] }
  ];
  const form = await modelForm({ probe: async () => results.shift() });
  await form.click("探测 Provider");
  assert.deepEqual(form.names(), ["runware"]);
  assert.match(form.modal.textContent, /发现 1 个 Provider/);
  assert.match(form.choices()[0].textContent, /上游列出/);
  await form.click("探测 Provider");
  assert.deepEqual(form.names(), ["alibaba"]);
  await form.click("全选");
  await form.click("保存");
  assert.deepEqual(form.saved()[0].providers.sort(), ["alibaba"]);
});

test("changing model inputs clears stale choices and unconfirmed selections", async () => {
  const form = await modelForm({ probe: async () => ({ ok: true, pipeline: "planner", providers: ["runware"] }) });
  await form.click("探测 Provider");
  const inputs = nodes(form.modal, node => node.tag === "input" && !node.attributes.type);
  inputs[1].value = "cline-pass/deepseek-v4.1-flash";
  inputs[1].emit("input");
  assert.deepEqual(form.names(), []);
  assert.equal(form.button("全选").disabled, true);
  await form.click("全选");
  await form.click("保存");
  assert.deepEqual(form.saved()[0].providers, []);
});

test("late probe response cannot repopulate a different model's choices", async () => {
  let resolve;
  const form = await modelForm({ probe: () => new Promise(done => { resolve = done; }) });
  const probing = form.click("探测 Provider");
  const input = nodes(form.modal, node => node.tag === "input" && !node.attributes.type)[0];
  input.value = "other-model";
  input.emit("input");
  resolve({ ok: true, providers: ["old-model-provider"] });
  await probing;
  assert.deepEqual(form.names(), ["saved"]);
  assert.equal(form.button("全选").disabled, true);
});

test("ignored upstream routing is explicit and cannot enable fake provider choices", async () => {
  const form = await modelForm({ probe: async () => ({
    ok: false, pinnable: false, pinning_status: "routing_ignored", pipeline: "planner",
    actual_provider: "openai-compatible-private", canonical_slug: "private/deepseek-v4p1-flash-contributor",
    error: "ignored restriction", providers: ["alibaba"]
  }) });
  await form.click("探测 Provider");
  assert.match(form.modal.textContent, /当前模型不支持固定 Provider/);
  assert.match(form.modal.textContent, /实际命中：openai-compatible-private/);
  assert.match(form.modal.textContent, /private\/deepseek-v4p1-flash-contributor/);
  assert.doesNotMatch(form.names().join(","), /alibaba/);
  assert.equal(form.button("全选").disabled, true);
});

test("provider catalog without a detected pipeline is not selectable", async () => {
  const form = await modelForm({ probe: async () => ({ ok: true, providers: ["runware"] }) });
  await form.click("探测 Provider");
  assert.match(form.modal.textContent, /探测失败：/);
  assert.equal(form.button("全选").disabled, true);
  await form.click("保存");
  assert.deepEqual(form.saved()[0].providers, []);
});

test("editing a confirmed provider selection keeps it without requiring another probe", async () => {
  const form = await modelForm({ entry: { providers: ["gmicloud"], provider_pipeline: "direct" } });
  await form.click("保存");
  assert.deepEqual(form.saved()[0].providers, ["gmicloud"]);
  assert.equal(form.saved()[0].provider_pipeline, "direct");
});
