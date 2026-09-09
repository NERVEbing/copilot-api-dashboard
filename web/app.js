const $ = (id) => document.getElementById(id);
const escapeHTML = (value) => String(value ?? "—").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const numeric = (value) => typeof value === "number" && Number.isFinite(value);
const number = (value) => numeric(value) ? value.toLocaleString("en-US", { maximumFractionDigits: 3 }) : "—";
const percent = (value) => value.toLocaleString("en-US", { maximumFractionDigits: 1 });
const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 2 });
function tokens(value) {
  return numeric(value) ? escapeHTML(compact.format(value)) : "—";
}
const empty = (message = "—") => `<p class="empty">${escapeHTML(message)}</p>`;
const state = { account: "", period: "last_30_days", page: 1, totalPages: 1, dashboardErrors: [], eventErrors: [] };
let dashboardController;
let eventsController;
let refreshID = 0;
let chartDays = null;
const tableViews = new Map();
const sorts = { accounts: { column: 0, direction: 1 }, models: { column: 5, direction: -1 }, days: { column: 0, direction: 1 } };

function money(costs) {
  if (!Array.isArray(costs) || costs.length === 0) return "—";
  return costs.map((cost) => {
    if (!numeric(cost.total_cost_nanos)) return "—";
    const amount = cost.total_cost_nanos / 1e9;
    const display = amount > 0 && amount < 0.01 ? "<0.01" : amount.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
    return `<span class="money">${escapeHTML(`${cost.currency} ${display}`)}</span>`;
  }).join("");
}

function table(label, headers, rows, sortID, disabledCost = false) {
  const sort = sorts[sortID];
  return `<div class="table-scroll"${sortID ? ` data-table="${sortID}"` : ""} tabindex="0" role="region" aria-label="${escapeHTML(label)}"><table><caption class="sr-only">${escapeHTML(label)}</caption><thead><tr>${headers.map((h, i) => {
    const disabled = h === "Cost" && disabledCost;
    const active = sort && !disabled && sort.column === i;
    const content = sort ? `<button type="button" class="sort" data-sort="${sortID}" data-column="${i}"${disabled ? ' disabled title="Cost sorting requires a single currency"' : ""}>${escapeHTML(h)} <span aria-hidden="true">${disabled ? "" : active ? sort.direction === 1 ? "↑" : "↓" : "↕"}</span></button>` : escapeHTML(h);
    return `<th scope="col"${sort ? ` aria-sort="${active ? sort.direction === 1 ? "ascending" : "descending" : "none"}"` : ""}${i && !["Model", "Source", "Target", "Operation", "Error"].includes(h) ? ' class="number"' : ""}>${content}</th>`;
  }).join("")}</tr></thead><tbody>${rows.join("")}</tbody></table></div>`;
}

function sortableTable(id, label, headers, items, getters, renderRow, costs) {
  const currencies = new Set(items.flatMap((item) => (costs(item) ?? []).map((cost) => cost.currency)));
  const disabledCost = currencies.size !== 1;
  if (disabledCost && sorts[id].column === headers.length - 1) sorts[id] = { column: 0, direction: 1 };
  const view = () => {
    const sort = sorts[id];
    const column = sort.column;
    const missing = (value) => value == null || (typeof value === "number" && Number.isNaN(value));
    const compare = (a, b) => typeof a === "string" ? a.localeCompare(b, "en", { sensitivity: "base" }) : a === b ? 0 : a < b ? -1 : 1;
    const sorted = [...items].sort((a, b) => {
      const av = getters[column](a), bv = getters[column](b);
      if (missing(av) || missing(bv)) return Number(missing(av)) - Number(missing(bv)) || compare(getters[0](a), getters[0](b));
      return compare(av, bv) * sort.direction || compare(getters[0](a), getters[0](b));
    });
    return table(label, headers, sorted.map(renderRow), id, disabledCost);
  };
  tableViews.set(id, view);
  return view();
}

function costValue(costs) {
  return costs?.length === 1 && numeric(costs[0].total_cost_nanos) ? costs[0].total_cost_nanos : null;
}

