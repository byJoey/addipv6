'use strict';

const $ = (s) => document.querySelector(s);
const el = (tag, cls) => { const n = document.createElement(tag); if (cls) n.className = cls; return n; };

const S = {
  csrf: '',
  platform: '',
  version: '',
  ifaces: [],
  iface: '',
  prefix: '',
  rows: [],
  checked: new Set(),
  pending: [],
  cf: { configured: false, mode: 'token', zones: [], zoneId: '', zoneName: '', records: [], checked: new Set() },
  busy: false,
};

/* ---------- 基础工具 ---------- */

async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (S.csrf) opt.headers['X-CSRF-Token'] = S.csrf;
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  if (res.status === 401) { showLogin(); throw new Error('登录已过期，重新登一下'); }
  let data = null;
  const text = await res.text();
  if (text) { try { data = JSON.parse(text); } catch (_) { data = null; } }
  if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
  return data;
}

function log(msg, kind) {
  const line = el('div', 'log-line' + (kind ? ' ' + kind : ''));
  const t = el('time');
  t.textContent = new Date().toTimeString().slice(0, 8);
  const m = el('span', 'msg');
  m.textContent = msg;
  line.append(t, m);
  const box = $('#log');
  box.append(line);
  box.scrollTop = box.scrollHeight;
  while (box.childElementCount > 500) box.firstElementChild.remove();
}

function toast(msg, kind) {
  const n = el('div', 'toast' + (kind ? ' ' + kind : ''));
  n.textContent = msg;
  $('#toast').append(n);
  setTimeout(() => n.remove(), 4200);
}

function fail(err) {
  const msg = err && err.message ? err.message : String(err);
  log(msg, 'err');
  toast(msg, 'err');
}

function busy(on) {
  S.busy = on;
  document.body.classList.toggle('busy', on);
}

function confirmBox({ title, text, list, force }) {
  return new Promise((resolve) => {
    const dlg = $('#confirm');
    $('#dlg-title').textContent = title;
    $('#dlg-text').textContent = text || '';
    const listBox = $('#dlg-list');
    if (list && list.length) {
      listBox.textContent = list.slice(0, 40).join('\n') + (list.length > 40 ? '\n… 还有 ' + (list.length - 40) + ' 条' : '');
      listBox.classList.remove('hidden');
    } else listBox.classList.add('hidden');
    $('#dlg-force-wrap').classList.toggle('hidden', !force);
    $('#dlg-force').checked = false;

    const done = (ok) => {
      dlg.close();
      $('#dlg-ok').removeEventListener('click', onOk);
      $('#dlg-cancel').removeEventListener('click', onCancel);
      resolve({ ok, force: $('#dlg-force').checked });
    };
    const onOk = () => done(true);
    const onCancel = () => done(false);
    $('#dlg-ok').addEventListener('click', onOk);
    $('#dlg-cancel').addEventListener('click', onCancel);
    dlg.showModal();
  });
}

/* ---------- 登录 ---------- */

function showLogin() {
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  setTimeout(() => $('#pw').focus(), 30);
}

function showApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const err = $('#login-err');
  err.classList.add('hidden');
  try {
    const r = await api('POST', '/api/login', { password: $('#pw').value });
    S.csrf = r.csrf;
    S.version = r.version;
    S.platform = r.platform;
    $('#pw').value = '';
    showApp();
    log('登录成功', 'ok');
    await boot();
  } catch (e2) {
    err.textContent = e2.message;
    err.classList.remove('hidden');
  }
});

$('#logout').addEventListener('click', async () => {
  try { await api('POST', '/api/logout'); } catch (_) {}
  S.csrf = '';
  showLogin();
});

/* ---------- 主题 ---------- */

function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  localStorage.setItem('addipv6-theme', t);
  $('#theme').firstElementChild.firstElementChild.setAttribute('href', t === 'dark' ? '#i-sun' : '#i-moon');
}

$('#theme').addEventListener('click', () => {
  applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
});

/* ---------- 地址表 ---------- */

function currentIface() {
  return S.ifaces.find((i) => i.name === S.iface);
}

