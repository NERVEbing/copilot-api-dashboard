const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { test } = require("node:test");
const vm = require("node:vm");

function app(baseURI = "http://localhost/", fetchImpl = () => new Promise(() => {})) {
  const elements = new Map();
  const node = (properties = {}) => {
    const listeners = new Map(), attributes = new Map();
    let innerHTML = "", innerHTMLWrites = 0;
    return Object.assign({
      hidden: true, style: {}, offsetWidth: 190, offsetHeight: 100,
      addEventListener(type, listener) { listeners.set(type, listener); },
      dispatch(type, event = {}) {
        const dispatched = { preventDefault() { this.defaultPrevented = true; }, pointerType: "mouse", clientX: 0, ...event };
        listeners.get(type)?.(dispatched);
        return dispatched;
      },
      closest(selector) { return selector === "button[data-sort]" && this.dataset?.sort ? this : null; },
      focus() { document.activeElement = this; },
      setAttribute(name, value) { attributes.set(name, String(value)); },
      getAttribute(name) { return attributes.get(name); },
      get innerHTML() { return innerHTML; },
      set innerHTML(value) { innerHTML = value; innerHTMLWrites++; },
      get innerHTMLWrites() { return innerHTMLWrites; },
    }, properties);
  };
  const guide = node(), active = node(), tooltip = node();
  const svg = node({ getBoundingClientRect() { return { left: 0, top: 0, width: 768, height: 220 }; } });
  const hitArea = node();
  const plot = node({
    hidden: false,
    getBoundingClientRect() { return { left: 0, top: 0, width: 768, height: 220 }; },
    querySelector(selector) { return ({ svg, ".chart-hit-area": hitArea, ".chart-guide": guide, ".chart-active": active, ".chart-tooltip": tooltip })[selector] ?? null; },
    focus() { this.dispatch("focus"); },
  });
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, node({
      innerHTML: "", textContent: "", hidden: false, clientWidth: 800, options: [],
      classList: { toggle() {} }, querySelector(selector) { return id === "trend" && selector === ".chart-plot" && this.innerHTML.includes('class="chart-plot"') ? plot : null; },
      replaceChildren(...options) { this.options = options; },
      add(option) { this.options = [...this.options, option]; },
    }));
    return elements.get(id);
  };
  const accountSortButtons = Array.from({ length: 9 }, (_, column) => node({ dataset: { sort: "accounts", column: String(column) } }));
  const accountTable = node({
    scrollLeft: 0,
    contains(target) { return target === this || accountSortButtons.includes(target); },
    querySelector(selector) { return accountSortButtons[Number(selector.match(/data-column="(\d+)"/)?.[1])]; },
  });
  Object.defineProperty(accountTable, "outerHTML", { set(value) {
    element("accounts").innerHTML = value;
    if (accountTable.contains(document.activeElement)) document.activeElement = null;
    accountTable.scrollLeft = 0;
  } });
  const document = node({
    baseURI, getElementById: element, activeElement: null,
    querySelector(selector) { return selector === '[data-table="accounts"]' ? accountTable : null; },
  });
  const location = new URL(baseURI);
  const historyCalls = [];
  const navigate = (method) => (_state, _title, url) => { historyCalls.push([method, String(url)]); location.href = new URL(url, location.href).href; };
  const windowListeners = new Map();
  const context = vm.createContext({
    document, location,
    history: { pushState: navigate("push"), replaceState: navigate("replace") },
    window: { addEventListener(type, listener) { windowListeners.set(type, listener); } },
    ResizeObserver: class { observe() {} },
    Option: class { constructor(text, value) { this.text = text; this.value = value; } },
    AbortController, URL, URLSearchParams, Intl,
    fetch: fetchImpl,
  });
  vm.runInContext(readFileSync(`${__dirname}/app.js`, "utf8"), context);
  return { run: (code) => vm.runInContext(code, context), element, document, accountTable, chart: { plot, svg, hitArea, guide, active, tooltip }, location, historyCalls, popstate: () => windowListeners.get("popstate")() };
}

test("API URLs follow the document base path", () => {
  const { run } = app("https://example.test/copilot-dashboard/");
  assert.equal(run("new URL('api/v1/dashboard', appBaseURL).pathname"), "/copilot-dashboard/api/v1/dashboard");
});

