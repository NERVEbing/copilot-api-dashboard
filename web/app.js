const $ = (id) => document.getElementById(id);
const appBaseURL = new URL(".", document.baseURI);
const escapeHTML = (value) => String(value ?? "—").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const numeric = (value) => typeof value === "number" && Number.isFinite(value);
const number = (value) => numeric(value) ? value.toLocaleString("en-US", { maximumFractionDigits: 3 }) : "—";
const percent = (value) => value.toLocaleString("en-US", { maximumFractionDigits: 1 });
const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 2 });
function tokens(value) {
  return numeric(value) ? escapeHTML(compact.format(value)) : "—";
}
const empty = (message = "—") => `<p class="empty">${escapeHTML(message)}</p>`;
const periods = ["today", "this_week", "last_7_days", "this_month", "last_30_days", "lifetime"];
const defaultPeriod = "last_30_days";
const timeZone = new Intl.DateTimeFormat().resolvedOptions().timeZone;
const state = { account: "", period: defaultPeriod, page: 1, totalPages: 1, dashboardErrors: [], eventErrors: [], showAccountNames: false };
let dashboardController;
let eventsController;
let refreshID = 0;
let chartDays = null;
let hasAccountAliases = false;
let shownEvents = "";
const tableViews = new Map();
const sorts = { accounts: { column: 3, direction: -1 }, models: { column: 6, direction: -1 }, days: { column: 0, direction: 1 } };

function cacheHitRate(usage) {
  const counts = [usage?.input_tokens, usage?.cache_read_input_tokens, usage?.cache_creation_input_tokens];
  if (counts.some((value) => !numeric(value) || value < 0)) return null;
  const total = counts.reduce((sum, value) => sum + value, 0);
  return numeric(total) && total > 0 ? usage.cache_read_input_tokens / total * 100 : null;
}

function cacheHitRateText(usage) {
  const rate = cacheHitRate(usage);
  return rate === null ? "—" : `${percent(rate)}%`;
}

function duration(value) {
  if (!numeric(value) || value < 0) return "—";
  return value < 1000 ? `${value.toFixed(0)} ms` : `${(value / 1000).toFixed(2)} s`;
}

function requestOutcome(value) {
  switch (value) {
    case "completed": return "Completed";
    case "cancelled": return '<span class="outcome outcome-cancelled">Cancelled</span>';
    case "error": return '<span class="outcome outcome-error">Error</span>';
    default: return "—";
  }
}

function money(costs) {
  if (!Array.isArray(costs) || costs.length === 0) return "—";
  return costs.map((cost) => {
    if (!numeric(cost.total_cost_nanos)) return "—";
    const amount = cost.total_cost_nanos / 1e9;
    const subcent = amount > 0 && amount < 0.01;
    const display = subcent && amount < 0.000001 ? "<0.000001" : amount.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: subcent ? 6 : 2 });
    return `<span class="money">${escapeHTML(`${cost.currency} ${display}`)}</span>`;
  }).join("");
}

function headerCell(label, index, sortID, disabledCost, className = "", attributes = "") {
  const sort = sorts[sortID];
  const disabled = label === "Cost" && disabledCost;
  const active = sort && !disabled && sort.column === index;
  const content = sort ? `<button type="button" class="sort" data-sort="${sortID}" data-column="${index}"${disabled ? ' disabled title="Cost sorting requires a single currency"' : ""}>${escapeHTML(label)} <span aria-hidden="true">${disabled ? "" : active ? sort.direction === 1 ? "↑" : "↓" : "↕"}</span></button>` : escapeHTML(label);
  return `<th scope="col"${attributes}${sort ? ` aria-sort="${active ? sort.direction === 1 ? "ascending" : "descending" : "none"}"` : ""}${className ? ` class="${className}"` : ""}>${content}</th>`;
}

