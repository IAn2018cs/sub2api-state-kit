'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const ui = require('../ui/assets/app.js');

function configured(overrides = {}) {
  return { ...ui.DEFAULT_CONFIG, accounts: [{ account_id: 7, enabled: false, plan: 'pro', models: ['gpt-6-astra'] }], ...overrides };
}

test('empty configuration and newly imported accounts default off', () => {
  assert.deepEqual(ui.normalizeConfig({}), { ...ui.DEFAULT_CONFIG, accounts: [] });
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7 }] }).accounts[0].enabled, false);
  assert.equal(ui.normalizeConfig({ accounts: [{ account_id: 7 }] }).accounts[0].plan, 'pro');
  assert.equal(ui.validateConfig(configured()).enabled, false);
});

test('requires dynamic proxy only when global and account switches are both on', () => {
  assert.doesNotThrow(() => ui.validateConfig(configured({ enabled: true })));
  const config = configured({ enabled: true }); config.accounts[0].enabled = true;
  assert.throws(() => ui.validateConfig(config), /填写动态代理/);
  config.dynamic_proxy_url = 'socks5h://user-sid-{random}:placeholder@proxy.example:1080';
  assert.equal(ui.validateConfig(config), config);
  config.dynamic_proxy_url = 'javascript:alert(1)';
  assert.throws(() => ui.validateConfig(config), /HTTP/);
});

test('validates bounds, renewal horizon, duplicate accounts and model allowlist', () => {
  assert.throws(() => ui.validateConfig(configured({ max_attempts: 33 })), /1–32/);
  assert.throws(() => ui.validateConfig(configured({ ttl_minutes: 61 })), /1–60/);
  assert.throws(() => ui.validateConfig(configured({ cooldown_seconds: 29 })), /30–3600/);
  assert.throws(() => ui.validateConfig(configured({ ttl_minutes: 10, refresh_before_minutes: 10 })), /必须小于/);
  const config = configured(); config.accounts.push({ ...config.accounts[0] });
  assert.throws(() => ui.validateConfig(config), /重复/);
  config.accounts.pop(); config.accounts[0].models = ['gpt-6-astra', 'gpt-6-astra'];
  assert.throws(() => ui.validateConfig(config), /不能重复/);
  config.accounts[0].models = ['<img src=x onerror=alert(1)>'];
  assert.throws(() => ui.validateConfig(config), /模型须以/);
  config.accounts[0].models = ['gpt-6-astra']; config.accounts[0].plan = 'team';
  assert.doesNotThrow(() => ui.validateConfig(config));
});

test('status tolerates pre-initialization, de-duplicates safe IDs, never labels unknown state as raw text', () => {
  assert.deepEqual(ui.parseStatus({ healthy: true }), { host_ready: false, account_ids: [], tickets: [], events: [], message: '' });
  const status = ui.parseStatus({ status_json: JSON.stringify({ host_ready: true, account_ids: [8, 2, 8, null, -1, '9', '9007199254740992'], tickets: [] }) });
  assert.deepEqual(status.account_ids, [2, 8, 9]);
  assert.deepEqual(ui.stateLabel('raw-sensitive-ticket-content'), ['未知状态', 'warning']);
  assert.deepEqual(ui.stateLabel('ready'), ['可用', 'success']);
  assert.equal(ui.errorLabel('unknown-raw-ticket'), '操作未完成，请检查账号与插件设置。');
  assert.equal(ui.errorLabel('upstream_rate_limited'), '上游限流（429）');
  assert.throws(() => ui.parseStatus({ status_json: 'broken{' }), /格式不正确/);
});

test('redacts credentials and common ticket fields from error display', () => {
  const redacted = ui.redactError('proxy socks5h://my-user:my-secret@proxy.example:1080 x-codex-turn-state=opaque-state authorization=Bearer-token');
  assert.equal(redacted.includes('my-secret'), false);
  assert.equal(redacted.includes('opaque-state'), false);
  assert.equal(redacted.includes('Bearer-token'), false);
  assert.equal(ui.remainingText(127), '2 分 7 秒');
  assert.equal(ui.remainingText(-1), '—');
});

class Node {
  constructor(tag) { this.tagName = tag; this.children = []; this.listeners = {}; this.attributes = {}; this._value = ''; this.textContent = ''; this.hidden = false; this.disabled = false; this.checked = false; }
  get value() { return this._value; }
  set value(value) { this._value = String(value); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = children; }
  addEventListener(type, fn) { this.listeners[type] = fn; }
  setAttribute(key, value) { this.attributes[key] = value; }
  async fire(type, event = {}) { if (this.listeners[type]) return this.listeners[type]({ preventDefault() {}, ...event }); }
  click() { return this.fire('click'); }
}

function uiHarness() {
  const elements = new Map();
  const calls = { load: 0, save: [], test: 0, status: 0, dispose: 0 };
  const timers = new Map();
  const document = { getElementById: id => { if (!elements.has(id)) elements.set(id, new Node('div')); return elements.get(id); },
    createElement: tag => new Node(tag), documentElement: { scrollHeight: 900 }, body: new Node('body'), visibilityState: 'visible' };
  const config = configured();
  let status = { host_ready: true, account_ids: [7, 12], tickets: [{ account_id: 7, plan: 'pro', model: 'gpt-6-astra', state: 'ready', remaining_seconds: 600, attempts: 1 }] };
  const bridge = { ready() {}, resize() {}, dispose() { calls.dispose++; },
    async load() { calls.load++; return { config }; },
    async save(value) { calls.save.push(value); return { config: value }; },
    async test() { calls.test++; return { result: { message: '检查通过' } }; },
    async status() { calls.status++; return { result: { status_json: JSON.stringify(status) } }; } };
  const global = { document, Sub2APIPluginBridge: bridge, setInterval: fn => { timers.set(1, fn); return 1; }, clearInterval: id => timers.delete(id), addEventListener() {}, removeEventListener() {} };
  const runtime = ui.start(global);
  return { elements, get: document.getElementById, calls, timers, runtime, setStatus: value => { status = value; } };
}
const settle = () => new Promise(resolve => setImmediate(resolve));