function usedValue(quota) {
  return quota?.unlimited === false && numeric(quota.entitlement) && numeric(quota.remaining) ? quota.entitlement - quota.remaining : null;
}

function metric(label, value) {
  return `<article class="metric"><div class="metric-label">${escapeHTML(label)}</div><div class="metric-value">${value}</div></article>`;
}

function quotaUsed(quota) {
  if (quota?.unlimited === true) return "—";
  return number(usedValue(quota));
}

function quotaRemaining(quota) {
  if (quota?.unlimited === true) return "Unlimited";
  return number(quota?.remaining);
}

function quotaTotal(quota) {
  return quota?.unlimited === true ? "Unlimited" : number(quota?.entitlement);
}

function renderQuotas(account) {
  $("quotas").innerHTML = [["chat", "Chat"], ["completions", "Completions"], ["premium_interactions", "Premium interactions"]].map(([key, label]) => {
    const q = account?.quota_snapshots?.[key];
    const unlimited = q?.unlimited === true;
    const usedPercent = !unlimited && numeric(q?.percent_remaining) ? 100 - q.percent_remaining : null;
    const progress = unlimited ? 100 : numeric(usedPercent) ? Math.min(100, Math.max(0, usedPercent)) : null;
    return `<article class="quota"><div class="quota-top"><h3>${label}</h3><span class="${unlimited ? "unlimited" : "muted"}">${unlimited ? "Unlimited" : numeric(usedPercent) ? `${percent(usedPercent)}% used` : "—"}</span></div>
      ${progress === null ? empty() : `<progress value="${progress}" max="100" aria-label="${label}: ${unlimited ? "unlimited" : `${percent(usedPercent)}% used`}"></progress>`}
      <div class="quota-values"><span>${quotaUsed(q)} used / ${quotaTotal(q)}</span><span>${quotaRemaining(q)} remaining</span></div></article>`;
  }).join("");
}

function renderMetrics(totals) {
  $("metrics").classList.toggle("single", Boolean(state.account));
  const values = [metric("Total tokens", tokens(totals?.total_tokens)), metric("Requests", number(totals?.request_count)), metric("Cost", money(totals?.costs))];
  if (state.account) values.push(metric("Input tokens", tokens(totals?.input_tokens)), metric("Output tokens", tokens(totals?.output_tokens)), metric("Cache read", tokens(totals?.cache_read_input_tokens)), metric("Cache creation", tokens(totals?.cache_creation_input_tokens)));
  $("metrics").innerHTML = values.join("");
}