function rebuildRows() {
  const it = currentIface();
  const rows = [];
  if (it) {
    for (const a of it.addresses) {
      if (a.scope !== 'global') continue;
      rows.push({
        key: a.prefix, addr: a.addr, prefix: a.prefix, status: 'active',
        managed: a.managed, isSource: a.isSource, flags: a.flags || [],
      });
    }
  }
  const have = new Set(rows.map((r) => r.key));
  for (const p of S.pending) {
    if (have.has(p)) continue;
    rows.push({ key: p, addr: p.split('/')[0], prefix: p, status: 'pending', managed: false, isSource: false, flags: [] });
  }
  rows.sort((a, b) => {
    if (a.status !== b.status) return a.status === 'pending' ? -1 : 1;
    return a.prefix.localeCompare(b.prefix);
  });
  S.rows = rows;
  for (const k of [...S.checked]) if (!rows.some((r) => r.key === k)) S.checked.delete(k);
}

function tag(text, cls) {
  const n = el('span', 'tag' + (cls ? ' ' + cls : ''));
  n.textContent = text;
  return n;
}

function renderAddrs() {
  const body = $('#addr-rows');
  body.textContent = '';
  for (const r of S.rows) {
    const tr = el('tr');
    if (S.checked.has(r.key)) tr.classList.add('picked');

    const tdC = el('td', 'check');
    const cb = el('input');
    cb.type = 'checkbox';
    cb.checked = S.checked.has(r.key);
    cb.addEventListener('change', () => {
      if (cb.checked) S.checked.add(r.key); else S.checked.delete(r.key);
      renderAddrs();
    });
    tdC.append(cb);

    const tdA = el('td', 'addr');
    tdA.textContent = r.prefix;

    const tdS = el('td', 'tags');
    if (r.status === 'pending') tdS.append(tag('待添加', 'warn'));
    if (r.isSource) tdS.append(tag('出口', 'accent'));
    if (r.managed) tdS.append(tag('本工具'));
    for (const f of r.flags) {
      if (f === 'permanent' || f === 'noprefixroute') continue;
      tdS.append(tag(f, f === 'dadfailed' ? 'danger' : ''));
    }
    if (!tdS.childElementCount && r.status === 'active') tdS.append(tag('系统自带'));

    const tdX = el('td');
    if (r.status === 'pending') {
      const b = el('button', 'ghost icon-only');
      b.title = '不要这个';
      b.innerHTML = '<svg class="icon"><use href="#i-x"/></svg>';
      b.addEventListener('click', () => {
        S.pending = S.pending.filter((p) => p !== r.key);
        S.checked.delete(r.key);
        rebuildRows(); renderAddrs();
      });
      tdX.append(b);
    }

    tr.append(tdC, tdA, tdS, tdX);
    body.append(tr);
  }

  $('#addr-empty').classList.toggle('hidden', S.rows.length > 0);
  const picked = S.rows.filter((r) => S.checked.has(r.key));
  const pendingPicked = picked.filter((r) => r.status === 'pending');
  const activePicked = picked.filter((r) => r.status === 'active');

  $('#add-pending').disabled = pendingPicked.length === 0 || S.platform !== 'linux';
  $('#add-pending').firstElementChild.nextSibling;
  $('#add-pending').lastChild.textContent = pendingPicked.length ? '加到网卡 (' + pendingPicked.length + ')' : '加到网卡';
  $('#del-addr').disabled = picked.length === 0 || S.platform !== 'linux';
  $('#set-source').disabled = activePicked.length !== 1 || S.platform !== 'linux';
  $('#check-all-addr').checked = S.rows.length > 0 && picked.length === S.rows.length;

  const total = S.rows.filter((r) => r.status === 'active').length;
  const mine = S.rows.filter((r) => r.managed).length;
  $('#addr-count').textContent = '网卡上 ' + total + ' 个，其中本工具加的 ' + mine + ' 个'
    + (S.pending.length ? '；待添加 ' + S.pending.length + ' 个' : '')
    + '；勾中 ' + picked.length + ' 个';
  $('#addr-stat').textContent = S.iface || '-';

  const canSend = activePicked.length > 0 && S.cf.configured && S.cf.zoneId;
  $('#cf-create').disabled = !canSend;
  $('#cf-create-label').textContent = activePicked.length
    ? '把勾中的 ' + activePicked.length + ' 个解析过去'
    : '解析勾中的地址';
  updateHint();
}

