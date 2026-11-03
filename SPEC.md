# Ballast — Especificación v0.2

> Dashboard local para controlar Ollama: ver modelos, expulsarlos de VRAM,
> descargar nuevos por nombre y contabilizar los tokens gastados por cada modelo
> desde el día en que se instaló.
>
> Enfoque inicial: **Ollama**. Diseño preparado para crecer a otros motores
> locales (LM Studio, llama.cpp, vLLM) y a APIs en la nube.
>
> **Regla de oro:** el dashboard es un observador. Nunca debe impedir que el
> equipo siga trabajando. Si el dashboard falla, Ollama y los clientes deben
> seguir funcionando.

---

## 1. Objetivo

Una aplicación **local, sin cuentas ni telemetría**, que se instala con un
comando en Linux, macOS y Windows, y responde a cuatro preguntas:

1. ¿Qué modelos tengo y cuáles están ocupando VRAM ahora mismo?
2. ¿Puedo expulsar uno para liberar memoria sin tocar la terminal?
3. ¿Puedo descargar un modelo nuevo desde aquí?
4. ¿Cuántos tokens he gastado con cada modelo desde que lo descargué?
5. ¿Cuánto espacio libre me queda en los discos y cuánto ocupan los modelos?

Requisitos transversales: atractivo, arranque instantáneo, **un solo binario**,
cero dependencias en tiempo de ejecución, consumo mínimo.

### No objetivos (v1)

- No es un chat. No sustituye a OpenCode, Open WebUI ni a la CLI.
- No entrena, no hace fine-tuning, no convierte modelos.
- No gestiona usuarios, permisos ni equipos.
- No envía nada a Internet salvo la descarga de modelos que pidas.
- **No incluye catálogo ni búsqueda online de modelos** (aplazado, ver §11).

---

## 2. El problema central: contar tokens

Ollama **no guarda** un histórico de consumo. Solo devuelve los contadores de la
petición actual (`prompt_eval_count`, `eval_count`). Cualquier cliente que hable
directamente con `:11434` es invisible para el dashboard.

| Modo | Cómo | Pros | Contras |
|---|---|---|---|
| **A. Pasivo** | Solo `/api/ps` y `/api/tags` | Cero riesgo, cero configuración | No hay tokens, solo estados |
| **B. Proxy** | El dashboard escucha en `:11435` y reenvía a `:11434`, registrando cada petición | Exacto, por cliente, por endpoint, en vivo | Los clientes deben apuntar al proxy |
| **C. Híbrido** | Proxy + importación del log de Ollama | Exacto hacia delante y algo de histórico | El parseo del log es frágil |

La v1 implementa **A + B + C**, con el proxy **activado por defecto y
desactivable desde Ajustes** (decisión 2). Si se desactiva, el dashboard avisa de
que los tokens dejarán de contarse y ofrece la importación desde el log.

### 2.1 Proxy (modo principal)

- Escucha en `127.0.0.1:11435` por defecto, configurable.
- Reenvía **todo** a `OLLAMA_HOST` (`127.0.0.1:11434`): rutas nativas `/api/*` y
  compatibles `/v1/*`.
- De cada petición guarda: fecha, método, ruta, modelo, estado HTTP, si fue
  streaming, tokens de entrada y salida, duración y error. **Nunca el
  contenido** del prompt ni de la respuesta.
- De la respuesta extrae `prompt_eval_count`, `eval_count` y `total_duration`
  de Ollama, o `usage.prompt_tokens` / `usage.completion_tokens` de la API
  compatible con OpenAI.
  - Sin streaming: del JSON de respuesta.
  - Con streaming: del último objeto con `"done": true` o `usage`.
- **Reglas de no interferencia** (críticas), tal como están implementadas:
  - Proxy con transporte propio: no reescribe cuerpos, no añade cabeceras
    propias, no reintenta y no impone timeouts por debajo de los de Ollama.
  - El streaming (NDJSON y SSE) se vuelca al cliente con `Flush` en cada
    escritura; nunca se acumula el cuerpo entero. Solo se guardan los últimos
    256 KB para leer los contadores.
  - Las métricas van a una cola en memoria de 256 elementos con un *worker*.
    Si SQLite falla o la cola se llena, **se descarta la métrica** y la
    petición continúa.
  - Si el proxy no puede conectar con Ollama, devuelve `502` con el error.