test("manual refresh syncs before reading while ordinary refresh only reads", async () => {
  const calls = [];
  const fetch = async (url, options) => {
    calls.push({ method: options.method, path: url.pathname });
    const data = url.pathname.endsWith("/dashboard") ? { period: "last_30_days", accounts: [], totals: null, by_model: null, days: null } : { synced: true };
    return { ok: true, status: 200, json: async () => ({ data, errors: [] }) };
  };
  const { run } = app("http://localhost/", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  calls.length = 0;
  await run("refresh(true)");
  assert.deepEqual(calls, [
    { method: "POST", path: "/api/v1/sync" },
    { method: "GET", path: "/api/v1/dashboard" },
  ]);
  calls.length = 0;
  await run("refresh()");
  assert.deepEqual(calls, [{ method: "GET", path: "/api/v1/dashboard" }]);
});

test("compact tokens handle unit boundaries", () => {
  const { run } = app();
  for (const [value, expected] of [[0, "0"], [999, "999"], [1247, "1.25K"], [219212271, "219.21M"], [1000000000, "1B"], [999999, "1M"]]) {
    assert.equal(run(`tokens(${value})`), expected);
  }
  assert.equal(run("tokens(null)"), "—");
  assert.equal(run("number(12345)"), "12,345");
  assert.equal(run("percent(79.746)"), "79.7");
});

test("money formats amounts and distinguishes subcent from zero", () => {
  const { run } = app();
  const money = (value) => run(`money([{currency:'USD',total_cost_nanos:${value}}])`);
  assert.ok(money(170124278240).includes("USD 170.12"));
  assert.equal(money(3250000), '<span class="money">USD 0.00325</span>');
  assert.equal(money(10000000), '<span class="money">USD 0.01</span>');
  assert.ok(money(1).includes("USD &lt;0.000001"));
  assert.equal(money(0), '<span class="money">USD 0.00</span>');
  assert.equal(run("money([])"), "—");
  assert.equal(run("money(null)"), "—");
  assert.ok(run("money([{currency:'<img>',total_cost_nanos:1}])").includes("&lt;img&gt;"));
});

test("cache hit rate includes cache creation and distinguishes missing from zero", () => {
  const { run } = app();
  for (const [usage, expected] of [
    [{ input_tokens: 100, cache_read_input_tokens: 20, cache_creation_input_tokens: 10 }, "15.4%"],
    [{ input_tokens: 0, cache_read_input_tokens: 5, cache_creation_input_tokens: 0 }, "100%"],
    [{ input_tokens: 10, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 }, "0%"],
    [{ input_tokens: 0, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 }, "—"],
    [null, "—"], [{}, "—"],
    [{ input_tokens: 10, cache_read_input_tokens: 2 }, "—"],
    [{ input_tokens: null, cache_read_input_tokens: 2, cache_creation_input_tokens: 0 }, "—"],
    [{ input_tokens: -1, cache_read_input_tokens: 2, cache_creation_input_tokens: 0 }, "—"],
    [{ input_tokens: 10, cache_read_input_tokens: "2", cache_creation_input_tokens: 0 }, "—"],
  ]) assert.equal(run(`cacheHitRateText(${JSON.stringify(usage)})`), expected);
  assert.equal(run("cacheHitRateText({input_tokens:Infinity,cache_read_input_tokens:2,cache_creation_input_tokens:0})"), "—");
  assert.equal(run("cacheHitRateText({input_tokens:NaN,cache_read_input_tokens:2,cache_creation_input_tokens:0})"), "—");
  assert.equal(run("cacheHitRateText({input_tokens:Number.MAX_VALUE,cache_read_input_tokens:Number.MAX_VALUE,cache_creation_input_tokens:0})"), "—");
});

test("cache summary uses combined counters for all accounts and selected-account totals", () => {
  const { run, element } = app();
  run(`const usageAccounts = [
    {login:'small',totals:{input_tokens:1,cache_read_input_tokens:9,cache_creation_input_tokens:0}},
    {login:'large',totals:{input_tokens:90,cache_read_input_tokens:10,cache_creation_input_tokens:0}}
  ]; renderDashboard({accounts:usageAccounts,totals:{input_tokens:91,cache_read_input_tokens:19,cache_creation_input_tokens:0}});`);
  assert.ok(element("metrics").innerHTML.includes("17.3%"));
  assert.ok(!element("metrics").innerHTML.includes("50%"));
  assert.equal((element("metrics").innerHTML.match(/class="metric"/g) ?? []).length, 4);
  run("state.account='small';renderDashboard({accounts:usageAccounts,totals:usageAccounts[0].totals})");
  assert.ok(element("metrics").innerHTML.includes("90%"));
  assert.equal((element("metrics").innerHTML.match(/class="metric"/g) ?? []).length, 8);
});

test("cache column sorts unrounded rates and keeps missing rates last", () => {
  const { run, element } = app();
  run(`const cacheModels = [
    {model:'missing'},
    {model:'alpha',input_tokens:9999,cache_read_input_tokens:11,cache_creation_input_tokens:0},
    {model:'zeta',input_tokens:9999,cache_read_input_tokens:10,cache_creation_input_tokens:0},
    {model:'zero',input_tokens:10,cache_read_input_tokens:0,cache_creation_input_tokens:0}
  ]; sorts.models={column:5,direction:1};renderModels(cacheModels);`);
  const names = () => [...element("models").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["zero", "zeta", "alpha", "missing"]);
  assert.equal((element("models").innerHTML.match(/>0\.1%<\/td>/g) ?? []).length, 2);
  run("sorts.models.direction=-1;renderModels(cacheModels)");
  assert.deepEqual(names(), ["alpha", "zeta", "zero", "missing"]);
});

test("event duration distinguishes missing and zero and follows upstream units", () => {
  const { run } = app();
  for (const [value, expected] of [[0, "0 ms"], [12.5, "13 ms"], [999, "999 ms"], [1000, "1.00 s"], [1234.5, "1.23 s"], [null, "—"], [-1, "—"], ["5", "—"]]) {
    assert.equal(run(`duration(${JSON.stringify(value)})`), expected);
  }
  for (const expression of ["undefined", "NaN", "Infinity", "-Infinity"]) assert.equal(run(`duration(${expression})`), "—");
});

test("events show HTTP completion outcomes and retain timing through pagination", () => {
  const { run, element } = app();
  const items = [
    { model: "completed", duration_ms: 1234.5, request_outcome: "completed" },
    { model: "cancelled", duration_ms: 0, request_outcome: "cancelled" },
    { model: "error", duration_ms: 12.5, request_outcome: "error" },
    { model: "historical", duration_ms: null, request_outcome: null },
    { model: "unknown", request_outcome: "<img src=x onerror=alert(1)>" },
  ];
  run(`renderEvents(${JSON.stringify({ page: 2, total_pages: 3, total: 45, items })})`);
  const html = element("events").innerHTML;
  assert.ok(html.includes("HTTP/body completion. Completed does not certify model success."));
  assert.ok(html.includes('<td class="number">1.23 s</td>'));
  assert.ok(html.includes('<td class="number">0 ms</td>'));
  assert.ok(html.includes('<td class="number">13 ms</td>'));
  assert.ok(html.includes("<td>Completed</td>"));
  assert.ok(html.includes('<td><span class="outcome outcome-cancelled">Cancelled</span></td>'));
  assert.ok(html.includes('<td><span class="outcome outcome-error">Error</span></td>'));
  assert.equal((html.match(/<td>—<\/td><\/tr>/g) ?? []).length, 2);
  assert.ok(!html.includes("<img"));
  assert.ok(!html.includes("<td>Success</td>"));
  assert.equal(element("events-count").textContent, `45 requests · Times in ${Intl.DateTimeFormat().resolvedOptions().timeZone}`);
  assert.equal(element("page-label").textContent, "Page 2 of 3");
  assert.equal(element("previous").disabled, false);
  assert.equal(element("next").disabled, false);
});

test("numeric sorting uses original values, keeps missing last, and does not mutate input", () => {
  const { run, element } = app();
  run(`const models = [
    {model:'missing',total_tokens:null}, {model:'lower',total_tokens:1246},
    {model:'higher',total_tokens:1247}, {model:'zero',total_tokens:0}
  ]; renderModels(models);`);
  const names = () => [...element("models").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["higher", "lower", "zero", "missing"]);
  run("sorts.models.direction = 1; renderModels(models)");
  assert.deepEqual(names(), ["zero", "lower", "higher", "missing"]);
  assert.equal(run("models[0].model"), "missing");
  assert.ok(element("models").innerHTML.includes('aria-sort="ascending"'));
});

test("accounts sort by used values descending by default and display quota usage", () => {
  const { run, element } = app();
  run(`const accounts = [
    {login:'Missing'},
    {login:'Unlimited',quota_snapshots:{premium_interactions:{unlimited:true,credits_used:26129}}},
    {login:'ZeroQuota',quota_snapshots:{premium_interactions:{unlimited:false,quota_remaining:0,entitlement:0,percent_remaining:0}}},
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,credits_used:5000,quota_remaining:800,entitlement:1000}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,credits_used:90,quota_remaining:10,entitlement:100,percent_remaining:10}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,has_quota:false,credits_used:1,quota_remaining:1,entitlement:300}}}
  ]; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["Unlimited", "RemainingLow", "AbsoluteHigh", "PercentHigh", "Missing", "ZeroQuota"]);
  assert.ok(element("accounts").innerHTML.includes(">Usage "));
  assert.ok(element("accounts").innerHTML.includes(">Total "));
  assert.ok(!element("accounts").innerHTML.includes("Premium"));
  assert.ok(!element("accounts").innerHTML.includes('scope="colgroup"'));
  assert.ok(element("accounts").innerHTML.includes('<progress value="90" max="100" aria-label="90% used"></progress><span class="quota-usage-value">90%</span>'));
  assert.ok(element("accounts").innerHTML.includes('<span class="unlimited">Unlimited</span>'));
  assert.ok(run("quotaUsage({unlimited:false,entitlement:100,quota_remaining:0,percent_remaining:0})").includes('class="quota-usage exhausted"><progress value="100" max="100" aria-label="100% used"></progress><span class="quota-usage-value">100%</span>'));
  assert.equal(run("usedPercentValue({unlimited:false,entitlement:0,quota_remaining:0,percent_remaining:0})"), null);
  assert.equal(run("quotaUsage({unlimited:false,entitlement:0,quota_remaining:0,percent_remaining:0})"), "—");
});

test("d toggles only account table names and preserves sorting, focus, scroll, and requests", () => {
  const calls = [];
  const { run, element, document, accountTable } = app("http://localhost/", (...args) => { calls.push(args); return new Promise(() => {}); });
  run(`renderDashboard({accounts:[
    {login:'account-a',display_name:'Zulu'}, {login:'account-b',display_name:'Alpha'}, {login:'unmapped'}
  ]}); sorts.accounts = {column:0,direction:1}; redrawTable('accounts'); state.page = 2;`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["account-a", "account-b", "unmapped"]);
  const originalOptions = element("account").options;
  const otherViews = ["identity", "metrics", "trend", "models", "events"].map((id) => element(id).innerHTML);
  accountTable.scrollLeft = 120;
  const focused = accountTable.querySelector('[data-column="0"]');
  focused.focus();
  const requestCount = calls.length;
  assert.equal(document.dispatch("keydown", { key: "d" }).defaultPrevented, true);
  assert.deepEqual(names(), ["Alpha", "unmapped", "Zulu"]);
  assert.equal(document.activeElement, focused);
  assert.equal(accountTable.scrollLeft, 120);
  assert.equal(run("sorts.accounts.column"), 0);
  assert.equal(run("sorts.accounts.direction"), 1);
  assert.equal(run("state.page"), 2);
  assert.equal(run("state.account"), "");
  assert.equal(calls.length, requestCount);
  assert.equal(element("account").options, originalOptions);
  assert.deepEqual(["identity", "metrics", "trend", "models", "events"].map((id) => element(id).innerHTML), otherViews);
  document.dispatch("keydown", { key: "d" });
  assert.deepEqual(names(), ["account-a", "account-b", "unmapped"]);
});

test("display mode survives refresh and account changes while queries and details retain logins", async () => {
  const calls = [];
  const dashboard = { accounts: [{ login: "account-a", display_name: "Alice" }] };
  const fetch = async (url, options) => {
    calls.push({ method: options.method, path: url.pathname, account: url.searchParams.get("account") });
    const data = url.pathname.endsWith("/dashboard") ? dashboard : url.pathname.endsWith("/events") ? { page: 1, total_pages: 1, total: 0, items: [] } : { synced: true };
    return { ok: true, status: 200, json: async () => ({ data, errors: [] }) };
  };
  const { run, element, document } = app("http://localhost/", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  document.dispatch("keydown", { key: "d" });
  await run("refresh(true)");
  assert.ok(element("accounts").innerHTML.includes('class="text-cell">Alice'));
  element("account").value = "account-a";
  element("account").dispatch("change");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(element("accounts-section").hidden, true);
  assert.ok(element("identity").innerHTML.includes("<h2>account-a</h2>"));
  assert.deepEqual(element("account").options.map((option) => [option.text, option.value]), [["All accounts", ""], ["account-a", "account-a"]]);
  assert.deepEqual(calls.slice(-2), [
    { method: "GET", path: "/api/v1/dashboard", account: "account-a" },
    { method: "GET", path: "/api/v1/events", account: "account-a" },
  ]);
  document.dispatch("keydown", { key: "d" });
  assert.equal(run("state.showAccountNames"), true);
  element("account").value = "";
  element("account").dispatch("change");
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(element("accounts").innerHTML.includes('class="text-cell">Alice'));
  const reloaded = app("http://localhost/", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(reloaded.element("accounts").innerHTML.includes('class="text-cell">account-a'));
});

test("account shortcut ignores editing, modifiers, composition, repeats, unavailable mappings, and loading", () => {
  const { run, element, document } = app();
  run("renderAccounts([{login:'account-a',display_name:'Alice'}])");
  for (const event of [
    { key: "x" }, { ctrlKey: true }, { metaKey: true }, { altKey: true },
    { repeat: true }, { isComposing: true }, { defaultPrevented: true },
    { target: { isContentEditable: true } },
    ...["input", "textarea", "select"].map((tag) => ({ target: { closest(selector) { return selector.split(", ").includes(tag) ? this : null; } } })),
  ]) {
    const before = element("accounts").innerHTML;
    document.dispatch("keydown", { key: "d", ...event });
    assert.equal(element("accounts").innerHTML, before);
    assert.equal(run("state.showAccountNames"), false);
  }
  element("accounts-section").hidden = true;
  document.dispatch("keydown", { key: "d" });
  assert.equal(run("state.showAccountNames"), false);
  element("accounts-section").hidden = false;
  run("renderAccounts([{login:'unmapped'}])");
  document.dispatch("keydown", { key: "d" });
  assert.equal(run("state.showAccountNames"), false);
  run("renderAccounts([{login:'account-a',display_name:'Alice'}]); tableViews.clear()");
  document.dispatch("keydown", { key: "d" });
  assert.equal(run("state.showAccountNames"), false);
});

test("display names disambiguate duplicate labels and escape HTML", () => {
  const { run, element, document } = app();
  run(`renderAccounts([
    {login:'account-a',display_name:'Alice'}, {login:'account-b',display_name:'Alice'},
    {login:'Alice'}, {login:'account-c',display_name:'<img src=x onerror=alert(1)>'}
  ])`);
  document.dispatch("keydown", { key: "D" });
  const html = element("accounts").innerHTML;
  assert.ok(html.includes('class="text-cell">Alice (account-a)'));
  assert.ok(html.includes('class="text-cell">Alice (account-b)'));
  assert.ok(html.includes('class="text-cell">Alice</td>'));
  assert.ok(html.includes("&lt;img src=x onerror=alert(1)&gt;"));
  assert.ok(!html.includes("<img"));
});

test("account quota cards treat zero entitlement as unavailable", () => {
  const { run, element } = app();
  run(`renderQuotas({quota_snapshots:{premium_interactions:{unlimited:false,quota_remaining:0,entitlement:0,percent_remaining:0}}})`);
  const html = element("quotas").innerHTML;
  assert.ok(html.includes("Premium interactions"));
  assert.ok(!html.includes("100% used"));
  assert.ok(!html.includes("Premium interactions: 100% used"));
  assert.equal((html.match(/<progress /g) ?? []).length, 0);
  assert.ok(html.includes("— used / —"));
  assert.ok(html.includes("— remaining"));
});

test("account quota cards preserve metered, unlimited, and exhausted states", () => {
  const { run, element } = app();
  run(`renderQuotas({quota_snapshots:{
    chat:{unlimited:false,credits_used:50,quota_remaining:150,entitlement:200,percent_remaining:75},
    completions:{unlimited:true},
    premium_interactions:{unlimited:false,quota_remaining:0,entitlement:100,percent_remaining:0}
  }})`);
  const html = element("quotas").innerHTML;
  assert.ok(html.includes("Chat: 25% used"));
  assert.ok(html.includes("Unlimited"));
  assert.ok(html.includes("Premium interactions: 100% used"));
  assert.ok(html.includes("<span>100 used / 100</span>"));
});

test("cost sorting permits a single currency and resets when currencies become incomparable", () => {
  const { run, element } = app();
  run(`const models = [
    {model:'A',costs:[{currency:'USD',total_cost_nanos:200}]},
    {model:'B',costs:[{currency:'USD',total_cost_nanos:100}]}
  ]; sorts.models = {column:8,direction:1}; renderModels(models);`);
  assert.ok(element("models").innerHTML.indexOf('class="text-cell">B') < element("models").innerHTML.indexOf('class="text-cell">A'));
  run("models[1].costs[0].currency = 'EUR'; renderModels(models)");
  assert.ok(element("models").innerHTML.includes('disabled title="Cost sorting requires a single currency"'));
  assert.equal(run("sorts.models.column"), 0);
});

test("account quota absolute values remain independently sortable", () => {
  const { run, element } = app();
  run(`const accounts = [
    {login:'Missing'},
    {login:'Unlimited',quota_snapshots:{premium_interactions:{unlimited:true}}},
    {login:'ZeroQuota',quota_snapshots:{premium_interactions:{unlimited:false,quota_remaining:0,entitlement:0,percent_remaining:0}}},
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,credits_used:5000,remaining:0,quota_remaining:800,entitlement:1000,percent_remaining:80}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,credits_used:90,remaining:700,quota_remaining:10,entitlement:100,percent_remaining:10}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,credits_used:1,remaining:50,quota_remaining:1,entitlement:300,percent_remaining:0.3}}}
  ]; sorts.accounts = {column:3,direction:-1}; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["RemainingLow", "AbsoluteHigh", "PercentHigh", "Missing", "Unlimited", "ZeroQuota"]);
  run("sorts.accounts = {column:2,direction:-1}; renderAccounts(accounts)");
  assert.deepEqual(names(), ["RemainingLow", "PercentHigh", "AbsoluteHigh", "Missing", "Unlimited", "ZeroQuota"]);
  run("sorts.accounts = {column:4,direction:1}; renderAccounts(accounts)");
  assert.deepEqual(names(), ["RemainingLow", "PercentHigh", "AbsoluteHigh", "Missing", "Unlimited", "ZeroQuota"]);
  run("sorts.accounts = {column:5,direction:-1}; renderAccounts(accounts)");
  assert.deepEqual(names(), ["AbsoluteHigh", "RemainingLow", "PercentHigh", "Missing", "Unlimited", "ZeroQuota"]);
  assert.ok(element("accounts").innerHTML.includes('>1,000</td>'));
  assert.equal(run("quotaTotal(accounts[1].quota_snapshots.premium_interactions)"), "Unlimited");
  assert.equal(run("quotaUsed(accounts[2].quota_snapshots.premium_interactions)"), "—");
  assert.equal(run("quotaRemaining(accounts[2].quota_snapshots.premium_interactions)"), "—");
  assert.equal(run("quotaTotal(accounts[2].quota_snapshots.premium_interactions)"), "—");
  assert.equal(run("quotaTotal(null)"), "—");
});

