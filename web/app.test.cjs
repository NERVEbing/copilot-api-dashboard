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
      dispatch(type, event = {}) { listeners.get(type)?.({ preventDefault() {}, pointerType: "mouse", clientX: 0, ...event }); },
      setAttribute(name, value) { attributes.set(name, String(value)); },
      getAttribute(name) { return attributes.get(name); },
      get innerHTML() { return innerHTML; },
      set innerHTML(value) { innerHTML = value; innerHTMLWrites++; },
      get innerHTMLWrites() { return innerHTMLWrites; },
    }, properties);
  };
  const guide = node(), active = node(), tooltip = node();
  const svg = node({ getBoundingClientRect() { return { left: 0, width: 768, height: 220 }; } });
  const hitArea = node();
  const plot = node({
    hidden: false,
    querySelector(selector) { return ({ svg, ".chart-hit-area": hitArea, ".chart-guide": guide, ".chart-active": active, ".chart-tooltip": tooltip })[selector] ?? null; },
    focus() { this.dispatch("focus"); },
  });
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      innerHTML: "", textContent: "", hidden: false, clientWidth: 800,
      classList: { toggle() {} }, querySelector(selector) { return id === "trend" && selector === ".chart-plot" && this.innerHTML.includes('class="chart-plot"') ? plot : null; },
      setAttribute() {}, addEventListener() {}, replaceChildren() {},
    });
    return elements.get(id);
  };
  const context = vm.createContext({
    document: { baseURI, getElementById: element },
    ResizeObserver: class { observe() {} },
    Option: class { constructor(text, value) { this.text = text; this.value = value; } },
    AbortController, URL, URLSearchParams, Intl,
    fetch: fetchImpl,
  });
  vm.runInContext(readFileSync(`${__dirname}/app.js`, "utf8"), context);
  return { run: (code) => vm.runInContext(code, context), element, chart: { plot, svg, hitArea, guide, active, tooltip } };
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
  assert.ok(money(1).includes("USD &lt;0.01"));
  assert.equal(money(0), '<span class="money">USD 0.00</span>');
  assert.equal(run("money([])"), "—");
  assert.equal(run("money(null)"), "—");
  assert.ok(run("money([{currency:'<img>',total_cost_nanos:1}])").includes("&lt;img&gt;"));
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

test("account quota usage percentage is visual and sorted descending by default", () => {
  const { run, element } = app();
  run(`const accounts = [
    {login:'Missing'},
    {login:'Unlimited',quota_snapshots:{premium_interactions:{unlimited:true}}},
    {login:'ZeroQuota',quota_snapshots:{premium_interactions:{unlimited:false,remaining:0,entitlement:0,percent_remaining:0}}},
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:800,entitlement:1000}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:10,entitlement:100,percent_remaining:10}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,remaining:1,entitlement:300}}}
  ]; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["RemainingLow", "PercentHigh", "AbsoluteHigh", "Missing", "Unlimited", "ZeroQuota"]);
  assert.ok(element("accounts").innerHTML.includes(">Usage "));
  assert.ok(element("accounts").innerHTML.includes(">Total "));
  assert.ok(!element("accounts").innerHTML.includes("Premium"));
  assert.ok(!element("accounts").innerHTML.includes('scope="colgroup"'));
  assert.ok(element("accounts").innerHTML.includes('<progress value="90" max="100" aria-label="90% used"></progress><span class="quota-usage-value">90%</span>'));
  assert.ok(element("accounts").innerHTML.includes('<span class="unlimited">Unlimited</span>'));
  assert.ok(run("quotaUsage({unlimited:false,entitlement:100,remaining:0,percent_remaining:-4.2})").includes('class="quota-usage exhausted"><progress value="100" max="100" aria-label="104.2% used"></progress><span class="quota-usage-value">104.2%</span>'));
  assert.equal(run("usedPercentValue({unlimited:false,entitlement:0,remaining:0,percent_remaining:0})"), null);
  assert.equal(run("quotaUsage({unlimited:false,entitlement:0,remaining:0,percent_remaining:0})"), "—");
});

test("account quota cards treat zero entitlement as unavailable", () => {
  const { run, element } = app();
  run(`renderQuotas({quota_snapshots:{premium_interactions:{unlimited:false,remaining:0,entitlement:0,percent_remaining:0}}})`);
  const html = element("quotas").innerHTML;
  assert.ok(html.includes("Premium interactions"));
  assert.ok(!html.includes("100% used"));
  assert.ok(!html.includes("Premium interactions: 100% used"));
  assert.equal((html.match(/<progress /g) ?? []).length, 0);
  assert.ok(html.includes("— used / —"));
  assert.ok(html.includes("— remaining"));
});

test("account quota cards preserve metered, unlimited, and over-quota states", () => {
  const { run, element } = app();
  run(`renderQuotas({quota_snapshots:{
    chat:{unlimited:false,remaining:150,entitlement:200,percent_remaining:75},
    completions:{unlimited:true},
    premium_interactions:{unlimited:false,remaining:0,entitlement:100,percent_remaining:-4.2}
  }})`);
  const html = element("quotas").innerHTML;
  assert.ok(html.includes("Chat: 25% used"));
  assert.ok(html.includes("Unlimited"));
  assert.ok(html.includes("Premium interactions: 104.2% used"));
  assert.ok(html.includes("<span>100 used / 100</span>"));
});

test("cost sorting permits a single currency and resets when currencies become incomparable", () => {
  const { run, element } = app();
  run(`const models = [
    {model:'A',costs:[{currency:'USD',total_cost_nanos:200}]},
    {model:'B',costs:[{currency:'USD',total_cost_nanos:100}]}
  ]; sorts.models = {column:7,direction:1}; renderModels(models);`);
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
    {login:'ZeroQuota',quota_snapshots:{premium_interactions:{unlimited:false,remaining:0,entitlement:0,percent_remaining:0}}},
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:800,entitlement:1000}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:10,entitlement:100}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,remaining:1,entitlement:300}}}
  ]; sorts.accounts = {column:3,direction:-1}; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["RemainingLow", "AbsoluteHigh", "PercentHigh", "Missing", "Unlimited", "ZeroQuota"]);
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
