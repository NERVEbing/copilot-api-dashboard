const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { test } = require("node:test");
const vm = require("node:vm");

function app(baseURI = "http://localhost/") {
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      innerHTML: "", textContent: "", hidden: false, clientWidth: 800,
      classList: { toggle() {} }, querySelector() { return null; },
      setAttribute() {}, addEventListener() {}, replaceChildren() {},
    });
    return elements.get(id);
  };
  const context = vm.createContext({
    document: { baseURI, getElementById: element },
    ResizeObserver: class { observe() {} },
    AbortController, URL, URLSearchParams, Intl,
    fetch: () => new Promise(() => {}),
  });
  vm.runInContext(readFileSync(`${__dirname}/app.js`, "utf8"), context);
  return { run: (code) => vm.runInContext(code, context), element };
}

test("API URLs follow the document base path", () => {
  const { run } = app("https://example.test/copilot-dashboard/");
  assert.equal(run("new URL('api/v1/dashboard', appBaseURL).pathname"), "/copilot-dashboard/api/v1/dashboard");
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

test("account quota usage percentage is visual, grouped and sorted descending by default", () => {
  const { run, element } = app();
  run(`const accounts = [
    {login:'Missing'},
    {login:'Unlimited',quota_snapshots:{premium_interactions:{unlimited:true}}},
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:800,entitlement:1000}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:10,entitlement:100,percent_remaining:10}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,remaining:1,entitlement:300}}}
  ]; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["RemainingLow", "PercentHigh", "AbsoluteHigh", "Missing", "Unlimited"]);
  assert.ok(element("accounts").innerHTML.includes('scope="colgroup" colspan="4" class="quota-group">Premium quota'));
  assert.ok(element("accounts").innerHTML.includes('<progress value="90" max="100" aria-label="90% used"></progress><span class="quota-usage-value">90%</span>'));
  assert.ok(element("accounts").innerHTML.includes('<span class="unlimited">Unlimited</span>'));
  assert.ok(run("quotaUsage({unlimited:false,percent_remaining:-4.2})").includes('class="quota-usage exhausted"><progress value="100" max="100" aria-label="104.2% used"></progress><span class="quota-usage-value">104.2%</span>'));
  assert.equal(run("usedPercentValue({unlimited:false,entitlement:0,remaining:0})"), null);
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
    {login:'AbsoluteHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:800,entitlement:1000}}},
    {login:'PercentHigh',quota_snapshots:{premium_interactions:{unlimited:false,remaining:10,entitlement:100}}},
    {login:'RemainingLow',quota_snapshots:{premium_interactions:{unlimited:false,remaining:1,entitlement:300}}}
  ]; sorts.accounts = {column:3,direction:-1}; renderAccounts(accounts);`);
  const names = () => [...element("accounts").innerHTML.matchAll(/class="text-cell">([^<]+)/g)].map((match) => match[1]);
  assert.deepEqual(names(), ["RemainingLow", "AbsoluteHigh", "PercentHigh", "Missing", "Unlimited"]);
  run("sorts.accounts = {column:4,direction:1}; renderAccounts(accounts)");
  assert.deepEqual(names(), ["RemainingLow", "PercentHigh", "AbsoluteHigh", "Missing", "Unlimited"]);
  run("sorts.accounts = {column:5,direction:-1}; renderAccounts(accounts)");
  assert.deepEqual(names(), ["AbsoluteHigh", "RemainingLow", "PercentHigh", "Missing", "Unlimited"]);
  assert.ok(element("accounts").innerHTML.includes('>1,000</td>'));
  assert.equal(run("quotaTotal(accounts[1].quota_snapshots.premium_interactions)"), "Unlimited");
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
