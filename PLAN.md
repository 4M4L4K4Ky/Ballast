# Ballast — Plan de desarrollo para los agentes locales

> Documento de trabajo. Convierte `SPEC.md` en tareas pequeñas, verificables y
> asignables a `orquestador → dev → qa` con modelos locales de 8B.
>
> Regla: **una tarea por sesión**. Se verifica, se commitea y se pasa a la
> siguiente. Nada de pedir "hazme F2 entero".

---

## 1. Punto de partida

Funciona a día de hoy (último commit `19a821a`):

- Binario Go que sirve la UI en `127.0.0.1:11435`.
- API completa del dashboard: `status`, `models`, `models/{name}`, `ps`,
  `memory`, `disk`, `game-mode`, `usage/*`, `load`, `unload`, `vram/release`.
- UI con buscador, ordenación, ficha del modelo, gráfica de memoria real,
  modo juego, descargas, panel de discos, consumo y auto-expulsión.
- Proxy `:11435 → :11434` con contador de tokens en SQLite.
- Auto-expulsión con las seis condiciones de `SPEC.md` §6 y modelos fijados.
- `install.sh`, `Makefile`, workflow de release y licencia.

Lo que **no** hay todavía: releases publicadas (T5.1) y catálogo online
(aplazado).

La tarjeta "Total modelos" es la suma del tamaño de los modelos, no el disco: el
disco real está en el panel de Almacenamiento (T1.1/T1.2).

### Cómo compilar y probar

```sh
cd ~/proyectos/ballast
make build          # bin/ballast
./bin/ballast       # http://127.0.0.1:11435
make vet test
```

Comprobaciones rápidas:

```sh
curl -s localhost:11435/api/v1/status
curl -s localhost:11435/api/v1/models | head -c 400
df -h ~/.ollama/models          # referencia para validar /api/v1/disk
```

---

## 2. Reglas para los agentes

1. **No tocar `SPEC.md`** salvo que una tarea lo pida explícitamente.
2. **No añadir dependencias.** Solo biblioteca estándar de Go, con una
   excepción aprobada: `modernc.org/sqlite` (Go puro, sin CGO) para el
   histórico de consumo. Si una tarea parece necesitar otro paquete, la tarea
   está mal definida: avisa.
3. **No romper lo que ya funciona.** Antes de commitear: `make build` y
   `curl localhost:11435/api/v1/status` deben seguir respondiendo.
4. **Un commit por tarea**, mensaje en español, sin mezclar tareas.
5. **Sin framework de frontend.** `web/` son tres archivos planos.
6. **El proxy nunca altera ni bloquea una petición.** Si algo falla, se pierde la
   métrica, no la petición.
7. Si una tarea requiere más de ~150 líneas nuevas, pártela y dilo.

### Definición de terminado (DoD)

Una tarea está terminada cuando:

- `make build` compila sin warnings y `make vet` pasa.
- El criterio de aceptación se ha comprobado **a mano**, con el comando indicado.
- La UI no muestra errores en la consola del navegador.
- Hay un commit con el ID de la tarea en el mensaje, por ejemplo `T1.1: endpoint /api/v1/disk`.

---

## 3. Cómo se lanza cada tarea

Al `orquestador`, en una sesión nueva, con el directorio de trabajo
`~/proyectos/ballast`:

```
Implementa la tarea T1.1 del archivo PLAN.md. Lee PLAN.md y SPEC.md antes de
escribir código. Cuando termines, ejecuta la comprobación de aceptación y
enséñame la salida.
```

El `orquestador` delega en `dev`. Para revisar después:

```
Revisa la tarea T1.1 con qa: compila, ejecuta la comprobación de aceptación y
dime si cumple el DoD de PLAN.md.
```

---

## 4. Tareas

### Fase 1 — Métricas del sistema

#### T1.1 · Endpoint `GET /api/v1/disk`

> **Estado: hecha (commit `87720b3`).**

- **Objetivo**: devolver total, libre y usado de cada disco real, y marcar el que
  contiene el directorio de modelos.
- **Archivos**: `internal/disk/disk_unix.go`, `internal/disk/disk_windows.go`,
  `internal/disk/disk.go` (tipos y detección de `models_dir`),
  `cmd/ballast/main.go` (registrar la ruta).