- Botón **"Configurar mis clientes"** que muestra el snippet exacto (por ejemplo
  el `baseURL` de `opencode.json`) y avisa si detecta tráfico directo por
  `:11434` que se está perdiendo.

### 2.2 Importación desde el log (histórico)

Ollama escribe en el journal (Linux) o en su log líneas como:

```
slot print_timing: id 0 | task 0 | eval time = 49531.40 ms / 643 tokens
slot print_timing: id 0 | task 0 | total time = 67983.20 ms / 11267 tokens
```

Un importador puntual (acción manual en Ajustes) asocia cada bloque a su modelo y
rellena `usage` con `source = 'log-import'`. No es sincronización continua. Si el
parseo falla, se aborta sin escribir nada.

### 2.3 Fecha de instalación del modelo

`GET /api/tags` da `modified_at` (última escritura del modelo, normalmente la
descarga). Reglas:

1. Si el dashboard hizo el pull, `installed_at` = fin del pull.
2. Si el modelo ya existía, `installed_at` = `modified_at` y
   `installed_at_source = 'estimated'`.
3. Si se puede leer el manifiesto en disco, se usa su fecha de creación y
   `installed_at_source = 'manifest'`.

En la UI, los estimados se muestran con `~` y un tooltip explicativo.

---

## 3. Descarga de modelos (v1, sin catálogo)

Decisión 4: **se aplaza la búsqueda online**. Ollama no tiene API de búsqueda y el
scraping de `ollama.com` se rompe sin avisar, así que no entra en v1.

La v1 ofrece:

- Campo de texto **"Descargar modelo"** con validación de formato `nombre:tag`.
- Autocompletado con los nombres ya instalados y con una lista de los últimos 20
  descargados por el usuario.
- Al confirmar, `POST /api/pull` con progreso en vivo (SSE): estado, bytes,
  porcentaje y velocidad.
- Cancelar en curso y borrar el modelo si quedó a medias.
- El catálogo completo (descripciones, tags, tamaños, filtros por capacidad) queda
  en el backlog como fase F5, generado por CI y empaquetado en el binario.

---

## 4. Arquitectura

### 4.1 Stack: mínimo consumo y máxima portabilidad (decisión 1)

**Backend: Go 1.22+**

- Binario estático único, sin runtime ni instalador.
- `embed.FS` para meter la UI dentro del ejecutable.
- `modernc.org/sqlite` (Go puro) → sin CGO, compilación cruzada trivial para
  linux/darwin/windows × amd64/arm64.
- `httputil.ReverseProxy` para el proxy de Ollama.

**Frontend: HTML + CSS + JavaScript sin framework y sin paso de build**

- Tres archivos servidos desde `embed.FS`: `index.html`, `styles.css`, `app.js`.
- **Cero `node_modules`, cero npm, cero bundler.** El proyecto se compila con
  `go build` y nada más.
- Gráficos con **uPlot** (~45 KB, canvas) vendorizado en `web/vendor/uplot.min.js`.
- Actualización en vivo por `EventSource` (SSE) nativo del navegador.
- CSS propio, ~12 KB, con variables para tema claro/oscuro.

**Por qué esto y no Svelte/React:** el requisito es el mínimo consumo y la máxima
portabilidad. Un framework añade cadena de build (Node), dependencias y superficie
de fallo, y no aporta nada a cinco pantallas. Con JS plano el binario queda en
unos 12–14 MB, arranca en menos de 200 ms y se compila en cualquier máquina con Go
instalado. Si en el futuro la UI crece mucho, se migra a Svelte sin tocar el
backend.

**Por qué no Electron/Tauri:** 150 MB de runtime para algo que se abre en el
navegador en 200 ms.

### 4.2 Estructura del repositorio

```
ballast/
├─ cmd/ballast/main.go           # flags, arranque, servidor
├─ internal/ollama/client.go     # cliente de la API de Ollama
├─ internal/proxy/proxy.go       # proxy :11435 -> :11434 + métricas
├─ internal/store/               # SQLite, migraciones, consultas
├─ internal/api/                 # handlers REST/SSE
├─ internal/autounload/          # política de expulsión segura
├─ web/
│  ├─ index.html
│  ├─ styles.css
│  ├─ app.js
│  └─ vendor/uplot.min.js
├─ .github/workflows/            # build matriz + release
└─ SPEC.md
```