test("unlimited quotas display available credits independently of an entitlement", () => {
  const { run } = app();
  assert.equal(run("usedValue({unlimited:true,entitlement:0,remaining:0,credits_used:26129})"), 26129);
  assert.equal(run("quotaUsed({unlimited:true,entitlement:0,remaining:0,credits_used:26129})"), "26,129");
  assert.equal(run("usedValue({unlimited:true,has_quota:true,credits_used:0})"), 0);
  for (const fields of ["", "credits_used:null", "credits_used:-1", "has_quota:false,credits_used:500"]) {
    assert.equal(run(`usedValue({unlimited:true,${fields}})`), null);
    assert.equal(run(`quotaUsed({unlimited:true,${fields}})`), "—");
  }
  assert.equal(run("usedPercentValue({unlimited:true,credits_used:26129,percent_remaining:100})"), null);
});

test("metered quotas use the allowance basis in account rows and detail cards", () => {
  const { run, element } = app();
  run(`const quota = {unlimited:false,has_quota:false,credits_used:14517,entitlement:13000,remaining:807,quota_remaining:807.7,percent_remaining:6.2};
    const account = {login:'Capped',quota_snapshots:{premium_interactions:quota}};
    renderAccounts([account]); renderQuotas(account);`);
  assert.equal(run("usedValue(quota)"), 12192.3);
  assert.equal(run("usedPercentValue(quota)"), 93.8);
  assert.equal(run("quotaRemaining(quota)"), "807.7");
  assert.ok(element("accounts").innerHTML.includes('<td class="number">12,192.3</td><td class="number">807.7</td><td class="number">13,000</td>'));
  assert.ok(element("quotas").innerHTML.includes("Premium interactions: 93.8% used"));
  assert.ok(element("quotas").innerHTML.includes("12,192.3 used / 13,000"));
  assert.ok(element("quotas").innerHTML.includes("807.7 remaining"));
});

