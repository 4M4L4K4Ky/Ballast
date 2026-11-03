// Ballast — lógica de la interfaz. Sin dependencias.

const REFRESH_MS = 5000;
const LS_SORT_KEY = 'ballast.sort.key';
const LS_SORT_DIR = 'ballast.sort.dir';

let refreshing = false;
let allModels = [];
let openName = null;   // modelo cuya ficha está abierta
let openDetail = null; // respuesta de /api/v1/models/{name}
let lastFocus = null;

const ui = {
  q: '',
  key: localStorage.getItem(LS_SORT_KEY) || 'state',
  dir: localStorage.getItem(LS_SORT_DIR) || 'asc',
};

const el = {
  statusDot: document.getElementById('status-dot'),
  statusText: document.getElementById('status-text'),
  models: document.getElementById('stat-models'),
  loaded: document.getElementById('stat-loaded'),
  vram: document.getElementById('stat-vram'),
  total: document.getElementById('stat-total'),
  body: document.getElementById('models-body'),
  hint: document.getElementById('models-hint'),
  search: document.getElementById('search'),
  vramBar: document.getElementById('vram-bar'),
  vramCount: document.getElementById('vram-count'),
  vramDetail: document.getElementById('vram-detail'),
  releaseAll: document.getElementById('release-all'),
  drawer: document.getElementById('drawer'),
  overlay: document.getElementById('drawer-overlay'),
  drawerState: document.getElementById('drawer-state'),
  drawerTitle: document.getElementById('drawer-title'),
  drawerBody: document.getElementById('drawer-body'),
  drawerActions: document.getElementById('drawer-actions'),
  drawerClose: document.getElementById('drawer-close'),
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

function count(n) {
  if (!n) return '';
  if (n >= 1e9) return `${(n / 1e9).toFixed(2).replace('.', ',')} B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(0)} M`;
  return String(n);
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function date(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('es-ES', {
    day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

// Tiempo restante hasta que Ollama suelte el modelo.
function remaining(iso) {
  if (!iso) return '';
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return '';
  if (ms <= 0) return 'ahora mismo';
  const min = Math.round(ms / 60000);
  if (min < 1) return 'menos de 1 min';
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  return `${h} h ${min % 60} min`;
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

async function copyName(name) {
  try {
    await navigator.clipboard.writeText(name);
  } catch {
    // Fallback para navegadores sin permiso de portapapeles.
    const ta = document.createElement('textarea');
    ta.value = name;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
  toast(`Copiado: ${name}`, 'ok');
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
    allModels = models;
    render(models);
  } catch (err) {
    el.statusDot.className = 'dot err';
    el.statusText.textContent = 'Sin conexión';
    el.body.innerHTML = `<tr><td colspan="7" class="empty">${esc(err.message)}</td></tr>`;
  } finally {
    refreshing = false;
  }
}

function matches(m) {
  if (!ui.q) return true;
  const hay = [
    m.name,
    m.details?.family,
    m.details?.parameter_size,
    m.details?.quantization_level,
    ...(m.capabilities || []),
  ].join(' ').toLowerCase();
  return ui.q.toLowerCase().split(/\s+/).filter(Boolean).every((t) => hay.includes(t));
}

function compare(a, b) {
  let r = 0;
  switch (ui.key) {
    case 'name': r = a.name.localeCompare(b.name); break;
    case 'size': r = (a.size || 0) - (b.size || 0); break;
    case 'context': r = (a.details?.context_length || 0) - (b.details?.context_length || 0); break;
    case 'state': r = a.loaded === b.loaded ? 0 : (a.loaded ? -1 : 1); break;
    default: break;
  }
  if (r === 0) r = a.name.localeCompare(b.name);
  return ui.dir === 'desc' ? -r : r;
}

function visible(models) {
  return models.filter(matches).sort(compare);
}

function render(models) {
  const loaded = models.filter((m) => m.loaded);
  const vram = loaded.reduce((acc, m) => acc + (m.size_vram || 0), 0);
  const totalSize = models.reduce((acc, m) => acc + (m.size || 0), 0);
  const shown = visible(models);

  el.models.textContent = models.length;
  el.loaded.textContent = loaded.length;
  el.vram.textContent = vram ? bytes(vram) : '0 B';
  el.total.textContent = bytes(totalSize);

  el.hint.textContent = ui.q
    ? `${shown.length} de ${models.length} modelos`
    : `${models.length} modelos · ${loaded.length} en VRAM`;

  el.vramBar.hidden = loaded.length === 0;
  if (loaded.length) {
    el.vramCount.textContent = loaded.length === 1
      ? '1 modelo ocupando la GPU'
      : `${loaded.length} modelos ocupando la GPU`;
    el.vramDetail.textContent = `${loaded.map((m) => m.name).join(', ')} · ${bytes(vram)}`;
  }

  markSortHeaders();

  if (!models.length) {
    el.body.innerHTML = '<tr><td colspan="7" class="empty">No hay modelos instalados.</td></tr>';
    return;
  }
  if (!shown.length) {
    el.body.innerHTML = `<tr><td colspan="7" class="empty">Ningún modelo coincide con «${esc(ui.q)}».</td></tr>`;
    return;
  }

  el.body.innerHTML = shown.map((m) => {
    const caps = (m.capabilities || []).map((c) => `<span>${esc(c)}</span>`).join('');
    const state = m.loaded
      ? `<span class="badge loaded">En VRAM · ${bytes(m.size_vram)}</span>`
      : '<span class="badge idle">Descargado</span>';
    const action = m.loaded
      ? `<button class="danger" data-unload="${esc(m.name)}">Expulsar</button>`
      : `<button data-load="${esc(m.name)}">Cargar</button>`;
    return `
      <tr class="row" data-name="${esc(m.name)}" tabindex="0">
        <td class="model-name">
          <span>${esc(m.name)}</span>
          <button class="copy" data-copy="${esc(m.name)}" title="Copiar nombre"
                  aria-label="Copiar nombre de ${esc(m.name)}">⧉</button>
        </td>
        <td class="dim">${bytes(m.size)}</td>
        <td class="dim">${esc(m.details?.quantization_level || '—')}</td>
        <td class="dim">${ctx(m.details?.context_length)}</td>
        <td><div class="caps">${caps}</div></td>
        <td>${state}</td>
        <td>${action}</td>
      </tr>`;
  }).join('');

  syncDrawer();
}

function markSortHeaders() {
  document.querySelectorAll('th[data-sort]').forEach((th) => {
    const active = th.dataset.sort === ui.key;
    th.classList.toggle('sorted', active);
    th.classList.toggle('desc', active && ui.dir === 'desc');
    th.setAttribute('aria-sort', active ? (ui.dir === 'desc' ? 'descending' : 'ascending') : 'none');
  });
}

// ---------- ficha del modelo ----------

function detailRows(m) {
  const rows = [];
  const add = (label, value, mono = false) => {
    if (value === '' || value === null || value === undefined) return;
    rows.push(`<dt>${esc(label)}</dt><dd${mono ? ' class="mono"' : ''}>${value}</dd>`);
  };

  add('Tamaño en disco', bytes(m.size));
  if (m.loaded) {
    add('Ocupa en VRAM', bytes(m.size_vram));
    if (m.expires_at) add('Se libera en', esc(remaining(m.expires_at)));
  }
  add('Familia', esc(m.details?.family || ''));
  add('Arquitectura', esc(m.architecture || ''));
  add('Parámetros', esc(m.details?.parameter_size || count(m.parameter_count)));
  add('Quantización', esc(m.details?.quantization_level || ''));
  const context = m.context_length || m.details?.context_length;
  add('Contexto', context ? `${ctx(context)} (${context.toLocaleString('es-ES')} tokens)` : '');
  add('Embedding', m.embedding_length ? m.embedding_length.toLocaleString('es-ES') : '');
  add('Capas', m.block_count ? String(m.block_count) : '');
  add('Cabezas de atención', m.head_count ? String(m.head_count) : '');
  add('Formato', esc(m.details?.format || m.format || ''));
  const caps = (m.capabilities || []).map((c) => `<span class="badge">${esc(c)}</span>`).join(' ');
  add('Capacidades', caps);
  add('Digest', esc((m.digest || '').slice(0, 16)), true);
  add('Modificado', esc(date(m.modified_at)));

  return rows.join('');
}

function renderDetail() {
  const m = openDetail;
  if (!m) return;

  el.drawerState.innerHTML = m.loaded
    ? `<span class="badge loaded">En VRAM · ${bytes(m.size_vram)}</span>`
    : '<span class="badge idle">Descargado</span>';
  el.drawerTitle.textContent = m.name;

  let html = `<dl class="detail-grid">${detailRows(m)}</dl>`;
  if (m.parameters) {
    html += `<p class="detail-section">Parámetros del Modelfile</p>
             <pre class="detail-pre">${esc(m.parameters)}</pre>`;
  }
  el.drawerBody.innerHTML = html;

  el.drawerActions.innerHTML = `
    <button data-copy="${esc(m.name)}">Copiar nombre</button>
    ${m.loaded
      ? `<button class="danger" data-unload="${esc(m.name)}">Expulsar</button>`
      : `<button data-load="${esc(m.name)}">Cargar</button>`}`;
}

async function openDetailFor(name) {
  openName = name;
  openDetail = null;
  lastFocus = document.activeElement;

  el.drawer.hidden = false;
  el.overlay.hidden = false;
  document.body.classList.add('drawer-open');
  el.drawerTitle.textContent = name;
  el.drawerState.textContent = '—';
  el.drawerBody.innerHTML = '<p class="empty">Cargando ficha…</p>';
  el.drawerActions.innerHTML = '';
  el.drawerClose.focus();

  try {
    const detail = await api(`/api/v1/models/${encodeURIComponent(name)}`);
    if (openName !== name) return; // se abrió otra ficha mientras cargaba
    openDetail = detail;
    renderDetail();
  } catch (err) {
    el.drawerBody.innerHTML = `<p class="empty">${esc(err.message)}</p>`;
  }
}

function closeDrawer() {
  openName = null;
  openDetail = null;
  el.drawer.hidden = true;
  el.overlay.hidden = true;
  document.body.classList.remove('drawer-open');
  if (lastFocus && document.contains(lastFocus)) lastFocus.focus();
}

// Refresca solo el estado de la ficha abierta, sin volver a pedir /api/show.
function syncDrawer() {
  if (!openName) return;
  const m = allModels.find((x) => x.name === openName);
  if (!m) {
    closeDrawer();
    return;
  }
  if (!openDetail) return;
  const changed = openDetail.loaded !== m.loaded
    || openDetail.size_vram !== m.size_vram
    || openDetail.expires_at !== m.expires_at;
  if (!changed) return;

  openDetail.loaded = m.loaded;
  openDetail.size_vram = m.size_vram;
  openDetail.expires_at = m.expires_at;
  renderDetail();
}

// ---------- acciones ----------

async function loadModel(name, btn) {
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Cargando…';
  }
  try {
    await api(`/api/v1/models/${encodeURIComponent(name)}/load`, {
      method: 'POST',
      body: JSON.stringify({ keep_alive: '10m' }),
    });
    toast(`${name} cargado en VRAM (10 min)`, 'ok');
    await refresh();
  } catch (err) {
    toast(err.message, 'err');
    if (btn) {
      btn.disabled = false;
      btn.textContent = 'Cargar';
    }
  }
}

async function unloadModel(name, btn) {
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Expulsando…';
  }
  try {
    await api(`/api/v1/models/${encodeURIComponent(name)}/unload`, { method: 'POST' });
    toast(`${name} expulsado de VRAM`, 'ok');
    await refresh();
  } catch (err) {
    toast(err.message, 'err');
    if (btn) {
      btn.disabled = false;
      btn.textContent = 'Expulsar';
    }
  }
}

// Un solo manejador para la tabla y la ficha lateral.
document.addEventListener('click', async (event) => {
  const copy = event.target.closest('[data-copy]');
  if (copy) {
    event.stopPropagation();
    await copyName(copy.dataset.copy);
    return;
  }

  const load = event.target.closest('[data-load]');
  if (load) {
    await loadModel(load.dataset.load, load);
    return;
  }

  const unload = event.target.closest('[data-unload]');
  if (unload) {
    await unloadModel(unload.dataset.unload, unload);
    return;
  }

  const row = event.target.closest('tr.row');
  if (row) openDetailFor(row.dataset.name);
});

// Teclado: Enter sobre una fila abre la ficha.
el.body.addEventListener('keydown', (event) => {
  if (event.key !== 'Enter' && event.key !== ' ') return;
  const row = event.target.closest('tr.row');
  if (!row) return;
  event.preventDefault();
  openDetailFor(row.dataset.name);
});

el.drawerClose.addEventListener('click', closeDrawer);
el.overlay.addEventListener('click', closeDrawer);

document.addEventListener('keydown', (event) => {
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || '');
  if (event.key === '/' && !typing) {
    event.preventDefault();
    el.search.focus();
    return;
  }
  if (event.key === 'Escape') {
    if (!el.drawer.hidden) closeDrawer();
    else if (document.activeElement === el.search) el.search.blur();
  }
});

// ---------- buscador y orden ----------

el.search.addEventListener('input', () => {
  ui.q = el.search.value.trim();
  render(allModels);
});

document.querySelectorAll('th[data-sort]').forEach((th) => {
  th.addEventListener('click', () => {
    const key = th.dataset.sort;
    if (ui.key === key) {
      ui.dir = ui.dir === 'asc' ? 'desc' : 'asc';
    } else {
      ui.key = key;
      ui.dir = 'asc';
    }
    localStorage.setItem(LS_SORT_KEY, ui.key);
    localStorage.setItem(LS_SORT_DIR, ui.dir);
    render(allModels);
  });
});

// ---------- VRAM ----------

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