### 4.3 Distribución

- **GitHub Releases** con binarios por plataforma:
  `ballast_{version}_{os}_{arch}` (linux/darwin/windows × amd64/arm64).
- Instalador de una línea:
  `curl -fsSL https://raw.githubusercontent.com/4M4L4K4Ky/Ballast/main/install.sh | sh`
  (detecta OS/arch, descarga, deja el binario en `~/.local/bin`).
- `docker run -p 11435:11435 -v ~/.ballast:/data ghcr.io/4m4l4k4ky/ballast`
- Homebrew tap y winget: fase posterior.
- Licencia MIT.

### 4.4 Datos y configuración

| Sistema | Ruta por defecto |
|---|---|
| Linux | `~/.local/share/ballast/ballast.db` |
| macOS | `~/Library/Application Support/ballast/ballast.db` |
| Windows | `%APPDATA%\ballast\ballast.db` |

| Variable | Por defecto | Uso |
|---|---|---|
| `BALLAST_OLLAMA_URL` | `http://127.0.0.1:11434` | Ollama de destino |
| `BALLAST_LISTEN` | `127.0.0.1:11435` | Puerto de UI y proxy |
| `BALLAST_PROXY` | `true` | Activa el proxy |
| `BALLAST_DB` | ruta XDG | SQLite |
| `BALLAST_RETENTION_DAYS` | `0` (guardar todo) | Purga de `usage` |

---

## 5. Modelo de datos (SQLite)

