# versecam

Panel multi-agente para la terminal: gestiona varios worktrees en paralelo,
cada uno con sus agentes de IA (Claude Code, kimi code o cualquier CLI
compatible), servicios, puertos reales, memoria, git y PRs. Todo sobre tmux:
las sesiones sobreviven al panel y se puede hacer attach desde cualquier
terminal.

Escrito en Go sin dependencias externas (solo la stdlib). Requiere tmux;
opcionales: docker (infraestructura del proyecto y databases por worktree),
notificaciones de escritorio y un sidecar de voz local (TTS).

## Filosofia

versecam viene con el 80% hecho: el modelo de trabajo (worktrees + agentes +
servicios sobre tmux), el panel y todo lo que se puede detectar solo. El 20%
que falta es lo que cambia de una maquina a otra y de un proyecto a otro, y
ESO se completa con tu propia IA, no con formularios ni con un instalador.

Por eso a lo largo de este README hay **prompts listos para pegarle a tu
agente**: uno para configurar tu proyecto ([Servicios](#servicios)) y otro
para portarlo a tu sistema ([macOS](#macos)). Un sistema pensado para IA,
terminado por IA.

Si completas una parte y sirve para todos (por ejemplo la capa de macOS), es
justo el tipo de aporte que este repo espera de vuelta.

## Plataformas

versecam es **Unix**. Todo el diseño descansa en tmux, en procesos y en
comandos del sistema.

- **Linux**: donde esta desarrollado y probado.
- **macOS**: el nucleo funciona igual (tmux, git, docker y el resto son los
  mismos), pero hay cuatro archivos con comandos que solo existen en Linux.
  Ver [macOS](#macos).
- **Windows**: no, y no es el plan. La arquitectura ES tmux y no hay
  equivalente nativo. Lo que si funciona sin tocar nada es **WSL2 con WSLg**:
  Ubuntu por dentro y la ventana mostrandose nativa en el escritorio.

## Herramientas

versecam no trae nada empaquetado: usa lo que ya tienes instalado y, si algo
falta, esa funcion simplemente no aparece. Nada revienta por una herramienta
ausente.

**Necesarias** (sin esto no arranca o no sirve de nada):

| Herramienta | Para que |
|---|---|
| `go` 1.23+ | compilar (solo al instalar) |
| `tmux` | el motor: cada agente, shell y servicio es una sesion tmux |
| `git` | worktrees, ramas, diffs y el arbol de cambios |
| un CLI de agente | `claude`, `kimi` o cualquiera que corra en una terminal |

**Recomendadas** (cada una habilita una parte del panel):

| Herramienta | Que habilita | Sin ella |
|---|---|---|
| `docker` + compose | infraestructura del proyecto: postgres, redis, rabbit, nats... con estado, logs y start/stop | no aparece el bloque de infraestructura ni el diagrama de dependencias |
| `gh` | PRs con checks en el panel de GitHub | solo ramas y commits locales |
| `ss` (iproute2) | el puerto REAL de cada servicio, cruzando procesos con sockets en LISTEN | las tarjetas no muestran puerto |
| `notify-send` (libnotify) | avisos de escritorio cuando un agente queda esperando | sin notificaciones |
| sidecar TTS local + `paplay`/`aplay`/`ffplay` | lectura por voz de las respuestas del agente | la voz queda apagada |
| `libgtk-3-dev` + `libwebkit2gtk-4.0-dev` | compilar la app de escritorio (solo Linux) | te queda el TUI y el modo web |
| `mysql` en un contenedor | chequeo de databases por worktree (`db_container`) | sin ese chequeo |

```bash
make install           # CLI (TUI + modo web) en ~/.local/bin/versecam
make desktop-install   # app de escritorio + lanzador de aplicaciones
```

## Datos sensibles

En el repo no vive NADA sensible, y esa es una regla del proyecto: lo que
identifica a tu maquina, a tu empresa o a tus cuentas se queda fuera, en
archivos que tu ya tienes.

| Archivo | Que guarda | Donde vive |
|---|---|---|
| `~/.config/versecam/config.json` | tus workspaces, rutas, `github_org`, JQL de Jira y `db_user`/`db_pass` | tu home, nunca el repo |
| `~/.config/versecam/state.json` | perfil por worker, tickets asociados y proyectos recientes | tu home |
| `<proyecto>/.mcp.json` | credenciales de Jira (`ATLASSIAN_*`) | el repo de TU proyecto, ignorado por git |
| `<proyecto>/.env` | la configuracion de tus servicios | el repo de TU proyecto, ignorado por git |

Como los usa el panel, para que sepas exactamente que toca:

- Del `.mcp.json` **reutiliza** las credenciales que ya configuraste para el
  MCP de Jira de Claude Code, y las manda solo a la API de Atlassian. No las
  copia, no las guarda y no las muestra.
- De los `.env` lee **unicamente numeros de puerto**, para deducir que
  servicio se conecta con cual y dibujar el diagrama. Ningun valor de esos
  archivos se muestra en el panel ni sale de tu maquina.
- El unico dato sensible que tu escribes en la configuracion de versecam es
  `db_pass`, y vive en tu `~/.config`, no aqui.

Si agregas una integracion nueva, sigue la misma regla: credenciales por
variables de entorno o por un archivo del proyecto ya ignorado, jamas en el
codigo ni en el config del repo.

## macOS

> No probado todavia: nadie lo ha corrido en un Mac. Lo que sigue es el mapa
> exacto de lo que falta, para que tu IA lo cierre en una sesion.

Requisitos: `brew install go tmux git gh docker` y las Command Line Tools de
Xcode (`xcode-select --install`), que la app de escritorio usa cgo.

El binario **compila tal cual** para macOS (verificado con
`GOOS=darwin go build ./...`), y funciona todo lo que pasa por tmux, git y
docker: workers, agentes, worktrees, servicios, infraestructura, diagrama,
explorador, Jira y PRs. Lo que falla en silencio son cuatro detecciones que
usan comandos que macOS no tiene:

| Archivo | Usa | Equivalente en macOS |
|---|---|---|
| `internal/ws/proc.go` | `/proc/<pid>/cwd` y `/stat` | `lsof -a -p <pid> -d cwd` y `ps -o ppid=,etime=` |
| `internal/ws/ports.go` | `ss -tlnp` | `lsof -iTCP -sTCP:LISTEN -P -n` |
| `internal/ws/notify.go` | `notify-send` | `osascript -e 'display notification'` |
| `internal/voice/speak.go` | `paplay` / `aplay` | `afplay` |

Ninguno rompe nada: sin adaptar, te quedas sin los puertos en las tarjetas,
sin detectar agentes que corren fuera de tmux, sin notificaciones y sin
sonido. El resto del panel funciona.

El `Makefile` tambien es de Linux en la parte de instalacion: crea un
`.desktop` y copia iconos a `hicolor`. En macOS la app de escritorio se
empaqueta como `.app` (lo mas corto es `wails build -tags desktop`).

Prompt para tu IA:

> Porta versecam a macOS. Es un proyecto Go sin dependencias externas y ya
> compila para darwin; lo que falta son cuatro archivos que ejecutan comandos
> que solo existen en Linux: `internal/ws/proc.go` (lee `/proc/<pid>/cwd` y
> `/proc/<pid>/stat` para el cwd, el proceso padre y el uptime),
> `internal/ws/ports.go` (`ss -tlnp` para los puertos en LISTEN y sus PIDs),
> `internal/ws/notify.go` (`notify-send`) y `internal/voice/speak.go`
> (`paplay`/`aplay`). Separa cada uno en `<archivo>_linux.go` y
> `<archivo>_darwin.go` con build tags, dejando la version de Linux intacta y
> escribiendo la de macOS con `lsof`, `ps`, `osascript` y `afplay`; manten las
> mismas firmas para no tocar quien las llama. Agrega al Makefile un target
> que empaquete la app de escritorio como `.app`. Verifica con
> `GOOS=darwin go build ./...`, y en el Mac comprueba que las tarjetas
> muestran puertos y que se detectan los agentes fuera de tmux.

## Estructura del codigo

```
main.go              arranque: flags y wiring de TUI o web
internal/execx/      ejecucion de comandos externos con timeout
internal/tmuxx/      wrappers de tmux + saneo del entorno global
internal/config/     workspaces, perfiles, manifiesto y estado persistente
internal/ws/         dominio: workers, servicios, agentes, git, jira, /proc
internal/voice/      cliente del sidecar de voz (TTS) y anuncios
internal/tui/        el panel de terminal (render, arbol, resaltado)
internal/webui/      API HTTP + SSE + proxy de voz del panel web
web/                 assets del panel web (embebidos en el binario)
```

Las reglas que mantienen esto modular, y que conviene respetar si lo
extiendes con tu IA:

- **Todo lo que habla con el sistema pasa por `internal/execx`** (comandos con
  timeout) o por `internal/tmuxx`. Ningun paquete lanza procesos por su
  cuenta, asi ninguna llamada puede colgar el render.
- **`internal/ws` es el dominio y no sabe de interfaz.** El TUI y la app de
  escritorio son dos frontends del MISMO paquete: si agregas una deteccion
  ahi, aparece en los dos.
- **Cero dependencias externas en el modulo raiz**: solo la stdlib. La app de
  escritorio vive en `desktop/` con su propio `go.mod` porque Wails si trae
  dependencias. Mantener esa frontera es lo que hace que el CLI compile en
  cualquier Unix sin instalar nada.
- **Lo que es de un sistema operativo, aparte.** Los comandos que solo existen
  en Linux estan concentrados en cuatro archivos (ver [macOS](#macos)); al
  portar, se separan con build tags y no se toca a quien los llama.
- **Nada se declara si se puede detectar.** La infraestructura sale de docker
  y de los compose; las dependencias del diagrama, de los puertos en la
  configuracion. Preferimos deducir y equivocarnos poco antes que pedirle al
  usuario otro archivo de configuracion.

## Configuracion global

`~/.config/versecam/config.json` declara perfiles de agente y workspaces:

```json
{
  "profiles": [
    { "name": "personal", "env": { "CLAUDE_CONFIG_DIR": "/home/tu-usuario/.claude-personal" } },
    { "name": "trabajo", "env": { "CLAUDE_CONFIG_DIR": "/home/tu-usuario/.claude-trabajo" } },
    { "name": "kimi", "cmd": "kimi" }
  ],
  "workspaces": [
    {
      "name": "mi-empresa",
      "root": "/home/tu-usuario/mi-empresa",
      "principal_dir": "principal",
      "workers_dir": "workers",
      "manifest": ".worktrees.json",
      "services_conf": ".services.conf",
      "github_org": "mi-org",
      "agent_prefix": "agent",
      "agent_profile": "trabajo"
    },
    {
      "name": "mi-proyecto",
      "root": "/home/tu-usuario/proyectos/mi-proyecto",
      "agent_prefix": "agent-mp",
      "agent_profile": "personal",
      "services_conf": "/home/tu-usuario/.config/versecam/services-mi-proyecto.conf"
    }
  ]
}
```

Todo es opcional menos `name` y `root`. Sin `manifest`, los worktrees del
workspace son los `git worktree` del repo; sin `services_conf` no hay
servicios; `agent_prefix` debe ser unico por workspace para que las sesiones
tmux no colisionen. Si el root mismo es un repo git, se lista como proyecto.
`db_container`/`db_user`/`db_pass` habilitan el chequeo de databases por
worktree contra un MySQL en docker.

`versecam` sin flags detecta el workspace por el directorio actual; fuera de
todo workspace configurado, usa el repo git donde estes parado (ad-hoc).

```bash
versecam                  # TUI, workspace segun cwd
versecam -ws mi-proyecto  # workspace explicito
versecam -web             # servidor web en http://127.0.0.1:9100
versecam -notify=false    # sin notificaciones de escritorio
```

## Perfiles de agente

El bloque `profiles` define con que se lanza cada agente: cuentas de Claude
(via `CLAUDE_CONFIG_DIR`) u otros CLIs (`cmd`). Cada workspace declara su
perfil por defecto con `agent_profile`; `:prof <nombre>` fija el perfil del
WORKER seleccionado (persistido en `state.json`: cada worktree puede usar una
cuenta distinta), `:prof` lista y `:prof -` borra el override. El trust de
folders de Claude es por perfil: usar el correcto evita el dialogo "do you
trust this folder".

## Servicios

Formato de `services_conf` (una linea por servicio):

```
nombre:::directorio-relativo:::comando:::descripcion
```

Si el workspace define `vel_bin` (un gestor propio), las acciones pasan por
el; si no, versecam gestiona las sesiones tmux directamente. El puerto que se
muestra por servicio es el REAL: se cruza el arbol de procesos del pane con
los sockets en LISTEN (`ss -tlnp`).

### Infraestructura detectada sola

El panel de servicios (`Ctrl+S`) trae DOS bloques. Arriba los procesos de
desarrollo del `services_conf`; abajo la infraestructura del proyecto
(postgres, mysql, mongo, redis, rabbit, nats, kafka, elastic, minio,
localstack, mailpit...), que NO hay que declarar en ningun lado:

- Los contenedores que docker ya conoce se atribuyen al worker por la etiqueta
  `com.docker.compose.project.working_dir`, o sea la carpeta desde la que se
  levanto el compose. Cada worktree ve lo suyo. El `db_container` del
  workspace tambien cuenta, aunque no venga de un compose.
- Los servicios de infraestructura declarados en los `docker-compose*.yml` del
  repo que todavia no existen como contenedor salen como `sin crear`, y `s`
  los levanta (`docker compose up -d`, o `docker-compose` si esta app no tiene
  el plugin). Se ignoran los manifiestos de despliegue (`*aws*`, `*prod*`,
  `*staging*`, `*deploy*`, `*ci*`): no describen la maquina local.

Sobre cada uno funcionan las mismas teclas: `s` levanta, `x` apaga, `R`
reinicia y el panel derecho muestra `docker logs` en vivo.

### Mosaico de logs (`enter`)

`enter` fija el servicio seleccionado en un mosaico: cada uno queda en su
propio panel, con su nombre y estado en la cabecera y sus logs en vivo, y se
siguen refrescando todos a la vez. Otro `enter` lo suelta y `0` vacia el
mosaico. Las teclas de scroll (`J/K u f b`) actuan sobre el panel del servicio
seleccionado, que se marca con borde cian; en la lista, los que estan en el
mosaico llevan un `▣`.

El mosaico se guarda por proyecto y worker, asi que te vas a escribirle al
agente, vuelves con `Ctrl+S` y tus terminales siguen agrupadas igual. Tambien
sobrevive a cerrar la app.

### Vista de diagrama (`a`)

Dentro del mismo panel, `a` cambia entre la lista y un diagrama por capas:
aplicaciones arriba, servicios en medio, infraestructura abajo, con las
dependencias dibujadas entre las cajas. La seleccion es la misma (`j/k`,
`s`, `x`, `R` siguen funcionando sobre el nodo resaltado) y el estado se
refresca solo: las lineas animadas son dependencias donde los DOS extremos
estan corriendo.

Las dependencias tampoco se declaran. Salen de cruzar los puertos que menciona
la configuracion de cada servicio (`.env`, `.env.local`, `.env.development`,
`.env.example`) con los puertos que expone la infraestructura y con el puerto
que cada proceso anuncia en su descripcion del `services_conf` — asi un front
encuentra a su backend y el backend a su postgres. Entre contenedores se usa el
`depends_on` del compose. Solo se dibujan las relaciones que van hacia abajo en
la jerarquia: la configuracion se cita en los dos sentidos (el backend tambien
conoce la URL del front) y dibujarlo todo dejaria de leerse como jerarquia.

Lo que la deteccion NO puede adivinar es como se levanta TU codigo (que
comando, en que carpeta, con que puerto): eso es el `services_conf`. Si
compartes el framework, esta es la instruccion para que la IA de quien lo
instale termine la configuracion de su proyecto:

> Configura versecam para este proyecto. 1) Agrega el workspace a
> `~/.config/versecam/config.json` con `name`, `root` y, si aplica,
> `manifest`, `workers_dir`, `principal_dir`, `github_org`, `jira_jql`,
> `agent_prefix` y `vel_prefix` unicos para no chocar con otros proyectos.
> 2) Crea el `services_conf` con una linea por proceso de desarrollo en el
> formato `nombre:::directorio-relativo:::comando:::descripcion`, sacando los
> comandos reales del repo (scripts de package.json, Makefile, docker,
> README). 3) NO declares ahi bases de datos, colas ni caches: versecam
> detecta solo los contenedores del proyecto y los servicios de sus
> docker-compose. 4) Verifica que cada comando corre desde la raiz del repo y
> deja en la descripcion el puerto que expone.

## La vista principal

Split de tres zonas: lista de workers a la izquierda (ordenada por actividad
de IA mas reciente), la terminal del worker seleccionado al centro con
pestañas literales arriba (varios agentes y shells por worker), y el
explorador de archivos a la derecha cuando se abre con `d`.

- Cada agente del worker se lista con su animacion de progreso, su % de
  contexto (leido de la statusline de Claude, con fallback al transcript) y
  la tarea que esta trabajando, actualizandose en vivo.
- Los servicios del worker viven en la barra superior con colores (verde
  corriendo con su puerto, gris apagado).
- Los agentes corriendo en terminales sueltas (fuera de tmux) se detectan por
  su cwd y se listan, aunque no se pueden adjuntar.
- `Tab` pasa el teclado a la pestaña activa (cursor real incluido) y `Ctrl+q`
  vuelve. Nada te saca del panel a pantalla completa.

## Explorador y visor (d)

Arbol del worktree con iconos, mostrando por defecto SOLO los archivos que
trae la rama (contra su merge-base con develop/main), en su estructura de
carpetas comprimida estilo editor; `a` alterna al arbol completo. `enter`
abre el visor integrado: resaltado de sintaxis propio, numeros de linea y
las lineas que trajo la rama con fondo verde solido, inline en el mismo
archivo.

## Universos paralelos (worktrees)

En workspaces sin manifiesto, `:wt <nombre>` crea un `git worktree` del
proyecto completo en `<root>-workers/<nombre>` con su propia rama: una copia
paralela capaz de correr todo el stack. Aparece como worker con sus agentes,
pestañas, servicios y explorador propios. Si el agente de una sesion se muda
a un worktree recien creado, la sesion pasa a pertenecerle sola.

## Voz local

`v` enciende la voz: al terminar cada turno, el panel lee en voz alta la
respuesta del agente usando un sidecar TTS local (HTTP `POST /tts` -> WAV,
por defecto en `:9111`, configurable con `VERSECAM_VOICE_URL`) y reproduce
con paplay/aplay/ffplay (en macOS, `afplay`: ver [macOS](#macos)). Nada sale a
internet.

## Atajos

`?` dentro del panel muestra la ayuda completa con scroll. Resumen: `j/k`
mover, `h/l` pestañas, `Tab` escribir al agente, `s` otro agente, `n` otro
shell, `d` explorador, `z` zoom, `x x` detener, `enter` detalle de servicios,
`:` comandos (`:start <prompt>`, `:send <texto>`, `:prof`, `:wt`, `:stopall!`,
`:help`), `q` salir. Dentro de un attach de tmux: `Ctrl+b d` vuelve.

## Notificaciones

Con notify-send, el panel avisa cuando un agente queda esperando
instrucciones o su sesion muere, con cooldown de 1 minuto por worker.
`-notify=false` lo apaga. En macOS el equivalente es `osascript`; ver
[macOS](#macos).

## Jira

El panel consulta la API REST de Atlassian directo (nada de intermediarios)
con las MISMAS credenciales que el MCP de Jira de Claude Code: las lee del
`.mcp.json` del workspace (o la ruta de `mcp_conf` en config.json). Si ya
usas el MCP, no configuras nada extra. Si no:

1. Genera tu token en https://id.atlassian.com/manage-profile/security/api-tokens
2. Declaralo en el `.mcp.json` de la raiz del workspace (NO lo commitees):

```json
{
  "mcpServers": {
    "jira": {
      "env": {
        "ATLASSIAN_SITE_NAME": "tu-sitio",
        "ATLASSIAN_USER_EMAIL": "tu@correo.com",
        "ATLASSIAN_API_TOKEN": "tu-token"
      }
    }
  }
}
```

El JQL por defecto trae tus tickets asignados sin cerrar; se personaliza por
workspace con `jira_jql` en config.json. En la app de escritorio, `Ctrl+T`
abre el tablero por fases; en el panel web, el boton Jira.

## App de escritorio

`make desktop-install` compila la app Wails (`versecam-desktop`), la instala
en `~/.local/bin` y crea el lanzador de aplicaciones. En Linux requiere
`libgtk-3-dev` y `libwebkit2gtk-4.0-dev`; en macOS, las Command Line Tools de
Xcode y empaquetarla como `.app` (ver [macOS](#macos)). Es la replica del TUI
con mejores herramientas de diseño: tarjetas animadas, terminal cruda con
pestañas, explorador flotante (`Ctrl+D`), Jira (`Ctrl+T`) y voz (`Ctrl+V` /
`Ctrl+Shift+V`), todo por teclado.

La ventana no depende de ninguna terminal: al arrancar se re-lanza en su
propia sesion (`setsid`), asi que la shell que la abrio queda libre y cerrarla
no mata la app. Los logs se van a `~/.config/versecam/desktop.log`;
`VERSECAM_FOREGROUND=1` deja el proceso pegado a la terminal para verlos en
vivo.

Al abrirla desde el buscador de aplicaciones no hay directorio actual del que
deducir el proyecto, asi que la primera pantalla es el selector: recientes
(los ultimos 5, guardados en `state.json`), los workspaces del config.json y
un "abrir otra carpeta..." para cualquier repo suelto. `Ctrl+P` lo reabre para
cambiar de proyecto sin cerrar la app. Lanzada desde una terminal (o con
`VERSECAM_WS=nombre`) sigue abriendo el proyecto del cwd, sin preguntar.