- **Contrato**: el de `SPEC.md` §8.7. Cópialo tal cual.
- **Prohibido**: `gopsutil` y cualquier dependencia.
- **Aceptación**:
  ```sh
  curl -s localhost:11435/api/v1/disk | python3 -m json.tool
  df -h ~/.ollama/models
  ```
  El `total_bytes` y `free_bytes` del sistema que contiene los modelos deben
  coincidir con `df -h` con un margen del 1 %. `contains_models` debe ser `true`
  en exactamente una entrada.

#### T1.2 · UI de almacenamiento

> **Estado: hecha (commit `87720b3`).**

- **Objetivo**: añadir el panel "Almacenamiento" con los discos reales. La
  tarjeta "Total modelos" se queda como está: es la suma del tamaño de los
  modelos, no el disco.
- **Archivos**: `web/index.html`, `web/styles.css`, `web/app.js`.
- **Detalle**: tarjeta con `libre de total` y barra; panel con una fila por
  sistema de archivos; ámbar si queda menos del 10 % libre.
- **Aceptación**: abrir `http://127.0.0.1:11435`, comparar con `df -h` y con el
  gestor de discos del sistema. Sin errores en consola.

#### T1.3 · Cargar modelo y keep-alive

> **Estado: hecha (commit `067d66a`).**

- **Objetivo**: `POST /api/v1/models/{name}/load` con `keep_alive` y
  `PUT /api/v1/models/{name}/keepalive`.
- **Archivos**: `cmd/ballast/main.go` (o `internal/api/` si ya se migró).
- **Aceptación**:
  ```sh
  curl -s -X POST localhost:11435/api/v1/models/qwen3:1.7b/load -d '{"keep_alive":"5m"}'
  curl -s localhost:11434/api/ps        # debe aparecer cargado
  ```
  Y en la UI, un botón "Cargar" en los modelos descargados.

#### T1.4 · Gráfica de memoria real de la GPU

> **Estado: hecha (commit `4331384`).**

- **Objetivo**: medir la VRAM de verdad, no solo lo que Ollama declara, y
  dibujarla en vivo.
- **Archivos**: `internal/mem/*`, `cmd/ballast/main.go`, `web/*`.
- **Aceptación**:
  ```sh
  curl -s 'localhost:11435/api/v1/memory?since=0' | python3 -m json.tool
  cat /sys/class/drm/card1/device/mem_info_vram_used
  ```
  El `used_bytes` de la respuesta coincide con sysfs (±1 muestra) y la gráfica
  avanza al cargar y expulsar un modelo.

#### T1.5 · Modo juego

> **Estado: hecha (commit `4331384`).**

- **Objetivo**: liberar la GPU y bloquear cargas desde Ballast hasta desactivarlo.
- **Archivos**: `internal/state/state.go`, `cmd/ballast/main.go`, `web/*`.
- **Aceptación**:
  ```sh
  curl -s -X POST localhost:11435/api/v1/game-mode -d '{"enabled":true}'
  curl -s -o /dev/null -w '%{http_code}\n' -X POST \
    localhost:11435/api/v1/models/qwen3:1.7b/load -d '{"keep_alive":"5m"}'  # 423
  ```
  Tras reiniciar el binario, `status.game_mode` sigue en `true`.

#### T1.6 · Buscador, orden, copiar nombre y ficha

> **Estado: hecha (commit `856a296`).**

- **Objetivo**: encontrar un modelo entre muchos y ver sus detalles sin salir de
  la página.
- **Archivos**: `web/*`, `cmd/ballast/main.go` (`GET /models/{name}`).
- **Aceptación**: `/` enfoca el buscador; pulsar una cabecera ordena; pulsar una
  fila abre la ficha con arquitectura, capas y parámetros del Modelfile.

---

### Fase 2 — Persistencia y descargas

#### T2.1 · SQLite y migraciones

> **Estado: hecha (commit `19a821a`).**

- **Objetivo**: crear la base de datos con el esquema de `SPEC.md` §5 al
  arrancar, de forma idempotente.
- **Archivos**: `internal/store/store.go`, `internal/store/migrations.go`.
- **Nota**: `modernc.org/sqlite` es la única dependencia permitida del proyecto.
  Si no está disponible en el entorno, implementa primero un `store` en memoria
  con la misma interfaz y dilo.
- **Aceptación**: arrancar dos veces seguidas no duplica tablas ni pierde datos;
  el archivo `.db` aparece en `~/.local/share/ballast/`.

