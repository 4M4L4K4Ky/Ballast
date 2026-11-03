# Ballast

Dashboard local para [Ollama](https://ollama.com): ve tus modelos, expúlsalos de
la VRAM con un clic y —próximamente— contabiliza los tokens gastados con cada uno.

Un solo binario. Sin Node, sin Python, sin contenedores, sin telemetría.

```
go build -o ballast ./cmd/ballast
./ballast
# abre http://127.0.0.1:11435
```

## Estado

Versión temprana. Lo que ya funciona:

- Lista de modelos con tamaño, cuantización, contexto y capacidades.
- Estado en vivo: qué modelos están en VRAM y cuánta memoria ocupan.
- **Expulsar** un modelo de la VRAM (`keep_alive: 0`).
- Interfaz oscura, responsive, sin dependencias de frontend.

Lo que viene, por fases (ver [SPEC.md](SPEC.md)):

| Fase | Contenido |
|---|---|
| F1 | Cargar modelos, keep-alive, fijar contra auto-expulsión |
| F2 | Descargar modelos por nombre con progreso en vivo |
| F3 | Proxy `:11435 → :11434` y contabilidad de tokens por modelo |
| F4 | Auto-expulsión segura (nunca interrumpe trabajo en curso) |
| F5 | Releases multiplataforma, `install.sh`, Docker |
| F6 | Catálogo y búsqueda online (aplazado) |

## Instalación

```sh
curl -fsSL https://raw.githubusercontent.com/4M4L4K4Ky/ballast/main/install.sh | sh
```

Deja el binario en `~/.local/bin/ballast`.

## Uso

```
ballast [flags]

  --listen  dirección de escucha        (por defecto 127.0.0.1:11435)
  --ollama  URL de Ollama               (por defecto http://127.0.0.1:11434)
  --version muestra la versión y sale
```

Variables de entorno equivalentes: `BALLAST_LISTEN`, `BALLAST_OLLAMA_URL`.

## Cómo está hecho

- **Go** con la biblioteca estándar. Binario estático de ~8 MB, sin CGO.
- **HTML + CSS + JavaScript** sin framework ni paso de build. Todo va embebido
  con `embed.FS`.
- Pensado para no estorbar: si Ballast se cae, Ollama y tus clientes siguen
  funcionando igual.

## Desarrollo

```sh
make build     # binario en bin/ballast
make run       # compila y arranca en :11435
make cross     # binarios para linux/darwin/windows × amd64/arm64
make vet test
```

La especificación completa, con modelo de datos, API y criterios de aceptación,
está en [SPEC.md](SPEC.md). El reparto en tareas para los agentes locales está en
[PLAN.md](PLAN.md).

## Licencia

MIT.