```sql
CREATE TABLE models (
  name                TEXT PRIMARY KEY,      -- "qwen3:4b"
  digest              TEXT,
  size_bytes          INTEGER,
  family              TEXT,
  parameter_size      TEXT,                  -- "4.0B"
  quantization        TEXT,                  -- "Q4_K_M"
  context_length      INTEGER,
  capabilities        TEXT,                  -- JSON
  installed_at        TEXT NOT NULL,
  installed_at_source TEXT NOT NULL,         -- pull | manifest | estimated
  last_seen_at        TEXT,
  keep_alive          TEXT,                  -- "5m", "0", NULL = por defecto
  pinned              INTEGER DEFAULT 0,     -- protegido contra auto-expulsión
  deleted_at          TEXT
);

CREATE TABLE usage (
  id                INTEGER PRIMARY KEY,
  ts                TEXT NOT NULL,
  model_name        TEXT NOT NULL,
  client            TEXT,
  client_ip         TEXT,
  endpoint          TEXT,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_duration_ms INTEGER,
  load_duration_ms  INTEGER,
  eval_duration_ms  INTEGER,
  status            INTEGER,
  source            TEXT NOT NULL DEFAULT 'proxy'   -- proxy | log-import
);
CREATE INDEX idx_usage_ts ON usage(ts);
CREATE INDEX idx_usage_model_ts ON usage(model_name, ts);

CREATE TABLE events (
  id         INTEGER PRIMARY KEY,
  ts         TEXT NOT NULL,
  type       TEXT NOT NULL,  -- pull | delete | load | unload | auto-unload | error | settings
  model_name TEXT,
  detail     TEXT
);

CREATE TABLE pulls (
  id          INTEGER PRIMARY KEY,
  model_name  TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  status      TEXT NOT NULL,  -- running | done | error | cancelled
  bytes_total INTEGER,
  bytes_done  INTEGER,
  error       TEXT
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

Consultas clave:

- Tokens desde la instalación:
  `SELECT SUM(prompt_tokens+completion_tokens) FROM usage WHERE model_name=? AND ts>=?`
- Cargados: en vivo desde `/api/ps`, nunca desde la BD.
- Serie temporal: `GROUP BY date(ts)` o `strftime('%Y-%m-%dT%H', ts)`.

---

## 6. Expulsión automática sin romper el trabajo (decisión 3)

Objetivo: liberar VRAM cuando no molesta, **jamás** interrumpir algo en curso.

### 6.1 Condiciones para expulsar (todas obligatorias)

1. La auto-expulsión está **activada** en Ajustes. Por defecto **desactivada**.
2. El modelo lleva **más de N minutos sin recibir peticiones** (por defecto 30,
   configurable entre 5 y 240).
3. El proxy **no tiene ninguna petición en vuelo** para ese modelo (contador
   atómico por modelo; se decrementa en `defer`).
4. El modelo **no está marcado como fijado** (`pinned = 1`).
5. Ollama responde a `/api/ps` y confirma que el modelo sigue cargado.
6. El proxy está activo. **Si el proxy está desactivado, la auto-expulsión se
   desactiva sola**: sin visibilidad del tráfico no se puede garantizar que el
   modelo esté libre.

### 6.2 Comportamiento

- Comprobación cada 60 s, en una goroutine aislada.
- Expulsión con `POST /api/generate {"model": m, "keep_alive": 0}`.
- Cada expulsión se registra en `events` con `type = 'auto-unload'` y el motivo
  (`idle 34m, 0 en vuelo`).
- Si falla, se reintenta una vez y se registra el error. Nunca en bucle.
- Botón **"Pausar 1 hora"** visible en Resumen, para cuando estás a punto de
  lanzar un trabajo largo.

### 6.3 Salvaguardas adicionales

- Los modelos `pinned` se muestran con un candado y nunca se tocan.
- Si el dashboard arranca y detecta modelos ya cargados, espera un ciclo completo
  de reloj antes de considerar expulsarlos (evita el arranque en frío).
- Nunca se expulsa el modelo que el usuario acaba de cargar desde la UI en los
  últimos 10 minutos.

---

## 7. API del dashboard (`/api/v1`)

| Método | Ruta | Descripción |
|---|---|---|
| GET | `/status` | Versión de Ollama, alcanzable, proxy on/off, uptime, tamaño BD |
| GET | `/models` | Lista con `installed_at`, `tokens_since_install`, `loaded`, `size_vram`, `pinned` |
| GET | `/models/:name` | Detalle: metadatos, pulls, uso agregado |
| DELETE | `/models/:name` | Borra el modelo y registra el evento |
| POST | `/models/:name/unload` | `{"keep_alive": 0}` |
| POST | `/vram/release` | Expulsa de golpe todos los modelos cargados |
| POST | `/models/:name/load` | Carga con el `keep_alive` indicado |
| PUT | `/models/:name/keepalive` | Fija `keep_alive` por defecto |
| PUT | `/models/:name/pinned` | Protege o desprotege de la auto-expulsión |
| POST | `/models/pull` | Inicia descarga por nombre; devuelve `pull_id` |
| GET | `/pulls/:id/events` | SSE: `status`, `completed`, `total`, `percent`, `speed` |
| DELETE | `/pulls/:id` | Cancela la descarga |
| GET | `/ps` | Modelos cargados ahora, normalizado |
| GET | `/disk` | Espacio total, libre y ocupado de cada disco, con el de modelos destacado |
| GET | `/memory` | Foto de la VRAM real y serie temporal (`?since=ms`) |
| GET | `/game-mode` | Estado del modo juego |
| POST | `/game-mode` | `{"enabled": true}` libera la VRAM y bloquea cargas |
| GET | `/usage/summary?from=&to=` | Totales por modelo y por cliente |
| GET | `/usage/series?from=&to=&bucket=hour\|day` | Serie para gráficos |
| GET | `/usage/export.csv` | Exportación |
| GET | `/events?limit=` | Auditoría |
| GET/PUT | `/settings` | Ajustes, incluido `auto_unload` y `proxy_enabled` |
| GET | `/stream` | SSE: cargados, VRAM, tokens/s en vivo |

El proxy escucha en el mismo puerto: cualquier ruta no reconocida se reenvía a
Ollama, de modo que un cliente puede usar `http://127.0.0.1:11435` como si fuera
Ollama.

---

## 8. Interfaz

Estética: fondo `#0b0d10`, tarjetas `#14171c` con borde sutil, acento `#7c5cff` y
`#22d3ee`, tipografía del sistema, iconos SVG en línea. Sin sombras duras. Modo
claro opcional.

### 8.1 Resumen (pantalla inicial)

- 5 tarjetas: modelos instalados · modelos en VRAM · VRAM usada · **total de los
  modelos en disco** · **tokens de hoy**. La tarjeta de tamaño es la suma de los
  modelos, no el disco del sistema: el disco va aparte, en el panel de
  Almacenamiento (§8.7). La de tokens solo aparece si la base de datos funciona.