test("partial metered quotas preserve known values and distinguish missing from zero", () => {
  const { run } = app();
  const cases = [
    { quota: { percent_remaining: 25 }, used: null, percent: 75, remaining: null, total: "—" },
    { quota: { entitlement: 400, percent_remaining: 25 }, used: 300, percent: 75, remaining: null, total: "400" },
    { quota: { quota_remaining: 100, percent_remaining: 25 }, used: null, percent: 75, remaining: 100, total: "—" },
    { quota: { entitlement: 400, quota_remaining: 100 }, used: 300, percent: null, remaining: 100, total: "400" },
    { quota: { entitlement: 400, quota_remaining: null, percent_remaining: null }, used: null, percent: null, remaining: null, total: "400" },
    { quota: { entitlement: 0, quota_remaining: 0, percent_remaining: 0 }, used: null, percent: null, remaining: null, total: "—" },
    { quota: { entitlement: 400, quota_remaining: 0, percent_remaining: 0 }, used: 400, percent: 100, remaining: 0, total: "400" },
    { quota: { entitlement: 400, quota_remaining: 400, percent_remaining: 100 }, used: 0, percent: 0, remaining: 400, total: "400" },
    { quota: { entitlement: 400, quota_remaining: 500, percent_remaining: 100 }, used: 0, percent: 0, remaining: 500, total: "400" },
  ];
  for (const { quota, ...expected } of cases) {
    const input = JSON.stringify({ unlimited: false, ...quota });
    const actual = JSON.parse(run(`JSON.stringify({used:usedValue(${input}),percent:usedPercentValue(${input}),remaining:remainingValue(${input}),total:quotaTotal(${input})})`));
    assert.deepEqual(actual, expected, input);
  }
  assert.equal(run("usedPercentValue({unlimited:false,percent_remaining:-4.2})"), 100);
  assert.equal(run("usedPercentValue({unlimited:false,percent_remaining:105})"), 0);
});