/* ---------- 顶部 ---------- */

function renderHeader() {
  const sel = $('#iface');
  sel.textContent = '';
  for (const i of S.ifaces) {
    const o = el('option');
    o.value = i.name;
    o.textContent = i.name + (i.up ? '' : '（没起来）');
    sel.append(o);
  }
  sel.value = S.iface;

  const it = currentIface();
  const ps = $('#prefix');
  ps.textContent = '';
  const list = (it && it.prefixes) || [];
  for (const p of list) {
    const o = el('option');
    o.value = p;
    o.textContent = p;
    ps.append(o);
  }
  if (!list.length) {
    const o = el('option');
    o.value = '';
    o.textContent = '没有可用网段';
    ps.append(o);
  }
  if (S.prefix && list.includes(S.prefix)) ps.value = S.prefix;
  else S.prefix = list[0] || '';
  ps.value = S.prefix;

  $('#ver').textContent = S.version || '';
  const plat = $('#platform');
  plat.textContent = S.platform === 'linux' ? 'linux' : '演示模式';
  plat.className = 'tag' + (S.platform === 'linux' ? '' : ' warn');
  $('#demo-banner').classList.toggle('hidden', S.platform === 'linux');
}

/* ---------- Cloudflare ---------- */

function updateHint() {
  const name = $('#cf-name').value.trim();
  const zone = S.cf.zoneName || '你的域名';
  const n = S.rows.filter((r) => S.checked.has(r.key) && r.status === 'active').length || 2;
  const hint = $('#cf-hint');
  if (/\{(n|i)(:\d+)?\}/.test(name)) {
    const one = name.replace(/\{(n|i)(:(\d+))?\}/g, (m, k, _c, w) => {
      const v = k === 'n' ? 1 : 0;
      return w ? String(v).padStart(Number(w), '0') : String(v);
    });
    hint.textContent = '每个地址一条记录，第一条是 ' + (one ? one + '.' + zone : zone) + '。';
  } else {
    const full = name && name !== '@' ? name + '.' + zone : zone;
    hint.textContent = n + ' 个地址会全挂在 ' + full + ' 上做轮询。想一个地址一条就在名字里写 {n}。';
  }
}

function modeLabel(m) { return m === 'globalkey' ? '全局 Key' : 'Token'; }

function renderMode() {
  const m = S.cf.mode;
  for (const b of document.querySelectorAll('#cf-mode button')) {
    b.setAttribute('aria-pressed', String(b.dataset.mode === m));
  }
  $('#cf-row-token').classList.toggle('hidden', m !== 'token');
  $('#cf-row-key').classList.toggle('hidden', m !== 'globalkey');
  $('#cf-mode-note').textContent = m === 'globalkey'
    ? '全局 Key 是整个账户的权限，泄露等于账号没了。能用 Token 就别用它。'
    : '在 CF 后台用 Edit zone DNS 模板建，权限给 Zone - DNS - Edit 就够。';
}

function renderCF() {
  $('#cf-token-row').classList.toggle('hidden', S.cf.configured);
  $('#cf-main').classList.toggle('hidden', !S.cf.configured);
  $('#cf-reset').classList.toggle('hidden', !S.cf.configured);
  renderMode();
  const st = $('#cf-stat');
  st.textContent = S.cf.configured
    ? modeLabel(S.cf.mode) + ' · ' + (S.cf.tokenHint || '已连接')
    : '未连接';
  st.className = 'tag' + (S.cf.configured ? ' accent' : '');

  const sel = $('#cf-zone');
  sel.textContent = '';
  for (const z of S.cf.zones) {
    const o = el('option');
    o.value = z.id;
    o.textContent = z.name + (z.status && z.status !== 'active' ? '（' + z.status + '）' : '');
    sel.append(o);
  }
  if (S.cf.zoneId) sel.value = S.cf.zoneId;
  else if (S.cf.zones.length) { S.cf.zoneId = S.cf.zones[0].id; S.cf.zoneName = S.cf.zones[0].name; sel.value = S.cf.zoneId; }
  updateHint();
}