function renderTrend(days) {
  chartDays = days;
  $("trend-section").hidden = state.period === "today";
  if (state.period === "today") { $("trend").innerHTML = ""; return; }
  const expanded = $("trend").querySelector(".chart-details")?.open;
  if (!Array.isArray(days)) { $("trend").innerHTML = empty(); return; }
  if (!days.length) { $("trend").innerHTML = empty("No daily usage for this period."); return; }
  const width = Math.max(280, $("trend").clientWidth - 32), height = 200, left = 50, right = 12, top = 12, bottom = 32;
  const values = days.map((day) => numeric(day.totals?.total_tokens) ? day.totals.total_tokens : 0);
  const max = Math.max(1, ...values);
  const x = (i) => days.length === 1 ? (width + left - right) / 2 : left + i * (width - left - right) / (days.length - 1);
  const y = (value) => height - bottom - value / max * (height - top - bottom);
  const points = values.map((value, i) => `${x(i)},${y(value)}`).join(" ");
  const ticks = [0, max / 2, max].map((value) => `<line class="grid" x1="${left}" x2="${width - right}" y1="${y(value)}" y2="${y(value)}"/><text x="${left - 10}" y="${y(value) + 4}" text-anchor="end">${escapeHTML(new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(value))}</text>`).join("");
  const labelIndexes = width < 450 ? [0, days.length - 1] : [0, Math.floor((days.length - 1) / 2), days.length - 1];
  const labels = [...new Set(labelIndexes)].map((i) => `<text x="${x(i)}" y="${height - 7}" text-anchor="${days.length === 1 ? "middle" : i === 0 ? "start" : i === days.length - 1 ? "end" : "middle"}">${escapeHTML(days[i].date)}</text>`).join("");
  const dots = days.length <= 31 ? values.map((value, i) => `<circle cx="${x(i)}" cy="${y(value)}" r="3"><title>${escapeHTML(days[i].date)}: ${tokens(value)} tokens</title></circle>`).join("") : "";
  $("trend").innerHTML = `<div class="chart"><svg viewBox="0 0 ${width} ${height}" role="img" aria-label="Daily total tokens. Daily breakdown is available in the daily values table.">${ticks}<polygon class="area" points="${x(0)},${y(0)} ${points} ${x(days.length - 1)},${y(0)}"/><polyline class="line" points="${points}"/>${dots}${labels}</svg></div>
    <details class="chart-details"${expanded ? " open" : ""}><summary>Daily values</summary>${sortableTable("days", "Daily values", ["Date", "Tokens", "Requests", "Cost"], days,
      [(d) => d.date, (d) => d.totals?.total_tokens, (d) => d.totals?.request_count, (d) => costValue(d.totals?.costs)],
      (day) => `<tr><td>${escapeHTML(day.date)}</td><td class="number">${tokens(day.totals?.total_tokens)}</td><td class="number">${number(day.totals?.request_count)}</td><td class="number">${money(day.totals?.costs)}</td></tr>`, (d) => d.totals?.costs)}</details>`;
}