function table(label, headers, rows, sortID, disabledCost = false) {
  const cells = headers.map((header, index) => headerCell(header, index, sortID, disabledCost, index && !["Model", "Plan", "Endpoint", "Outcome", "Target", "Operation", "Error"].includes(header) ? "number" : "", header === "Outcome" ? ' title="HTTP/body completion. Completed does not certify model success."' : "")).join("");
  return `<div class="table-scroll"${sortID ? ` data-table="${sortID}"` : ""} tabindex="0" role="region" aria-label="${escapeHTML(`${label} table`)}"><table><caption class="sr-only">${escapeHTML(label)}</caption><thead><tr>${cells}</tr></thead><tbody>${rows.join("")}</tbody></table></div>`;
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

function redrawTable(id) {
  const container = document.querySelector(`[data-table="${id}"]`), scrollLeft = container.scrollLeft;
  const focused = document.activeElement, restoreFocus = container.contains(focused);
  const column = restoreFocus ? focused.closest("button[data-sort]")?.dataset.column : undefined;
  container.outerHTML = tableViews.get(id)();
  const replacement = document.querySelector(`[data-table="${id}"]`);
  replacement.scrollLeft = scrollLeft;
  if (restoreFocus) (column == null ? replacement : replacement.querySelector(`[data-column="${column}"]`)).focus({ preventScroll: true });
}

function costValue(costs) {
  return costs?.length === 1 && numeric(costs[0].total_cost_nanos) ? costs[0].total_cost_nanos : null;
}

function validMeteredQuota(quota) {
  return quota?.unlimited === false && numeric(quota.entitlement) && quota.entitlement > 0;
}

function remainingValue(quota) {
  if (quota?.unlimited !== false || quota.entitlement === 0) return null;
  return numeric(quota.quota_remaining) && quota.quota_remaining >= 0 ? quota.quota_remaining : null;
}

function usedValue(quota) {
  if (quota?.unlimited === true) {
    return quota.has_quota !== false && numeric(quota.credits_used) && quota.credits_used >= 0 ? quota.credits_used : null;
  }
  if (!validMeteredQuota(quota)) return null;
  const remaining = remainingValue(quota);
  if (numeric(remaining)) return Math.max(0, quota.entitlement - remaining);
  const usedPercent = usedPercentValue(quota);
  return numeric(usedPercent) ? quota.entitlement * usedPercent / 100 : null;
}

function usedPercentValue(quota) {
  if (quota?.unlimited !== false || quota.entitlement === 0) return null;
  return numeric(quota.percent_remaining) ? 100 - Math.min(100, Math.max(0, quota.percent_remaining)) : null;
}

function metric(label, value) {
  return `<article class="metric"><div class="metric-label">${escapeHTML(label)}</div><div class="metric-value">${value}</div></article>`;
}

function quotaUsed(quota) {
  return number(usedValue(quota));
}

function quotaRemaining(quota) {
  if (quota?.unlimited === true) return "Unlimited";
  return number(remainingValue(quota));
}

function quotaTotal(quota) {
  if (quota?.unlimited === true) return "Unlimited";
  return validMeteredQuota(quota) ? number(quota.entitlement) : "—";
}

function usageLevel(usedPercent) {
  const rounded = Math.round(usedPercent * 10) / 10;
  return rounded >= 100 ? "exhausted" : rounded >= 90 ? "warning" : "";
}

function quotaUsage(quota) {
  if (quota?.unlimited === true) return '<span class="unlimited">Unlimited</span>';
  const usedPercent = usedPercentValue(quota);
  if (!numeric(usedPercent)) return "—";
  const rounded = Math.round(usedPercent * 10) / 10;
  const display = `${percent(rounded)}%`;
  const progress = Math.min(100, Math.max(0, rounded));
  const level = usageLevel(rounded);
  return `<span class="quota-usage${level ? ` ${level}` : ""}"><progress value="${progress}" max="100" aria-label="${display} used"></progress><span class="quota-usage-value">${display}</span></span>`;
}

function renderQuotas(account) {
  $("quotas").innerHTML = [["premium_interactions", "Premium interactions"], ["chat", "Chat"], ["completions", "Completions"]].map(([key, label]) => {
    const q = account?.quota_snapshots?.[key];
    if (q?.unlimited === true) {
      return `<article class="quota"><div class="quota-top"><h3>${label}</h3><span class="unlimited">Unlimited</span></div>${numeric(usedValue(q)) ? `<div class="quota-values"><span>${quotaUsed(q)} used</span></div>` : ""}</article>`;
    }
    const usedPercent = usedPercentValue(q);
    const level = numeric(usedPercent) ? usageLevel(usedPercent) : "";
    return `<article class="quota"><div class="quota-top"><h3>${label}</h3><span class="${level || "muted"}">${numeric(usedPercent) ? `${percent(usedPercent)}% used` : "—"}</span></div>
      ${numeric(usedPercent) ? `<progress${level ? ` class="${level}"` : ""} value="${Math.min(100, Math.max(0, usedPercent))}" max="100" aria-label="${label}: ${percent(usedPercent)}% used"></progress>` : empty()}
      <div class="quota-values"><span>${quotaUsed(q)} used / ${quotaTotal(q)}</span><span>${quotaRemaining(q)} remaining</span></div></article>`;
  }).join("");
}

function renderMetrics(totals) {
  $("metrics").classList.toggle("single", Boolean(state.account));
  const values = [metric("Total tokens", tokens(totals?.total_tokens)), metric("Requests", number(totals?.request_count)), metric("Cost", money(totals?.costs))];
  if (state.account) values.push(metric("Input tokens", tokens(totals?.input_tokens)), metric("Output tokens", tokens(totals?.output_tokens)), metric("Cache read", tokens(totals?.cache_read_input_tokens)), metric("Cache creation", tokens(totals?.cache_creation_input_tokens)));
  values.push(metric("Cache hit rate", cacheHitRateText(totals)));
  $("metrics").innerHTML = values.join("");
}

function trendTooltip(day) {
  if (day.recorded === false) return `<strong>${escapeHTML(day.date)}</strong><span class="chart-tooltip-empty">No recorded usage</span>`;
  return `<strong>${escapeHTML(day.date)}</strong><dl><div><dt>Total tokens</dt><dd>${tokens(day.totals?.total_tokens)}</dd></div><div><dt>Requests</dt><dd>${number(day.totals?.request_count)}</dd></div><div><dt>Cost</dt><dd>${money(day.totals?.costs)}</dd></div></dl>`;
}

function trendIndexAt(position, start, end, count) {
  if (count <= 1 || end <= start) return 0;
  return Math.round((Math.min(end, Math.max(start, position)) - start) / (end - start) * (count - 1));
}

function setupTrendInteraction(days, geometry) {
  const plot = $("trend").querySelector(".chart-plot");
  if (!plot) return;
  const svg = plot.querySelector("svg"), hitArea = plot.querySelector(".chart-hit-area"), guide = plot.querySelector(".chart-guide"), active = plot.querySelector(".chart-active"), tooltip = plot.querySelector(".chart-tooltip");
  const { width, height, left, right, x, y, values } = geometry;
  let selected = -1;

  const hide = (reset = false) => {
    if (reset) selected = -1;
    guide.hidden = true;
    active.hidden = true;
    tooltip.hidden = true;
  };
  const show = (index) => {
    if (index < 0 || index >= days.length) return;
    if (index === selected && !tooltip.hidden) return;
    selected = index;
    const pointX = x(index), pointY = y(values[index]);
    guide.setAttribute("x1", pointX);
    guide.setAttribute("x2", pointX);
    active.setAttribute("cx", pointX);
    active.setAttribute("cy", pointY);
    guide.hidden = false;
    active.hidden = false;
    tooltip.innerHTML = trendTooltip(days[index]);
    tooltip.hidden = false;

    const bounds = svg.getBoundingClientRect(), plotBounds = plot.getBoundingClientRect();
    const scale = Math.min(bounds.width / width, bounds.height / height);
    const offsetX = (bounds.width - width * scale) / 2, offsetY = (bounds.height - height * scale) / 2;
    const tooltipWidth = tooltip.offsetWidth, tooltipHeight = tooltip.offsetHeight;
    const screenX = bounds.left - plotBounds.left + offsetX + pointX * scale;
    const screenY = bounds.top - plotBounds.top + offsetY + pointY * scale;
    const inset = 8, gap = 12;
    const minLeft = inset, maxLeft = Math.max(minLeft, plotBounds.width - tooltipWidth - inset);
    const minTop = inset, maxTop = Math.max(minTop, plotBounds.height - tooltipHeight - inset);
    const clamp = (value, min, max) => Math.min(max, Math.max(min, value));
    let tooltipLeft = clamp(screenX - tooltipWidth / 2, minLeft, maxLeft);
    let tooltipTop;
    const above = screenY - tooltipHeight - gap, below = screenY + gap;
    if (above >= minTop) tooltipTop = above;
    else if (below <= maxTop) tooltipTop = below;
    else {
      const besideLeft = screenX - tooltipWidth - gap, besideRight = screenX + gap;
      if (besideLeft >= minLeft) tooltipLeft = besideLeft;
      else if (besideRight <= maxLeft) tooltipLeft = besideRight;
      tooltipTop = clamp(screenY - tooltipHeight / 2, minTop, maxTop);
    }
    tooltip.style.left = `${tooltipLeft}px`;
    tooltip.style.top = `${tooltipTop}px`;
  };
  const showAtPointer = (event) => {
    const bounds = svg.getBoundingClientRect();
    const scale = Math.min(bounds.width / width, bounds.height / height);
    const offsetX = (bounds.width - width * scale) / 2;
    const position = (event.clientX - bounds.left - offsetX) / scale;
    show(trendIndexAt(position, left, width - right, days.length));
  };

  hitArea.addEventListener("pointermove", showAtPointer);
  hitArea.addEventListener("pointerdown", (event) => {
    showAtPointer(event);
    if (event.pointerType !== "mouse") plot.focus({ preventScroll: true });
  });
  hitArea.addEventListener("pointerleave", (event) => {
    if (event.pointerType === "mouse") hide();
  });
  plot.addEventListener("focus", () => show(selected >= 0 ? selected : days.length - 1));
  plot.addEventListener("blur", () => hide(true));
  plot.addEventListener("keydown", (event) => {
    let index = selected >= 0 ? selected : days.length - 1;
    if (event.key === "ArrowLeft") index = Math.max(0, index - 1);
    else if (event.key === "ArrowRight") index = Math.min(days.length - 1, index + 1);
    else if (event.key === "Home") index = 0;
    else if (event.key === "End") index = days.length - 1;
    else if (event.key === "Escape") { hide(); return; }
    else return;
    event.preventDefault();
    show(index);
  });
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
  const padded = days.some((day) => day.recorded === false);
  const dots = days.length <= 31 ? values.map((value, i) => `<circle class="chart-point" cx="${x(i)}" cy="${y(value)}" r="3"/>`).join("") : "";
  $("trend").innerHTML = `<div class="chart"><div class="chart-plot" tabindex="0" role="group" aria-label="Daily total tokens. Use Left and Right Arrow keys to inspect dates."><svg viewBox="0 0 ${width} ${height}" role="img" aria-label="Daily total tokens. Daily breakdown is available in the daily values table.">${ticks}<polygon class="area" points="${x(0)},${y(0)} ${points} ${x(days.length - 1)},${y(0)}"/><polyline class="line" points="${points}"/>${dots}<line class="chart-guide" y1="${top}" y2="${height - bottom}" hidden/><circle class="chart-active" r="5" hidden/>${labels}<rect class="chart-hit-area" x="${left}" y="${top}" width="${width - left - right}" height="${height - top - bottom}"/></svg><div id="trend-tooltip" class="chart-tooltip" role="status" aria-atomic="true" hidden></div></div>${padded ? '<p class="chart-note">Dates without recorded usage are shown as zero.</p>' : ""}</div>
    <details class="chart-details"${expanded ? " open" : ""}><summary>Daily values</summary>${sortableTable("days", "Daily values", ["Date", "Tokens", "Requests", "Cost"], days,
      [(d) => d.date, (d) => d.totals?.total_tokens, (d) => d.totals?.request_count, (d) => costValue(d.totals?.costs)],
      (day) => `<tr><td>${escapeHTML(day.date)}${day.recorded === false ? ' <span class="not-recorded">Not recorded</span>' : ""}</td><td class="number">${tokens(day.totals?.total_tokens)}</td><td class="number">${number(day.totals?.request_count)}</td><td class="number">${money(day.totals?.costs)}</td></tr>`, (d) => d.totals?.costs)}</details>`;
  setupTrendInteraction(days, { width, height, left, right, x, y, values });
}

function renderModels(models) {
  if (!Array.isArray(models)) { $("models").innerHTML = empty(); return; }
  if (!models.length) { $("models").innerHTML = empty("No model usage for this period."); return; }
  $("models").innerHTML = sortableTable("models", "Model breakdown", ["Model", "Input", "Output", "Cache read", "Cache creation", "Cache hit rate", "Tokens", "Requests", "Cost"], models,
    [(m) => m.model, ...["input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"].map((key) => (m) => m[key]), cacheHitRate, (m) => m.total_tokens, (m) => m.request_count, (m) => costValue(m.costs)],
    (m) => `<tr><td class="text-cell">${escapeHTML(m.model)}</td>${[m.input_tokens, m.output_tokens, m.cache_read_input_tokens, m.cache_creation_input_tokens].map((v) => `<td class="number">${tokens(v)}</td>`).join("")}<td class="number">${cacheHitRateText(m)}</td><td class="number">${tokens(m.total_tokens)}</td><td class="number">${number(m.request_count)}</td><td class="number">${money(m.costs)}</td></tr>`, (m) => m.costs);
}

function renderAccounts(accounts) {
  hasAccountAliases = accounts.some((a) => a.display_name);
  if (!accounts.length) {
    $("accounts").innerHTML = empty(state.dashboardErrors.length ? "No accounts available." : "No accounts found. Add endpoints to the endpoints file, or run Copilot API containers on a Docker network shared with the dashboard.");
    return;
  }
  const names = accounts.map((a) => a.display_name || a.login), counts = new Map();
  for (const name of names) counts.set(name, (counts.get(name) ?? 0) + 1);
  const labels = new Map(accounts.map((a, i) => [a.login, a.display_name && counts.get(names[i]) > 1 ? `${names[i]} (${a.login})` : names[i]]));
  const accountName = (a) => state.showAccountNames ? labels.get(a.login) : a.login;
  $("accounts").innerHTML = sortableTable("accounts", "Account usage", ["Account", "Plan", "Usage", "Used", "Remaining", "Total", "Tokens", "Requests", "Cost"], accounts,
    [accountName, (a) => a.copilot_plan, (a) => usedPercentValue(a.quota_snapshots?.premium_interactions), (a) => usedValue(a.quota_snapshots?.premium_interactions), (a) => remainingValue(a.quota_snapshots?.premium_interactions), (a) => validMeteredQuota(a.quota_snapshots?.premium_interactions) ? a.quota_snapshots.premium_interactions.entitlement : null, (a) => a.totals?.total_tokens, (a) => a.totals?.request_count, (a) => costValue(a.totals?.costs)],
    (a) => `<tr><td class="text-cell">${escapeHTML(accountName(a))}</td><td>${escapeHTML(a.copilot_plan)}</td><td class="number">${quotaUsage(a.quota_snapshots?.premium_interactions)}</td><td class="number">${quotaUsed(a.quota_snapshots?.premium_interactions)}</td><td class="number">${quotaRemaining(a.quota_snapshots?.premium_interactions)}</td><td class="number">${quotaTotal(a.quota_snapshots?.premium_interactions)}</td><td class="number">${tokens(a.totals?.total_tokens)}</td><td class="number">${number(a.totals?.request_count)}</td><td class="number">${money(a.totals?.costs)}</td></tr>`, (a) => a.totals?.costs);
}

function renderErrors() {
  const unique = new Map([...state.dashboardErrors, ...state.eventErrors].map((e) => [JSON.stringify([e.target, e.operation, e.message]), e]));
  $("errors-section").hidden = unique.size === 0;
  $("errors-alert").hidden = unique.size === 0;
  $("errors-alert").innerHTML = unique.size ? `<span>${number(unique.size)} data source ${unique.size === 1 ? "error" : "errors"}.</span> <a href="#errors-section">View details</a>` : "";
  $("errors").innerHTML = unique.size ? table("Data source errors", ["Target", "Operation", "Error"], [...unique.values()].map((e) => `<tr><td>${escapeHTML(e.target)}</td><td>${escapeHTML(e.operation)}</td><td>${escapeHTML(e.message)}</td></tr>`)) : "";
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
  shownEvents = "";
  $("events").innerHTML = empty(message);
  $("events").setAttribute("aria-busy", "false");
  $("events-count").textContent = "";
  $("page-label").textContent = "";
  $("previous").disabled = true;
  $("next").disabled = true;
}

function renderEvents(data) {
  if (!data) { resetEvents(); return; }
  shownEvents = JSON.stringify([state.account, state.period]);
  state.page = data.page;
  state.totalPages = data.total_pages;
  $("events-count").textContent = `${number(data.total)} requests${timeZone ? ` · Times in ${timeZone}` : ""}`;
  $("page-label").textContent = `Page ${number(data.page)} of ${number(data.total_pages)}`;
  $("previous").disabled = data.page <= 1;
  $("next").disabled = data.page >= data.total_pages;
  $("events").innerHTML = data.items.length ? table("Request events", ["Time", "Model", "Endpoint", "Input", "Output", "Cache read", "Cache creation", "Duration", "Tokens", "Cost", "Outcome"], data.items.map((e) => {
    const date = new Date(e.created_at_ms);
    const time = Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-US");
    return `<tr><td title="${escapeHTML(e.created_at_utc)}">${escapeHTML(time)}</td><td class="text-cell">${escapeHTML(e.model)}</td><td>${escapeHTML(e.endpoint)}</td>${[e.input_tokens, e.output_tokens, e.cache_read_input_tokens, e.cache_creation_input_tokens].map((v) => `<td class="number">${tokens(v)}</td>`).join("")}<td class="number">${duration(e.duration_ms)}</td><td class="number">${tokens(e.total_tokens)}</td><td class="number">${money(e.cost ? [e.cost] : null)}</td><td>${requestOutcome(e.request_outcome)}</td></tr>`;
  })) : empty("No request events for this period.");
}

async function request(path, query, signal, method = "GET") {
  const target = state.account || "Dashboard";
  const operation = path.split("/").pop();
  try {
    const url = new URL(path, appBaseURL);
    url.search = query.toString();
    const response = await fetch(url, { method, signal, cache: "no-store", credentials: "omit" });
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
  $("events").setAttribute("aria-busy", "true");
  try {
    const result = await request("api/v1/events", new URLSearchParams({ account: state.account, period: state.period, page: state.page, page_size: 20 }), controller.signal);
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

function setSyncing(syncing) {
  $("refresh").textContent = syncing ? "Syncing…" : "Refresh";
  $("refresh").setAttribute("aria-disabled", String(syncing));
}

async function refresh(sync = false) {
  dashboardController?.abort();
  eventsController?.abort();
  const controller = new AbortController();
  dashboardController = controller;
  const id = ++refreshID;
  state.page = 1;
  $("content").setAttribute("aria-busy", "true");
  $("events").setAttribute("aria-busy", "true");
  $("previous").disabled = true;
  $("next").disabled = true;
  setSyncing(sync);
  $("announcement").textContent = sync ? "Syncing usage…" : "Loading usage…";
  const query = new URLSearchParams({ period: state.period });
  if (state.account) query.set("account", state.account);
  try {
    let syncErrors = [];
    if (sync) {
      const syncQuery = new URLSearchParams();
      if (state.account) syncQuery.set("account", state.account);
      const syncResult = await request("api/v1/sync", syncQuery, controller.signal, "POST");
      if (id !== refreshID || controller.signal.aborted) return;
      syncErrors = syncResult.errors;
    }
    const result = await request("api/v1/dashboard", query, controller.signal);
    if (id !== refreshID || controller.signal.aborted) return;
    state.dashboardErrors = [...syncErrors, ...result.errors];
    state.eventErrors = [];
    tableViews.clear();
    const found = renderDashboard(result.data);
    const urlAccount = new URLSearchParams(location.search).get("account");
    if (urlAccount && urlAccount !== state.account && urlAccount.toLowerCase() === state.account.toLowerCase()) {
      const url = new URL(location.href);
      url.searchParams.set("account", state.account);
      history.replaceState(null, "", url.href);
    }
    renderErrors();
    $("announcement").textContent = state.dashboardErrors.length ? "Usage loaded with data source errors listed below." : "Usage loaded.";
    if (state.account && shownEvents !== JSON.stringify([state.account, state.period])) resetEvents("Loading events…");
    $("events-section").hidden = !state.account;
    if (state.account && found) void loadEvents(); else resetEvents();
  } catch (error) {
    if (!controller.signal.aborted) throw error;
  } finally {
    if (id === refreshID && !controller.signal.aborted) {
      $("content").setAttribute("aria-busy", "false");
      setSyncing(false);
    }
  }
}

function filterQuery(account, period) {
  const query = new URLSearchParams();
  if (account) query.set("account", account);
  if (period !== defaultPeriod) query.set("period", period);
  return query;
}

function filterURL() {
  const url = new URL(location.href);
  url.search = filterQuery(state.account, state.period).toString();
  url.hash = "";
  return url.href;
}

function selectAccountOption(login) {
  const select = $("account");
  if (login && ![...select.options].some((option) => option.value === login)) select.add(new Option(login, login));
  select.value = login;
}

function locationFilters() {
  const query = new URLSearchParams(location.search);
  const period = query.get("period") ?? defaultPeriod;
  return { account: query.get("account") ?? "", period: periods.includes(period) ? period : defaultPeriod, validPeriod: periods.includes(period) };
}

function readLocation() {
  const filters = locationFilters();
  state.account = filters.account;
  state.period = filters.period;
  selectAccountOption(state.account);
  $("period").value = state.period;
  return filters.validPeriod;
}

function selectFilters(account, period) {
  state.account = account;
  state.period = period;
  selectAccountOption(account);
  $("period").value = period;
  history.pushState(null, "", filterURL());
  void refresh();
}

$("controls").addEventListener("submit", (event) => {
  event.preventDefault();
  if ($("refresh").getAttribute("aria-disabled") !== "true") void refresh(true);
});
$("account").addEventListener("change", () => selectFilters($("account").value, state.period));
$("period").addEventListener("change", () => selectFilters(state.account, $("period").value));
window.addEventListener("popstate", () => {
  const { account, period } = locationFilters();
  if (account.toLowerCase() === state.account.toLowerCase() && period === state.period) return;
  readLocation();
  void refresh();
});
$("previous").addEventListener("click", () => { if (state.page > 1) { state.page--; void loadEvents(); } });
$("next").addEventListener("click", () => { if (state.page < state.totalPages) { state.page++; void loadEvents(); } });
document.addEventListener("keydown", (event) => {
  if (!["d", "D"].includes(event.key) || event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey || event.repeat || event.isComposing) return;
  if (event.target?.isContentEditable || event.target?.closest?.("input, textarea, select")) return;
  if ($("accounts-section").hidden || !hasAccountAliases || !tableViews.has("accounts")) return;
  event.preventDefault();
  state.showAccountNames = !state.showAccountNames;
  redrawTable("accounts");
  $("announcement").textContent = state.showAccountNames ? "Account display names shown." : "Account logins shown.";
});
$("content").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-sort]");
  if (!button || button.disabled) return;
  const id = button.dataset.sort, column = Number(button.dataset.column), sort = sorts[id];
  sort.direction = sort.column === column ? -sort.direction : 1;
  sort.column = column;
  redrawTable(id);
});
let chartWidth = 0;
new ResizeObserver(([entry]) => {
  const width = Math.round(entry.contentRect.width);
  if (width !== chartWidth) { chartWidth = width; if (chartDays) renderTrend(chartDays); }
}).observe($("trend"));
if (!readLocation()) history.replaceState(null, "", filterURL());
void refresh();