function renderRecords() {
  const body = $('#rec-rows');
  body.textContent = '';
  for (const r of S.cf.records) {
    const tr = el('tr');
    if (S.cf.checked.has(r.id)) tr.classList.add('picked');

    const tdC = el('td', 'check');
    const cb = el('input');
    cb.type = 'checkbox';
    cb.checked = S.cf.checked.has(r.id);
    cb.addEventListener('change', () => {
      if (cb.checked) S.cf.checked.add(r.id); else S.cf.checked.delete(r.id);
      renderRecords();
    });
    tdC.append(cb);

    const tdN = el('td');
    tdN.textContent = r.name;
    const tdV = el('td', 'content');
    tdV.textContent = r.content;
    const tdT = el('td', 'num');
    tdT.textContent = r.ttl === 1 ? 'auto' : r.ttl;
    const tdM = el('td', 'tags');
    if (r.proxied) tdM.append(tag('代理', 'accent'));
    if (r.managed) tdM.append(tag('本工具'));

    tr.append(tdC, tdN, tdV, tdT, tdM);
    body.append(tr);
  }
  $('#rec-empty').classList.toggle('hidden', S.cf.records.length > 0);
  const n = S.cf.checked.size;
  $('#cf-delete').disabled = n === 0;
  $('#check-all-rec').checked = S.cf.records.length > 0 && n === S.cf.records.length;
  const mine = S.cf.records.filter((r) => r.managed).length;
  $('#rec-count').textContent = 'AAAA 记录 ' + S.cf.records.length + ' 条，其中本工具建的 ' + mine + ' 条；勾中 ' + n + ' 条';
}

async function loadZones(quiet) {
  try {
    const r = await api('GET', '/api/cf/zones');
    S.cf.zones = r.zones || [];
    if (!S.cf.zones.some((z) => z.id === S.cf.zoneId)) {
      S.cf.zoneId = S.cf.zones.length ? S.cf.zones[0].id : '';
      S.cf.zoneName = S.cf.zones.length ? S.cf.zones[0].name : '';
    }
    renderCF();
    if (!quiet) log('拉到 ' + S.cf.zones.length + ' 个站点', 'ok');
    if (S.cf.zoneId) await loadRecords(true);
  } catch (e) { fail(e); }
}

async function loadRecords(quiet) {
  if (!S.cf.zoneId) return;
  try {
    const r = await api('GET', '/api/cf/records?zoneId=' + encodeURIComponent(S.cf.zoneId));
    S.cf.records = r.records || [];
    for (const k of [...S.cf.checked]) if (!S.cf.records.some((x) => x.id === k)) S.cf.checked.delete(k);
    renderRecords();
    if (!quiet) log(S.cf.zoneName + ' 有 ' + S.cf.records.length + ' 条 AAAA', 'ok');
  } catch (e) { fail(e); }
}

/* ---------- 载入 ---------- */

async function loadState(quiet) {
  const st = await api('GET', '/api/state');
  S.platform = st.platform;
  S.version = st.version;
  S.ifaces = st.interfaces || [];
  if (!S.ifaces.some((i) => i.name === S.iface)) S.iface = (st.iface && S.ifaces.some((i) => i.name === st.iface)) ? st.iface : (S.ifaces[0] ? S.ifaces[0].name : '');
  S.cf.configured = st.cloudflare.configured;
  S.cf.tokenHint = st.cloudflare.tokenHint;
  if (st.cloudflare.authMode) S.cf.mode = st.cloudflare.authMode;
  if (st.cloudflare.email && !$('#cf-email').value) $('#cf-email').value = st.cloudflare.email;
  if (!S.cf.zoneId) { S.cf.zoneId = st.cloudflare.zoneId || ''; S.cf.zoneName = st.cloudflare.zoneName || ''; }
  if (st.cloudflare.recordName && !$('#cf-name').value) $('#cf-name').value = st.cloudflare.recordName;
  if (st.cloudflare.ttl) $('#cf-ttl').value = st.cloudflare.ttl;
  $('#cf-proxied').checked = !!st.cloudflare.proxied;

  renderHeader();
  rebuildRows();
  renderAddrs();
  renderCF();
  if (!quiet) log('网卡读取完毕，' + S.ifaces.length + ' 块有全局 IPv6', 'ok');
}

async function boot() {
  try {
    await loadState();
    if (S.cf.configured) await loadZones(true);
    log('准备好了。配置在 ' + (await api('GET', '/api/state')).configPath);
  } catch (e) { fail(e); }
}