test("Today hides daily usage, other periods restore it, and table sorting leaves chart chronological", () => {
  const { run, element } = app();
  run(`const days = [{date:'2026-09-08',totals:{total_tokens:100}}, {date:'2026-09-09',totals:{total_tokens:200}}];
    state.period = 'today'; renderTrend(days);`);
  assert.equal(element("trend-section").hidden, true);
  assert.equal(element("trend").innerHTML, "");
  run("state.period = 'last_7_days'; renderTrend(days)");
  assert.equal(element("trend-section").hidden, false);
  const chart = element("trend").innerHTML.split("</svg>")[0];
  run("sorts.days = {column:1,direction:-1}; renderTrend(days)");
  assert.equal(element("trend").innerHTML.split("</svg>")[0], chart);
  assert.ok(element("trend").innerHTML.indexOf('<td>2026-09-09') < element("trend").innerHTML.indexOf('<td>2026-09-08'));
  run("renderTrend(null)");
  assert.ok(element("trend").innerHTML.includes("—"));
});

test("daily usage identifies zero-filled dates without changing their displayed values", () => {
  const { run, element } = app();
  run(`state.period = 'last_7_days'; renderTrend([
    {date:'2026-09-08',recorded:false,totals:{total_tokens:0,request_count:0,costs:[]}},
    {date:'2026-09-09',recorded:true,totals:{total_tokens:200,request_count:1,costs:[]}}
  ])`);
  assert.ok(element("trend").innerHTML.includes("Dates without recorded usage are shown as zero."));
  assert.ok(!element("trend").innerHTML.includes("<title>"));
  assert.ok(run("trendTooltip({date:'2026-09-08',recorded:false,totals:{total_tokens:0}})").includes("No recorded usage"));
  assert.ok(element("trend").innerHTML.includes('<td>2026-09-08 <span class="not-recorded">Not recorded</span></td><td class="number">0</td><td class="number">0</td>'));
  run(`renderTrend([{date:'2026-09-09',recorded:true,totals:{total_tokens:200}}])`);
  assert.ok(!element("trend").innerHTML.includes("Dates without recorded usage"));
  assert.ok(!element("trend").innerHTML.includes("Not recorded"));
});

