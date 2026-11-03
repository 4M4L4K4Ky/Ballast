# Ballast — Plan de desarrollo para los agentes locales

> Documento de trabajo. Convierte `SPEC.md` en tareas pequeñas, verificables y
> asignables a `orquestador → dev → qa` con modelos locales de 8B.
>
> Regla: **una tarea por sesión**. Se verifica, se commitea y se pasa a la
> siguiente. Nada de pedir "hazme F2 entero".

---

## 1. Punto de partida

Ya existe y funciona (commit `e359c35`):

- Binario Go que sirve la UI en `127.0.0.1:11435`.
- `GET /api/v1/status`, `/models`, `/ps` y `POST /models/{name}/unload`.
- UI oscura con 13 modelos reales, estado en VRAM y botón Expulsar probado.
- `install.sh`, `Makefile`, workflow de release y licencia.

Lo que **no** hay todavía: SQLite, proxy, tokens, descargas, auto-expulsión y la
medición real de discos. La tarjeta "Disco" actual suma el tamaño de los modelos;
es provisional y la sustituye la tarea T1.

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
2. **No añadir dependencias.** Solo biblioteca estándar de Go. Si una tarea
   parece necesitar un paquete externo, la tarea está mal definida: avisa.
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

- **Objetivo**: añadir el panel "Almacenamiento" con los discos reales. La
  tarjeta "Total modelos" se queda como está: es la suma del tamaño de los
  modelos, no el disco.
- **Archivos**: `web/index.html`, `web/styles.css`, `web/app.js`.
- **Detalle**: tarjeta con `libre de total` y barra; panel con una fila por
  sistema de archivos; ámbar si queda menos del 10 % libre.
- **Aceptación**: abrir `http://127.0.0.1:11435`, comparar con `df -h` y con el
  gestor de discos del sistema. Sin errores en consola.

#### T1.3 · Cargar modelo y keep-alive

- **Objetivo**: `POST /api/v1/models/{name}/load` con `keep_alive` y
  `PUT /api/v1/models/{name}/keepalive`.
- **Archivos**: `cmd/ballast/main.go` (o `internal/api/` si ya se migró).
- **Aceptación**:
  ```sh
  curl -s -X POST localhost:11435/api/v1/models/qwen3:1.7b/load -d '{"keep_alive":"5m"}'
  curl -s localhost:11434/api/ps        # debe aparecer cargado
  ```
  Y en la UI, un botón "Cargar" en los modelos descargados.

---

### Fase 2 — Persistencia y descargas

#### T2.1 · SQLite y migraciones

- **Objetivo**: crear la base de datos con el esquema de `SPEC.md` §5 al
  arrancar, de forma idempotente.
- **Archivos**: `internal/store/store.go`, `internal/store/migrations.go`.
- **Nota**: `modernc.org/sqlite` es la única dependencia permitida del proyecto.
  Si no está disponible en el entorno, implementa primero un `store` en memoria
  con la misma interfaz y dilo.
- **Aceptación**: arrancar dos veces seguidas no duplica tablas ni pierde datos;
  el archivo `.db` aparece en `~/.local/share/ballast/`.

#### T2.2 · Descarga por nombre con progreso

- **Objetivo**: `POST /api/v1/models/pull` + `GET /api/v1/pulls/{id}/events` (SSE)
  y `DELETE /api/v1/pulls/{id}`.
- **Archivos**: `internal/api/pull.go`, `cmd/ballast/main.go`.
- **Aceptación**: descargar `qwen3:0.6b`, ver el porcentaje avanzar y cancelar a
  mitad; el modelo no queda corrupto (`ollama list` coherente).

#### T2.3 · UI de descargas

- **Objetivo**: campo `nombre:tag`, lista de descargas activas, cancelar.
- **Archivos**: `web/*`.
- **Aceptación**: descarga completa de principio a fin sin recargar la página.

---

### Fase 3 — Proxy y tokens

#### T3.1 · Proxy de paso

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

- **Objetivo**: guardar `prompt_eval_count`, `eval_count` y duraciones de cada
  petición, en modo normal y en streaming (`done: true`).
- **Archivos**: `internal/proxy/metrics.go`.
- **Aceptación**: 10 peticiones por el proxy generan 10 filas y la suma coincide
  con los `eval_count` devueltos por Ollama.

#### T3.3 · Endpoints de uso y gráfico

- **Objetivo**: `/usage/summary`, `/usage/series`, `/usage/export.csv` y el
  gráfico de tokens por día en el Resumen.
- **Archivos**: `internal/api/usage.go`, `web/*`.
- **Aceptación**: el gráfico muestra datos reales tras 20 peticiones y el CSV se
  abre bien en LibreOffice.

---

### Fase 4 — Auto-expulsión segura

#### T4.1 · Fijar modelos

- **Objetivo**: `PUT /api/v1/models/{name}/pinned` persistido en SQLite.
- **Aceptación**: reiniciar Ballast y el candado sigue puesto.

#### T4.2 · Política de expulsión

- **Objetivo**: las seis condiciones de `SPEC.md` §6, con contador de peticiones
  en vuelo por modelo y registro en `events`.
- **Aceptación**: con un modelo fijado y otro con una petición en curso, ninguno
  se expulsa al pasar el tiempo límite; uno libre sí, y queda registrado.

#### T4.3 · UI de auto-expulsión

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
T1.1 → T1.2 → T1.3 → T2.1 → T2.2 → T2.3
     → T3.1 → T3.2 → T3.3 → T4.1 → T4.2 → T4.3 → T5.1 → T5.2
```

T1.1 y T1.2 son la **prueba de fuego**: tocan backend, frontend y verificación
real contra el sistema, sin riesgo de romper nada de lo que ya funciona.

---

## 6. Bitácora de la prueba de fuego

Rellénala según avancen los agentes.

| Tarea | Agente | Estado | Verificado con | Notas |
|---|---|---|---|---|
| T1.1 | | pendiente | `df -h` | |
| T1.2 | | pendiente | captura | |
| T1.3 | | pendiente | `/api/ps` | |

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