#### T2.2 · Descarga por nombre con progreso

> **Estado: hecha (commit `13ba98f`).** Era la prueba de fuego: primera
> escritura de estado nuevo, flujo largo de Ollama y cancelación a mitad.

- **Objetivo**: `POST /api/v1/models/pull` + `GET /api/v1/pulls/{id}/events` (SSE)
  y `DELETE /api/v1/pulls/{id}`.
- **Archivos**: `internal/api/pull.go`, `cmd/ballast/main.go`.
- **Reglas**:
  - El flujo de Ollama (`/api/pull`) es NDJSON; se reenvía al navegador como SSE
    con `status`, `completed`, `total`, `percent` y `done`.
  - Cancelar cierra el contexto de la petición a Ollama. No se borra nada a mano.
  - Un error de red no deja el modelo a medias: se informa y se puede reintentar.
- **Aceptación**: descargar `qwen3:0.6b`, ver el porcentaje avanzar y cancelar a
  mitad; el modelo no queda corrupto (`ollama list` coherente).

#### T2.3 · UI de descargas

> **Estado: hecha (commit `13ba98f`).**

- **Objetivo**: campo `nombre:tag`, lista de descargas activas, cancelar.
- **Archivos**: `web/*`.
- **Aceptación**: descarga completa de principio a fin sin recargar la página.

---

### Fase 3 — Proxy y tokens

#### T3.1 · Proxy de paso

> **Estado: hecha (commit `19a821a`).**

- **Objetivo**: escuchar `:11435` y reenviar a Ollama todo lo que no sea
  `/api/v1/*` ni un archivo estático.
- **Archivos**: `internal/proxy/proxy.go`, `cmd/ballast/main.go`.
- **Aceptación**:
  ```sh
  curl -s localhost:11435/api/tags | head -c 120     # igual que en :11434
  curl -s localhost:11435/v1/models | head -c 120
  ```
  El streaming (`"stream": true`) debe llegar trozo a trozo, sin acumularse.

#### T3.2 · Extracción de contadores

> **Estado: hecha (commit `19a821a`).**

- **Objetivo**: guardar `prompt_eval_count`, `eval_count` y duraciones de cada
  petición, en modo normal y en streaming (`done: true`).
- **Archivos**: `internal/proxy/metrics.go`.
- **Aceptación**: 10 peticiones por el proxy generan 10 filas y la suma coincide
  con los `eval_count` devueltos por Ollama.

#### T3.3 · Endpoints de uso y gráfico

> **Estado: hecha (commit `19a821a`).**

- **Objetivo**: `/usage/summary`, `/usage/series`, `/usage/export.csv` y el
  gráfico de tokens por día en el Resumen.
- **Archivos**: `internal/api/usage.go`, `web/*`.
- **Aceptación**: el gráfico muestra datos reales tras 20 peticiones y el CSV se
  abre bien en LibreOffice.

---

### Fase 4 — Auto-expulsión segura

#### T4.1 · Fijar modelos

> **Estado: hecha.** Tabla `pinned` en SQLite y candado en la tabla y la ficha.

- **Objetivo**: `PUT /api/v1/models/{name}/pinned` persistido en SQLite.
- **Aceptación**: reiniciar Ballast y el candado sigue puesto.

#### T4.2 · Política de expulsión

> **Estado: hecha.** `internal/autounload` con las seis condiciones, contador en
> vuelo en el proxy y pruebas unitarias con reloj falso.

- **Objetivo**: las seis condiciones de `SPEC.md` §6, con contador de peticiones
  en vuelo por modelo y registro en `events`.
- **Aceptación**: con un modelo fijado y otro con una petición en curso, ninguno
  se expulsa al pasar el tiempo límite; uno libre sí, y queda registrado.

#### T4.3 · UI de auto-expulsión

> **Estado: hecha.** Panel con interruptor, minutos, pausa y últimos eventos.

- **Objetivo**: interruptor, minutos configurables, botón "Pausar 1 hora".
- **Aceptación**: activar, esperar el ciclo y ver la expulsión en el historial.

---

### Fase 5 — Distribución

#### T5.1 · Releases

- **Objetivo**: comprobar que el workflow genera los 6 binarios y que
  `install.sh` funciona en un directorio limpio.
