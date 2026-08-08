// Package tmuxx: los wrappers de tmux del panel. Agentes, shells y servicios
// viven en sesiones tmux; este paquete es la unica puerta hacia ellas.
package tmuxx

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"versecam/internal/execx"
)

// Run ejecuta un comando tmux con timeout.
func Run(args ...string) (string, error) {
	return execx.RunTimeout(10*time.Second, "", "tmux", args...)
}

// tmuxSessions se cachea ~1s: el render a 7fps y el refresco por worker la
// consultan constantemente. Crear o matar sesiones invalida el cache.
var (
	sessListMu sync.Mutex
	sessList   []string
	sessListAt time.Time
)

func SessionsInvalidate() {
	sessListMu.Lock()
	sessListAt = time.Time{}
	sessListMu.Unlock()
}

func Sessions() []string {
	sessListMu.Lock()
	defer sessListMu.Unlock()
	if time.Since(sessListAt) < time.Second {
		return sessList
	}
	out, err := Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		sessList, sessListAt = nil, time.Now()
		return nil
	}
	var sessions []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			sessions = append(sessions, line)
		}
	}
	sessList, sessListAt = sessions, time.Now()
	return sessions
}

// exact fuerza coincidencia EXACTA en targets de SESION (has-session,
// kill-session): sin el "=", tmux resuelve por prefijo.
func exact(session string) string {
	return "=" + session
}

// Exists verifica el nombre EXACTO contra la lista de sesiones: los comandos
// de pane (capture, display) no aceptan "=" y resuelven por prefijo, asi que
// una sesion apagada puede "prestarse" la de un worker si no se valida antes.
func Exists(session string) bool {
	for _, name := range Sessions() {
		if name == session {
			return true
		}
	}
	return false
}

func HasSession(name string) bool {
	_, err := Run("has-session", "-t", exact(name))
	return err == nil
}

// tmuxCapture usa -J para unir las lineas partidas por el ancho del pane y
// descarta el relleno vacio del final: un pane de 50 filas casi siempre trae
// decenas de lineas en blanco despues del contenido real.
func Capture(session string, lines int) string {
	if !Exists(session) {
		return ""
	}
	out, err := Run("capture-pane", "-p", "-J", "-t", session, "-S", fmt.Sprintf("-%d", lines))
	if err != nil {
		return ""
	}
	rows := strings.Split(out, "\n")
	last := len(rows)
	for last > 0 && strings.TrimSpace(rows[last-1]) == "" {
		last--
	}
	return strings.Join(rows[:last], "\n")
}

// tmuxCaptureColor conserva los codigos SGR del pane para que el visor
// muestre los mismos colores que la terminal.
func CaptureColor(session string, lines int) string {
	if !Exists(session) {
		return ""
	}
	out, err := Run("capture-pane", "-p", "-e", "-J", "-t", session, "-S", fmt.Sprintf("-%d", lines))
	if err != nil {
		return ""
	}
	rows := strings.Split(out, "\n")
	last := len(rows)
	for last > 0 && strings.TrimSpace(StripSGR(rows[last-1])) == "" {
		last--
	}
	return strings.Join(rows[:last], "\n")
}

// tmuxScreenAndCursor trae en UN solo subproceso la pantalla visible del pane
// (sin scrollback, con colores, filas 1 a 1 contra el pane) y la posicion del
// cursor: la primera linea de la salida es el cursor y el resto la pantalla.
func ScreenAndCursor(session string) (lines []string, x, y int, visible bool) {
	if !Exists(session) {
		return nil, 0, 0, false
	}
	out, err := Run("display-message", "-p", "-t", session,
		"#{cursor_x} #{cursor_y} #{cursor_flag}",
		";", "capture-pane", "-p", "-e", "-t", session)
	if err != nil {
		return nil, 0, 0, false
	}
	head, rest, _ := strings.Cut(out, "\n")
	var flag int
	fmt.Sscan(strings.TrimSpace(head), &x, &y, &flag)
	return strings.Split(rest, "\n"), x, y, flag == 1
}

var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func StripSGR(s string) string {
	return sgrPattern.ReplaceAllString(s, "")
}

func PanePID(session string) string {
	if !Exists(session) {
		return ""
	}
	out, err := Run("list-panes", "-t", session, "-F", "#{pane_pid}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(out, "\n")[0])
}

// tmuxSessionStarted devuelve el epoch en el que se creo la sesion.
func SessionStarted(session string) int64 {
	out, err := Run("display-message", "-p", "-t", session, "#{session_created}")
	if err != nil {
		return 0
	}
	var epoch int64
	fmt.Sscan(strings.TrimSpace(out), &epoch)
	return epoch
}

// tmuxSessionActivity devuelve el epoch de la ultima actividad del pane.
func SessionActivity(session string) int64 {
	out, err := Run("display-message", "-p", "-t", session, "#{session_activity}")
	if err != nil {
		return 0
	}
	var epoch int64
	fmt.Sscan(strings.TrimSpace(out), &epoch)
	return epoch
}

func Kill(session string) error {
	_, _ = Run("send-keys", "-t", session, "C-c")
	time.Sleep(300 * time.Millisecond)
	_, err := Run("kill-session", "-t", exact(session))
	SessionsInvalidate()
	return err
}