/* ---------- 事件 ---------- */

$('#iface').addEventListener('change', (e) => {
  S.iface = e.target.value;
  S.prefix = '';
  S.pending = [];
  S.checked.clear();
  renderHeader(); rebuildRows(); renderAddrs();
});

$('#prefix').addEventListener('change', (e) => { S.prefix = e.target.value; });

$('#reload').addEventListener('click', async () => {
  try { await loadState(); } catch (e) { fail(e); }
});

$('#gen').addEventListener('click', async () => {
  const count = Number($('#count').value);
  if (!count || count < 1) { toast('数量填个正数', 'err'); return; }
  busy(true);
  try {
    const r = await api('POST', '/api/addresses/preview', { iface: S.iface, prefix: S.prefix, count });
    S.pending = [...new Set([...r.addrs, ...S.pending])];
    for (const a of r.addrs) S.checked.add(a);
    rebuildRows(); renderAddrs();
    log('在 ' + r.prefix + ' 里生成了 ' + r.addrs.length + ' 个', 'ok');
    if (r.warning) log(r.warning, 'warn');
    if ($('#auto-add').checked) await addPending();
  } catch (e) { fail(e); } finally { busy(false); }
});

async function addPending() {
  const targets = S.rows.filter((r) => r.status === 'pending' && S.checked.has(r.key)).map((r) => r.key);
  if (!targets.length) return;
  busy(true);
  try {
    const res = await api('POST', '/api/addresses/add', { iface: S.iface, addrs: targets });
    log('加到 ' + S.iface + '：成功 ' + res.succeed + '，失败 ' + res.failed, res.failed ? 'warn' : 'ok');
    for (const it of res.items) if (!it.ok) log('  ' + it.target + ' → ' + it.error, 'err');
    if (res.warning) { log(res.warning, 'warn'); toast('出站源地址变了，看日志', 'warn'); }
    S.pending = S.pending.filter((p) => !res.items.some((i) => i.ok && i.target === p));
    await loadState(true);
    for (const it of res.items) if (it.ok) S.checked.add(it.target);
    renderAddrs();
    if (res.succeed) toast('加上了 ' + res.succeed + ' 个');
  } catch (e) { fail(e); } finally { busy(false); }
}

$('#add-pending').addEventListener('click', addPending);

