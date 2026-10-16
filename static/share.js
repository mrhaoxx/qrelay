// Publisher: camera -> jsQR -> module bitmap + decoded text -> WebSocket.
// Uploads are tiny: a scan message is a few hundred bytes and is only sent
// when the result changes (plus a 1 Hz keepalive). The camera preview is
// uploaded only while a viewer has asked for it.
(() => {
  const roomId = location.pathname.split('/').pop();
  const key = new URLSearchParams(location.search).get('k') || '';
  const watchURL = `${location.origin}/watch/${roomId}`;
  const M = window.QRModules;

  const $ = (id) => document.getElementById(id);
  const video = $('video'), overlay = $('overlay'), hint = $('hint');
  const det = document.createElement('canvas');    // downscaled frame for detection
  const prev = document.createElement('canvas');   // tiny preview
  const dctx = det.getContext('2d', { willReadFrequently: true });
  const pctx = prev.getContext('2d');
  const octx = overlay.getContext('2d');

  $('link').value = watchURL;
  $('linkqr').src = `/qr.png?size=360&data=${encodeURIComponent(watchURL)}`;
  $('copy').onclick = async () => {
    try { await navigator.clipboard.writeText(watchURL); toast('已复制观看链接'); }
    catch { $('link').select(); document.execCommand('copy'); toast('已复制观看链接'); }
  };

  let toastTimer;
  function toast(msg) {
    const t = $('toast'); t.textContent = msg; t.classList.add('show');
    clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('show'), 1800);
  }

  // ---- WebSocket with reconnect ----
  let ws = null, wsOpen = false, retry = 0, sentOnce = false;
  let demand = { preview: false };
  function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws/share/${roomId}?k=${encodeURIComponent(key)}`);
    ws.onopen = () => { wsOpen = true; retry = 0; lastScanKey = null; setConn('ok', '已连接'); };
    ws.onmessage = (e) => {
      let m; try { m = JSON.parse(e.data); } catch { return; }
      if (m.type === 'status') $('viewers').textContent = m.viewers;
      if (m.type === 'demand') { demand = m; $('stPrev').textContent = m.preview ? '开（有观看者请求）' : '关'; }
      if (m.type === 'history') renderHistory(m.items || []);
    };
    ws.onclose = (e) => {
      wsOpen = false;
      if (e.code === 1008) { setConn('bad', '已被新的发布端替代'); return; }
      if (e.code === 1006 && retry === 0 && !sentOnce) {
        fetch(`/ws/share/${roomId}?k=${encodeURIComponent(key)}`).then(r => {
          if (r.status === 403) setConn('bad', '链接中的密钥无效，请重新创建房间');
        }).catch(() => {});
      }
      setConn('warn', '连接断开，重试中…');
      retry++;
      setTimeout(connect, Math.min(1000 * 2 ** retry, 10000));
    };
    ws.onerror = () => {};
  }
  function setConn(cls, text) { $('dot').className = 'dot ' + cls; $('conn').textContent = text; }
  connect();

  // ---- History (kept by the server) ----
  $('export').href = `/api/rooms/${roomId}/history?download=1`;
  function renderHistory(items) {
    $('histCount').textContent = items.length ? `${items.length} 条` : '';
    $('histEmpty').hidden = items.length > 0;
    $('clearHist').disabled = items.length === 0;
    M.historyList($('history'), items, { liveText: lastText || undefined });
  }
  $('clearHist').onclick = () => {
    if (!wsOpen || !confirm('清空本房间的历史记录？观看端的列表也会被清空。')) return;
    ws.send(JSON.stringify({ type: 'clearHistory' }));
  };

  // ---- Camera ----
  let stream = null;
  async function startCamera(deviceId) {
    if (stream) stream.getTracks().forEach(t => t.stop());
    const videoC = deviceId ? { deviceId: { exact: deviceId } } : { facingMode: { ideal: 'environment' } };
    videoC.width = { ideal: 1280 }; videoC.height = { ideal: 720 };
    try {
      stream = await navigator.mediaDevices.getUserMedia({ video: videoC, audio: false });
    } catch (err) {
      $('camErr').textContent = '无法打开摄像头：' + err.message +
        (location.protocol !== 'https:' && location.hostname !== 'localhost' ? '（非 localhost 需要 HTTPS，可用 -self-signed 启动）' : '');
      hint.textContent = '摄像头不可用';
      return;
    }
    $('camErr').textContent = '';
    video.srcObject = stream;
    await video.play();
    hint.textContent = '将二维码对准镜头';
    await listCameras();
  }
  async function listCameras() {
    const devs = (await navigator.mediaDevices.enumerateDevices()).filter(d => d.kind === 'videoinput');
    const sel = $('cameras'); const cur = sel.value;
    sel.innerHTML = '<option value="">默认摄像头</option>' +
      devs.map((d, i) => `<option value="${d.deviceId}">${d.label || '摄像头 ' + (i + 1)}</option>`).join('');
    const active = stream && stream.getVideoTracks()[0].getSettings().deviceId;
    sel.value = devs.some(d => d.deviceId === active) ? active : cur;
  }
  $('cameras').onchange = () => startCamera($('cameras').value || undefined);
  startCamera();

  // ---- Detection + send loop ----
  let paused = false, sentCount = 0, bytesWindow = [];
  let lastScanKey = null, lastScanSent = 0, lastPreviewSent = 0, previewBusy = false, lastText = null;
  renderHistory([]);
  $('toggle').onclick = () => {
    paused = !paused;
    $('toggle').textContent = paused ? '继续发送' : '暂停发送';
    toast(paused ? '已暂停发送' : '已继续发送');
  };

  const DET_MAX_W = 800;        // detection resolution cap
  const PREVIEW_W = 240;        // preview thumbnail width
  const KEEPALIVE_MS = 1000;    // resend an unchanged scan this often
  const PREVIEW_MS = 500;       // preview at most 2 fps

  function tick() {
    try { step(); } catch (err) { console.error(err); }
    setTimeout(tick, +$('rate').value);
  }

  function step() {
    if (!stream || video.readyState < 2 || !video.videoWidth) return;
    const w = video.videoWidth, h = video.videoHeight;
    if (overlay.width !== w || overlay.height !== h) {
      overlay.width = w; overlay.height = h;
      $('stRes').textContent = `${w}×${h}`;
    }
    const scale = Math.min(1, DET_MAX_W / w);
    const dw = Math.round(w * scale), dh = Math.round(h * scale);
    if (det.width !== dw || det.height !== dh) { det.width = dw; det.height = dh; }
    dctx.drawImage(video, 0, 0, dw, dh);
    const t0 = performance.now();
    const code = jsQR(dctx.getImageData(0, 0, dw, dh).data, dw, dh, { inversionAttempts: 'dontInvert' });
    $('stDet').textContent = `${(performance.now() - t0).toFixed(0)} ms`;

    octx.clearRect(0, 0, w, h);
    let msg, keyStr;
    if (code && code.matrix) {
      const L = code.location;
      const pts = [L.topLeftCorner, L.topRightCorner, L.bottomRightCorner, L.bottomLeftCorner]
        .map(p => ({ x: p.x / scale, y: p.y / scale }));
      octx.strokeStyle = '#3ccf7a'; octx.lineWidth = Math.max(2, w / 300);
      octx.beginPath(); pts.forEach((p, i) => i ? octx.lineTo(p.x, p.y) : octx.moveTo(p.x, p.y));
      octx.closePath(); octx.stroke();

      const dim = code.matrix.width;
      const modules = M.packFromMatrix(code.matrix);
      msg = { type: 'scan', found: true, text: code.data, dim, modules, version: code.version, mirrored: !!code.mirrored };
      keyStr = code.data + '|' + modules;
      lastText = code.data;

      const d = $('decoded'); d.textContent = code.data; d.classList.remove('empty');
      $('stMod').textContent = `${dim}×${dim}（版本 ${code.version}${code.mirrored ? '，镜像' : ''}）`;
      if (keyStr !== lastScanKey) M.draw($('modules'), M.unpack(modules, dim), dim, 4, 2);
      hint.textContent = '已识别，正在分享';
    } else {
      msg = { type: 'scan', found: false };
      keyStr = 'none';
      hint.textContent = '将二维码对准镜头';
    }

    const now = performance.now();
    if (keyStr !== lastScanKey || now - lastScanSent >= KEEPALIVE_MS) {
      if (send(JSON.stringify(msg))) { lastScanKey = keyStr; lastScanSent = now; }
    }
    if (demand.preview && !previewBusy && now - lastPreviewSent >= PREVIEW_MS) {
      const pw = PREVIEW_W, ph = Math.round(h * PREVIEW_W / w);
      if (prev.width !== pw || prev.height !== ph) { prev.width = pw; prev.height = ph; }
      pctx.drawImage(video, 0, 0, pw, ph);
      previewBusy = true; lastPreviewSent = now;
      prev.toBlob(async (blob) => {
        try { if (blob) send(M.pack({ type: 'preview' }, new Uint8Array(await blob.arrayBuffer()))); }
        finally { previewBusy = false; }
      }, 'image/jpeg', 0.5);
    }
  }

  function send(data) {
    if (paused || !wsOpen || ws.readyState !== 1) return false;
    if (ws.bufferedAmount > 512 * 1024) return false;
    ws.send(data);
    sentOnce = true; sentCount++;
    $('stSent').textContent = sentCount;
    const now = Date.now(), len = typeof data === 'string' ? new TextEncoder().encode(data).length : data.byteLength;
    bytesWindow.push([now, len]);
    while (bytesWindow.length && now - bytesWindow[0][0] > 5000) bytesWindow.shift();
    const bps = bytesWindow.reduce((a, b) => a + b[1], 0) / 5;
    $('stBw').textContent = bps >= 1e3 ? (bps / 1e3).toFixed(1) + ' KB/s' : bps.toFixed(0) + ' B/s';
    return true;
  }

  tick();
  window.addEventListener('beforeunload', () => { if (stream) stream.getTracks().forEach(t => t.stop()); });
})();
