(function (global, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else api.start(global);
})(typeof window === 'object' ? window : null, function () {
  'use strict';
  const DEFAULT_CONFIG = Object.freeze({ enabled: false, dynamic_proxy_url: '', harvest_dial_proxy_url: '', observe_exit_ip: false, ttl_minutes: 60,
    refresh_before_minutes: 10, max_attempts: 8, attempt_interval_seconds: 10, cooldown_seconds: 300 });
  const NUMBERS = Object.freeze({ ttl_minutes: [1, 60, '票据有效期'], refresh_before_minutes: [0, 59, '提前续期'],
    max_attempts: [1, 32, '每轮最多尝试'], attempt_interval_seconds: [1, 300, '尝试间隔'], cooldown_seconds: [30, 3600, '失败后冷却'] });
  const STATES = Object.freeze({ disabled: ['已关闭', ''], waiting_host: ['等待宿主', 'warning'],
    waiting_account: ['等待账号', 'warning'], queued: ['等待获取', ''], harvesting: ['正在获取', ''],
    ready: ['可用', 'success'], renewing: ['正在续期', ''], cooldown: ['冷却中', 'warning'],
    expired: ['已过期', 'warning'], error: ['获取失败', 'error'] });
  const MODEL_PATTERN = /^gpt-[A-Za-z0-9][A-Za-z0-9._-]{0,94}$/;
  const ERRORS = Object.freeze({ proxy_auth_failed:'代理用户名或密码验证失败', front_proxy_failed:'前置代理连接或 CONNECT 被拒绝', transport_timeout:'代理连接或请求超时', transport_tls_failed:'TLS 连接或证书验证失败', attempts_exhausted: '本轮尝试已用完', identity_unavailable: '暂时无法取得账号授权或业务代理',
    invalid_dynamic_proxy: '动态代理配置无效', harvest_failed: '动态代理获取票据未成功', unexpected_state_length: '票据长度与所选套餐不符',
    identity_changed: '账号授权信息发生变化', fixed_proxy_validation_failed: '票据未通过原业务代理验证',
    ticket_persistence_failed: '票据保存失败', upstream_unauthorized: '上游拒绝授权（401）', upstream_forbidden: '上游拒绝访问（403）',
    upstream_rate_limited: '上游限流（429）', upstream_rejected: '上游拒绝请求', model_mismatch: '返回模型不匹配，正在重新获取票据',
    state_312: '收到 312 状态，正在重新获取票据', model_mismatch_persistence_failed: '返回模型不匹配，票据失效记录保存失败',
    state_312_persistence_failed: '收到 312 状态，票据失效记录保存失败' });
  const MESSAGES = Object.freeze({ 'STATE disabled; requests use the account business proxy': 'STATE 已关闭，请求使用账号原有业务代理。',
    'STATE active only for explicitly enabled account/model pairs': 'STATE 仅对手动开启的账号与模型生效。',
    'waiting for host services': '正在等待宿主服务初始化。' });
  function accountID(value) {
    if (typeof value !== 'number' && typeof value !== 'string') return null;
    if (!/^[1-9]\d*$/.test(String(value).trim())) return null;
    const number = Number(value);
    return Number.isSafeInteger(number) && number > 0 ? number : null;
  }
  function normalizeConfig(input) {
    const source = input && typeof input === 'object' && !Array.isArray(input) ? input : {};
    const config = Object.assign({}, DEFAULT_CONFIG);
    Object.keys(DEFAULT_CONFIG).forEach(function (key) {
      if (source[key] !== undefined) config[key] = source[key];
    });
    config.accounts = Array.isArray(source.accounts) ? source.accounts.map(function (account) {
      return { account_id: account.account_id, enabled: account.enabled === true,
        plan: account.plan || 'pro', models: Array.isArray(account.models) ? account.models.slice() : ['gpt-6-astra'] };
    }) : [];
    return config;
  }
  function validateConfig(config) {
    if (typeof config.enabled !== 'boolean') throw new Error('总开关格式不正确。');
    if (typeof config.observe_exit_ip !== 'boolean') throw new Error('出口 IP 检测开关格式不正确。');
    if (typeof config.harvest_dial_proxy_url !== 'string') throw new Error('前置代理地址格式不正确。');
    if (/[{}]/.test(config.harvest_dial_proxy_url)) throw new Error('前置代理不能使用会话占位符。');
    for (const proxyValue of [config.dynamic_proxy_url, config.harvest_dial_proxy_url]) {
    if (typeof proxyValue !== 'string') throw new Error('动态代理地址格式不正确。');
    if (proxyValue) {
      try {
        if (proxyValue.length > 4096 || /[\r\n\t]/.test(proxyValue)) throw new Error();
        const expanded = proxyValue.replace(/\{(?:random|sid)\}/g, '123456');
        if (/[{}]/.test(expanded)) throw new Error();
        const url = new URL(expanded);
        if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(url.protocol) || !url.hostname || url.search || url.hash || (url.pathname && url.pathname !== '/')) throw new Error();
      } catch (_) { throw new Error('代理须为完整的 HTTP(S) 或 SOCKS5(H) 地址。'); }
    }
    }
    Object.keys(NUMBERS).forEach(function (key) {
      const bounds = NUMBERS[key];
      if (!Number.isInteger(config[key]) || config[key] < bounds[0] || config[key] > bounds[1]) {
        throw new Error(bounds[2] + '须为 ' + bounds[0] + '–' + bounds[1] + ' 之间的整数。');
      }
    });
    if (config.refresh_before_minutes >= config.ttl_minutes) throw new Error('提前续期必须小于票据有效期。');
    if (!Array.isArray(config.accounts) || config.accounts.length > 256) throw new Error('最多配置 256 个账号。');
    const ids = new Set();
    let totalModels = 0;
    config.accounts.forEach(function (account) {
      if (accountID(account.account_id) === null) throw new Error('账号 ID 须为正整数。');
      if (ids.has(account.account_id)) throw new Error('账号 ID ' + account.account_id + ' 重复。');
      ids.add(account.account_id);
      if (typeof account.enabled !== 'boolean') throw new Error('账号开关格式不正确。');
      if (!['pro', 'team'].includes(account.plan)) throw new Error('请选择 Pro 或 Team 套餐。');
      if (!Array.isArray(account.models) || !account.models.length || account.models.length > 16) throw new Error('每个账号须填写 1–16 个模型。');
      totalModels += account.models.length;
      const models = new Set();
      account.models.forEach(function (model) {
        if (typeof model !== 'string' || !MODEL_PATTERN.test(model)) throw new Error('模型须以 gpt- 开头，只能包含字母、数字、点、下划线和连字符，最长 99 个字符。');
        if (models.has(model)) throw new Error('同一账号的模型名称不能重复。');
        models.add(model);
      });
    });
    if (totalModels > 1024) throw new Error('最多配置 1024 个账号与模型组合。');
    if (config.enabled && config.accounts.some(function (account) { return account.enabled; }) && !config.dynamic_proxy_url) {
      throw new Error('启用账号前，请填写动态代理地址。');
    }
    return config;
  }
  function stateLabel(state) { return Object.prototype.hasOwnProperty.call(STATES, state) ? STATES[state] : ['未知状态', 'warning']; }
  function errorLabel(code) { return Object.prototype.hasOwnProperty.call(ERRORS, code) ? ERRORS[code] : '操作未完成，请检查账号与插件设置。'; }
  function redactError(value) {
    return String(value || '')
      .replace(/(?:https?|socks5h?):\/\/[^\s/]*@/gi, '[代理凭据已隐藏]@')
      .replace(/(?:x-codex-turn-state|authorization|access_token|refresh_token|api_key|password)\s*[:=]\s*[^\s,;]+/gi, '[敏感字段已隐藏]')
      .replace(/\beyJ[A-Za-z0-9_-]{15,}(?:\.[A-Za-z0-9_-]+){0,2}/g, '[票据已隐藏]')
      .slice(0, 400);
  }
  function parseStatus(result) {
    let status = result && result.status_json;
    if (typeof status === 'string') {
      try { status = JSON.parse(status); } catch (_) { throw new Error('宿主返回的状态格式不正确。'); }
    }
    if (!status || typeof status !== 'object' || Array.isArray(status)) status = {};
    return { host_ready: status.host_ready === true,
      account_ids: Array.isArray(status.account_ids) ? Array.from(new Set(status.account_ids.map(accountID).filter(function (id) { return id !== null; }))).sort(function (a, b) { return a - b; }) : [],
      tickets: Array.isArray(status.tickets) ? status.tickets.filter(function (ticket) { return ticket && accountID(ticket.account_id) !== null; }).slice(0, 4096) : [],
      events: Array.isArray(status.events) ? status.events.slice(-200) : [],
      message: redactError(MESSAGES[status.message] || status.message || result && result.message || '') };
  }
  function remainingText(seconds) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return '—';
    const minutes = Math.floor(value / 60);
    return minutes ? minutes + ' 分 ' + Math.floor(value % 60) + ' 秒' : Math.floor(value) + ' 秒';
  }

  function start(global) {
    const document = global.document;
    const bridge = global.Sub2APIPluginBridge;
    const byID = function (id) { return document.getElementById(id); };
    let loaded = false;
    let busy = false;
    let dirty = false;
    let statusBusy = false;
    let closed = false;
    let pollTimer;
    let resizeObserver;
    let accounts = [];
    const numberIDs = { ttl_minutes: 'ttl-minutes', refresh_before_minutes: 'refresh-before-minutes',
      max_attempts: 'max-attempts', attempt_interval_seconds: 'attempt-interval-seconds', cooldown_seconds: 'cooldown-seconds' };
    function element(tag, text, className) {
      const node = document.createElement(tag);
      if (text !== undefined) node.textContent = String(text);
      if (className) node.className = className;
      return node;
    }
    function notice(message, kind) {
      const node = byID('notice');
      node.textContent = redactError(message);
      node.className = 'notice' + (kind ? ' ' + kind : '');
      node.hidden = !message;
    }
    function updateSaveState(text) {
      byID('save-state').textContent = text || (dirty ? '有未保存修改' : '配置已加载');
      byID('save-state').className = dirty ? 'dirty' : 'muted';
    }
    function markDirty() { if (loaded) { dirty = true; updateSaveState(); } }
    function setBusy(value) {
      busy = value;
      byID('config-fields').disabled = !loaded || busy;
      byID('save-config').disabled = !loaded || busy;
      byID('test-config').disabled = !loaded || busy;
    }
    function renderAccounts() {
      const body = byID('accounts-body');
      body.replaceChildren();
      accounts.forEach(function (account, index) {
        const row = element('tr');
        row.appendChild(element('td', account.account_id, 'account-id'));
        const enabledCell = element('td');
        const enabled = element('input');
        enabled.type = 'checkbox'; enabled.checked = account.enabled === true;
        enabled.setAttribute('aria-label', '启用账号 ' + account.account_id);
        enabled.addEventListener('change', function () { account.enabled = enabled.checked; markDirty(); });
        enabledCell.appendChild(enabled); row.appendChild(enabledCell);
        const planCell = element('td');
        const plan = element('select'); plan.setAttribute('aria-label', '账号 ' + account.account_id + ' 的套餐');
        [['pro', 'Pro · 292'], ['team', 'Team · 332']].forEach(function (entry) {
          const option = element('option', entry[1]); option.value = entry[0]; plan.appendChild(option);
        });
        plan.value = account.plan;
        plan.addEventListener('change', function () { account.plan = plan.value; markDirty(); });
        planCell.appendChild(plan); row.appendChild(planCell);
        const modelsCell = element('td');
        const models = element('input'); models.type = 'text'; models.value = account.models.join(', '); models.spellcheck = false;
        models.autocomplete = 'off'; models.setAttribute('aria-label', '账号 ' + account.account_id + ' 的模型');
        models.addEventListener('input', function () { account.models = models.value.split(',').map(function (v) { return v.trim(); }).filter(Boolean); markDirty(); });
        modelsCell.appendChild(models); row.appendChild(modelsCell);
        const deleteCell = element('td'); const remove = element('button', '删除', 'delete-button'); remove.type = 'button';
        remove.setAttribute('aria-label', '删除账号 ' + account.account_id + ' 的插件配置');
        remove.addEventListener('click', function () { accounts.splice(index, 1); renderAccounts(); markDirty(); });
        deleteCell.appendChild(remove); row.appendChild(deleteCell); body.appendChild(row);
      });
      byID('accounts-empty').hidden = accounts.length !== 0;
      byID('account-count').textContent = accounts.length + ' 个账号';
    }
    function applyConfig(input) {
      const config = normalizeConfig(input);
      byID('enabled').checked = config.enabled === true;
      byID('dynamic-proxy-url').value = config.dynamic_proxy_url;
      byID('harvest-dial-proxy-url').value = config.harvest_dial_proxy_url;
      byID('observe-exit-ip').checked = config.observe_exit_ip;
      Object.keys(numberIDs).forEach(function (key) { byID(numberIDs[key]).value = config[key]; });
      accounts = config.accounts;
      renderAccounts();
      dirty = false;
      updateSaveState();
    }
    function formConfig() {
      const config = { enabled: byID('enabled').checked, dynamic_proxy_url: byID('dynamic-proxy-url').value.trim(), harvest_dial_proxy_url: byID('harvest-dial-proxy-url').value.trim(), observe_exit_ip: byID('observe-exit-ip').checked };
      Object.keys(numberIDs).forEach(function (key) {
        const raw = byID(numberIDs[key]).value.trim();
        config[key] = raw === '' ? NaN : Number(raw);
      });
      config.accounts = accounts.map(function (account) { return {
        account_id: account.account_id, enabled: account.enabled, plan: account.plan, models: account.models.slice()
      }; });
      return validateConfig(config);
    }
    function renderStatus(status) {
      const connection = byID('connection-status');
      connection.textContent = status.host_ready ? '宿主已连接' : '等待宿主初始化';
      connection.className = 'badge ' + (status.host_ready ? 'success' : 'warning');
      byID('status-summary').textContent = status.message || (status.host_ready ? '状态已更新' : '等待宿主提供账号信息；可先保存配置。');
      const options = byID('detected-accounts'); options.replaceChildren();
      status.account_ids.forEach(function (id) { const option = element('option'); option.value = id; options.appendChild(option); });
      byID('account-discovery').textContent = status.account_ids.length ? '发现 ' + status.account_ids.length + ' 个账号 ID。宿主不提供账号名称，请在账号页核对。' : '暂未发现账号 ID，也可以手动填写。宿主不会向此页面提供账号 Token。';
      const body = byID('tickets-body'); body.replaceChildren();
      status.tickets.forEach(function (ticket) {
        const row = element('tr'); const account = element('td', accountID(ticket.account_id));
        const model = typeof ticket.model === 'string' && MODEL_PATTERN.test(ticket.model) ? ticket.model : '未知模型';
        account.appendChild(element('span', model, 'status-model')); row.appendChild(account);
        row.appendChild(element('td', ticket.plan === 'team' ? 'Team · 332' : ticket.plan === 'pro' ? 'Pro · 292' : '—'));
        const state = stateLabel(ticket.state); const stateCell = element('td');
        stateCell.appendChild(element('span', state[0], 'badge ' + state[1])); row.appendChild(stateCell);
        const remaining = element('td', remainingText(ticket.remaining_seconds));
        if (typeof ticket.expires_at === 'string' && Number.isFinite(Date.parse(ticket.expires_at))) remaining.title = '到期时间：' + new Date(ticket.expires_at).toLocaleString('zh-CN');
        row.appendChild(remaining);
        const attempts = Number.isSafeInteger(ticket.attempts) && ticket.attempts > 0 ? '本轮尝试 ' + ticket.attempts + ' 次' : '—';
        const detail = element('td', attempts, 'error-detail');
        if (ticket.last_error) detail.appendChild(element('div', errorLabel(ticket.last_error)));
        row.appendChild(detail); body.appendChild(row);
      });
      byID('tickets-empty').hidden = status.tickets.length !== 0;
      const logs = byID('activity-body'); logs.replaceChildren();
      status.events.slice().reverse().forEach(function (entry) {
        if (!entry || accountID(entry.account_id) === null) return;
        const row = element('tr');
        const time = Number.isFinite(Date.parse(entry.time)) ? new Date(entry.time).toLocaleTimeString('zh-CN') : '—';
        row.appendChild(element('td', time + ' / ' + accountID(entry.account_id)));
        const phase = { harvest: '动态采集', validate: '业务出口复验', restore: '恢复复验', collection: '采集轮次', watchdog: '异常守护' }[entry.phase] || '—';
        row.appendChild(element('td', phase + (entry.chained ? ' · 前置代理' : '') + (Number.isSafeInteger(entry.attempt) && entry.attempt > 0 ? ' · 第 ' + entry.attempt + ' 次' : '')));
        const ip = typeof entry.exit_ip === 'string' && /^[0-9a-fA-F:.]{2,45}$/.test(entry.exit_ip) ? entry.exit_ip : '—';
        row.appendChild(element('td', ip));
        const result = { started: '开始', ip_observed: '已检测出口', ip_check_failed: '出口检测失败，继续模型探测', model_matched: '返回模型匹配', model_mismatch: '返回模型不匹配', incomplete_response: '响应未完整结束', transport_failed: '代理连接或传输失败', transport_unavailable: '代理配置不可用', ready: '票据可用', cancelled: '已取消' }[entry.result] || errorLabel(entry.result);
        const cell = element('td', result);
        const details = [];
        if (Number.isSafeInteger(entry.http_status) && entry.http_status > 0) details.push('HTTP ' + entry.http_status);
        if (typeof entry.actual_model === 'string' && MODEL_PATTERN.test(entry.actual_model)) details.push(entry.actual_model);
        if (Number.isSafeInteger(entry.state_bytes) && entry.state_bytes >= 0) details.push('STATE ' + entry.state_bytes + ' 字节');
        if (Number.isSafeInteger(entry.duration_ms) && entry.duration_ms >= 0) details.push((entry.duration_ms / 1000).toFixed(1) + ' 秒');
        cell.appendChild(element('div', details.join(' · '), 'muted')); row.appendChild(cell); logs.appendChild(row);
      });
      byID('activity-empty').hidden = logs.children.length !== 0;
    }
    async function refreshStatus() {
      if (closed || statusBusy || !bridge) return;
      statusBusy = true; byID('refresh-status').disabled = true;
      try {
        const response = await bridge.status();
        if (!closed) renderStatus(parseStatus(response.result));
      } catch (error) {
        if (!closed) {
          byID('connection-status').textContent = '状态暂不可用';
          byID('connection-status').className = 'badge warning';
          byID('status-summary').textContent = redactError(error.message);
        }
      } finally { statusBusy = false; if (!closed) byID('refresh-status').disabled = false; }
    }
    byID('config-form').addEventListener('input', markDirty);
    byID('config-form').addEventListener('change', markDirty);
    async function saveConfig(event) {
      event.preventDefault(); if (busy || !loaded) return;
      let config;
      try { config = formConfig(); } catch (error) { notice(error.message, 'error'); return; }
      setBusy(true); updateSaveState('正在保存…');
      try {
        const response = await bridge.save(config);
        if (closed) return;
        applyConfig(response.config); updateSaveState('已保存');
        notice(config.enabled ? '设置已保存。仅开启的账号会参与票据获取与注入。' : '设置已保存。STATE Kit 已关闭，正常请求继续转发。', 'success');
        await refreshStatus();
      } catch (error) { if (!closed) { notice(error.message, 'error'); updateSaveState('保存未确认；重新打开配置页可核对宿主结果。'); } }
      finally { if (!closed) setBusy(false); }
    }
    // The host iframe does not grant allow-forms: save via Bridge on an explicit
    // button click instead of relying on sandbox-blocked native form submission.
    byID('save-config').addEventListener('click', saveConfig);
    byID('config-form').addEventListener('submit', saveConfig);
    byID('add-account').addEventListener('click', function () {
      const id = accountID(byID('new-account-id').value);
      if (id === null) { notice('请输入有效的正整数账号 ID。', 'error'); return; }
      if (accounts.some(function (account) { return account.account_id === id; })) { notice('此账号已在列表中。', 'error'); return; }
      if (accounts.length >= 256) { notice('最多配置 256 个账号。', 'error'); return; }
      accounts.push({ account_id: id, enabled: false, plan: 'pro', models: ['gpt-6-astra'] });
      renderAccounts(); markDirty(); byID('new-account-id').value = ''; notice('已添加账号 ' + id + '，默认关闭。选择套餐和模型后，可手动开启并保存。');
    });
    byID('new-account-id').addEventListener('keydown', function (event) {
      if (event.key === 'Enter') { event.preventDefault(); byID('add-account').click(); }
    });
    byID('toggle-proxy').addEventListener('click', function () {
      const input = byID('dynamic-proxy-url'); const reveal = input.type === 'password';
      input.type = reveal ? 'text' : 'password'; byID('toggle-proxy').textContent = reveal ? '隐藏' : '显示';
      byID('toggle-proxy').setAttribute('aria-pressed', String(reveal));
    });
    byID('toggle-front-proxy').addEventListener('click', function () {
      const input = byID('harvest-dial-proxy-url'); const reveal = input.type === 'password';
      input.type = reveal ? 'text' : 'password'; byID('toggle-front-proxy').textContent = reveal ? '隐藏' : '显示';
      byID('toggle-front-proxy').setAttribute('aria-pressed', String(reveal));
    });
    byID('test-config').addEventListener('click', async function () {
      if (busy || !loaded) return;
      setBusy(true); notice('正在检查已保存配置；未保存修改不参与检查。');
      try {
        const response = await bridge.test();
        if (!closed) notice((response.result && response.result.message || '已保存配置检查完成。') + (dirty ? ' 当前表单还有未保存修改。' : ''), 'success');
      } catch (error) { if (!closed) notice(error.message, 'error'); }
      finally { if (!closed) setBusy(false); }
    });
    byID('refresh-status').addEventListener('click', refreshStatus);
    function resize() { try { bridge.resize(document.documentElement.scrollHeight); } catch (_) { /* Context may already be closed. */ } }
    function stop() {
      if (closed) return;
      closed = true; global.clearInterval(pollTimer);
      if (resizeObserver) resizeObserver.disconnect();
      if (bridge) bridge.dispose();
      global.removeEventListener('pagehide', stop);
    }
    global.addEventListener('pagehide', stop);
    (async function () {
      try {
        if (!bridge) throw new Error('配置桥接未加载，请重新打开插件配置页。');
        bridge.ready();
        const response = await bridge.load();
        if (closed) return;
        applyConfig(response.config); loaded = true; setBusy(false); resize();
        if (global.ResizeObserver) { resizeObserver = new global.ResizeObserver(resize); resizeObserver.observe(document.body); }
        await refreshStatus();
        if (!closed) pollTimer = global.setInterval(function () { if (document.visibilityState !== 'hidden') refreshStatus(); }, 5000);
      } catch (error) { if (!closed) { notice(error.message, 'error'); updateSaveState('配置未加载'); byID('connection-status').textContent = '连接失败'; } }
    })();
    return { stop: stop, refreshStatus: refreshStatus };
  }
  return { DEFAULT_CONFIG: DEFAULT_CONFIG, normalizeConfig: normalizeConfig, validateConfig: validateConfig,
    accountID: accountID, parseStatus: parseStatus, stateLabel: stateLabel, errorLabel: errorLabel, redactError: redactError, remainingText: remainingText, start: start };
});