function renderModels(models) {
  if (!Array.isArray(models)) { $("models").innerHTML = empty(); return; }
  if (!models.length) { $("models").innerHTML = empty("No model usage for this period."); return; }
  $("models").innerHTML = sortableTable("models", "Model breakdown", ["Model", "Input", "Output", "Cache read", "Cache creation", "Tokens", "Requests", "Cost"], models,
    [(m) => m.model, ...["input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "total_tokens", "request_count"].map((key) => (m) => m[key]), (m) => costValue(m.costs)],
    (m) => `<tr><td class="text-cell">${escapeHTML(m.model)}</td>${[m.input_tokens, m.output_tokens, m.cache_read_input_tokens, m.cache_creation_input_tokens, m.total_tokens].map((v) => `<td class="number">${tokens(v)}</td>`).join("")}<td class="number">${number(m.request_count)}</td><td class="number">${money(m.costs)}</td></tr>`, (m) => m.costs);
}

function renderAccounts(accounts) {
  if (!accounts.length) { $("accounts").innerHTML = empty("No accounts available."); return; }
  $("accounts").innerHTML = sortableTable("accounts", "Account usage", ["Account", "Plan", "Premium used", "Premium remaining", "Premium total", "Tokens", "Requests", "Cost"], accounts,
    [(a) => a.login, (a) => a.copilot_plan, (a) => usedValue(a.quota_snapshots?.premium_interactions), (a) => a.quota_snapshots?.premium_interactions?.unlimited ? Infinity : a.quota_snapshots?.premium_interactions?.remaining, (a) => a.quota_snapshots?.premium_interactions?.unlimited === true ? Infinity : a.quota_snapshots?.premium_interactions?.entitlement, (a) => a.totals?.total_tokens, (a) => a.totals?.request_count, (a) => costValue(a.totals?.costs)],
    (a) => `<tr><td class="text-cell">${escapeHTML(a.login)}</td><td class="number">${escapeHTML(a.copilot_plan)}</td><td class="number">${quotaUsed(a.quota_snapshots?.premium_interactions)}</td><td class="number">${quotaRemaining(a.quota_snapshots?.premium_interactions)}</td><td class="number">${quotaTotal(a.quota_snapshots?.premium_interactions)}</td><td class="number">${tokens(a.totals?.total_tokens)}</td><td class="number">${number(a.totals?.request_count)}</td><td class="number">${money(a.totals?.costs)}</td></tr>`, (a) => a.totals?.costs);
}

function renderErrors() {
  const unique = new Map([...state.dashboardErrors, ...state.eventErrors].map((e) => [JSON.stringify([e.target, e.operation, e.message]), e]));
  $("errors-section").hidden = unique.size === 0;
  $("errors").innerHTML = unique.size ? table("Failed requests", ["Target", "Operation", "Error"], [...unique.values()].map((e) => `<tr><td>${escapeHTML(e.target)}</td><td>${escapeHTML(e.operation)}</td><td>${escapeHTML(e.message)}</td></tr>`)) : "";
}

function updateAccounts(accounts) {
  const selected = accounts.find((a) => a.login.toLowerCase() === state.account.toLowerCase());
  if (selected) state.account = selected.login;
  const options = [new Option("All accounts", ""), ...accounts.map((a) => new Option(a.login, a.login))];
  if (state.account && !selected) options.push(new Option(state.account, state.account));
  $("account").replaceChildren(...options);
  $("account").value = state.account;
}

function renderDashboard(data) {
  const accounts = data?.accounts ?? [];
  updateAccounts(accounts);
  const account = accounts.find((a) => a.login === state.account);
  $("identity").hidden = !state.account;
  $("quotas").hidden = !state.account;
  $("accounts-section").hidden = Boolean(state.account);
  $("identity").innerHTML = state.account ? `<h2>${escapeHTML(state.account)}</h2><span class="muted">${escapeHTML(account?.copilot_plan)} · Quota resets ${escapeHTML(account?.quota_reset_date)}</span>` : "";
  if (state.account) renderQuotas(account);
  renderMetrics(data?.totals);
  renderTrend(data?.days);
  renderModels(data?.by_model);
  renderAccounts(accounts);
  return Boolean(account);
}

function resetEvents(message) {
  $("events").innerHTML = empty(message);
  $("events-count").textContent = "";
  $("page-label").textContent = "";
  $("previous").disabled = true;
  $("next").disabled = true;
}

function renderEvents(data) {
  if (!data) { resetEvents(); return; }
  state.page = data.page;
  state.totalPages = data.total_pages;
  $("events-count").textContent = `${number(data.total)} requests`;
  $("page-label").textContent = `Page ${number(data.page)} of ${number(data.total_pages)}`;
  $("previous").disabled = data.page <= 1;
  $("next").disabled = data.page >= data.total_pages;
  $("events").innerHTML = data.items.length ? table("Request events", ["Time", "Model", "Source", "Input", "Output", "Cache read", "Cache creation", "Tokens", "Cost"], data.items.map((e) => {
    const date = new Date(e.created_at_ms);
    const time = Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-US");
    return `<tr><td title="${escapeHTML(e.created_at_utc)}">${escapeHTML(time)}</td><td class="text-cell">${escapeHTML(e.model)}</td><td>${escapeHTML(e.source)}</td>${[e.input_tokens, e.output_tokens, e.cache_read_input_tokens, e.cache_creation_input_tokens, e.total_tokens].map((v) => `<td class="number">${tokens(v)}</td>`).join("")}<td class="number">${money(e.cost ? [e.cost] : null)}</td></tr>`;
  })) : empty("No request events for this period.");
}

async function request(path, query, signal) {
  const target = state.account || "Dashboard";
  const operation = path.split("/").pop();
  try {
    const response = await fetch(`${path}?${query}`, { signal, cache: "no-store", credentials: "omit" });
    let body;
    try { body = await response.json(); } catch {
      if (signal.aborted) throw new DOMException("Aborted", "AbortError");
      return { data: null, errors: [{ target, operation, message: `Invalid JSON response (HTTP ${response.status})` }] };
    }
    if (!body || !Array.isArray(body.errors) || !("data" in body)) return { data: null, errors: [{ target, operation, message: `Invalid response (HTTP ${response.status})` }] };
    if (!response.ok && !body.errors.length) return { data: null, errors: [{ target, operation, message: `Request returned HTTP ${response.status}` }] };
    return body;
  } catch (error) {
    if (signal.aborted) throw error;
    return { data: null, errors: [{ target, operation, message: "Network request failed" }] };
  }
}

async function loadEvents() {
  eventsController?.abort();
  const controller = new AbortController();
  eventsController = controller;
  const id = refreshID;
  state.eventErrors = [];
  renderErrors();
  resetEvents("Loading events…");
  $("events").setAttribute("aria-busy", "true");
  try {
    const result = await request("/api/v1/events", new URLSearchParams({ account: state.account, period: state.period, page: state.page, page_size: 20 }), controller.signal);
    if (id !== refreshID || controller.signal.aborted) return;
    state.eventErrors = result.errors;
    renderEvents(result.data);
    renderErrors();
    $("announcement").textContent = result.data ? `Events page ${result.data.page} loaded.` : "Events request failed.";
  } catch (error) {
    if (!controller.signal.aborted) throw error;
  } finally {
    if (id === refreshID && !controller.signal.aborted) $("events").setAttribute("aria-busy", "false");
  }
}

async function refresh() {
  dashboardController?.abort();
  eventsController?.abort();
  const controller = new AbortController();
  dashboardController = controller;
  const id = ++refreshID;
  state.page = 1;
  state.dashboardErrors = [];
  state.eventErrors = [];
  renderErrors();
  $("content").setAttribute("aria-busy", "true");
  $("announcement").textContent = "Loading usage…";
  $("identity").hidden = true;
  chartDays = null;
  tableViews.clear();
  $("trend-section").hidden = state.period === "today";
  $("quotas").hidden = true;
  $("accounts-section").hidden = Boolean(state.account);
  for (const [element, message] of [["metrics", "Loading usage…"], ["trend", "Loading daily usage…"], ["models", "Loading models…"], ["accounts", "Loading accounts…"]]) $(element).innerHTML = empty(message);
  $("events-section").hidden = !state.account;
  $("events").setAttribute("aria-busy", "false");
  resetEvents("Loading events…");
  const query = new URLSearchParams({ period: state.period });
  if (state.account) query.set("account", state.account);
  try {
    const result = await request("/api/v1/dashboard", query, controller.signal);
    if (id !== refreshID || controller.signal.aborted) return;
    state.dashboardErrors = result.errors;
    const found = renderDashboard(result.data);
    renderErrors();
    $("announcement").textContent = result.errors.length ? "Usage loaded with failed requests listed below." : "Usage loaded.";
    if (state.account && found) void loadEvents(); else resetEvents();
  } catch (error) {
    if (!controller.signal.aborted) throw error;
  } finally {
    if (id === refreshID && !controller.signal.aborted) $("content").setAttribute("aria-busy", "false");
  }
}

$("controls").addEventListener("submit", (event) => { event.preventDefault(); void refresh(); });
$("account").addEventListener("change", () => { state.account = $("account").value; void refresh(); });
$("period").addEventListener("change", () => { state.period = $("period").value; void refresh(); });
$("previous").addEventListener("click", () => { if (state.page > 1) { state.page--; void loadEvents(); } });
$("next").addEventListener("click", () => { if (state.page < state.totalPages) { state.page++; void loadEvents(); } });
$("content").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-sort]");
  if (!button || button.disabled) return;
  const id = button.dataset.sort, column = Number(button.dataset.column), sort = sorts[id];
  sort.direction = sort.column === column ? -sort.direction : 1;
  sort.column = column;
  const container = button.closest(".table-scroll"), scrollLeft = container.scrollLeft;
  container.outerHTML = tableViews.get(id)();
  const replacement = document.querySelector(`[data-table="${id}"]`);
  replacement.scrollLeft = scrollLeft;
  replacement.querySelector(`[data-column="${column}"]`).focus({ preventScroll: true });
});
let chartWidth = 0;
new ResizeObserver(([entry]) => {
  const width = Math.round(entry.contentRect.width);
  if (width !== chartWidth) { chartWidth = width; if (chartDays) renderTrend(chartDays); }
}).observe($("trend"));
void refresh();
