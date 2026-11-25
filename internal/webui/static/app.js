/**
 * PAM 的浏览器传输与界面层。只访问同源 REST，会话由 HttpOnly cookie 维护；
 * 访问密钥只在登录表单提交时短暂使用，不存入浏览器持久存储。
 */
import { AuditCenter } from './audit.js';

const state = {
  authenticated: false,
  view: "transactions",
  transactions: { items: [], total: 0, limit: 50, offset: 0, filters: {}, request: 0, busy: false },
  assets: { items: [], total: 0, limit: 24, offset: 0, request: 0, busy: false },
  detailRequest: 0,
  editingId: null,
  assetBusy: false,
  assetLoaded: true,
  sessionRequest: 0,
  sessionEpoch: 0,
  countsRequest: 0,
  assetRequest: 0,
  toastTimer: null,
};
const statuses = { active: "使用中", stored: "已收纳", loaned: "已借出", sold: "已售出", retired: "已退役", lost: "已遗失" };
const numbers = new Intl.NumberFormat("zh-CN");
const auditCenter = new AuditCenter(request);
/** 标准账单来源的本地展示目录；空键表示全部，图标只引用随页面提供的 SVG，不接受远端地址。 */
const platformDefinitions = Object.freeze({
  "": { label: "全部", icon: "all" },
  wechat: { label: "微信", icon: "wechat" },
  alipay: { label: "支付宝", icon: "alipay" },
  cmb: { label: "招商银行", icon: "cmb" },
  ccb: { label: "建设银行", icon: "ccb" },
  other: { label: "其他", icon: "other" },
});

/** 按 id 取得静态页面元素；缺失时抛固定配置错误，不回显业务数据。@param {string} id 元素标识。@returns {HTMLElement} 已存在的元素。 */
function element(id) {
  const value = document.getElementById(id);
  if (!value) throw new Error("Missing interface element");
  return value;
}

/** 创建仅含安全文本的 DOM 节点。@param {string} tag HTML 标签。@param {string} className 样式类，可空。@param {string} text 显示文本，可空。@returns {HTMLElement} 未挂载的新节点。 */
function node(tag, className = "", text = "") {
  const value = document.createElement(tag);
  if (className) value.className = className;
  value.textContent = text;
  return value;
}

/** 创建本地 SVG 图标，不解析业务文本为标记。@param {string} platform 目录中的标准来源或空串全部。@returns {SVGElement} 装饰性图标，读屏由旁边的可见名称和按钮标签提供。 */
function platformIcon(platform) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.classList.add("platform-icon");
  svg.setAttribute("viewBox", "0 0 32 32");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `#platform-icon-${platformDefinitions[platform].icon}`);
  svg.append(use);
  return svg;
}

/** 初始化原生按钮组成的单选来源筛选；按钮自带 Enter/Space 键盘操作，不模拟不可访问的点击区域。@returns {void} 渲染固定目录，无业务数据或金额合计。 */
function renderPlatformFilters() {
  const fragment = document.createDocumentFragment();
  for (const [platform, definition] of Object.entries(platformDefinitions)) {
    const button = node("button", "platform-chip");
    button.type = "button";
    button.dataset.platformFilter = platform;
    button.setAttribute("aria-label", `筛选账单来源：${definition.label}`);
    button.append(platformIcon(platform), node("span", "", definition.label));
    // 点击当前来源时回到全部；此处只改变查询条件，不产生任何业务写入。
    button.addEventListener("click", () => selectPlatform(platform));
    fragment.append(button);
  }
  element("platform-filters").replaceChildren(fragment);
  syncPlatformFilters();
}

/** 将隐藏表单值同步到筛选按钮的可见及读屏状态。@returns {void} 始终只有一个 aria-pressed=true，空值表示全部。 */
function syncPlatformFilters() {
  const selected = element("platform-filter-value").value;
  for (const button of element("platform-filters").querySelectorAll("[data-platform-filter]")) {
    button.setAttribute("aria-pressed", String(button.dataset.platformFilter === selected));
  }
}

/** 对完整账本应用或取消来源筛选。@param {string} platform 标准枚举，空串清除来源。@param {boolean} focusFilter 是否由列表/详情跳回对应筛选按钮。@returns {void} 保留其他表单条件、清零 offset 后请求服务器，非法表单不改变现有筛选。 */
function selectPlatform(platform, focusFilter = false) {
  if (!Object.hasOwn(platformDefinitions, platform)) return;
  const form = element("filters");
  if (!form.reportValidity()) return;
  try {
    const filters = readFilters();
    const selected = filters.platform === platform ? "" : platform;
    if (selected) filters.platform = selected; else delete filters.platform;
    element("platform-filter-value").value = selected;
    state.transactions.filters = filters;
    state.transactions.offset = 0;
    syncPlatformFilters();
    message("global-notice", "");
    if (element("detail-dialog").open) element("detail-dialog").close();
    // 列表重绘会移除原按钮；把键盘焦点转移到仍存在的筛选项，避免焦点丢失。
    if (focusFilter) element("platform-filters").querySelector(`[data-platform-filter="${selected}"]`)?.focus();
    void loadTransactions();
  } catch (error) { message("global-notice", error.message); }
}