- Barra de modelos cargados: tamaño en VRAM, tiempo restante de `keep_alive`,
  candado si está fijado, botón **Expulsar** y botón destacado **Liberar toda la
  VRAM** para dejar la GPU libre al terminar de trabajar. La barra solo aparece
  si hay algo cargado.
- Gráfica de memoria de la GPU en tiempo real (§8.8).
- Panel de consumo: barras de tokens por día (14 días) y tabla por modelo.
- Últimos 10 eventos, incluidos los `auto-unload` con su motivo (pendiente).

### 8.2 Modelos

Tabla ordenable y filtrable:

| Modelo | Tamaño | Quant | Contexto | Capacidades | Instalado | Tokens desde instalación | Estado | Acciones |
|---|---|---|---|---|---|---|---|---|

Implementado ya (v0.1):

- **Buscador** por nombre, familia, quantización y capacidades. Varios términos
  separados por espacios se combinan con AND. Atajo `/` para enfocar.
- **Ordenación** pulsando las cabeceras (nombre, tamaño, contexto, estado), con
  indicador de dirección. Se recuerda en `localStorage`.
- **Copiar nombre** con un botón en la fila y en la ficha.
- **Ficha lateral** al pulsar una fila: estado, tamaño, VRAM, tiempo hasta que se
  libere, familia, arquitectura, parámetros, quantización, contexto, embedding,
  capas, cabezas de atención, formato, capacidades, digest, fecha y parámetros
  del Modelfile. Se sirve desde `GET /api/v1/models/:name` (`/api/show`).
  La ficha se actualiza sola y permite cargar/expulsar sin cerrarla.

Pendiente: columna "Tokens desde instalación" (fase 3), Fijar y Eliminar.

Acciones: Cargar · Expulsar · Fijar · Keep-alive · Eliminar (confirmando el
nombre) · Copiar nombre.

### 8.3 Descargar

- Campo único `nombre:tag` con autocompletado de instalados y recientes.
- Validación de formato antes de llamar a Ollama.
- Lista de descargas activas con porcentaje, velocidad y cancelar.
- Aviso de espacio en disco disponible antes de empezar.

### 8.4 Uso

Implementado (v0.1):

- Panel **Consumo** bajo la tabla de modelos.
- Barras de los últimos 14 días (entrada + salida por día).
- Tabla por modelo: peticiones, tokens de entrada, de salida, total y último uso.
- `GET /api/v1/usage/export.csv` descarga todo el histórico.
- **Límite**: solo se cuenta lo que pasa por `:11435`. Las peticiones que van
  directas a Ollama (`:11434`) no se ven.

Pendiente: filtros por fecha y cliente, tok/s medio y comparativa de velocidad
entre modelos (`eval_duration`).

### 8.5 Ajustes

Host de Ollama · puerto · **proxy on/off** · **auto-expulsión on/off + minutos** ·
modelos fijados · retención · tema · importar histórico del log · snippet para
clientes.

### 8.6 Detalles de UX

- Todo se actualiza por SSE; sin botón de refrescar.
- Destructivas con confirmación escribiendo el nombre.
- Atajos: `/` enfoca la descarga, `Esc` cierra paneles.
- Responsive hasta 380 px; en móvil la tabla pasa a tarjetas.
- Estados vacíos con acción sugerida, nunca una pantalla en blanco.

### 8.7 Almacenamiento

Requisito añadido para la prueba de fuego: el dashboard debe mostrar el espacio
**total, libre y ocupado** de los discos, no solo lo que suman los modelos.

- **Resumen**: el espacio de disco **no** ocupa tarjeta propia. La tarjeta
  "Total modelos" suma el tamaño de los modelos; el disco vive en este panel.
- **Encabezado del panel**: disco donde viven los modelos, con
  `412 GB libres de 931 GB` y una barra de ocupación.
- **Panel "Almacenamiento"** (debajo de Modelos): una fila por sistema de
  archivos con barra, tamaño total, usado, libre y porcentaje. La fila que
  contiene el directorio de modelos va marcada.
- Si el disco de modelos baja del 10 % libre, el panel se pinta en ámbar y
  aparece un aviso: "Quedan 38 GB: un modelo de 70B no cabrá".

#### Contrato de `GET /api/v1/disk`