test('passive status refresh preserves unsaved form and never invokes test or save', async () => {
  const h = uiHarness(); await settle();
  h.get('dynamic-proxy-url').value = 'socks5h://unsaved:password@proxy.example:1080';
  await h.get('config-form').fire('input');
  h.setStatus({ host_ready: true, account_ids: [7, 12, 13], tickets: [] });
  await h.runtime.refreshStatus();
  assert.equal(h.get('dynamic-proxy-url').value, 'socks5h://unsaved:password@proxy.example:1080');
  assert.equal(h.get('save-state').textContent, '有未保存修改');
  assert.equal(h.calls.load, 1); assert.equal(h.calls.save.length, 0); assert.equal(h.calls.test, 0);
  assert.equal(h.get('detected-accounts').children.length, 3);
  h.runtime.stop(); assert.equal(h.timers.size, 0);
});

test('adding account defaults off, saved-config check does not save or overwrite edits', async () => {
  const h = uiHarness(); await settle();
  h.get('new-account-id').value = '12'; await h.get('add-account').click();
  const row = h.get('accounts-body').children[1];
  assert.equal(row.children[1].children[0].checked, false);
  assert.equal(row.children[2].children[0].value, 'pro');
  await h.get('test-config').click();
  assert.equal(h.calls.test, 1); assert.equal(h.calls.save.length, 0);
  assert.equal(h.get('accounts-body').children.length, 2);
  assert.match(h.get('notice').textContent, /未保存修改/);
  h.runtime.stop();
});

test('explicit save button works without native form submission in sandbox', async () => {
  const h = uiHarness(); await settle();
  h.get('new-account-id').value = '12'; await h.get('add-account').click();
  await h.get('save-config').click();
  assert.equal(h.calls.save.length, 1);
  assert.equal(h.calls.save[0].enabled, false);
  assert.equal(h.calls.save[0].accounts[1].enabled, false);
  assert.equal(h.get('save-state').textContent, '已保存');
  assert.match(h.get('notice').textContent, /STATE Kit 已关闭/);
  h.runtime.stop();
});

test('status rendering uses text nodes and never displays unrecognized raw state or model', async () => {
  const h = uiHarness(); await settle();
  h.setStatus({ host_ready: true, account_ids: [7], tickets: [{ account_id: 7, plan: 'pro', model: '<img src=x onerror=alert(1)>', state: 'SECRET-STATE-VALUE', last_error: 'x-codex-turn-state=SECRET-STATE-VALUE', remaining_seconds: 9 }] });
  await h.runtime.refreshStatus();
  function text(node) { return String(node.textContent) + node.children.map(text).join(''); }
  const rendered = text(h.get('tickets-body'));
  assert.equal(rendered.includes('SECRET-STATE-VALUE'), false);
  assert.equal(rendered.includes('<img'), false);
  assert.match(rendered, /未知状态/);
  h.runtime.stop();
});

test('front proxy settings validate, save and preserve unsaved edits during log refresh', async () => {
  const config = configured({ harvest_dial_proxy_url: 'socks5h://front:secret@example.test:1080', observe_exit_ip: true });
  assert.doesNotThrow(() => ui.validateConfig(config));
  assert.throws(() => ui.validateConfig({ ...config, harvest_dial_proxy_url: 'http://user-{sid}@proxy.test' }), /占位符/);
  assert.throws(() => ui.validateConfig({ ...config, harvest_dial_proxy_url: 'file:///secret' }), /HTTP/);
  const h = uiHarness(); await settle();
  h.get('harvest-dial-proxy-url').value = config.harvest_dial_proxy_url;
  h.get('observe-exit-ip').checked = true;
  await h.get('config-form').fire('input');
  h.setStatus({ host_ready:true, events: [{ account_id:7, time:'2026-09-19T12:00:00Z', phase:'harvest', result:'model_matched', exit_ip:'203.0.113.8', actual_model:'gpt-test', http_status:200, state_bytes:292, attempt:2, chained:true }] });
  await h.runtime.refreshStatus();
  assert.equal(h.get('harvest-dial-proxy-url').value, config.harvest_dial_proxy_url);
  assert.equal(h.get('activity-body').children.length, 1);
  const row = h.get('activity-body').children[0];
  assert.match(row.children[1].textContent,/前置代理/);
  assert.equal(row.children[2].textContent,'203.0.113.8');
  await h.get('save-config').click();
  assert.equal(h.calls.save[0].harvest_dial_proxy_url,config.harvest_dial_proxy_url);
  assert.equal(h.calls.save[0].observe_exit_ip,true);
  h.runtime.stop();
});

test('activity does not render arbitrary model, IP, phase or error text', async () => {
 const h=uiHarness();await settle();
 h.setStatus({host_ready:true,events:[{account_id:7,phase:'SECRET',result:'SECRET',exit_ip:'SECRET',actual_model:'SECRET',state_bytes:'SECRET'}]});
 await h.runtime.refreshStatus();
 function text(n){return String(n.textContent)+n.children.map(text).join('');}
 assert.equal(text(h.get('activity-body')).includes('SECRET'),false);
 h.runtime.stop();
});