/** 渲染一笔交易的全部标准来源，不按商户、摘要或当前分页推断来源。@param {Array<string>|null} platforms 后端 platforms 数组，允许缺失以兼容滚动发布。@returns {HTMLElement} 来源按钮组；未提供标记时只展示中性提示，不伪造微信/支付宝渠道。 */
function platformBadges(platforms) {
  const group = node("div", "platform-badges");
  const known = new Set(Array.isArray(platforms) ? platforms.filter((value) => value !== "" && Object.hasOwn(platformDefinitions, value)) : []);
  for (const [platform, definition] of Object.entries(platformDefinitions)) {
    if (!known.has(platform)) continue;
    const button = node("button", "platform-badge");
    button.type = "button";
    button.dataset.platform = platform;
    button.title = `筛选${definition.label}账单来源；同一笔可有多个来源，仍只计一笔。来源不代表支付渠道。`;
    button.setAttribute("aria-label", `筛选账单来源：${definition.label}`);
    button.setAttribute("aria-pressed", String(state.transactions.filters.platform === platform));
    button.append(platformIcon(platform), node("span", "", definition.label));
    // 来源点击只触发全库筛选，不冒泡到整行的交易详情处理器。
    button.addEventListener("click", (event) => { event.stopPropagation(); selectPlatform(platform, true); });
    group.append(button);
  }
  if (!group.childElementCount) group.append(node("span", "platform-unmarked", "未标注"));
  return group;
}

/** 安全展示或清空消息。@param {string} id 消息元素标识。@param {string} message 要显示的说明；空串隐藏。@returns {void} 修改对应元素。 */
function message(id, message) {
  const target = element(id);
  target.textContent = message;
  target.hidden = !message;
}

/** 调用同源 REST 并统一处理错误；不生成 SQL 或给浏览器设置 Bearer token。@param {string} path 同源绝对路径。@param {object} options fetch 选项。@returns {Promise<any>} 成功 JSON 或 204 的 null；网络/HTTP 错误拒绝。 */
async function request(path, options = {}) {
  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (options.method && options.method !== "GET") headers.set("Content-Type", "application/json");
  let response;
  try { response = await fetch(path, { ...options, headers, credentials: "same-origin", cache: "no-store", redirect: "error" }); }
  catch { throw new Error("暂时无法连接 NAS，请检查网络后重试。"); }
  // 会话接口的拒绝正文可以是纯文本；先处理 401，保证用户得到准确的登录提示。
  if (response.status === 401) {
    if (path !== "/web/session") {
      showAuthentication();
      void checkSession();
      throw new Error("登录已失效，请重新登录。");
    }
    throw new Error("访问密钥无效，请检查后重试。");
  }
  let body = null;
  if (response.status !== 204) {
    try { body = await response.json(); }
    catch { if (response.ok) throw new Error("服务返回了无法识别的响应，请稍后重试。"); }
  }
  if (!response.ok) {
    if (response.status === 429) throw new Error("请求较多，请稍候再试。");
    if (response.status === 404) throw new Error("这条记录已不存在，请刷新列表。");
    const detail = body?.error?.message;
    throw new Error(detail ? `操作未完成：${detail}` : "操作未完成，请稍后重试。");
  }
  return body;
}

/** 将十进制字符串格式化为金额；绝不经 Number 或 parseFloat。@param {string|null} value API 的非负十进制字符串。@returns {string} 千位分组且至少两位小数的文本；未知返回破折号。 */
function money(value) {
  if (typeof value !== "string" || !/^\d+(\.\d+)?$/.test(value)) return "—";
  const [whole, raw = ""] = value.split(".");
  const fraction = raw.replace(/0+$/, "").padEnd(2, "0");
  return `${whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",")}.${fraction}`;
}