$('#del-addr').addEventListener('click', async () => {
  const targets = S.rows.filter((r) => S.checked.has(r.key));
  const live = targets.filter((r) => r.status === 'active').map((r) => r.key);
  const ghosts = targets.filter((r) => r.status === 'pending').map((r) => r.key);
  if (ghosts.length) {
    S.pending = S.pending.filter((p) => !ghosts.includes(p));
    for (const g of ghosts) S.checked.delete(g);
  }
  if (!live.length) { rebuildRows(); renderAddrs(); log('丢掉 ' + ghosts.length + ' 个还没下发的'); return; }

  const hasSource = targets.some((r) => r.isSource);
  const c = await confirmBox({
    title: '删 ' + live.length + ' 个地址',
    text: hasSource ? '里面有当前出口地址，删了出站会断。' : '从 ' + S.iface + ' 上摘掉，删完立刻生效。',
    list: live,
    force: true,
  });
  if (!c.ok) return;
  busy(true);
  try {
    const res = await api('POST', '/api/addresses/delete', { iface: S.iface, addrs: live, force: c.force });
    log('删除：成功 ' + res.succeed + '，失败 ' + res.failed, res.failed ? 'warn' : 'ok');
    for (const it of res.items) if (!it.ok) log('  ' + it.target + ' → ' + it.error, 'err');
    S.checked.clear();
    await loadState(true);
    renderAddrs();
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#set-source').addEventListener('click', async () => {
  const picked = S.rows.filter((r) => S.checked.has(r.key) && r.status === 'active');
  if (picked.length !== 1) return;
  const addr = picked[0].addr;
  const c = await confirmBox({ title: '把出口换成这个', text: addr + ' 会成为出站流量的源地址。', list: [addr] });
  if (!c.ok) return;
  busy(true);
  try {
    const r = await api('POST', '/api/route/source', { iface: S.iface, source: addr });
    log('出口已切到 ' + r.source + (r.gateway ? '（网关 ' + r.gateway + '）' : '（点对点链路，没有网关）'), 'ok');
    toast('出口换好了');
    await loadState(true);
    renderAddrs();
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#persist').addEventListener('click', async () => {
  const c = await confirmBox({ title: '装开机恢复', text: '装一个开机自启的服务，重启后按记录把地址加回来。' });
  if (!c.ok) return;
  busy(true);
  try {
    const r = await api('POST', '/api/persist', {});
    log('已装好（' + r.method + '）：' + r.path + ' — ' + (r.detail || ''), 'ok');
    toast('开机保持已打开');
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#restore').addEventListener('click', async () => {
  busy(true);
  try {
    const res = await api('POST', '/api/restore', {});
    log('恢复：成功 ' + res.succeed + '，失败 ' + res.failed, res.failed ? 'warn' : 'ok');
    for (const it of res.items) if (!it.ok) log('  ' + it.target + ' → ' + it.error, 'err');
    await loadState(true);
    renderAddrs();
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#check-all-addr').addEventListener('change', (e) => {
  if (e.target.checked) for (const r of S.rows) S.checked.add(r.key);
  else S.checked.clear();
  renderAddrs();
});
$('#sel-all').addEventListener('click', () => { for (const r of S.rows) S.checked.add(r.key); renderAddrs(); });
$('#sel-mine').addEventListener('click', () => { S.checked.clear(); for (const r of S.rows) if (r.managed) S.checked.add(r.key); renderAddrs(); });
$('#sel-pending').addEventListener('click', () => { S.checked.clear(); for (const r of S.rows) if (r.status === 'pending') S.checked.add(r.key); renderAddrs(); });
$('#sel-none').addEventListener('click', () => { S.checked.clear(); renderAddrs(); });

document.querySelector('#cf-mode').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-mode]');
  if (!b) return;
  S.cf.mode = b.dataset.mode;
  renderMode();
});

$('#cf-save').addEventListener('click', async () => {
  const mode = S.cf.mode;
  let payload;
  if (mode === 'globalkey') {
    const email = $('#cf-email').value.trim();
    const key = $('#cf-key').value.trim();
    if (!email || !key) { toast('邮箱和全局 Key 都要填', 'err'); return; }
    payload = { mode, email, key };
  } else {
    const token = $('#cf-token').value.trim();
    if (!token) { toast('先把 Token 贴进来', 'err'); return; }
    payload = { mode, token };
  }
  busy(true);
  try {
    const r = await api('POST', '/api/cf/token', payload);
    $('#cf-token').value = '';
    $('#cf-key').value = '';
    S.cf.configured = true;
    S.cf.mode = mode;
    S.cf.zones = r.zones || [];
    if (S.cf.zones.length) { S.cf.zoneId = S.cf.zones[0].id; S.cf.zoneName = S.cf.zones[0].name; }
    await loadState(true);
    renderCF();
    log(modeLabel(mode) + ' 验过了，能看到 ' + S.cf.zones.length + ' 个站点', 'ok');
    if (S.cf.zoneId) await loadRecords(true);
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#cf-reset').addEventListener('click', async () => {
  const c = await confirmBox({ title: '换凭据', text: '当前的 ' + modeLabel(S.cf.mode) + ' 会从配置文件里清掉。' });
  if (!c.ok) return;
  try {
    await api('DELETE', '/api/cf/token');
    const keep = S.cf.mode;
    S.cf = { configured: false, mode: keep, zones: [], zoneId: '', zoneName: '', records: [], checked: new Set() };
    renderCF(); renderRecords(); renderAddrs();
    log('凭据已清除');
  } catch (e) { fail(e); }
});

$('#cf-zone').addEventListener('change', async (e) => {
  S.cf.zoneId = e.target.value;
  const z = S.cf.zones.find((x) => x.id === S.cf.zoneId);
  S.cf.zoneName = z ? z.name : '';
  S.cf.checked.clear();
  updateHint();
  try {
    await api('POST', '/api/cf/zone', { zoneId: S.cf.zoneId, zoneName: S.cf.zoneName });
  } catch (_) {}
  await loadRecords();
});

$('#cf-reload-zones').addEventListener('click', () => loadZones(false));
$('#cf-reload-records').addEventListener('click', () => loadRecords(false));
$('#cf-name').addEventListener('input', updateHint);

$('#cf-create').addEventListener('click', async () => {
  const picked = S.rows.filter((r) => S.checked.has(r.key));
  const live = picked.filter((r) => r.status === 'active').map((r) => r.addr);
  const ghosts = picked.filter((r) => r.status === 'pending');
  if (!live.length) { toast('先勾几个已经在网卡上的地址', 'err'); return; }
  if (ghosts.length) log(ghosts.length + ' 个还没加到网卡，这次跳过它们', 'warn');

  const name = $('#cf-name').value.trim();
  const rr = /\{(n|i)(:\d+)?\}/.test(name);
  const c = await confirmBox({
    title: '解析 ' + live.length + ' 个地址到 ' + S.cf.zoneName,
    text: rr ? '每个地址一条 AAAA。' : '全部挂在同一个名字下做轮询。',
    list: live,
  });
  if (!c.ok) return;

  busy(true);
  try {
    const res = await api('POST', '/api/cf/records/create', {
      zoneId: S.cf.zoneId, zoneName: S.cf.zoneName, name,
      addrs: live, ttl: Number($('#cf-ttl').value) || 60, proxied: $('#cf-proxied').checked,
    });
    log('解析完成：新建 ' + res.created + '，已存在跳过 ' + res.skipped + '，失败 ' + res.failed,
      res.failed ? 'warn' : 'ok');
    for (const it of res.items) if (it.error) log('  ' + it.name + ' ' + it.content + ' → ' + it.error, 'err');
    toast('新建 ' + res.created + ' 条');
    await loadRecords(true);
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#check-all-rec').addEventListener('change', (e) => {
  if (e.target.checked) for (const r of S.cf.records) S.cf.checked.add(r.id);
  else S.cf.checked.clear();
  renderRecords();
});
$('#rec-all').addEventListener('click', () => { for (const r of S.cf.records) S.cf.checked.add(r.id); renderRecords(); });
$('#rec-mine').addEventListener('click', () => { S.cf.checked.clear(); for (const r of S.cf.records) if (r.managed) S.cf.checked.add(r.id); renderRecords(); });
$('#rec-none').addEventListener('click', () => { S.cf.checked.clear(); renderRecords(); });

$('#cf-delete').addEventListener('click', async () => {
  const ids = [...S.cf.checked];
  if (!ids.length) return;
  const rows = S.cf.records.filter((r) => S.cf.checked.has(r.id));
  const c = await confirmBox({
    title: '删 ' + ids.length + ' 条 AAAA',
    text: '从 ' + S.cf.zoneName + ' 删掉，DNS 那边马上就没了。',
    list: rows.map((r) => r.name + '  ' + r.content),
  });
  if (!c.ok) return;
  busy(true);
  try {
    const res = await api('POST', '/api/cf/records/delete', { zoneId: S.cf.zoneId, ids });
    log('删除记录：成功 ' + res.deleted + '，已不存在 ' + res.skipped + '，失败 ' + res.failed,
      res.failed ? 'warn' : 'ok');
    for (const it of res.items) if (it.error) log('  ' + it.id + ' → ' + it.error, 'err');
    S.cf.checked.clear();
    await loadRecords(true);
  } catch (e) { fail(e); } finally { busy(false); }
});

$('#log-clear').addEventListener('click', () => { $('#log').textContent = ''; });
$('#log-copy').addEventListener('click', async () => {
  const text = [...$('#log').children].map((n) => n.textContent.replace(/^(\d\d:\d\d:\d\d)/, '$1 ')).join('\n');
  try { await navigator.clipboard.writeText(text); toast('日志复制好了'); }
  catch (_) { toast('浏览器不让复制，手动选吧', 'warn'); }
});

/* ---------- 起步 ---------- */

applyTheme(localStorage.getItem('addipv6-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));

(async () => {
  try {
    const s = await api('GET', '/api/session');
    S.version = s.version;
    S.platform = s.platform;
    if (s.authenticated) {
      S.csrf = s.csrf;
      showApp();
      await boot();
    } else showLogin();
  } catch (_) { showLogin(); }
})();
