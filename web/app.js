// Ballast — lógica de la interfaz. Sin dependencias.

const REFRESH_MS = 5000;
let refreshing = false;

const el = {
  statusDot: document.getElementById('status-dot'),
  statusText: document.getElementById('status-text'),
  models: document.getElementById('stat-models'),
  loaded: document.getElementById('stat-loaded'),
  vram: document.getElementById('stat-vram'),
  total: document.getElementById('stat-total'),
  body: document.getElementById('models-body'),
  vramBar: document.getElementById('vram-bar'),
  vramCount: document.getElementById('vram-count'),
  vramDetail: document.getElementById('vram-detail'),
  releaseAll: document.getElementById('release-all'),
  toast: document.getElementById('toast'),
};

// ---------- utilidades ----------

function bytes(n) {
  if (!n) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function ctx(n) {
  if (!n) return '—';
  return n >= 1024 ? `${Math.round(n / 1024)}K` : String(n);
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

let toastTimer;
function toast(msg, kind = '') {
  el.toast.textContent = msg;
  el.toast.className = `toast show ${kind}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.toast.className = 'toast'; }, 3500);
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Error ${res.status}`);
  return data;
}

// ---------- estado ----------

async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const status = await api('/api/v1/status');
    if (status.reachable) {
      el.statusDot.className = 'dot ok';
      el.statusText.textContent = `Ollama ${status.ollama_version} · v${status.version}`;
    } else {
      el.statusDot.className = 'dot err';
      el.statusText.textContent = 'Ollama no responde';
    }

    const { models } = await api('/api/v1/models');
    render(models);
  } catch (err) {
    el.statusDot.className = 'dot err';
    el.statusText.textContent = 'Sin conexión';
    el.body.innerHTML = `<tr><td colspan="7" class="empty">${esc(err.message)}</td></tr>`;
  } finally {
    refreshing = false;
  }
}

function render(models) {
  const loaded = models.filter((m) => m.loaded);
  const vram = loaded.reduce((acc, m) => acc + (m.size_vram || 0), 0);
  const totalSize = models.reduce((acc, m) => acc + (m.size || 0), 0);

  el.models.textContent = models.length;
  el.loaded.textContent = loaded.length;
  el.vram.textContent = vram ? bytes(vram) : '0 B';
  el.total.textContent = bytes(totalSize);

  el.vramBar.hidden = loaded.length === 0;
  if (loaded.length) {
    el.vramCount.textContent = loaded.length === 1
      ? '1 modelo ocupando la GPU'
      : `${loaded.length} modelos ocupando la GPU`;
    el.vramDetail.textContent = `${loaded.map((m) => m.name).join(', ')} · ${bytes(vram)}`;
  }

  if (!models.length) {
    el.body.innerHTML = '<tr><td colspan="7" class="empty">No hay modelos instalados.</td></tr>';
    return;
  }

  const sorted = [...models].sort((a, b) => {
    if (a.loaded !== b.loaded) return a.loaded ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  el.body.innerHTML = sorted.map((m) => {
    const caps = (m.capabilities || []).map((c) => `<span>${esc(c)}</span>`).join('');
    const state = m.loaded
      ? `<span class="badge loaded">En VRAM · ${bytes(m.size_vram)}</span>`
      : '<span class="badge idle">Descargado</span>';
    const action = m.loaded
      ? `<button class="danger" data-unload="${esc(m.name)}">Expulsar</button>`
      : `<button data-load="${esc(m.name)}">Cargar</button>`;
    return `
      <tr>
        <td class="model-name">${esc(m.name)}</td>
        <td class="dim">${bytes(m.size)}</td>
        <td class="dim">${esc(m.details?.quantization_level || '—')}</td>
        <td class="dim">${ctx(m.details?.context_length)}</td>
        <td><div class="caps">${caps}</div></td>
        <td>${state}</td>
        <td>${action}</td>
      </tr>`;
  }).join('');
}

// ---------- acciones ----------

el.body.addEventListener('click', async (event) => {
  const loadBtn = event.target.closest('[data-load]');
  if (loadBtn) {
    const name = loadBtn.dataset.load;
    loadBtn.disabled = true;
    loadBtn.textContent = 'Cargando…';
    try {
      await api(`/api/v1/models/${encodeURIComponent(name)}/load`, {
        method: 'POST',
        body: JSON.stringify({ keep_alive: '10m' }),
      });
      toast(`${name} cargado en VRAM (10 min)`, 'ok');
      await refresh();
    } catch (err) {
      toast(err.message, 'err');
      loadBtn.disabled = false;
      loadBtn.textContent = 'Cargar';
    }
    return;
  }

  const btn = event.target.closest('[data-unload]');
  if (!btn) return;
  const name = btn.dataset.unload;
  btn.disabled = true;
  btn.textContent = 'Expulsando…';
  try {
    await api(`/api/v1/models/${encodeURIComponent(name)}/unload`, { method: 'POST' });
    toast(`${name} expulsado de VRAM`, 'ok');
    await refresh();
  } catch (err) {
    toast(err.message, 'err');
    btn.disabled = false;
    btn.textContent = 'Expulsar';
  }
});

el.releaseAll.addEventListener('click', async () => {
  if (!confirm('¿Expulsar todos los modelos de la VRAM? Se liberará la GPU para jugar.')) return;
  el.releaseAll.disabled = true;
  el.releaseAll.textContent = 'Liberando…';
  try {
    const res = await api('/api/v1/vram/release', { method: 'POST' });
    const n = (res.unloaded || []).length;
    toast(n ? `VRAM liberada: ${n} modelo(s) expulsado(s)` : 'No había nada cargado', 'ok');
    await refresh();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    el.releaseAll.disabled = false;
    el.releaseAll.textContent = 'Liberar toda la VRAM';
  }
});

// ---------- arranque ----------

refresh();
setInterval(refresh, REFRESH_MS);

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) refresh();
});
