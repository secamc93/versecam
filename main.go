// versecam: panel de agentes Claude sobre tmux. Una TUI (default) y un panel
// web (-web) sobre el mismo backend: workers (git worktrees), agentes,
// servicios y voz local.
package main

import (
	"flag"
	"log"

	"versecam/internal/config"
	"versecam/internal/tmuxx"
	"versecam/internal/tui"
	"versecam/internal/webui"
	"versecam/internal/ws"
	webassets "versecam/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "direccion de escucha del modo web")
	webMode := flag.Bool("web", false, "levantar el servidor web en vez de la TUI")
	wsName := flag.String("ws", "", "workspace de ~/.config/versecam/config.json (default: por cwd)")
	notify := flag.Bool("notify", true, "notificaciones de escritorio cuando un agente queda en espera")
	flag.Parse()
	ws.NotificationsEnabled = *notify

	config.LoadState()
	// OpenProject y no InitWorkspace: el proyecto queda en los recientes, que
	// es lo que ofrece el selector de la app de escritorio.
	config.OpenProject(config.ResolveWorkspace(*wsName))
	// Antes de lanzar nada: un proxy muerto heredado por el servidor tmux deja
	// sin red a todos los agentes y servicios que abra el panel.
	deadProxies := tmuxx.SanitizeEnv()

	if !*webMode {
		tui.Run(deadProxies)
		return
	}
	log.Fatal(webui.Serve(*addr, webassets.FS))
}
