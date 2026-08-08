// versecam-desktop: la app de escritorio (Wails). Es el MISMO panel web en
// una ventana nativa: los assets embebidos y la API HTTP/SSE de
// internal/webui se sirven directo al webview, sin puerto abierto.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"versecam/assets"
	"versecam/internal/config"
	"versecam/internal/tmuxx"
	"versecam/internal/voice"
	"versecam/internal/webui"
	"versecam/internal/ws"
	webassets "versecam/web"
)

// detachEnv marca al proceso ya desprendido: el hijo lo ve y sigue derecho.
// terminalEnv le cuenta al hijo si al padre lo lanzaron desde una shell, dato
// que el hijo ya no puede averiguar solo (se quedo sin terminal).
const (
	detachEnv   = "VERSECAM_DETACHED"
	terminalEnv = "VERSECAM_FROM_TERMINAL"
)

// fromTerminal: nos abrieron escribiendo el comando en una shell, no desde el
// buscador de aplicaciones. Se pregunta por la terminal de CONTROL y no por
// stdin, porque stdin miente en los dos sentidos: el lanzador de aplicaciones
// entrega /dev/null (que tambien es un dispositivo de caracteres) y una
// redireccion en la shell tapa el tty sin que el proceso deje de depender de
// ella.
func fromTerminal() bool {
	if os.Getenv(detachEnv) == "1" {
		return os.Getenv(terminalEnv) == "1"
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// detach vuelve a lanzar el binario en su propia sesion (setsid) y mata al
// padre. Asi la ventana no depende de la terminal que la abrio: el shell
// recupera el prompt al instante y cerrar esa terminal ya no manda SIGHUP a la
// app. La salida se va a ~/.config/versecam/desktop.log porque el proceso
// desprendido no tiene a donde escribir.
// VERSECAM_FOREGROUND=1 desactiva esto (util para ver los logs en vivo).
func detach() {
	if os.Getenv(detachEnv) == "1" || os.Getenv("VERSECAM_FOREGROUND") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return // sin ruta propia no hay re-exec: seguimos pegados a la terminal
	}
	out := logFile()
	terminal := "0"
	if fromTerminal() {
		terminal = "1"
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), detachEnv+"=1", terminalEnv+"="+terminal)
	cmd.Stdin = nil
	if out != nil { // sin log, exec le da /dev/null al hijo
		cmd.Stdout = out
		cmd.Stderr = out
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "versecam-desktop: no se pudo desprender:", err)
		return
	}
	os.Exit(0)
}

// logFile abre el log del proceso desprendido; si no se puede, /dev/null sirve
// igual (nil deja los descriptores cerrados y cualquier print rompe la app).
func logFile() *os.File {
	home, err := os.UserHomeDir()
	if err == nil {
		dir := filepath.Join(home, ".config", "versecam")
		if os.MkdirAll(dir, 0o755) == nil {
			f, err := os.OpenFile(filepath.Join(dir, "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err == nil {
				return f
			}
		}
	}
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	return f
}

func main() {
	detach()

	config.LoadState()
	// De donde sale el proyecto: -ws explicito o el cwd cuando nos lanzaron
	// desde una shell. Desde el buscador de aplicaciones el cwd no dice nada
	// util (suele ser ~), asi que se arranca SIN workspace y el panel abre el
	// selector de proyecto con los recientes.
	if name := os.Getenv("VERSECAM_WS"); name != "" || fromTerminal() {
		config.OpenProject(config.ResolveWorkspace(name))
	}
	// Igual que el TUI: quitar proxys muertos del entorno global de tmux
	// antes de lanzar nada.
	_ = tmuxx.SanitizeEnv()

	// Voz en vivo: cuando el toggle esta encendido, cada agente que termina
	// su turno se lee en voz alta (mismo anunciador del TUI).
	go func() {
		var previous map[string]string
		for {
			// HoldProject: este bucle es el unico lector del contexto que no
			// viene de una peticion del panel, y cambiar de proyecto lo
			// reescribe entero.
			var view ws.WorkspaceView
			config.HoldProject(func() { view = ws.List() })
			voice.Announce(previous, view)
			previous = ws.AgentStatuses(view)
			time.Sleep(3 * time.Second)
		}
	}()

	// La app de escritorio usa SU frontend (web/desk): la replica del TUI
	// con tarjetas + terminal cruda, no el panel web viejo.
	deskFS, err := fs.Sub(webassets.FS, "desk")
	if err != nil {
		fmt.Fprintln(os.Stderr, "versecam-desktop:", err)
		os.Exit(1)
	}

	err = wails.Run(&options.App{
		Title:  "versecam",
		Width:  1500,
		Height: 950,
		AssetServer: &assetserver.Options{
			Assets:  deskFS,
			Handler: webui.Mux(webassets.FS), // /api/* cae aqui
		},
		BackgroundColour: &options.RGBA{R: 5, G: 6, B: 13, A: 255},
		Linux: &linux.Options{
			// El mismo logo del lanzador, ahora tambien como icono de la
			// ventana: es lo que pinta la barra de tareas / el dock cuando no
			// logra casar la ventana con el .desktop.
			Icon:             assets.LogoPNG,
			WebviewGpuPolicy: linux.WebviewGpuPolicyOnDemand,
			ProgramName:      "versecam",
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "versecam-desktop:", err)
		os.Exit(1)
	}
}