```json
{
  "models_dir": "/home/skull/.ollama/models",
  "models_dir_exists": true,
  "remote": false,
  "filesystems": [
    {
      "mount": "/",
      "device": "/dev/nvme0n1p2",
      "fstype": "btrfs",
      "total_bytes": 1000204886016,
      "free_bytes": 412000000000,
      "used_bytes": 588204886016,
      "used_percent": 58.8,
      "contains_models": true
    }
  ]
}
```

Reglas de implementación:

1. **Sin dependencias externas.** Linux/macOS con `syscall.Statfs` en un archivo
   con `//go:build unix`; Windows con `syscall.GetDiskFreeSpaceEx` en un archivo
   con `//go:build windows`. Nada de `gopsutil`.
2. En Linux se enumeran los sistemas de archivos de `/proc/self/mounts` y se
   descartan los virtuales: `tmpfs`, `proc`, `sysfs`, `devtmpfs`, `cgroup*`,
   `overlay`, `squashfs`, `fuse.*`, `nsfs`, `ramfs`.
3. En macOS se consultan `/` y cada entrada de `/Volumes/*`.
4. En Windows, `GetLogicalDrives` y solo las unidades con tipo fijo o extraíble.
5. `models_dir` sale de `BALLAST_MODELS_DIR`, luego de `OLLAMA_MODELS`, y por
   defecto `~/.ollama/models`. Si no existe, se usa el directorio personal y
   `models_dir_exists = false`.
6. `used_bytes = total_bytes - free_bytes` (sin contar reservados del sistema,
   que es lo que el usuario percibe como "libre").
7. Si `--ollama` apunta a otra máquina, `remote = true` y la UI muestra un aviso:
   el disco medido es el del equipo donde corre Ballast, no el de Ollama.
8. La llamada es de solo lectura y se cachea 10 s para no martillear el disco.

### 8.8 Memoria de la GPU en tiempo real

La tarjeta "VRAM ocupada" solo sabe lo que Ollama declara. Esta gráfica mide la
GPU de verdad.

- **Fuente**: sysfs (`/sys/class/drm/card*/device/mem_info_vram_*`), eligiendo la
  tarjeta con más VRAM. Sin `radeontop` ni dependencias.
- **Muestreo**: un punto por segundo en el servidor, ventana de 15 minutos
  (~900 puntos, 40 KB en memoria). No toca disco.
- **Panel**: área apilada sobre el total de la GPU, con la parte de Ollama
  (`size_vram` de `/api/ps`) y la del resto del sistema. Línea discontinua para
  GTT, que es donde desbordan los modelos cuando no caben en VRAM.
- **Cabecera**: `11,8 GB de 12,0 GB · 98 % · Ollama 9,8 GB · Otros 2,0 GB`.
- **Transporte**: `GET /api/v1/memory?since=<ms>` devuelve solo los puntos
  nuevos. El cliente pide cada 2 s y dibuja con SVG, sin librerías.
- **Primeros segundos**: la ventana se escala a los datos disponibles para que
  la gráfica no sea una raya pegada al borde derecho.
- **Sin soporte** (macOS, Windows, GPU sin driver amdgpu): el panel se oculta y
  el resto del dashboard sigue igual.

### 8.9 Modo juego

Un interruptor para dejar la GPU libre sin pelearse con la terminal.

- Al activarlo, Ballast expulsa todos los modelos y deja de aceptar cargas:
  `POST /api/v1/models/:name/load` responde `423 Locked`.
- El estado se guarda en `~/.local/share/ballast/state.json` y sobrevive a
  reinicios del binario.
- En la interfaz: botón en la cabecera, franja verde con "Modo juego activo" y
  botones de carga deshabilitados.
- **Límite honesto**: solo afecta a Ballast. Si otro cliente (OpenCode, `ollama
  run`, una app) pide un modelo directamente a `:11434`, Ollama lo carga igual.
  El modo juego deja de ser una promesa cuando el proxy (§2.1) esté en medio.

---

## 9. Requisitos no funcionales

- **Portabilidad**: un binario por plataforma. Sin Go, Node ni Python en destino.
- **Arranque**: < 200 ms hasta la primera pintada.
- **Consumo**: < 25 MB de RAM en reposo, < 1 % de CPU, BD < 5 MB por año.
- **Espacio en disco**: la medición se hace en el equipo donde corre Ballast y
  nunca bloquea la interfaz; si falla, se oculta el panel sin romper nada.
