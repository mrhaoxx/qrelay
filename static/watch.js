// Viewer: WebSocket -> module-exact replica or server-regenerated QR, plus history.
(() => {
  const roomId = location.pathname.split('/').pop();
  const $ = (id) => document.getElementById(id);
  const M = window.QRModules;
  const stage = $('stage'), pixelCanvas = $('pixel'), reparseImg = $('reparse');

  const params = new URLSearchParams(location.search);
  let mode = params.get('mode') || localStorage.getItem('qrelay.mode') || 'pixel';
  if (mode !== 'pixel' && mode !== 'reparse') mode = 'pixel';
  let frozen = false, pending = null;
  let last = null;        // last live scan with a QR (sticky)
  let bits = null;        // unpacked modules of `last`
  let lastRecv = 0;
  let online = false;
  let qr = null;          // { text, url, err } of the regenerated QR for `last`
  let previewURL = null;
  let history = [];       // newest first
  let viewing = null;     // history item being viewed instead of the live result
  let viewingBits = null;

  $('export').href = `/api/rooms/${roomId}/history?download=1`;

  let toastTimer;
  function toast(msg) {
    const t = $('toast'); t.textContent = msg; t.classList.add('show');
    clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('show'), 1800);
  }

  // ---- Controls ----
  function setMode(m) {
    mode = m;
    localStorage.setItem('qrelay.mode', m);
    document.querySelectorAll('.modes button').forEach(b => b.classList.toggle('active', b.dataset.mode === m));
    $('scale').style.display = m === 'pixel' ? '' : 'none';
    const u = new URL(location.href); u.searchParams.set('mode', m); history_replace(u);
    render();
  }
  function history_replace(u) { window.history.replaceState(null, '', u); }
  document.querySelectorAll('.modes button').forEach(b => b.onclick = () => setMode(b.dataset.mode));
  $('scale').onchange = render;
  $('freeze').onclick = () => {
    frozen = !frozen;
    $('freeze').textContent = frozen ? '解除冻结' : '冻结画面';
    $('freeze').classList.toggle('primary', frozen);
    toast(frozen ? '画面已冻结，便于扫描' : '已恢复实时更新');
    if (!frozen && pending) { applyScan(pending); pending = null; }
  };
  $('live').onclick = () => viewHistory(null);
  $('copy').onclick = async () => {
    const cur = viewing || last;
    if (!cur || !cur.text) return;
    try { await navigator.clipboard.writeText(cur.text); toast('已复制'); }
    catch { toast('复制失败，请手动选择文本'); }
  };
  $('wantPreview').onchange = () => {
    const on = $('wantPreview').checked;
    $('preview').hidden = !on || !previewURL; $('previewOff').hidden = on;
    sendWant();
  };
  window.addEventListener('resize', () => { if (mode === 'pixel') render(); });

  function viewHistory(item) {
    viewing = item;
    viewingBits = item ? M.unpack(item.modules, item.dim) : null;
    $('live').hidden = !item;
    if (item) toast('正在查看历史记录');
    render(); renderHistory();
  }

  // ---- WebSocket ----
  let ws, retry = 0;
  function sendWant() {
    if (ws && ws.readyState === 1) ws.send(JSON.stringify({ type: 'want', preview: $('wantPreview').checked }));
  }
  function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws/watch/${roomId}`);
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => { retry = 0; setConn(); sendWant(); };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        let m; try { m = JSON.parse(e.data); } catch { return; }
        if (m.type === 'status') { online = m.online; $('viewers').textContent = m.viewers; setConn(); }
        else if (m.type === 'scan' && m.found) { if (frozen) pending = m; else applyScan(m); }
        else if (m.type === 'history') {
          history = m.items || [];
          if (viewing && !history.some(it => it.text === viewing.text)) viewHistory(null);
          renderHistory();
        }
        return;
      }
      const { header, payload } = M.unpackMsg(e.data);
      if (header.type === 'qr') {
        if (qr && qr.url) URL.revokeObjectURL(qr.url);
        qr = { text: header.text, err: header.err || null,
          url: header.err ? null : URL.createObjectURL(new Blob([payload], { type: 'image/png' })) };
        if (!frozen) render();
      } else if (header.type === 'preview') {
        if (previewURL) URL.revokeObjectURL(previewURL);
        previewURL = URL.createObjectURL(new Blob([payload], { type: 'image/jpeg' }));
        $('preview').src = previewURL; $('preview').hidden = !$('wantPreview').checked;
      }
    };
    ws.onclose = () => {
      online = false; setConn('warn', '连接断开，重试中…');
      retry++; setTimeout(connect, Math.min(1000 * 2 ** retry, 10000));
    };
  }
  function setConn(cls, text) {
    if (cls) { $('dot').className = 'dot ' + cls; $('conn').textContent = text; return; }
    $('dot').className = 'dot ' + (online ? 'ok' : 'warn');
    $('conn').textContent = online ? '发布端在线' : '发布端离线';
    $('placeholder').textContent = online ? '发布端已连接，等待识别到二维码…' : '等待发布端…';
  }
  connect();

  function applyScan(m) {
    lastRecv = Date.now();
    if (last && last.modules === m.modules && last.text === m.text) { last = m; return; } // keepalive, nothing to redraw
    const textChanged = !last || last.text !== m.text;
    last = m;
    bits = M.unpack(m.modules, m.dim);
    render();
    if (textChanged) renderHistory();
  }

  // ---- Rendering ----
  function renderHistory() {
    $('histCount').textContent = history.length ? `${history.length} 条` : '';
    $('histEmpty').hidden = history.length > 0;
    M.historyList($('history'), history, {
      activeText: viewing ? viewing.text : undefined,
      liveText: last ? last.text : undefined,
      onClick: (it) => viewHistory(viewing && viewing.text === it.text ? null : it),
    });
  }

  function render() {
    const cur = viewing || last;
    const has = !!cur;
    $('placeholder').hidden = has;
    pixelCanvas.hidden = !(has && mode === 'pixel');
    reparseImg.hidden = !(has && mode === 'reparse');
    $('badge').hidden = !has;
    if (!has) return;

    const t = $('text');
    t.textContent = cur.text || ''; t.classList.toggle('empty', !cur.text);
    $('copy').disabled = !cur.text;
    const isURL = /^https?:\/\/\S+$/i.test(cur.text || '');
    $('open').hidden = !isURL; if (isURL) $('open').href = cur.text;
    $('meta').textContent = (viewing ? `历史记录 · ${M.fmtTime(viewing.last)} · ` : '') +
      `版本 ${cur.version} · ${cur.dim}×${cur.dim} 模块` + (cur.mirrored ? ' · 镜头画面为镜像' : '');
    const prefix = viewing ? '历史 · ' : '';

    if (mode === 'pixel') {
      const dim = cur.dim, quiet = 4, sel = $('scale').value;
      let k;
      if (sel === 'fit') {
        const availW = stage.clientWidth - 32, availH = Math.max(240, window.innerHeight - 220);
        k = Math.max(1, Math.floor(Math.min(availW, availH) / (dim + 2 * quiet)));
      } else k = +sel;
      M.draw(pixelCanvas, viewing ? viewingBits : bits, dim, k, quiet);
      $('badge').textContent = `${prefix}像素复刻 · ${dim}×${dim} 模块 @ ${k} px`;
    } else {
      const width = Math.min(512, stage.clientWidth - 32) + 'px';
      const qe = $('qrErr'); qe.style.display = 'none';
      if (viewing) {
        reparseImg.src = `/qr.png?size=512&data=${encodeURIComponent(cur.text)}`;
        reparseImg.style.width = width; $('badge').textContent = `${prefix}重新解析 · 服务端生成`;
      } else {
        const ok = qr && qr.url && qr.text === cur.text;
        if (ok) { reparseImg.src = qr.url; reparseImg.style.width = width; $('badge').textContent = '重新解析 · 服务端生成'; }
        else { reparseImg.removeAttribute('src'); $('badge').textContent = qr && qr.err ? '无法重新生成' : '等待服务端生成…'; }
        if (qr && qr.err) { qe.style.display = ''; qe.textContent = '重新生成失败：' + qr.err; }
      }
    }
  }

  // ---- Staleness indicator ----
  setInterval(() => {
    if (!last && !viewing) { $('age').textContent = ''; stage.classList.remove('dim'); return; }
    if (viewing) { $('age').textContent = '查看历史'; stage.classList.remove('dim'); return; }
    const age = (Date.now() - lastRecv) / 1000;
    $('age').textContent = frozen ? '已冻结' : (age < 2 ? '实时' : `${age.toFixed(0)} 秒前`);
    stage.classList.toggle('dim', !frozen && age > 4);
  }, 500);

  setMode(mode);
  renderHistory();
})();
