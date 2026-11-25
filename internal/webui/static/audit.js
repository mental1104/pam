/** 审计中心只读界面：全部图表使用同一服务端快照，原始 SQL 与参数仅经认证读取，无外部图表库或自动轮询。 */
const results = {success: {label: '成功', tone: 'success'}, rejected: {label: '策略拒绝', tone: 'rejected'}, error: {label: '执行错误', tone: 'error'}, running: {label: '未完成', tone: 'running'}};
const counts = new Intl.NumberFormat('zh-CN');
const clock = new Intl.DateTimeFormat('zh-CN', {timeZone:'Asia/Shanghai', month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', hour12:false});
const errorNotes = { '42703':'字段不存在', '57014':'查询超时或被取消', function_access:'函数未开放', operator_access:'运算符未开放', relation_access:'数据表未开放', select_side_effect:'查询包含副作用', sql_node:'查询结构未开放', statement_count:'一次提交了多条语句', statement_kind:'仅允许只读 SELECT', type_access:'类型转换未开放', parameter_type:'参数类型不受支持', parameter_count:'参数数量超限', sql_parse:'SQL 无法解析', audit_unavailable:'审计存储不可用', query_failed:'查询执行失败' };

/** 读取固定 id 的元素。@param {string} id 静态 DOM 标识。@returns {HTMLElement} 存在的节点；模板缺失立即失败。 */
function el(id) { const value = document.getElementById(id); if (!value) throw new Error('Missing audit element'); return value; }
/** 创建纯文本 DOM。@param {string} tag 标签。@param {string} style 样式类。@param {string} text 文本，不解释为 HTML。@returns {HTMLElement} 新节点。 */
function make(tag, style = '', text = '') { const value = document.createElement(tag); value.className = style; value.textContent = text; return value; }
/** 格式化非财务耗时。@param {number|null} value 毫秒；null 表示没有可计算的样本。@returns {string} 可见单位文本。 */
function duration(value) { return value === null ? '—' : value >= 1000 ? `${(value / 1000).toFixed(2)} s` : `${counts.format(value)} ms`; }
/** 格式化时刻。@param {string} value RFC3339。@returns {string} 固定北京时间；非法值显示破折号。 */
function stamp(value) { const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : clock.format(date); }
/** 解释稳定错误码。@param {string} code SQLSTATE 或应用策略码。@returns {string} 不猜测数据库错误原文。 */
function reason(code) { return errorNotes[code] || '未归类原因，以错误码为准'; }
/** 创建 SVG 节点及白名单属性。@param {string} tag SVG 标签。@param {object} attributes 几何或辅助属性。@returns {SVGElement} 新节点。 */
function svgNode(tag, attributes) { const value = document.createElementNS('http://www.w3.org/2000/svg', tag); for (const [key, item] of Object.entries(attributes)) value.setAttribute(key, String(item)); return value; }

/** AuditCenter 管理审计视图筛选、分页及并发请求；request 复用应用的同源认证传输，不保存浏览器凭据。 */
export class AuditCenter {
  /** constructor 绑定静态控件；request 为返回 JSON 的异步 HTTP 函数，不在构造时请求私人数据。 */
  constructor(request) {
    this.request = request; this.version = 0; this.offset = 0; this.filters = {}; this.selected = null; this.controller = null;
    el('audit-filters').addEventListener('submit', event => { event.preventDefault(); this.applyForm(); });
    el('audit-period').addEventListener('change', () => { const custom = el('audit-period').value === 'custom'; el('audit-from-field').hidden = !custom; el('audit-to-field').hidden = !custom; });
    el('audit-refresh').addEventListener('click', () => this.load());
    el('audit-reset').addEventListener('click', () => this.reset());
    el('audit-sort').addEventListener('change', () => { this.offset = 0; this.load(); });
    el('audit-prev').addEventListener('click', () => { this.offset = Math.max(0, this.offset - 20); this.load(); });
    el('audit-next').addEventListener('click', () => { this.offset += 20; this.load(); });
    el('audit-detail-close').addEventListener('click', () => el('audit-detail').close());
    el('audit-detail').addEventListener('click', event => { if (event.target === el('audit-detail')) { const box = event.target.getBoundingClientRect(); if (event.clientX < box.left || event.clientX > box.right) event.target.close(); } });
    el('audit-same-pattern').addEventListener('click', () => { if (this.selected) { this.drill({fingerprint:this.selected.fingerprint}); el('audit-detail').close(); } });
    el('audit-copy').addEventListener('click', async () => { if (!this.selected) return; try { await navigator.clipboard.writeText(this.selected.raw_sql ?? this.selected.sql); el('audit-copy').textContent = '已复制'; } catch { el('audit-copy').textContent = '请选中 SQL 手动复制'; } });
  }

  /** clear 取消未完成请求并删除全部私人快照；退出登录后迟到响应不能重新填回页面。@returns {void} 清空状态和 DOM。 */
  clear() {
    this.version++; this.controller?.abort(); this.selected = null; this.filters = {}; this.offset = 0;
    for (const id of ['audit-metrics','audit-timeline','audit-axis','audit-legend','audit-donut','audit-status-legend','audit-latency','audit-errors','audit-patterns','audit-records','audit-detail-body','audit-chips','audit-average','audit-pattern-count','audit-record-count','audit-page-note','audit-timeline-note','audit-bucket-readout']) el(id).replaceChildren();
    el('audit-content').hidden = true; el('audit-message').textContent = ''; el('audit-snapshot').textContent = '尚未读取';
    el('audit-filters').reset(); el('audit-sort').value = 'recent'; el('audit-from-field').hidden = true; el('audit-to-field').hidden = true;
    el('audit-detail').close();
  }

  /** reset 清除所有筛选并恢复近七天，重新读取真实数据。@returns {void} 不修改审计或账本。 */
  reset() { this.filters = {}; this.offset = 0; el('audit-filters').reset(); el('audit-from-field').hidden = true; el('audit-to-field').hidden = true; el('audit-sort').value = 'recent'; this.load(); }

  /** applyForm 保存显式筛选，保留模式/原因下钻条件并退出时间桶下钻。@returns {void} 非法日期先提示，不发请求。 */
  applyForm() {
    delete this.filters.from; delete this.filters.to;
    this.filters.status = el('audit-status').value;
    this.filters.search = el('audit-search').value.trim();
    this.filters.min_duration_ms = el('audit-slow').checked ? '1000' : '';
    this.offset = 0; this.load();
  }

  /** drill 合并 change 并从首页查询；空字符串表示撤销条件。@param {object} change 固定筛选字段到文本值。@returns {void} 更新可见控件与服务端结果。 */
  drill(change) {
    Object.assign(this.filters, change); this.offset = 0;
    el('audit-status').value = this.filters.status || '';
    el('audit-search').value = this.filters.search || '';
    el('audit-slow').checked = this.filters.min_duration_ms === '1000';
    this.load();
  }

  /** query 从当前表单及下钻生成 URL 参数。@returns {URLSearchParams} 新参数集；自定义日期不完整或倒置时抛说明。 */
  query() {
    const params = new URLSearchParams({limit:'20', offset:String(this.offset), sort:el('audit-sort').value});
    const period = el('audit-period').value;
    if (period !== 'all') {
      if (period === 'custom') {
        const start = el('audit-from').value, last = el('audit-to').value;
        if (!start || !last || start > last) throw new Error('请选择有效的开始和结束日期。');
        const end = new Date(`${last}T00:00:00+08:00`); end.setUTCDate(end.getUTCDate() + 1);
        params.set('from', `${start}T00:00:00+08:00`); params.set('to', end.toISOString());
      } else { params.set('from', new Date(Date.now() - Number(period) * 86400000).toISOString()); }
    }
    for (const [key, value] of Object.entries(this.filters)) if (value !== '') params.set(key, value);
    return params;
  }

  /** load 获取一致快照；显式取消旧请求并校验版本，失败显示重试，绝不以演示数填补。@returns {Promise<void>} 页面进入成功或错误状态。 */
  async load() {
    const version = ++this.version; this.controller?.abort(); this.controller = new AbortController();
    el('audit-view').setAttribute('aria-busy','true'); el('audit-message').textContent = '正在读取审计快照…';
    try {
      const params = this.query();
      const data = await this.request(`/api/v1/audit?${params}`, {signal:this.controller.signal});
      if (version !== this.version) return;
      this.render(data); el('audit-message').replaceChildren();
      if (!data.summary.total) el('audit-message').textContent = '此范围内没有审计记录。可以扩大时间范围或重置筛选。';
    } catch (error) {
      if (version !== this.version) return;
      el('audit-content').hidden = true;
      const retry = make('button','button secondary','重新读取'); retry.type = 'button'; retry.addEventListener('click', () => this.load());
      el('audit-message').replaceChildren(make('span','',error.message), retry);
    } finally { if (version === this.version) el('audit-view').setAttribute('aria-busy','false'); }
  }

  /** render 将 data 一致快照绘制为指标、图表及明细。@param {object} data 已认证 REST 数据。@returns {void} 不保存在 localStorage。 */
  render(data) {
    el('audit-content').hidden = false;
    el('audit-snapshot').textContent = `快照 ${stamp(data.generated_at)} · 北京时间`;
    const s = data.summary, terminal = s.total - s.running;
    const metrics = [
      ['调用总数', counts.format(s.total), `当前筛选 · ${s.running ? `${s.running} 条未完成` : '均已记录终态'}`, 'total'],
      ['成功率', terminal ? `${(s.success / terminal * 100).toFixed(1)}%` : '—', `${counts.format(s.success)} 次成功 / ${counts.format(terminal)} 次已完成`, 'success'],
      ['执行错误', counts.format(s.errors), `另有 ${counts.format(s.rejected)} 次策略拒绝`, 'error'],
      ['P95 总耗时', duration(s.p95_ms), `${counts.format(s.slow)} 次慢查询 ≥ 1 秒`, 'latency']
    ];
    el('audit-metrics').replaceChildren(...metrics.map(([label,value,note,tone]) => { const card = make('article',`audit-metric ${tone}`); card.append(make('span','audit-metric-label',label),make('strong','',value),make('small','',note)); return card; }));
    const chips=[];
    const labels={status:'结果',search:'SQL',error_code:'原因',fingerprint:'模式',min_duration_ms:'耗时',from:'时段起点',to:'时段终点',source:'调用来源'};
    for (const [key,value] of Object.entries(this.filters)) if (value) {
      const text = key === 'status' ? results[value]?.label : key === 'fingerprint' ? value.slice(0,10) : key === 'from' || key === 'to' ? stamp(value) : key === 'min_duration_ms' ? `≥ ${duration(Number(value))}` : value;
      const chip = make('button','audit-chip',`${labels[key] || key}：${text} ×`); chip.type='button'; chip.addEventListener('click',()=>{ this.drill({[key]:''}); }); chips.push(chip);
    }
    el('audit-chips').replaceChildren(...chips);
    this.timeline(data); this.donut(s); this.latency(data); this.errors(data); this.patterns(data); this.records(data);
  }

  /** timeline 绘制真实零填充的堆叠时间柱，点击按服务端桶边界下钻。@param {object} data 包含 timeline 和 bucket_seconds。@returns {void} 键盘和鼠标均可操作。 */
  timeline(data) {
    el('audit-timeline-note').textContent = `${stamp(data.from)} — ${stamp(data.to)} · 每 ${data.bucket_seconds / 3600} 小时`;
    const legend = Object.entries(results).map(([status,item]) => { const button = make('button',`audit-legend-item ${item.tone}`,item.label); button.type='button'; button.addEventListener('click',()=>this.drill({status})); return button; });
    el('audit-legend').replaceChildren(...legend);
    const total = b => b.success + b.rejected + b.errors + b.running;
    const peak = Math.max(1,...data.timeline.map(total));
    const bars = data.timeline.map(bucket => {
      const label = `${stamp(bucket.start)} 至 ${stamp(bucket.end)}：${total(bucket)} 次，成功 ${bucket.success}，拒绝 ${bucket.rejected}，错误 ${bucket.errors}，未完成 ${bucket.running}`;
      const button = make('button','audit-time-bar'); button.type='button'; button.title=label; button.setAttribute('aria-label',label);
      for (const [key,field] of [['success','success'],['rejected','rejected'],['error','errors'],['running','running']]) { const segment=make('span',`audit-bar-segment ${key}`); segment.style.height=`${bucket[field] / peak * 100}%`; button.append(segment); }
      button.addEventListener('mouseenter',()=>{el('audit-bucket-readout').textContent=label;}); button.addEventListener('focus',()=>{el('audit-bucket-readout').textContent=label;});
      button.addEventListener('click',()=>this.drill({from:bucket.start,to:bucket.end})); return button;
    });
    el('audit-timeline').replaceChildren(...bars);
    const points = data.timeline.filter((_,index,array)=>index===0||index===array.length-1||index===Math.floor(array.length/2));
    el('audit-axis').replaceChildren(...points.map(b=>make('span','',stamp(b.start))));
    el('audit-bucket-readout').textContent=`峰值 ${counts.format(peak === 1 && !data.summary.total ? 0 : peak)} 次 / 时段。悬停或聚焦查看详情，点击筛选。`;
  }

  /** donut 用 SVG 绘制状态环及可点选图例；零记录只画空环。@param {object} summary 同筛选计数。@returns {void} 标签和颜色同时表达状态。 */
  donut(summary) {
    const svg = svgNode('svg',{viewBox:'0 0 180 180',role:'img','aria-label':`结果分布，共 ${summary.total} 次调用`});
    svg.append(svgNode('circle',{cx:90,cy:90,r:67,fill:'none',stroke:'#edf1f3','stroke-width':18}));
    const circumference=2*Math.PI*67; let offset=0;
    const legend=[];
    for(const [key,item] of Object.entries(results)) {
      const count=summary[key==='error'?'errors':key]; const fraction=summary.total?count/summary.total:0;
      if(count) { const circle=svgNode('circle',{cx:90,cy:90,r:67,fill:'none','stroke-width':18,'stroke-dasharray':`${fraction*circumference} ${circumference}`,'stroke-dashoffset':-offset,transform:'rotate(-90 90 90)',class:`audit-ring ${key}`,tabindex:0,role:'button','aria-label':`${item.label} ${count} 次，筛选此状态`});
        circle.addEventListener('click',()=>this.drill({status:key})); circle.addEventListener('keydown',event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();this.drill({status:key});}});svg.append(circle);offset+=fraction*circumference;
      }
      const button=make('button',`audit-status-item ${key}`);button.type='button';button.dataset.auditStatus=key;button.append(make('span','',item.label),make('strong','',counts.format(count)),make('small','',`${(fraction*100).toFixed(1)}%`));button.addEventListener('click',()=>this.drill({status:key}));legend.push(button);
    }
    const label=svgNode('text',{x:90,y:87,'text-anchor':'middle',class:'audit-ring-total'});label.textContent=counts.format(summary.total);svg.append(label);
    const caption=svgNode('text',{x:90,y:110,'text-anchor':'middle',class:'audit-ring-caption'});caption.textContent='次调用';svg.append(caption);
    el('audit-donut').replaceChildren(svg);el('audit-status-legend').replaceChildren(...legend);
  }

  /** latency 展示固定区间分布，不混入拒绝或未完成样本。@param {object} data 含 latency 与 summary。@returns {void} 空样本显示零条和未知分位数。 */
  latency(data) {
    el('audit-average').textContent=`均值 ${duration(data.summary.average_ms)} · 最长 ${duration(data.summary.max_ms)}`;
    const labels={'0-100':'< 100 ms','100-500':'100–500 ms','500-1000':'500 ms–1 s','1000-3000':'1–3 s','3000+':'≥ 3 s'};
    const max=Math.max(1,...data.latency.map(item=>item.count));
    el('audit-latency').replaceChildren(...data.latency.map(item=>{const row=make('div','audit-distribution');const track=make('div','audit-track');const fill=make('span',item.key==='1000-3000'||item.key==='3000+'?'slow':'');fill.style.width=`${item.count/max*100}%`;track.append(fill);row.append(make('span','',labels[item.key]),track,make('strong','',counts.format(item.count)));return row;}));
  }

  /** errors 展示稳定代码与解释，点击仅增加 error_code 条件。@param {object} data 含当前筛选原因分组。@returns {void} 不展示数据库原始异常。 */
  errors(data) {
    el('audit-errors').replaceChildren(...data.error_codes.map(item=>{const button=make('button','audit-error-item');button.type='button';const text=make('span');text.append(make('strong','',reason(item.key)),make('small','',item.key));button.append(text,make('b','',counts.format(item.count)));button.addEventListener('click',()=>this.drill({error_code:item.key}));return button;}));
    if(!data.error_codes.length)el('audit-errors').append(make('p','audit-no-issues','当前筛选没有拒绝或错误原因。'));
  }

  /** patterns 展示当前筛选下的前八个脱敏模式。@param {object} data 含模式聚合。@returns {void} 点击保留其他条件并筛选指纹。 */
  patterns(data) {
    el('audit-pattern-count').textContent=`${data.patterns.length} 个模式`;
    el('audit-patterns').replaceChildren(...data.patterns.map((pattern,index)=>{const button=make('button','audit-pattern');button.type='button';button.dataset.fingerprint=pattern.fingerprint;
      const top=make('div','audit-pattern-top');top.append(make('span','audit-pattern-rank',String(index+1).padStart(2,'0')),make('strong','',`${counts.format(pattern.calls)} 次调用`),make('span','',`拒绝 / 错误 ${pattern.failures}`));
      button.append(top,make('code','',pattern.sql),make('small','',`平均 ${duration(pattern.average_ms)} · 最长 ${duration(pattern.max_ms)} · ${pattern.fingerprint.slice(0,12)} ↗`));button.addEventListener('click',()=>this.drill({fingerprint:pattern.fingerprint}));return button;
    }));
    if(!data.patterns.length)el('audit-patterns').append(make('p','audit-no-issues','没有符合条件的 SQL 模式。'));
  }

  /** records 绘制服务端分页及详情入口。@param {object} data 含 records 页和总数。@returns {void} 空页不伪装成空数据库。 */
  records(data) {
    const page=data.records;el('audit-record-count').textContent=counts.format(page.total);
    el('audit-records').replaceChildren(...page.items.map(record=>{const item=make('div');item.setAttribute('role','listitem');const button=make('button','audit-record');button.type='button';button.setAttribute('aria-label',`${stamp(record.executed_at)} ${results[record.status].label}，查看调用详情`);
      button.append(make('time','audit-record-time',stamp(record.executed_at)),make('span',`audit-status-pill ${record.status}`,results[record.status].label),make('code','audit-record-sql',record.raw_sql ?? record.sql),make('span',`audit-record-duration ${record.duration_ms>=1000?'slow':''}`,record.status==='running'?'未完成':duration(record.duration_ms)),make('span','audit-record-rows',record.status==='running'?'—':counts.format(record.row_count)),make('span','audit-record-arrow','↗'));
      button.addEventListener('click',()=>this.detail(record));item.append(button);return item;}));
    if(!page.items.length)el('audit-records').append(make('p','audit-no-issues',page.total?'此页没有记录，请返回上一页。':'没有符合条件的审计记录。'));
    el('audit-page-note').textContent=`${page.total?Math.min(page.offset+1,page.total):0}–${Math.min(page.offset+page.items.length,page.total)} / ${counts.format(page.total)} 条`;
    el('audit-prev').disabled=page.offset===0;el('audit-next').disabled=page.offset+page.items.length>=page.total;
  }

  /** detail 打开 record 的既有审计证据，不再执行 SQL。@param {object} record 当前页记录，含授权保存的原文和参数或历史空值。@returns {void} 对话框关闭后焦点返回调用按钮。 */
  detail(record) {
    this.selected=record;const body=el('audit-detail-body');const status=make('div',`audit-detail-state ${record.status}`,results[record.status].label);
    const grid=make('dl','audit-detail-grid');
    for(const [label,value] of [['执行时间',stamp(record.executed_at)+'（北京时间）'],['总耗时',record.status==='running'?'尚未记录终态':duration(record.duration_ms)],['返回行数',record.status==='running'?'—':counts.format(record.row_count)],['调用来源',record.source],['审计 ID',record.id],['模式指纹',record.fingerprint]])grid.append(make('dt','',label),make('dd','',value));
    const nodes=[status];
    if(record.error_code){const explanation=make('div','audit-explanation');explanation.append(make('strong','',reason(record.error_code)),make('p','',`错误码 ${record.error_code}。这里只展示稳定分类，不保存或回显数据库错误原文。`));nodes.push(explanation);}
    if(record.status==='running')nodes.push(make('p','audit-explanation','未完成表示审计尚无终态；调用可能已中断，不保证仍在执行。'));
    nodes.push(grid,make('h3','',record.raw_sql === null ? '历史 SQL 模式' : '原始 SQL'),make('pre','audit-sql',record.raw_sql ?? record.sql));
    if(record.raw_sql === null)nodes.push(make('p','audit-footnote','这条历史记录只保存了归一化模式，原始 SQL 和绑定参数无法还原。'));
    else {
      nodes.push(make('h3','','绑定参数'),make('pre','audit-sql audit-parameters',record.parameters_json ?? '未保存参数'));
      const pattern=make('details','audit-normalized');pattern.append(make('summary','','查看用于归类的 SQL 模式'),make('pre','audit-sql',record.sql));nodes.push(pattern);
    }
    if(record.raw_truncated)nodes.push(make('p','audit-explanation','本条原文或参数超过采集上限，未完整保存。'));
    nodes.push(make('p','audit-footnote','返回行数是客户端实际收到的行数，不代表数据库扫描行数。'));
    body.replaceChildren(...nodes);el('audit-copy').textContent='复制 SQL';el('audit-detail').showModal();
  }
}