- **Aceptación**: `BALLAST_DIR=/tmp/prueba sh install.sh` deja un binario que
  arranca y responde en `:11435`.

#### T5.2 · README con capturas

- **Objetivo**: tres capturas (Resumen, Modelos, Almacenamiento) y badges.
- **Aceptación**: el README explica instalar, arrancar y qué hace, en menos de
  dos pantallas.

---

## 5. Orden recomendado

```
T1.1 → T1.2 → T1.3 → T1.4 → T1.5 → T1.6
     → T2.1 → T2.2 → T2.3
     → T3.1 → T3.2 → T3.3 → T4.1 → T4.2 → T4.3 → T5.1 → T5.2
```

Hechas: T1.1–T1.6, T2.1–T2.3, T3.1–T3.3, T4.1–T4.3.

Queda la fase 5 (distribución). T5.1 necesita el repo público y el tag, que
están en pausa por decisión del usuario; T5.2 (capturas en el README) se puede
hacer ya.

---

## 6. Bitácora de la prueba de fuego

| Tarea | Agente | Estado | Verificado con | Notas |
|---|---|---|---|---|
| T1.1 | sesión principal | hecha | `curl /api/v1/disk` + `df -h` | total y libre coinciden; `/` marcado como `contains_models` |
| T1.2 | sesión principal | hecha | captura de pantalla | barras y aviso de poco espacio; sin errores en consola |
| T1.3 | sesión principal | hecha | `curl /api/ps` | carga con `keep_alive` y expulsión desde tabla y ficha |
| T1.4 | sesión principal | hecha | `mem_info_vram_used` | ±1 muestra; la gráfica responde a cargar y expulsar |
| T1.5 | sesión principal | hecha | `curl` con código 423 | el estado sobrevive al reinicio (`state.json`) |
| T1.6 | sesión principal | hecha | navegador | buscador, orden persistente y ficha desde `/api/show` |
| T2.1 | sesión principal | hecha | doble arranque | esquema idempotente; `ballast.db` en `~/.local/share/ballast` |
| T2.2 | sesión principal | hecha | `curl` SSE + `ollama list` | progreso agregado por digest; cancelar no deja modelo a medias |
| T2.3 | sesión principal | hecha | navegador | barra al 64 % visible; cancelada limpia y sin errores en consola |
| T3.1 | sesión principal | hecha | `curl /api/tags`, `/v1/models` | streaming NDJSON llega trozo a trozo |
| T3.2 | sesión principal | hecha | SQLite + `/usage/summary` | 15/8 sin streaming, 14/20 con streaming; la suma cuadra |
| T3.3 | sesión principal | hecha | navegador + CSV | barras de 14 días y tabla por modelo; CSV correcto |
| T4.1 | sesión principal | hecha | reinicio del binario | el candado sobrevive; tabla `pinned` |
| T4.2 | sesión principal | hecha | `ollama ps` + `events` | expulsó a los 6 min (`idle 6m0s, 0 en vuelo`); 8 pruebas unitarias con reloj falso |
| T4.3 | sesión principal | hecha | navegador | interruptor, minutos, pausa y eventos; candado en fila y ficha |

### Lo que ya está aprendido

- El proxy no puede usar `httputil.ReverseProxy` con `FlushInterval: -1` y a la
  vez leer los contadores: hace falta un `io.Writer` propio que vacíe en cada
  escritura y guarde solo la cola. Es lo que hay en `internal/proxy`.
- La VRAM del compositor (~1,6–2,1 GB) no es de Ollama: la gráfica la separa en
  "Otros" para no confundirla con modelos cargados.
- `used_bytes = total - disponible` da un porcentaje mayor que `df` porque
  incluye los reservados de root. Es lo que el usuario percibe como ocupado.
- El contador solo ve `:11435`. Para que OpenCode cuente, hay que apuntarlo con
  `OLLAMA_HOST=http://127.0.0.1:11435`.

### Señales de que el stack funciona

- El `dev` escribe el archivo con `write` y no pega el código en el chat.
- No inventa rutas ni nombres de archivo.
- `make build` pasa a la primera o tras un intento corregido por él mismo.
- El `qa` encuentra algo real, no inventado.

### Señales de que hay que parar y ajustar

- Repite el mismo error dos veces seguidas.
- Añade una dependencia sin pedir permiso.
- Modifica `SPEC.md` o `PLAN.md`.
- Toca archivos de otra tarea.