/** 提取来源日期与时间，避免未知时间被显示成午夜。@param {object} record 具有 occurred_at/original 的流水。@returns {{date:string,time:string}} 上海时区的显示文本；date 精度保留来源自然日。 */
function transactionDate(record) {
  const original = record.original || {};
  const precision = original.pam_timestamp_precision;
  const sourceDate = original.pam_source_date || original["日期"];
  const instant = new Date(record.occurred_at);
  if (Number.isNaN(instant.getTime())) return { date: "日期未知", time: "" };
  const parts = new Intl.DateTimeFormat("en-CA", { timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit" }).formatToParts(instant);
  const dateParts = Object.fromEntries(parts.map((part) => [part.type, part.value]));
  const date = /^\d{4}-\d{2}-\d{2}$/.test(String(sourceDate || "")) ? sourceDate : `${dateParts.year}-${dateParts.month}-${dateParts.day}`;
  if (precision === "date") return { date, time: "仅记录日期" };
  const options = { timeZone: "Asia/Shanghai", hour: "2-digit", minute: "2-digit", hour12: false };
  if (precision === "second") options.second = "2-digit";
  return { date, time: new Intl.DateTimeFormat("zh-CN", options).format(instant) };
}

/** 根据当前认证状态隐藏所有业务数据和打开的对话框。@returns {void} 清空页面中已呈现的财务记录。 */
function showAuthentication() {
  auditCenter.clear();
  state.authenticated = false;
  state.sessionEpoch += 1;
  state.sessionRequest += 1;
  state.countsRequest += 1;
  state.assetRequest += 1;
  state.transactions.request += 1;
  state.assets.request += 1;
  state.detailRequest += 1;
  Object.assign(state.transactions, { items: [], total: 0, offset: 0, filters: {}, busy: false });
  Object.assign(state.assets, { items: [], total: 0, offset: 0, busy: false });
  state.editingId = null;
  state.assetBusy = false;
  state.assetLoaded = false;
  for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close();
  element("transaction-rows").replaceChildren();
  element("asset-cards").replaceChildren();
  element("detail-body").replaceChildren();
  element('review-items').replaceChildren();
  element('source-ranges').replaceChildren();
  element('review-notice').textContent = '';
  element('import-review-panel').open = false;
  element("asset-form").reset();
  element("filters").reset();
  // hidden 输入的 value 会同步默认值，原生 reset 不足以撤销已选来源，必须显式清空。
  element("platform-filter-value").value = "";
  syncPlatformFilters();
  for (const id of ["transaction-total", "filtered-total", "asset-total", "transaction-count", "transaction-range", "asset-range"]) element(id).textContent = "—";
  message("global-notice", "");
  message("toast", "");
  element("app").hidden = true;
  element("auth-view").hidden = false;
}

/** 验证会话并取服务端提供的 NAS 登录地址；失败只显示可重试错误。@returns {Promise<void>} 成功认证后加载真实列表。 */
async function checkSession() {
  const version = ++state.sessionRequest;
  element("session-loading").hidden = false;
  element("session-retry").hidden = true;
  message("auth-error", "");
  try {
    const session = await request("/web/session");
    if (version !== state.sessionRequest) return;
    element("session-loading").hidden = true;
    if (session.authenticated) { await enterWorkspace(); return; }
    element("auth-actions").hidden = false;
    const login = element("nas-login");
    login.hidden = true;
    if (session.login_url) {
      const destination = new URL(session.login_url, location.href);
      if (destination.protocol === "https:" || (destination.origin === location.origin && destination.protocol === "http:")) {
        login.href = destination.href;
        login.hidden = false;
      }
    }
    element("token-details").open = login.hidden;
  } catch (error) {
    if (version !== state.sessionRequest) return;
    element("session-loading").hidden = true;
    element("session-retry").hidden = false;
    message("auth-error", error.message);
  }
}

/** 展示已认证工作空间并并行加载计数和当前页面。@returns {Promise<void>} 请求结束后页面具有成功或错误状态。 */
async function enterWorkspace() {
  state.authenticated = true;
  element("auth-view").hidden = true;
  element("app").hidden = false;
  element("access-token").value = "";
  await Promise.allSettled([refreshCounts(), state.view === 'audit' ? auditCenter.load() : state.view === "assets" ? loadAssets() : loadTransactions()]);
}

/** 从最小分页响应读取真实总数，不对金额求和或编造资产估值。@returns {Promise<void>} 更新指标；失败明确显示无法加载。 */
async function refreshCounts() {
  const epoch = state.sessionEpoch;
  const version = ++state.countsRequest;
  try {
    const [transactions, assets] = await Promise.all([request("/api/v1/transactions?limit=1&offset=0"), request("/api/v1/assets?limit=1&offset=0")]);
    if (!state.authenticated || epoch !== state.sessionEpoch || version !== state.countsRequest) return;
    element("transaction-total").textContent = numbers.format(transactions.total);
    element("asset-total").textContent = numbers.format(assets.total);
    message("global-notice", "");
  } catch (error) { if (state.authenticated && epoch === state.sessionEpoch && version === state.countsRequest) message("global-notice", `概况暂不可用：${error.message}`); }
}

/** 在业务区域展示加载、空记录或错误，并提供显式重试。@param {string} id 容器标识。@param {string} kind loading/empty/error。@param {string} detail 说明文本。@param {Function|null} retry 点击时启动一次重试。@returns {void} 替换安全文本节点。 */
function contentStatus(id, kind, detail, retry = null) {
  const target = element(id);
  target.hidden = false;
  const children = [node("span", kind === "loading" ? "spinner" : "status-icon", kind === "empty" ? "↔" : kind === "error" ? "!" : ""), node("strong", "", kind === "loading" ? "正在读取记录" : kind === "empty" ? "这里还没有记录" : "暂时无法加载"), node("span", "", detail)];
  if (retry) {
    const button = node("button", "button secondary", "重新加载");
    button.type = "button";
    button.addEventListener("click", retry);
    children.push(button);
  }
  target.replaceChildren(...children);
}

/** 切换已授权业务视图。@param {string} view transactions/assets/audit。@returns {void} 更新导航并加载目标页，审计不写入查询日志。 */
function changeView(view) {
  if (!['transactions','assets','audit'].includes(view)) return;
  state.view = view;
  for (const button of document.querySelectorAll("[data-view]")) {
    const active = button.dataset.view === view;
    button.classList.toggle("active", active);
    if (active) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
  }
  const label = view === 'audit' ? 'SQL 审计' : view === "assets" ? "实物资产" : "交易账本";
  element("page-title").replaceChildren(document.createTextNode(label), node("span", "title-dot", "."));
  element("breadcrumb-title").textContent = label;
  element("page-description").textContent = view === 'audit' ? '看见查询行为，读懂每一次拦截与执行。' : view === "assets" ? "认真记录身边的物品，让每一份拥有都清晰可见。" : "回溯每一笔收支，找到生活留下的线索。";
  element("transactions-view").hidden = view !== "transactions";
  element("assets-view").hidden = view !== "assets";
  element('audit-view').hidden = view !== 'audit';
  element('ledger-metrics').hidden = view === 'audit';
  document.title = `PAM · ${label}`;
  if (state.authenticated) void (view === 'audit' ? auditCenter.load() : view === "assets" ? loadAssets() : loadTransactions());
}

/** 把表单日期转换为固定上海自然日的 RFC3339 区间。@param {string} date HTML 日期字段。@param {boolean} nextDay 是否取次日零点作为排他上界。@returns {string} RFC3339 时间；空日期返回空串。 */
function dateBoundary(date, nextDay = false) {
  if (!date) return "";
  if (!nextDay) return `${date}T00:00:00+08:00`;
  const instant = new Date(`${date}T00:00:00Z`);
  instant.setUTCDate(instant.getUTCDate() + 1);
  return `${instant.toISOString().slice(0, 10)}T00:00:00+08:00`;
}

/** 从筛选表单创建含标准账单来源的规范条件，不解析金额为二进制浮点。@returns {object} 非空筛选字段，供服务器执行全库筛选；非法日期区间抛界面说明。 */
function readFilters() {
  const form = element("filters");
  const data = new FormData(form);
  const from = String(data.get("from") || "");
  const to = String(data.get("to") || "");
  if (from && to && from > to) throw new Error("开始日期不能晚于结束日期。");
  const filters = {};
  for (const key of ["search", "direction", "min_amount", "max_amount", "category", "source", "platform"]) {
    const value = String(data.get(key) || "").trim();
    if (value) filters[key] = value;
  }
  if (from) filters.from = dateBoundary(from);
  if (to) filters.to = dateBoundary(to, true);
  return filters;
}

/** 加载当前账本分页；请求世代保护较新的筛选不被迟到响应覆盖。@returns {Promise<void>} 更新表格、当前筛选计数及分页状态。 */
async function loadTransactions() {
  const current = state.transactions;
  const version = ++current.request;
  current.busy = true;
  element("transaction-table-wrap").hidden = true;
  contentStatus("transaction-status", "loading", "正在从你的 NAS 读取交易记录…");
  updatePagination("transaction", current);
  const parameters = new URLSearchParams({ ...current.filters, limit: String(current.limit), offset: String(current.offset) });
  try {
    const page = await request(`/api/v1/transactions?${parameters}`);
    if (version !== current.request || !state.authenticated) return;
    current.items = page.items;
    current.total = page.total;
    element("filtered-total").textContent = numbers.format(page.total);
    element("transaction-count").textContent = numbers.format(page.total);
    renderTransactions(page.items);
    if (!page.items.length) contentStatus("transaction-status", "empty", "试试调整筛选条件，或在导入记录后刷新。");
    else { element("transaction-status").hidden = true; element("transaction-table-wrap").hidden = false; }
  } catch (error) {
    if (version === current.request && state.authenticated) contentStatus("transaction-status", "error", error.message, loadTransactions);
  } finally {
    if (version === current.request) { current.busy = false; updatePagination("transaction", current); }
  }
}

/** 从文本计算稳定头像色号，避免依赖或伪造第三方商户标识。@param {string} text 商户文本。@returns {number} 0..4 的颜色索引，不参与金额计算。 */
function tone(text) {
  let result = 0;
  for (const character of text) result = (result + character.codePointAt(0)) % 5;
  return result;
}

/** 渲染真实流水及每笔完整来源；所有文本经 textContent 写入，来源按钮独立筛选，商户按钮支持键盘进入详情。@param {Array<object>} records 当前分页记录，platforms 为后端标准枚举数组。@returns {void} 替换表格正文，不把组合来源拆成多笔交易。 */
function renderTransactions(records) {
  const fragment = document.createDocumentFragment();
  for (const record of records) {
    const row = node("tr");
    const merchant = record.merchant || "未记录商户";
    const merchantCell = node("td");
    const merchantWrap = node("div", "merchant-cell");
    const avatar = node("span", `merchant-avatar tone-${tone(merchant)}`, Array.from(merchant)[0] || "·");
    avatar.setAttribute("aria-hidden", "true");
    const info = node("div", "merchant-info");
    const button = node("button", "merchant-button", merchant);
    button.type = "button";
    button.setAttribute("aria-label", `查看 ${merchant} 的交易详情`);
    info.append(button, node("div", "merchant-description", record.description || "未记录摘要"));
    merchantWrap.append(avatar, info);
    merchantCell.append(merchantWrap);
    const date = transactionDate(record);
    const dateCell = node("td");
    dateCell.append(node("span", "date-primary", date.date), node("span", "date-secondary", date.time));
    const category = node("td", "category-column");
    category.append(node("span", "category-tag", record.category || "未分类"));
    const source = node("td", "source-column");
    source.append(platformBadges(record.platforms));
    const amount = node("td", "amount-column");
    amount.append(node("span", `amount ${record.direction === "income" ? "income" : ""}`, `${record.direction === "income" ? "+" : "−"}${money(record.amount)}`), node("span", "currency", record.currency));
    const arrow = node("td", "row-arrow", "›");
    arrow.setAttribute("aria-hidden", "true");
    row.append(merchantCell, dateCell, category, source, amount, arrow);
    row.addEventListener("click", () => openTransaction(record.id));
    fragment.append(row);
  }
  element("transaction-rows").replaceChildren(fragment);
}

/** 根据分页的真实 total/offset 更新页码与按钮。@param {string} prefix transaction 或 asset。@param {object} page 含 total/limit/offset/busy 的分页状态。@returns {void} 不进行金额统计。 */
function updatePagination(prefix, page) {
  const start = page.total ? page.offset + 1 : 0;
  const end = Math.min(page.offset + page.limit, page.total);
  element(`${prefix}-range`).textContent = `显示 ${numbers.format(start)}–${numbers.format(end)} 条，共 ${numbers.format(page.total)} 条`;
  element(`${prefix}-page`).textContent = `${Math.floor(page.offset / page.limit) + 1} / ${Math.max(1, Math.ceil(page.total / page.limit))}`;
  element(`${prefix}-prev`).disabled = page.busy || page.offset === 0;
  element(`${prefix}-next`).disabled = page.busy || page.offset + page.limit >= page.total;
}

/** 在详情定义列表追加一项安全文本。@param {HTMLElement} list 目标 dl。@param {string} title 字段名称。@param {string|null} value 字段值。@returns {HTMLElement} 新建 dd，供复制操作追加按钮。 */
function detailField(list, title, value) {
  const content = node("dd", "", value || "未记录");
  list.append(node("dt", "", title), content);
  return content;
}

/** 打开并读取指定流水的标准来源和原始详情。@param {string} id UUID。@returns {Promise<void>} 更新抽屉并提供来源筛选；过期响应忽略。 */
async function openTransaction(id) {
  const dialog = element("detail-dialog");
  const body = element("detail-body");
  const version = ++state.detailRequest;
  body.replaceChildren(node("p", "muted", "正在读取交易详情…"));
  if (!dialog.open) dialog.showModal();
  try {
    const record = await request(`/api/v1/transactions/${encodeURIComponent(id)}`);
    if (version !== state.detailRequest || !dialog.open || !state.authenticated) return;
    const amount = node("div", `detail-amount ${record.direction === "income" ? "income" : ""}`, `${record.direction === "income" ? "+" : "−"}${money(record.amount)}`);
    const date = transactionDate(record);
    const fields = node("dl", "detail-fields");
    detailField(fields, "收支方向", record.direction === "income" ? "收入" : "支出");
    detailField(fields, "交易日期", `${date.date} ${date.time}`);
    detailField(fields, "币种", record.currency);
    detailField(fields, "分类", record.category || "未分类");
    detailField(fields, "交易摘要", record.description);
    const platforms = detailField(fields, "账单来源", "");
    platforms.replaceChildren(platformBadges(record.platforms));
    platforms.append(node("small", "platform-detail-note", "同一笔可有多个来源，仍只计一笔；账单来源不代表实际支付渠道。"));
    detailField(fields, "导入来源", record.source);
    const transactionId = detailField(fields, "交易 ID", record.id);
    const copy = node("button", "copy-button", "复制交易 ID");
    copy.type = "button";
    copy.addEventListener("click", () => copyText(record.id, copy));
    transactionId.append(document.createElement("br"), copy);
    detailField(fields, "外部流水号", record.external_id);
    detailField(fields, "导入批次", record.import_batch_id);
    const original = node("details", "original-record");
    original.open = true;
    original.append(node("summary", "", "原始来源记录"), node("pre", "", JSON.stringify(record.original ?? {}, null, 2)));
    body.replaceChildren(amount, node("p", "detail-merchant", `${record.currency} · ${record.merchant || "未记录商户"}`), fields, original, node("p", "detail-note", "付款记录是支付事实的线索，不等于对具体所购商品的确认。原始日期精度按来源保留。"));
  } catch (error) {
    if (version !== state.detailRequest || !dialog.open) return;
    const retry = node("button", "button secondary", "重新加载");
    retry.type = "button";
    retry.addEventListener("click", () => openTransaction(id));
    body.replaceChildren(node("p", "error-message", error.message), retry);
  }
}

/** 复制用户主动选择的交易标识。@param {string} value 要复制的 UUID。@param {HTMLButtonElement} button 结果反馈按钮。@returns {Promise<void>} 拒绝剪贴板时提示手动选择，不读取剪贴板。 */
async function copyText(value, button) {
  try { await navigator.clipboard.writeText(value); button.textContent = "已复制"; }
  catch { button.textContent = "请选中上方 ID 复制"; }
}

/** 加载资产分页，不生成默认资产或示例记录。@returns {Promise<void>} 渲染服务器数据或明确空/错误状态。 */
async function loadAssets() {
  const current = state.assets;
  const version = ++current.request;
  current.busy = true;
  element("asset-cards").replaceChildren();
  contentStatus("asset-status", "loading", "正在读取你的物品档案…");
  updatePagination("asset", current);
  try {
    const page = await request(`/api/v1/assets?limit=${current.limit}&offset=${current.offset}`);
    if (version !== current.request || !state.authenticated) return;
    current.items = page.items;
    current.total = page.total;
    element("asset-total").textContent = numbers.format(page.total);
    renderAssets(page.items);
    if (!page.items.length) contentStatus("asset-status", "empty", "从身边的一件物品开始，点击「登记资产」建立档案。");
    else element("asset-status").hidden = true;
  } catch (error) {
    if (version === current.request && state.authenticated) contentStatus("asset-status", "error", error.message, loadAssets);
  } finally {
    if (version === current.request) { current.busy = false; updatePagination("asset", current); }
  }
}

/** 将真实资产渲染为可编辑卡片。@param {Array<object>} assets 当前分页资产。@returns {void} 不计算估值，未知购买价明确留空。 */
function renderAssets(assets) {
  const fragment = document.createDocumentFragment();
  for (const asset of assets) {
    const card = node("article", "asset-card");
    const top = node("div", "asset-card-top");
    top.append(node("span", "asset-symbol", "▧"), node("span", `asset-status-tag ${asset.status === "active" ? "active" : ""}`, statuses[asset.status] || asset.status));
    const price = node("div", "asset-price", asset.purchase_price ? money(asset.purchase_price) : "购买价未记录");
    if (asset.purchase_price) price.append(node("small", "", asset.currency));
    const footer = node("div", "asset-card-footer");
    const edit = node("button", "", "查看与编辑");
    edit.type = "button";
    edit.setAttribute("aria-label", `查看与编辑 ${asset.name}`);
    edit.addEventListener("click", () => openAsset(asset.id));
    footer.append(node("span", "", asset.category || "未分类"), edit);
    card.append(top, node("h3", "", asset.name), node("p", "asset-model", [asset.brand, asset.model].filter(Boolean).join(" · ") || "品牌与型号未记录"), price, node("div", "asset-purchase-date", asset.purchase_date ? `${asset.purchase_date} 购入` : "购买日期未记录"), footer);
    fragment.append(card);
  }
  element("asset-cards").replaceChildren(fragment);
}

/** 打开新增或读取最新资产后填充表单。@param {string|null} id 已有资产 UUID；null 为新建。@returns {Promise<void>} 失败保留错误说明，不创建任何业务记录。 */
async function openAsset(id = null) {
  const version = ++state.assetRequest;
  const epoch = state.sessionEpoch;
  state.editingId = id;
  state.assetLoaded = !id;
  const form = element("asset-form");
  form.reset();
  form.elements.currency.value = "CNY";
  message("asset-form-error", "");
  element("asset-load-retry").hidden = true;
  element("asset-dialog-title").textContent = id ? "编辑资产" : "登记资产";
  element("delete-asset").hidden = !id;
  const dialog = element("asset-dialog");
  if (!dialog.open) dialog.showModal();
  if (!id) { setAssetBusy(false); return; }
  setAssetBusy(true);
  try {
    const asset = await request(`/api/v1/assets/${encodeURIComponent(id)}`);
    if (version !== state.assetRequest || epoch !== state.sessionEpoch || state.editingId !== id || !dialog.open || !state.authenticated) return;
    for (const input of form.elements) {
      if (input.name && Object.hasOwn(asset, input.name)) input.value = asset[input.name] ?? "";
    }
    state.assetLoaded = true;
  } catch (error) { if (version === state.assetRequest && epoch === state.sessionEpoch) { message("asset-form-error", error.message); element("asset-load-retry").hidden = false; } }
  finally { if (version === state.assetRequest && epoch === state.sessionEpoch) setAssetBusy(false); }
}

/** 同步资产写入期间的禁用状态，防止重复提交或请求尚未结束就关闭。@param {boolean} busy 是否正在读取/写入。@returns {void} 修改表单与关闭控件。 */
function setAssetBusy(busy) {
  state.assetBusy = busy;
  for (const input of element("asset-form").elements) input.disabled = busy || (!state.assetLoaded && Boolean(input.name));
  element("save-asset").disabled = busy || !state.assetLoaded;
  element("delete-asset").disabled = busy || !state.assetLoaded;
  for (const button of document.querySelectorAll('[data-close="asset-dialog"]')) button.disabled = busy;
  element("confirm-delete").disabled = busy;
  for (const button of document.querySelectorAll('[data-close="delete-dialog"]')) button.disabled = busy;
}

/** 从资产表单读取完整业务输入。@returns {object} 金额、日期和 UUID 都是字符串；未知购买资料为 null。 */
function assetPayload() {
  const data = new FormData(element("asset-form"));
  const result = {};
  for (const key of ["name", "category", "brand", "model", "serial_number", "currency", "merchant", "notes", "status"]) result[key] = String(data.get(key) || "").trim();
  result.currency = result.currency.toUpperCase();
  for (const key of ["purchase_date", "purchase_price", "related_transaction_id"]) result[key] = String(data.get(key) || "").trim() || null;
  return result;
}

/** 保存一次资产表单；成功后从服务器重新加载，失败保留草稿。@param {SubmitEvent} event 表单提交事件。@returns {Promise<void>} 仅经固定 POST/PUT 业务 API 写入。 */
async function saveAsset(event) {
  event.preventDefault();
  if (state.assetBusy || !state.assetLoaded) return;
  const payload = assetPayload();
  const id = state.editingId;
  const epoch = state.sessionEpoch;
  message("asset-form-error", "");
  setAssetBusy(true);
  try {
    await request(id ? `/api/v1/assets/${encodeURIComponent(id)}` : "/api/v1/assets", { method: id ? "PUT" : "POST", body: JSON.stringify(payload) });
    if (epoch !== state.sessionEpoch || !state.authenticated) return;
    element("asset-dialog").close();
    toast(id ? "资产信息已更新" : "资产已登记");
    if (!id) state.assets.offset = 0;
    await Promise.allSettled([loadAssets(), refreshCounts()]);
  } catch (error) { if (epoch === state.sessionEpoch) message("asset-form-error", error.message); }
  finally { if (epoch === state.sessionEpoch) setAssetBusy(false); }
}

/** 删除用户已明确确认的当前资产；不触及关联交易。@returns {Promise<void>} 成功关闭弹窗并刷新当前分页，失败保留重试。 */
async function deleteAsset() {
  if (state.assetBusy || !state.editingId) return;
  message("delete-error", "");
  const epoch = state.sessionEpoch;
  setAssetBusy(true);
  try {
    await request(`/api/v1/assets/${encodeURIComponent(state.editingId)}`, { method: "DELETE" });
    if (epoch !== state.sessionEpoch || !state.authenticated) return;
    element("delete-dialog").close();
    element("asset-dialog").close();
    toast("资产登记已删除，交易记录已保留");
    if (state.assets.items.length === 1 && state.assets.offset > 0) state.assets.offset = Math.max(0, state.assets.offset - state.assets.limit);
    await Promise.allSettled([loadAssets(), refreshCounts()]);
  } catch (error) { if (epoch === state.sessionEpoch) message("delete-error", error.message); }
  finally { if (epoch === state.sessionEpoch) setAssetBusy(false); }
}

/** 短暂展示已确认的业务操作结果。@param {string} value 纯文本结果。@returns {void} 重置单个提示定时器。 */
function toast(value) {
  clearTimeout(state.toastTimer);
  message("toast", value);
  state.toastTimer = setTimeout(() => message("toast", ""), 3500);
}

/** 按页移动账本或库存。@param {string} kind transactions/assets。@param {number} direction 只取 -1 或 1。@returns {void} 在合法边界内加载新分页。 */
function movePage(kind, direction) {
  const page = state[kind];
  if (page.busy) return;
  const next = page.offset + direction * page.limit;
  if (next < 0 || next >= page.total) return;
  page.offset = next;
  void (kind === "assets" ? loadAssets() : loadTransactions());
}

/** 绑定静态控件的公开界面行为，不自动提交写入或注入演示数据。@returns {void} 只注册一次事件监听。 */
function bindEvents() {
  for (const button of document.querySelectorAll("[data-view]")) button.addEventListener("click", () => changeView(button.dataset.view));
  element("session-retry").addEventListener("click", checkSession);
  // 登录只兑换服务器会话；无论成功或失败，认证材料都不进入持久浏览器存储。
  element("token-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = event.currentTarget.querySelector('button[type="submit"]');
    submit.disabled = true;
    message("auth-error", "");
    const token = element("access-token").value;
    try { await request("/web/session", { method: "POST", body: JSON.stringify({ token }) }); element("access-token").value = ""; await checkSession(); }
    catch (error) { message("auth-error", error.message); }
    finally { submit.disabled = false; }
  });
  // 退出必须先让服务器使 Cookie 失效，再清空已渲染的私人记录。
  element("logout").addEventListener("click", async () => {
    element("logout").disabled = true;
    try { await request("/web/session", { method: "DELETE" }); showAuthentication(); await checkSession(); }
    catch (error) { toast(error.message); }
    finally { element("logout").disabled = false; }
  });
  element("filters").addEventListener("submit", (event) => {
    event.preventDefault();
    try { state.transactions.filters = readFilters(); state.transactions.offset = 0; syncPlatformFilters(); message("global-notice", ""); void loadTransactions(); }
    catch (error) { message("global-notice", error.message); }
  });
  // hidden 来源字段的默认值会随选择改变；重置必须同时清空表单值、查询状态和 aria-pressed。
  element("reset-filters").addEventListener("click", () => { element("filters").reset(); element("platform-filter-value").value = ""; state.transactions.filters = {}; state.transactions.offset = 0; syncPlatformFilters(); message("global-notice", ""); void loadTransactions(); });
  element("transaction-page-size").addEventListener("change", (event) => { state.transactions.limit = parseInt(event.target.value, 10); state.transactions.offset = 0; void loadTransactions(); });
  element("transaction-prev").addEventListener("click", () => movePage("transactions", -1));
  element("transaction-next").addEventListener("click", () => movePage("transactions", 1));
  element("asset-prev").addEventListener("click", () => movePage("assets", -1));
  element("asset-next").addEventListener("click", () => movePage("assets", 1));
  element("create-asset").addEventListener("click", () => openAsset());
  element("asset-load-retry").addEventListener("click", () => openAsset(state.editingId));
  element("asset-form").addEventListener("submit", saveAsset);
  element("delete-asset").addEventListener("click", () => { message("delete-error", ""); element("delete-dialog").showModal(); });
  element("confirm-delete").addEventListener("click", deleteAsset);
  for (const button of document.querySelectorAll("[data-close]")) button.addEventListener("click", () => {
    if (button.dataset.close !== "detail-dialog" && state.assetBusy) return;
    element(button.dataset.close).close();
  });
  // 写入中禁止 Escape/遮罩关闭；普通详情可随时关闭，迟到响应通过世代保护忽略。
  for (const dialog of document.querySelectorAll("dialog")) {
    dialog.addEventListener("cancel", (event) => { if (dialog.id !== "detail-dialog" && state.assetBusy) event.preventDefault(); });
    dialog.addEventListener("click", (event) => { if (event.target === dialog && (dialog.id === "detail-dialog" || !state.assetBusy)) { const bounds = dialog.getBoundingClientRect(); if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close(); } });
  }
  element("detail-dialog").addEventListener("close", () => { state.detailRequest += 1; });
}

element("today").textContent = new Intl.DateTimeFormat("zh-CN", { timeZone: "Asia/Shanghai", year: "numeric", month: "long", day: "numeric" }).format(new Date());
renderPlatformFilters();
bindEvents();
void checkSession();

// 待核分页仅存非敏感偏移量；记录与证据不写浏览器持久存储。
let importReviewOffset = 0;

/** 读取同一 REST 服务的待核及来源范围。@param {number} offset 待核页偏移，非负。@returns {Promise<void>} 重新渲染；失败保留错误提示，绝不自动处理事项。 */
async function loadImportReviews(offset = 0) {
  const epoch = state.sessionEpoch;
  try {
    const [page, ranges] = await Promise.all([request(`/api/v1/imports/reviews?limit=20&offset=${offset}`), request('/api/v1/imports/ranges')]);
    if (!state.authenticated || state.sessionEpoch !== epoch) return;
    importReviewOffset = offset;
    element('source-ranges').replaceChildren(...ranges.map(value => node('p', 'muted', `${value.source} ${value.account || '账户未标注'}：${value.count} 笔，${value.first} 至 ${value.last}；完整覆盖未确认`)));
    element('review-items').replaceChildren(...page.items.map(renderImportReview));
    element('review-previous').disabled = offset === 0;
    element('review-next').disabled = offset + page.items.length >= page.total;
    element('review-notice').textContent = `待核 ${page.total} 项；当前 ${offset + (page.items.length ? 1 : 0)}–${offset + page.items.length}`;
  } catch (error) { element('review-notice').textContent = error.message; }
}

/** 将一个待核来源渲染成人工编辑表单。@param {object} review 服务端待核记录及不可变来源证据。@returns {HTMLElement} 未挂载的安全文本节点，不解释业务值为 HTML。 */
function renderImportReview(review) {
  const details = node('details', 'filter-panel');
  details.append(node('summary', '', `${review.kind} · ${review.source} · ${review.record.merchant || '未识别商户'} · ${review.record.amount || '金额缺失'}`));
  const evidence = node('pre', '', JSON.stringify(review.record.original, null, 2));
  evidence.style.whiteSpace = 'pre-wrap';
  details.append(evidence);
  const form = node('form');
  const actionLabel = node('label', 'field', '处理方式');
  const action = node('select');
  for (const [value, label] of [['link', '同一笔交易，关联证据'], ['create', '确认独立交易并新增'], ['correct', '更正历史交易'], ['dismiss', '忽略此项并保留记录']]) {
    const option = node('option', '', label); option.value = value; action.append(option);
  }
  actionLabel.append(action); form.append(actionLabel);
  const fields = {};
  for (const [key, label, value] of [
    ['reason', '人工核对理由', ''], ['transaction_id', '关联交易 ID', review.transaction_id || ''],
    ['occurred_at', '交易时间（含时区）', review.record.occurred_at || ''], ['amount', '金额', review.record.amount || ''],
    ['direction', '方向（income / expense）', review.record.direction || ''], ['currency', '币种', review.record.currency || 'CNY'],
    ['merchant', '商户', review.record.merchant || ''], ['description', '摘要', review.record.description || '']
  ]) {
    const labelNode = node('label', 'field', label); const input = node('input'); input.value = value; input.name = key;
    if (key === 'reason') input.required = true;
    labelNode.append(input); form.append(labelNode); fields[key] = input;
  }
  const confirmation = node('label', 'field', '我已核对来源证据及修改内容');
  const checked = node('input'); checked.type = 'checkbox'; checked.required = true; confirmation.append(checked); form.append(confirmation);
  const submit = node('button', 'button primary', '确认处理'); submit.type = 'submit'; form.append(submit);
  // 仅在明确提交时写入；服务端负责并发、重放和前后版本记录。
  form.addEventListener('submit', async event => {
    event.preventDefault(); submit.disabled = true;
    try {
      const decision = {action: action.value, reason: fields.reason.value};
      if (fields.transaction_id.value) decision.transaction_id = fields.transaction_id.value;
      if (action.value === 'create' || action.value === 'correct') {
        decision.record = {...review.record, original: {...review.record.original, pam_human_reviewed: true}};
        for (const key of ['occurred_at', 'amount', 'direction', 'currency', 'merchant', 'description']) decision.record[key] = fields[key].value;
      }
      await request(`/api/v1/imports/reviews/${review.id}/resolve`, {method: 'POST', body: JSON.stringify(decision)});
      await loadImportReviews(importReviewOffset); await loadTransactions();
    } catch (error) { element('review-notice').textContent = error.message; }
    finally { submit.disabled = false; }
  });
  details.append(form); return details;
}

// 点击展开或刷新才读取私人待核；初始化不请求未登录数据。
element('review-refresh').addEventListener('click', () => loadImportReviews(0));
element('review-previous').addEventListener('click', () => loadImportReviews(Math.max(0, importReviewOffset - 20)));
element('review-next').addEventListener('click', () => loadImportReviews(importReviewOffset + 20));
element('import-review-panel').addEventListener('toggle', () => { if (element('import-review-panel').open && state.authenticated) void loadImportReviews(0); });