test("daily usage provides an interactive exact-value tooltip without changing chart data", () => {
  const { run, element } = app();
  run(`state.period = 'last_7_days'; renderTrend([
    {date:'2026-09-08',recorded:true,totals:{total_tokens:1234567,request_count:42,costs:[{currency:'USD',total_cost_nanos:1250000000}]}},
    {date:'2026-09-09',recorded:true,totals:{total_tokens:2000000,request_count:50,costs:[]}}
  ])`);
  assert.ok(element("trend").innerHTML.includes('class="chart-plot" tabindex="0"'));
  assert.ok(element("trend").innerHTML.includes('class="chart-guide"'));
  assert.ok(element("trend").innerHTML.includes('class="chart-tooltip" role="status"'));
  assert.ok(run("trendTooltip({date:'2026-09-08',recorded:true,totals:{total_tokens:1234567,request_count:42,costs:[{currency:'USD',total_cost_nanos:1250000000}]}})").includes("1.23M"));
  assert.ok(run("trendTooltip({date:'2026-09-08',recorded:true,totals:{total_tokens:1234567,request_count:42,costs:[{currency:'USD',total_cost_nanos:1250000000}]}})").includes("USD 1.25"));
  assert.ok(run("trendTooltip({date:'2026-09-07',recorded:false,totals:{total_tokens:0}})").includes("No recorded usage"));
  assert.equal(run("trendIndexAt(50, 50, 750, 8)"), 0);
  assert.equal(run("trendIndexAt(400, 50, 750, 8)"), 4);
  assert.equal(run("trendIndexAt(800, 50, 750, 8)"), 7);
});