- **Privacidad**: cero telemetría. Solo sale a Internet para los pulls pedidos.
- **Seguridad**: escucha en `127.0.0.1`. Si se expone en LAN, token obligatorio.
- **No interferencia**:
  - El proxy nunca bloquea ni altera una petición.
  - Si SQLite falla, se pierden métricas, no peticiones.
  - Si el dashboard se cae, el cliente debe poder volver a `:11434` con un solo
    cambio de configuración; el `install.sh` deja documentado cómo.
  - Servicio con `Restart=always` (systemd), LaunchAgent (macOS) o tarea
    programada (Windows).
- **Accesibilidad**: contraste AA, teclado, `prefers-reduced-motion`.

---

## 10. Plan por fases

### F0 — Esqueleto (1 día)

Binario que sirve la UI y lee `/api/version`, `/api/tags` y `/api/ps`. SQLite
creada y migrada al arrancar.

**Aceptación**: `go build` produce un binario; abrir `:11435` lista los 13 modelos
reales con su estado.

### F1 — Control de VRAM (1 día)

Expulsar, cargar, keep-alive y fijar. Estado en vivo por SSE.

**Aceptación**: Expulsar sobre un modelo cargado vacía `/api/ps` en < 2 s y la
tarjeta desaparece sin recargar. Además, `GET /api/v1/disk` devuelve los discos
reales del equipo (al menos el que contiene `~/.ollama/models`), con total, libre
y usado coherentes con `df -h`, y el panel de Almacenamiento los pinta.

### F2 — Descargas por nombre (1 día)

Campo de descarga, pull con progreso, cancelar, borrar.

**Aceptación**: descargar `qwen3:1.7b` muestra el porcentaje en vivo y cancelar a
mitad no deja el modelo corrupto.

### F3 — Proxy y contabilidad (2 días)

Proxy `:11435 → :11434`, extracción de contadores (normal y streaming), uso y
gráficos, toggle en Ajustes.

**Aceptación**: 20 peticiones por el proxy desde dos clientes producen
exactamente 20 filas con tokens correctos, y el total coincide con la suma de los
`eval_count` devueltos por Ollama. Desactivar el proxy no rompe ninguna petición.

### F4 — Auto-expulsión segura (1 día)

Política de §6 con todas sus condiciones y su registro de eventos.

**Aceptación**: con un modelo fijado y una petición en curso, ninguna de las dos
cosas se expulsa aunque pase el tiempo límite.

### F5 — Empaquetado (1 día)

Matriz de CI, releases, `install.sh`, Dockerfile, README con capturas.

**Aceptación**: en una máquina limpia de cada OS, el comando de instalación deja
un binario funcional en un solo paso.

### F6 — Catálogo (aplazado)

Búsqueda online con catálogo pregenerado por CI. Se retoma cuando lo básico esté
rodando (§11).

---

## 11. Riesgos

| Riesgo | Impacto | Mitigación |
|---|---|---|
| El proxy se cae y deja a OpenCode sin Ollama | Alto | Servicio con auto-reinicio, proxy sin estado, revertir a `:11434` en un cambio |
| El usuario no apunta sus clientes al proxy y el conteo sale a cero | Alto | Aviso en Resumen, snippet automático, importación desde log |
| La auto-expulsión interrumpe un trabajo largo | Alto | Seis condiciones de §6, off por defecto, fijar modelos, pausa de 1 h |
| `modified_at` no refleja la descarga real | Bajo | Marcar como estimado, usar manifiesto si se puede |
| Streaming sin `done: true` por corte de red | Bajo | Fila con `status=incomplete` y sin tokens |
| SQLite con CGO complica la compilación cruzada | Medio | `modernc.org/sqlite` desde el inicio |
| Exponer el dashboard en LAN sin protección | Alto | Bind a localhost y token obligatorio si se cambia |
| Puerto 11435 ocupado | Bajo | Flag `--listen` y mensaje claro |

---

## 12. Backlog de crecimiento

1. **Catálogo y búsqueda online** (aplazado de v1): JSON generado por CI,
   empaquetado, filtros por capacidad y tamaño.
2. **Multi-host**: varias instancias de Ollama con selector.
3. **Benchmark**: N prompts contra varios modelos, comparando tok/s, VRAM y
   calidad subjetiva.
