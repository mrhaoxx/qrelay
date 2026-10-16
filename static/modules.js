// Shared helpers: QR module bitmaps (row-major, MSB-first, 1 = dark), binary
// message framing, and the history list widget.
window.QRModules = (() => {
  function packFromMatrix(matrix) {
    const dim = matrix.width, bytes = new Uint8Array((dim * dim + 7) >> 3);
    for (let y = 0, i = 0; y < dim; y++) for (let x = 0; x < dim; x++, i++) {
      if (matrix.get(x, y)) bytes[i >> 3] |= 0x80 >> (i & 7);
    }
    let s = ''; for (const b of bytes) s += String.fromCharCode(b);
    return btoa(s);
  }
  function unpack(b64, dim) {
    const raw = atob(b64), bits = new Uint8Array(dim * dim);
    for (let i = 0; i < bits.length; i++) bits[i] = (raw.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1;
    return bits;
  }
  // Draw the module grid 1:1: every module becomes a k×k block, with a quiet zone.
  function draw(canvas, bits, dim, k, quiet = 4) {
    const size = (dim + 2 * quiet) * k;
    canvas.width = size; canvas.height = size;
    canvas.style.width = size + 'px'; canvas.style.height = size + 'px';
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#fff'; ctx.fillRect(0, 0, size, size);
    ctx.fillStyle = '#000';
    for (let y = 0; y < dim; y++) for (let x = 0; x < dim; x++) {
      if (bits[y * dim + x]) ctx.fillRect((x + quiet) * k, (y + quiet) * k, k, k);
    }
  }
  function pack(headerObj, bytes) {
    const h = new TextEncoder().encode(JSON.stringify(headerObj));
    const out = new Uint8Array(4 + h.length + bytes.length);
    new DataView(out.buffer).setUint32(0, h.length);
    out.set(h, 4); out.set(bytes, 4 + h.length);
    return out;
  }
  function unpackMsg(buf) {
    const n = new DataView(buf).getUint32(0);
    const header = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 4, n)));
    return { header, payload: new Uint8Array(buf, 4 + n) };
  }
  function fmtTime(ms) {
    const d = new Date(ms), today = new Date().toDateString() === d.toDateString();
    return (today ? '' : d.toLocaleDateString() + ' ') + d.toLocaleTimeString();
  }
  // Render a history list into <ul>. opts: { activeText, liveText, onClick }
  function historyList(ul, items, opts = {}) {
    ul.innerHTML = '';
    for (const it of items) {
      const li = document.createElement('li');
      if (opts.activeText !== undefined && it.text === opts.activeText) li.classList.add('active');
      const t = document.createElement('div'); t.className = 't'; t.textContent = it.text;
      const m = document.createElement('div'); m.className = 'm';
      const isLive = opts.liveText !== undefined && it.text === opts.liveText;
      m.innerHTML = `<span>${fmtTime(it.last)}</span><span>×${it.count}</span><span>${it.dim}×${it.dim}</span>` +
        (isLive ? '<span class="live">● 当前</span>' : '');
      li.append(t, m);
      if (opts.onClick) li.onclick = () => opts.onClick(it);
      ul.appendChild(li);
    }
  }
  return { packFromMatrix, unpack, draw, pack, unpackMsg, fmtTime, historyList };
})();