test("daily usage interaction updates only on date changes and preserves keyboard position after Escape", () => {
  const { run, chart } = app();
  run(`state.period = 'last_7_days'; renderTrend([
    {date:'2026-09-08',recorded:true,totals:{total_tokens:100,request_count:1,costs:[]}},
    {date:'2026-09-09',recorded:true,totals:{total_tokens:200,request_count:2,costs:[]}},
    {date:'2026-09-10',recorded:true,totals:{total_tokens:300,request_count:3,costs:[]}}
  ])`);
  chart.hitArea.dispatch("pointermove", { clientX: 400 });
  assert.ok(chart.tooltip.innerHTML.includes("2026-09-09"));
  const writes = chart.tooltip.innerHTMLWrites;
  chart.hitArea.dispatch("pointermove", { clientX: 410 });
  assert.equal(chart.tooltip.innerHTMLWrites, writes);
  chart.plot.dispatch("keydown", { key: "Escape" });
  assert.equal(chart.tooltip.hidden, true);
  chart.plot.dispatch("keydown", { key: "ArrowRight" });
  assert.ok(chart.tooltip.innerHTML.includes("2026-09-10"));
});

test("touch selects the tapped date before focus and pointer events are limited to the hit area", () => {
  const { run, chart } = app();
  run(`state.period = 'last_7_days'; renderTrend([
    {date:'2026-09-08',recorded:true,totals:{total_tokens:100}},
    {date:'2026-09-09',recorded:true,totals:{total_tokens:200}},
    {date:'2026-09-10',recorded:true,totals:{total_tokens:300}}
  ])`);
  chart.plot.dispatch("pointermove", { clientX: 700 });
  assert.equal(chart.tooltip.hidden, true);
  chart.hitArea.dispatch("pointerdown", { pointerType: "touch", clientX: 50 });
  assert.ok(chart.tooltip.innerHTML.includes("2026-09-08"));
  assert.equal(chart.tooltip.innerHTMLWrites, 1);
});

test("daily usage tooltip stays inside the plot without changing its layout", () => {
  const { run, chart } = app();
  chart.plot.getBoundingClientRect = () => ({ left: 100, top: 200, width: 800, height: 220 });
  chart.svg.getBoundingClientRect = () => ({ left: 116, top: 204, width: 768, height: 220 });
  run(`state.period = 'last_7_days'; renderTrend([
    {date:'2026-09-08',recorded:true,totals:{total_tokens:300}},
    {date:'2026-09-09',recorded:true,totals:{total_tokens:200}},
    {date:'2026-09-10',recorded:true,totals:{total_tokens:140}}
  ])`);
  chart.hitArea.dispatch("pointermove", { clientX: 884 });
  const left = Number.parseFloat(chart.tooltip.style.left);
  const top = Number.parseFloat(chart.tooltip.style.top);
  assert.ok(chart.tooltip.innerHTML.includes("2026-09-10"));
  assert.equal(left, 570);
  assert.ok(left >= 8 && left + chart.tooltip.offsetWidth <= 792);
  assert.ok(top >= 8 && top + chart.tooltip.offsetHeight <= 212);
  assert.equal(chart.plot.style.paddingBottom, undefined);
});

test("filters follow the page URL and browser history", async () => {
  const calls = [];
  const fetch = async (url) => {
    calls.push(`${url.pathname}?${url.searchParams}`);
    const data = url.pathname.endsWith("/dashboard") ? { accounts: [{ login: "account-a" }] } : { page: 1, total_pages: 1, total: 0, items: [] };
    return { ok: true, status: 200, json: async () => ({ data, errors: [] }) };
  };
  const { element, historyCalls, location, popstate } = app("http://localhost/copilot/?account=account-a&period=last_7_days", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(calls[0], "/copilot/api/v1/dashboard?period=last_7_days&account=account-a");
  assert.equal(element("account").value, "account-a");
  assert.equal(element("period").value, "last_7_days");
  assert.deepEqual(historyCalls, []);
  element("period").value = "last_30_days";
  element("period").dispatch("change");
  element("account").value = "";
  element("account").dispatch("change");
  assert.deepEqual(historyCalls, [["push", "http://localhost/copilot/?account=account-a"], ["push", "http://localhost/copilot/"]]);
  await new Promise((resolve) => setImmediate(resolve));
  calls.length = 0;
  location.href = "http://localhost/copilot/?period=today";
  popstate();
  location.hash = "errors-section";
  popstate();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, ["/copilot/api/v1/dashboard?period=today"]);
  assert.equal(element("period").value, "today");
});

test("invalid URL periods fall back to the default without adding history", () => {
  const calls = [];
  const { element, historyCalls } = app("http://localhost/?period=yesterday&account=account-a", (url) => { calls.push(url.searchParams.get("period")); return new Promise(() => {}); });
  assert.deepEqual(historyCalls, [["replace", "http://localhost/?account=account-a"]]);
  assert.equal(element("period").value, "last_30_days");
  assert.deepEqual(calls, ["last_30_days"]);
});

function controllableFetch(accounts = ["account-a"]) {
  const pending = new Set(), calls = [];
  const fetch = async (url, options) => {
    const route = url.pathname.split("/").pop();
    calls.push(`${options.method} ${route} ${url.searchParams}`);
    if (pending.has(route)) return new Promise(() => {});
    const data = route === "dashboard" ? { accounts: accounts.map((login) => ({ login })), totals: { total_tokens: 1234 } }
      : route === "events" ? { page: Number(url.searchParams.get("page")), total_pages: 3, total: 45, items: [{ model: `${url.searchParams.get("account")}-p${url.searchParams.get("page")}` }] } : { synced: true };
    return { ok: true, status: 200, json: async () => ({ data, errors: [] }) };
  };
  return { fetch, pending, calls };
}