4. **Coste equivalente**: tokens → euros de API en la nube y vatios/hora.
5. **Playground**: mismo prompt contra varios modelos en paralelo.
6. **Gestión de blobs**: detectar capas huérfanas y liberar disco.
7. **Perfiles de memoria**: "modo 12 GB" que expulsa lo que no quepa antes de
   cargar otro.
8. **Otros motores**: LM Studio, llama.cpp server, vLLM y OpenAI-compatible.
9. **PWA** instalable y notificación de fin de descarga.
10. **Snapshots de configuración**: `Modelfile` y parámetros por modelo.

### Interfaz de proveedor (para no rehacerlo después)

```go
type Provider interface {
    Name() string
    Status(ctx context.Context) (Status, error)
    List(ctx context.Context) ([]Model, error)
    Loaded(ctx context.Context) ([]LoadedModel, error)
    Load(ctx context.Context, model, keepAlive string) error
    Unload(ctx context.Context, model string) error
    Pull(ctx context.Context, model string, progress func(PullProgress)) error
    Delete(ctx context.Context, model string) error
}
```

`OllamaProvider` es la primera implementación. El resto del código no debe
importar nada específico de Ollama.

---

## 13. Nombre

Criterios: inglés, corto, pronunciable en español, sin choque con productos
conocidos y con relación a "modelos locales" o a "ocupar/liberar memoria".

| Nombre | Idea | A favor | En contra |
|---|---|---|---|
| **Ballast** ⭐ | Lastre: el peso que llevas y sueltas para flotar. Los modelos *son* pesos | Metáfora exacta de expulsar, único, suena técnico | Hay que explicarlo una vez |
| **WeightRoom** | Los modelos son *weights* y la sala donde viven | Doble sentido perfecto, memorable | Dos palabras, algo largo |
| **Paddock** | Donde están tus modelos y los sacas a pista | Corto, evocador | Se asocia a F1 |
| **Loadout** | El conjunto de modelos que llevas cargado | Muy claro para jugadores | Informal |
| **Perch** | Los modelos se posan en VRAM y se van | Corto, bonito | Poco descriptivo |
| **Herd** | La manada de llamas | Simpático, temático | Ya no habrá solo llamas |

**Recomendado: `ballast`** — repositorio `ballast`, binario `ballast`, comando
`ballast --listen 127.0.0.1:11435`. Expulsar un modelo es, literalmente, soltar
lastre. Verificado que no hay repositorio con ese nombre; el usuario de GitHub
`ballast` existe, así que el repo es `https://github.com/4M4L4K4Ky/Ballast`.

---

## 14. Entorno de referencia

- Ollama **0.40.3** en `0.0.0.0:11434`, servicio de sistema, AMD Radeon 890M con
  12 GB de VRAM compartida.
- **13 modelos** instalados; `/api/tags` da `capabilities` y `details`.
- `/api/ps` vacío en reposo.
- OpenCode apunta a `http://127.0.0.1:11434`; para contar tokens hay que cambiarlo
  a `http://127.0.0.1:11435`.
- El journal contiene `eval time = ... / N tokens`, útil para la importación.

---

## 15. Decisiones cerradas

| # | Decisión | Resuelto |
|---|---|---|
| 1 | Stack | Go + HTML/CSS/JS sin framework ni npm: mínimo consumo y máxima portabilidad |
| 2 | Proxy | Activado por defecto, desactivable desde Ajustes |
| 3 | Auto-expulsión | Solo con seis condiciones de seguridad; off por defecto; nunca interrumpe trabajo en curso |
| 4 | Catálogo | Aplazado a F6; v1 descarga por nombre |
| 5 | Nombre | `Ballast` (propuesta principal, ver §13) |

---

## 16. Brief para el arquitecto

> Diseña la implementación de un dashboard local para Ollama llamado Ballast
> según `SPEC.md`. Entrega: árbol de paquetes Go, interfaces entre `proxy`,
> `store`, `ollama`, `api` y `autounload`, esquema de migraciones, contrato exacto
> de los eventos SSE y orden de implementación de F0–F4. No escribas la UI.
> Restricciones: un binario estático, SQLite en Go puro, sin CGO, sin framework
> de frontend, escucha en `127.0.0.1:11435`, el proxy nunca puede bloquear ni
> alterar una petición.