test("refresh keeps the current view, locks paging, and ignores repeat syncs while loading", async () => {
  const { fetch, pending, calls } = controllableFetch();
  const { run, element } = app("http://localhost/?account=account-a", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  const metrics = element("metrics").innerHTML, events = element("events").innerHTML;
  assert.ok(events.includes("account-a-p1"));
  pending.add("sync").add("dashboard").add("events");
  element("controls").dispatch("submit");
  assert.equal(element("metrics").innerHTML, metrics);
  assert.equal(element("events").innerHTML, events);
  assert.equal(element("content").getAttribute("aria-busy"), "true");
  assert.equal(element("next").disabled, true);
  assert.equal(element("refresh").textContent, "Syncing…");
  assert.equal(element("refresh").getAttribute("aria-disabled"), "true");
  const requests = calls.length;
  element("controls").dispatch("submit");
  assert.equal(calls.length, requests);
  run("void refresh()");
  assert.equal(element("refresh").textContent, "Refresh");
  assert.equal(element("refresh").getAttribute("aria-disabled"), "false");
});

test("event pages keep the current table and pager while the next page loads", async () => {
  const { fetch, pending } = controllableFetch();
  const { run, element } = app("http://localhost/?account=account-a", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  const events = element("events").innerHTML;
  pending.add("events");
  element("next").dispatch("click");
  assert.equal(run("state.page"), 2);
  assert.equal(element("events").innerHTML, events);
  assert.equal(element("events").getAttribute("aria-busy"), "true");
  assert.equal(element("next").disabled, false);
});

test("events are replaced only when the account or period changes", async () => {
  const { fetch, pending } = controllableFetch(["account-a", "account-b"]);
  const { element } = app("http://localhost/?account=account-a", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  const events = element("events").innerHTML;
  pending.add("events");
  element("controls").dispatch("submit");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(element("events").innerHTML, events);
  element("account").value = "account-b";
  element("account").dispatch("change");
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(element("events").innerHTML.includes("Loading events…"));
  assert.ok(!element("events").innerHTML.includes("account-a-p1"));
});

test("account URLs adopt the canonical login so in-page links do not reload", async () => {
  const { fetch, calls } = controllableFetch();
  const { element, historyCalls, location, popstate } = app("http://localhost/?account=ACCOUNT-A#content", fetch);
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(historyCalls, [["replace", "http://localhost/?account=account-a#content"]]);
  calls.length = 0;
  location.hash = "errors-section";
  popstate();
  location.href = "http://localhost/?account=ACCOUNT-A";
  popstate();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, []);
  assert.deepEqual(element("account").options.map((option) => option.value), ["", "account-a"]);
});

test("quota cards lead with premium interactions, omit bars for unlimited quotas, and warn from 90%", () => {
  const { run, element } = app();
  run(`renderQuotas({quota_snapshots:{
    chat:{unlimited:true}, completions:{unlimited:true},
    premium_interactions:{unlimited:false,entitlement:300,quota_remaining:30,percent_remaining:10}
  }})`);
  let html = element("quotas").innerHTML;
  assert.ok(html.indexOf("Premium interactions") < html.indexOf("Chat"));
  assert.equal((html.match(/<progress /g) ?? []).length, 1);
  assert.ok(html.includes('<span class="warning">90% used</span>'));
  assert.ok(html.includes('<progress class="warning" value="90"'));
  assert.ok(!html.includes("Unlimited remaining"));
  run("renderQuotas({quota_snapshots:{premium_interactions:{unlimited:true,credits_used:120}}})");
  html = element("quotas").innerHTML;
  assert.ok(html.includes("<span>120 used</span>"));
  assert.equal((html.match(/<progress /g) ?? []).length, 0);
  assert.equal(run("usageLevel(89.94)"), "");
  assert.equal(run("usageLevel(89.96)"), "warning");
  assert.equal(run("usageLevel(100)"), "exhausted");
  assert.ok(run("quotaUsage({unlimited:false,entitlement:100,quota_remaining:10,percent_remaining:10})").startsWith('<span class="quota-usage warning">'));
});

test("data source errors are summarized above the usage content", () => {
  const { run, element } = app();
  run(`state.dashboardErrors = [
    {target:'a',operation:'usage',message:'HTTP 500'}, {target:'a',operation:'usage',message:'HTTP 500'},
    {target:'b',operation:'token-usage',message:'timeout'}
  ]; renderErrors()`);
  assert.equal(element("errors-alert").hidden, false);
  assert.equal(element("errors-alert").innerHTML, '<span>2 data source errors.</span> <a href="#errors-section">View details</a>');
  assert.ok(element("errors").innerHTML.includes('aria-label="Data source errors table"'));
  run("state.dashboardErrors = []; renderErrors()");
  assert.equal(element("errors-alert").hidden, true);
});

test("an empty account list explains discovery unless data sources failed", () => {
  const { run, element } = app();
  run("renderDashboard({accounts:[]})");
  assert.ok(element("accounts").innerHTML.includes("Add endpoints to the endpoints file"));
  run("state.dashboardErrors = [{target:'a',operation:'usage',message:'x'}]; renderDashboard({accounts:[]})");
  assert.ok(element("accounts").innerHTML.includes("No accounts available."));
});
