package tui

// TUI del panel: la misma data que la web, pero dentro de la terminal.
// Sin dependencias externas: modo raw via stty y render con secuencias ANSI.
// Se maneja al estilo vim: modo normal (j/k/gg/G), busqueda con /, modo
// comando con : y navegacion lista -> detalle con l/h.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
	"versecam/internal/hl"
	"versecam/internal/tmuxx"
	"versecam/internal/voice"
	"versecam/internal/ws"
)

// Paleta cyberpunk del panel web (web/index.html) en truecolor ANSI.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[38;2;95;112;153m"  // muted azul acero
	ansiGreen   = "\x1b[38;2;166;255;43m"  // lime neon
	ansiYellow  = "\x1b[38;2;255;179;0m"   // amber
	ansiRed     = "\x1b[38;2;255;51;85m"   // rojo neon
	ansiCyan    = "\x1b[38;2;0;240;255m"   // cyan neon
	ansiMagenta = "\x1b[38;2;255;43;214m"  // magenta neon
	ansiText    = "\x1b[38;2;200;214;240m" // texto base
	ansiSep     = "\x1b[38;2;29;43;77m"    // lineas del marco
	ansiCursor  = "\x1b[48;2;0;240;255m\x1b[38;2;5;6;13m \x1b[0m"
	ansiMarkBG  = "\x1b[48;2;18;72;36m" // fondo verde de linea traida por la rama
	ansiBGOff   = "\x1b[49m"
)

const (
	modeNormal = iota
	modeCommand
	modeSearch
)

// Bordes redondeados de las tarjetas de worker. Escapes unicode para cumplir
// .claude/rules/no-unicode-in-code.md.
const (
	boxTL = "\u256d" // esquina superior izquierda redondeada
	boxTR = "\u256e"
	boxBL = "\u2570"
	boxBR = "\u256f"
	boxH  = "\u2500"
	boxV  = "\u2502"
)

const (
	viewList = iota
	viewDetail
)

// Frames del "atomo": un electron orbitando en caracteres braille. Escapes
// unicode para cumplir .claude/rules/no-unicode-in-code.md.
var orbitFrames = []string{
	"\u28fe", "\u28fd", "\u28fb", "\u283f", "\u287f", "\u28df", "\u28ef", "\u28f7",
}

func orbit(frame int) string {
	return orbitFrames[frame%len(orbitFrames)]
}

// Pulso "respirando" para estados quietos pero vivos (waiting).
var pulseFrames = []string{"\u00b7", "o", "O", "o"}

func pulse(frame int) string {
	return pulseFrames[(frame/3)%len(pulseFrames)]
}

// agentSpinner alterna cyan y magenta en cada vuelta completa del electron.
func agentSpinner(frame int) string {
	color := ansiCyan
	if (frame/len(orbitFrames))%2 == 1 {
		color = ansiMagenta
	}
	return color + orbit(frame) + ansiReset
}

// sttyCommand opera sobre la terminal real del usuario, por eso hereda stdin.
func sttyCommand(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func terminalSize() (rows, cols int) {
	out, err := sttyCommand("size")
	if err != nil {
		return 24, 80
	}
	fmt.Sscan(out, &rows, &cols)
	if rows <= 0 || cols <= 0 {
		return 24, 80
	}
	return rows, cols
}

type detailUpdate struct {
	generation int
	detail     *ws.WorkerDetail
	err        error
}

type tuiApp struct {
	savedStty      string
	view           ws.WorkspaceView
	selected       int
	message        string
	pendingStop    time.Time
	pendingStopAll time.Time
	mode           int
	input          []byte
	filter         string
	pendingG       bool

	viewMode      int
	detail        *ws.WorkerDetail
	detailID      string
	detailGen     int
	detailEvents  chan detailUpdate
	lastDetailReq time.Time
	svcSelected   int
	reader        *stdinReader

	focusRight   bool
	zoomed       bool
	helpMode     bool
	helpOffset   int
	diffMode     bool
	diffFiles    []ws.DiffEntry
	viewerMarks  map[int]bool
	viewerAllNew bool
	treeRoot     *treeNode
	treeFlat     []treeItem
	treeSel      int
	changed      map[string]string
	activeTab    map[string]string
	dirtyDirs    map[string]bool
	viewerRel    string
	viewerAbs    string
	viewerMtime  time.Time
	floatViewH   int
	branches     map[string]string
	diffAdds     int
	diffDels     int
	viewerLines  []string
	viewerOffset int
	treeAll      bool
	treeRootPath string
	rightSess    string
	monitor      *paneMonitor
	snap         *paneSnapshot
	wsEvents     chan ws.WorkspaceView
	sizedSess    string
	sizedW       int
	sizedH       int
	cursorSeq    string
	frame        int
	cachedRows   int
	cachedCols   int
	sizeFrame    int
	previewCache string
	previewFor   string
	previewFrame int
}

// size cachea el stty size: con animaciones a ~7 fps no se puede lanzar un
// subproceso por frame.
func (app *tuiApp) size() (rows, cols int) {
	if app.cachedCols == 0 || app.frame-app.sizeFrame >= 20 {
		app.cachedRows, app.cachedCols = terminalSize()
		app.sizeFrame = app.frame
	}
	return app.cachedRows, app.cachedCols
}

// agentPreview cachea la captura de tmux: se refresca cada ~1s aunque el
// panel repinte mas seguido por la animacion.
func (app *tuiApp) agentPreview(id string, lines int) string {
	if app.previewFor != id || app.frame-app.previewFrame >= 7 {
		app.previewCache = ws.AgentOutput(id, lines)
		app.previewFor = id
		app.previewFrame = app.frame
	}
	return app.previewCache
}

// Run arranca el panel. deadProxies son los proxys muertos que se quitaron
// del entorno global de tmux al arrancar, para avisarlo en pantalla.
func Run(deadProxies []string) {
	saved, err := sttyCommand("-g")
	if err != nil {
		fmt.Fprintln(os.Stderr, "se necesita una terminal interactiva (tty) para el modo -tui")
		os.Exit(1)
	}
	app := &tuiApp{savedStty: saved, detailEvents: make(chan detailUpdate, 4), activeTab: map[string]string{}}
	app.monitor = newPaneMonitor()
	// El panel arranca con el foco EN la terminal: escribir es lo primario y
	// la navegacion vive en Ctrl (j/k workers, h/l pestañas, s agente).
	// Ctrl+q baja a la lista para lo demas (:, /, d, detalle).
	app.focusRight = true

	if _, err := sttyCommand("raw", "-echo"); err != nil {
		fmt.Fprintln(os.Stderr, "no se pudo poner la terminal en modo raw:", err)
		os.Exit(1)
	}
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer app.restoreTerminal()

	app.reader = newStdinReader()
	go app.reader.loop()
	keyEvents := app.reader.events
	defer func() { _ = syscall.SetNonblock(0, false) }()

	workspaceEvents := make(chan ws.WorkspaceView, 1)
	app.wsEvents = workspaceEvents
	go func() {
		var previous map[string]string
		for {
			view := ws.List()
			ws.NotifyTransitions(previous, view)
			voice.Announce(previous, view)
			previous = ws.AgentStatuses(view)
			workspaceEvents <- view
			time.Sleep(3 * time.Second)
		}
	}()

	if len(deadProxies) > 0 {
		app.message = "proxys muertos quitados del entorno de tmux (" + strings.Join(deadProxies, ", ") +
			"): dejaban sin red a todo lo que lanza el panel. Reinicia las sesiones viejas para que tomen el entorno limpio"
	}

	// Si el panel arranca DENTRO de una sesion de agente, se monitorearia a
	// si mismo (bucle de espejos). Avisar de entrada.
	if os.Getenv("TMUX") != "" {
		if name, err := tmuxx.Run("display-message", "-p", "#{session_name}"); err == nil {
			session := strings.TrimSpace(name)
			if strings.HasPrefix(session, config.AgentPrefix+"-") || strings.HasPrefix(session, "agent-") {
				app.message = "OJO: el panel corre dentro de la sesion del agente " + session + "; abrelo en una terminal normal"
			}
		}
	}

	repaint := time.NewTicker(150 * time.Millisecond)
	defer repaint.Stop()

	for {
		select {
		case view := <-workspaceEvents:
			// La lista se reordena por actividad: mantener la seleccion en el
			// MISMO worker, no en la misma posicion.
			selectedID := ""
			if worker := app.currentWorker(); worker != nil {
				selectedID = worker.ID
			}
			app.view = view
			app.clampSelection()
			if selectedID != "" {
				for index, candidate := range app.visibleWorkers() {
					if candidate.ID == selectedID {
						app.selected = index
						break
					}
				}
			}
		case update := <-app.detailEvents:
			if update.generation == app.detailGen && app.viewMode == viewDetail {
				if update.err == nil {
					app.detail = update.detail
					app.clampService()
				} else {
					app.message = update.err.Error()
				}
			}
		case snap := <-app.monitor.updates:
			if snap.session == app.rightSess {
				app.snap = &snap
			}
		case <-repaint.C:
			app.frame++
			// El detalle se refresca solo mientras este abierto.
			if app.viewMode == viewDetail && time.Since(app.lastDetailReq) > 5*time.Second {
				app.requestDetail(false)
			}
			// El visor flotante sigue los cambios que la IA hace en disco.
			app.refreshViewer()
		case key := <-keyEvents:
			if quit := app.handleKey(key, keyEvents); quit {
				return
			}
			// Coalescer: procesar TODAS las teclas pendientes antes de repintar.
			// Pegar texto o teclear rapido no debe pagar un render por byte.
			for drained := false; !drained; {
				select {
				case key = <-keyEvents:
					if quit := app.handleKey(key, keyEvents); quit {
						return
					}
				default:
					drained = true
				}
			}
		}
		app.render()
	}
}

func (app *tuiApp) restoreTerminal() {
	// \x1b[0 q devuelve la forma del cursor al default de la terminal.
	fmt.Print("\x1b[0 q\x1b[?25h\x1b[?1049l")
	_, _ = sttyCommand(app.savedStty)
}

// stdinReader lee el teclado en una goroutine, pero con pausa: cuando un hijo
// (tmux attach) toma la terminal, dos lectores sobre el mismo tty se roban las
// teclas entre si y dejan la sesion inservible. El fd 0 se pone en modo
// no-bloqueante para poder interrumpir la lectura en curso con un deadline.
type stdinReader struct {
	file    *os.File
	events  chan byte
	pausing atomic.Bool
	ack     chan struct{}
	resume  chan struct{}
}

func newStdinReader() *stdinReader {
	_ = syscall.SetNonblock(0, true)
	return &stdinReader{
		file:   os.NewFile(0, "stdin"),
		events: make(chan byte, 64),
		ack:    make(chan struct{}),
		resume: make(chan struct{}),
	}
}

func (reader *stdinReader) loop() {
	buf := make([]byte, 64)
	for {
		if reader.pausing.Load() {
			reader.ack <- struct{}{}
			<-reader.resume
			continue
		}
		n, err := reader.file.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return
		}
		for _, b := range buf[:n] {
			reader.events <- b
		}
	}
}

// pause detiene la lectura y devuelve el fd a modo bloqueante para el hijo.
// Se drena events mientras se espera el ack por si la goroutine estaba
// bloqueada entregando teclas acumuladas.
func (reader *stdinReader) pause() {
	reader.pausing.Store(true)
	_ = reader.file.SetReadDeadline(time.Now())
	for {
		select {
		case <-reader.ack:
			_ = reader.file.SetReadDeadline(time.Time{})
			_ = syscall.SetNonblock(0, false)
			return
		case <-reader.events:
		}
	}
}

func (reader *stdinReader) unpause() {
	_ = syscall.SetNonblock(0, true)
	reader.pausing.Store(false)
	reader.resume <- struct{}{}
}

// paneContrast: el TUI de adentro (Claude Code) marca la fila seleccionada de
// sus menus solo con blanco brillante (SGR 97) frente a blanco normal (37).
// En paletas donde ambos se ven casi iguales la seleccion se pierde: se
// reescriben a truecolor con contraste real (blanco puro vs gris apagado).
var paneContrast = strings.NewReplacer(
	"\x1b[97m", "\x1b[38;2;255;255;255m",
	"\x1b[37m", "\x1b[38;2;100;115;145m",
)

// paneMonitor captura la sesion del panel derecho en su propia goroutine para
// que el render nunca lance subprocesos: solo pinta el ultimo snapshot. Con el
// foco en la terminal sondea a ~25fps y ademas se le puede dar un "poke" tras
// cada tecla para que el eco aparezca de inmediato.
type paneSnapshot struct {
	session string
	lines   []string
	curX    int
	curY    int
	curVis  bool
}

type paneMonitor struct {
	target  atomic.Value // string: sesion observada ("" = ninguna)
	focused atomic.Bool
	poke    chan struct{}
	updates chan paneSnapshot
}

func newPaneMonitor() *paneMonitor {
	monitor := &paneMonitor{poke: make(chan struct{}, 1), updates: make(chan paneSnapshot, 1)}
	monitor.target.Store("")
	go monitor.loop()
	return monitor
}

// watch fija que sesion observar; al cambiar de sesion captura de inmediato
// para no mostrar la pantalla de la pestaña anterior.
func (monitor *paneMonitor) watch(session string, focused bool) {
	previous, _ := monitor.target.Load().(string)
	monitor.target.Store(session)
	monitor.focused.Store(focused)
	if session != "" && session != previous {
		monitor.pokeNow()
	}
}

func (monitor *paneMonitor) pokeNow() {
	select {
	case monitor.poke <- struct{}{}:
	default:
	}
}

func (monitor *paneMonitor) loop() {
	for {
		session, _ := monitor.target.Load().(string)
		if session == "" {
			select {
			case <-monitor.poke:
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		lines, x, y, visible := tmuxx.ScreenAndCursor(session)
		snap := paneSnapshot{session: session, lines: lines, curX: x, curY: y, curVis: visible}
		// Solo interesa el snapshot mas reciente: descartar el que no se leyo.
		select {
		case <-monitor.updates:
		default:
		}
		monitor.updates <- snap
		interval := 150 * time.Millisecond
		if monitor.focused.Load() {
			interval = 40 * time.Millisecond
		}
		select {
		case <-monitor.poke:
			// Tras una tecla: darle un respiro a la app de adentro para que
			// alcance a ecoar antes de capturar.
			time.Sleep(15 * time.Millisecond)
		case <-time.After(interval):
		}
	}
}

// visibleWorkers aplica el filtro de / sobre ticket y descripcion.
func (app *tuiApp) visibleWorkers() []ws.WorkerSummary {
	if app.filter == "" {
		return app.view.Workers
	}
	needle := strings.ToLower(app.filter)
	var out []ws.WorkerSummary
	for _, worker := range app.view.Workers {
		haystack := strings.ToLower(worker.ID + " " + worker.Description)
		if strings.Contains(haystack, needle) {
			out = append(out, worker)
		}
	}
	return out
}

func (app *tuiApp) clampSelection() {
	count := len(app.visibleWorkers())
	if count == 0 {
		app.selected = 0
	} else if app.selected >= count {
		app.selected = count - 1
	}
}

func (app *tuiApp) clampService() {
	if app.detail == nil || len(app.detail.Services) == 0 {
		app.svcSelected = 0
	} else if app.svcSelected >= len(app.detail.Services) {
		app.svcSelected = len(app.detail.Services) - 1
	}
}

func (app *tuiApp) currentWorker() *ws.WorkerSummary {
	if app.viewMode == viewDetail && app.detail != nil {
		return &app.detail.WorkerSummary
	}
	visible := app.visibleWorkers()
	if app.selected < 0 || app.selected >= len(visible) {
		return nil
	}
	return &visible[app.selected]
}

// enterDetail arma un detalle rapido (sin git ni gh) para pintar de una, y
// pide el completo en background.
func (app *tuiApp) enterDetail() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	app.viewMode = viewDetail
	app.detailID = worker.ID
	app.svcSelected = 0
	app.detail = &ws.WorkerDetail{WorkerSummary: *worker}

	sessionKey := worker.ID
	if worker.IsPrincipal {
		sessionKey = ""
	}
	root := worker.Root
	if worker.IsPrincipal {
		root = config.PrincipalDir
	}
	app.detail.Services = ws.ServicesForWorker(root, sessionKey, ws.LoadServiceDefs(), ws.LiveSessions())
	ws.EnrichServicePorts(app.detail.Services)
	app.requestDetail(false)
}

func (app *tuiApp) exitDetail() {
	app.viewMode = viewList
	app.detail = nil
	app.detailID = ""
	app.detailGen++ // invalida respuestas en vuelo
}

func (app *tuiApp) requestDetail(refreshPRs bool) {
	if app.detailID == "" {
		return
	}
	app.detailGen++
	app.lastDetailReq = time.Now()
	generation, id := app.detailGen, app.detailID
	go func() {
		detail, err := ws.Detail(id, refreshPRs)
		app.detailEvents <- detailUpdate{generation: generation, detail: detail, err: err}
	}()
}

func (app *tuiApp) handleKey(key byte, more <-chan byte) (quit bool) {
	if app.mode == modeCommand || app.mode == modeSearch {
		return app.handleLineKey(key, more)
	}
	if app.helpMode {
		return app.handleHelpKey(key, more)
	}
	// El explorador flotante es modal: sus teclas ganan incluso con el foco
	// en la terminal.
	if app.diffMode {
		return app.handleDiffKey(key, more)
	}
	if app.focusRight {
		return app.handleFocusKey(key, more)
	}
	if key == 3 { // Ctrl-C sale desde cualquier vista
		return true
	}
	if app.viewMode == viewDetail {
		return app.handleDetailKey(key, more)
	}

	app.message = ""
	wasG := app.pendingG
	app.pendingG = false

	switch key {
	case 'q':
		return true
	case 27: // posible secuencia de flecha
		if next := readByteTimeout(more); next == '[' {
			switch readByteTimeout(more) {
			case 'A':
				app.moveSelection(-1)
			case 'B':
				app.moveSelection(1)
			case 'C': // flecha derecha entra al detalle
				app.enterDetail()
			}
		}
	case ':':
		app.mode = modeCommand
		app.input = nil
	case '/':
		app.mode = modeSearch
		app.input = nil
	case 'g':
		if wasG {
			app.selected = 0
		} else {
			app.pendingG = true
		}
	case 'G':
		if count := len(app.visibleWorkers()); count > 0 {
			app.selected = count - 1
		}
	case 'k':
		app.moveSelection(-1)
	case 'j':
		app.moveSelection(1)
	case '?':
		app.helpMode = true
	case 'z': // alejar el zoom de la conversacion: casi todo el panel para ella
		app.zoomed = !app.zoomed
	case ']', 'l': // pestaña siguiente de sesiones del worker
		app.cycleTab(1)
	case '[', 'h': // pestaña anterior
		app.cycleTab(-1)
	case 'n': // nueva pestaña shell en la raiz del worker
		app.newShellTab()
	case 'v': // voz: leer en voz alta las respuestas de los agentes
		app.toggleVoice()
	case 'V': // voz: leer el ultimo resultado del worker seleccionado
		app.speakLastReply()
	case 'd':
		app.toggleDiff()
	case 9: // Tab: pasar el teclado a la terminal del worker
		if app.rightSess != "" {
			app.focusRight = true
		} else {
			app.message = "este worker no tiene terminal viva ([Ctrl+s] agente, [n] shell)"
		}
	case 13, 10: // entrar al detalle del worker (acciones de servicios)
		app.enterDetail()
	case 'a':
		app.attachAgent()
	case 'F':
		app.externalTerminal()
	case 19: // Ctrl-s: lanzar agente. Era 's' a secas, pero un dedazo abria
		// pestañas por error.
		app.startAgent("")
	case 'i':
		app.interruptAgent()
	case 'x':
		app.stopAgentConfirm()
	case 'r':
		app.refreshWorkspace()
		app.message = "refrescando..."
	}
	return false
}

// refreshWorkspace recarga la lista en background y la entrega por el canal
// del loop principal: escribir app.view desde otra goroutine es un data race.
func (app *tuiApp) refreshWorkspace() {
	go func() {
		view := ws.List()
		select {
		case app.wsEvents <- view:
		default:
		}
	}()
}

// handleDetailKey navega dentro de un worker: servicios seleccionables y
// acciones sobre agente, shell y vel.
func (app *tuiApp) handleDetailKey(key byte, more <-chan byte) (quit bool) {
	app.message = ""
	switch key {
	case 'q', 'h':
		app.exitDetail()
	case 27:
		switch readByteTimeout(more) {
		case 0: // Escape solo: volver a la lista
			app.exitDetail()
		case '[':
			switch readByteTimeout(more) {
			case 'A':
				app.moveService(-1)
			case 'B':
				app.moveService(1)
			case 'D': // flecha izquierda vuelve
				app.exitDetail()
			}
		}
	case ':':
		app.mode = modeCommand
		app.input = nil
	case 'k':
		app.moveService(-1)
	case 'j':
		app.moveService(1)
	case 13, 10, 'l': // attach al servicio seleccionado
		app.attachService()
	case 's':
		app.serviceAction("start")
	case 'R':
		app.serviceAction("restart")
	case 'x':
		app.serviceAction("stop")
	case 'X':
		app.stopAllConfirm()
	case 'a':
		app.attachAgent()
	case 'V':
		app.speakLastReply()
	case 'F':
		app.externalTerminal()
	case 't':
		app.openShell()
	case 'p':
		app.requestDetail(true)
		app.message = "refrescando PRs..."
	case 'r':
		app.requestDetail(false)
		app.message = "refrescando..."
	}
	return false
}

func (app *tuiApp) moveService(delta int) {
	if app.detail == nil {
		return
	}
	count := len(app.detail.Services)
	if count == 0 {
		return
	}
	app.svcSelected = (app.svcSelected + delta + count) % count
}

func (app *tuiApp) currentService() *ws.ServiceState {
	if app.detail == nil || app.svcSelected < 0 || app.svcSelected >= len(app.detail.Services) {
		return nil
	}
	return &app.detail.Services[app.svcSelected]
}

func (app *tuiApp) attachService() {
	service := app.currentService()
	if service == nil {
		return
	}
	if !service.Running {
		app.message = service.Name + " esta detenido: [s] lo inicia"
		return
	}
	app.attachSession(service.Session)
}

func (app *tuiApp) serviceAction(action string) {
	service := app.currentService()
	if service == nil || app.detail == nil {
		return
	}
	root := app.detail.Root
	name := service.Name
	app.message = action + " " + name + "..."
	go func() {
		if _, err := ws.VelCommand(root, action, name); err != nil {
			app.message = action + " " + name + " fallo: " + err.Error()
		} else {
			app.message = action + " " + name + " OK"
		}
		app.requestDetail(false)
	}()
}

// workerTerminalSession resuelve la sesion de trabajo del worker: su agente
// si esta vivo, o su shell (creandolo si hace falta).
func (app *tuiApp) workerTerminalSession() string {
	worker := app.currentWorker()
	if worker == nil {
		return ""
	}
	sess := ws.AgentSession(worker.ID)
	if !worker.Agent.Running {
		sess = ws.ShellSession(worker.ID)
		if !tmuxx.HasSession(sess) {
			if _, err := tmuxx.Run("new-session", "-d", "-s", sess, "-c", worker.Root); err != nil {
				app.message = "no se pudo abrir el shell: " + err.Error()
				return ""
			}
		}
	}
	return sess
}

// externalTerminal abre la sesion en una ventana del sistema (gnome-terminal).
func (app *tuiApp) externalTerminal() {
	sess := app.workerTerminalSession()
	if sess == "" {
		return
	}
	term, args := findTerminal()
	if term == "" {
		app.message = "no se encontro emulador de terminal (gnome-terminal, tilix, kitty...)"
		return
	}
	cmd := exec.Command(term, append(args, "tmux", "attach", "-t", sess)...)
	if err := cmd.Start(); err != nil {
		app.message = "no se pudo abrir la terminal: " + err.Error()
		return
	}
	go func() { _ = cmd.Wait() }()
	app.message = "ventana del sistema sobre " + sess
}

// findTerminal detecta el emulador disponible y como pasarle un comando.
func findTerminal() (string, []string) {
	if custom := os.Getenv("TERMINAL"); custom != "" {
		return custom, []string{"-e"}
	}
	candidates := []struct {
		bin  string
		args []string
	}{
		{"gnome-terminal", []string{"--"}},
		{"tilix", []string{"-e"}},
		{"kitty", nil},
		{"alacritty", []string{"-e"}},
		{"konsole", []string{"-e"}},
		{"xterm", []string{"-e"}},
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate.bin); err == nil {
			return candidate.bin, candidate.args
		}
	}
	return "", nil
}

// selectProfile fija el perfil del WORKER seleccionado (persistido): cada
// worktree puede usar una cuenta distinta. ":prof -" borra el override.
func (app *tuiApp) selectProfile(name string) {
	available := strings.Join(config.ProfileNames(), " ")
	if available == "" {
		app.message = "sin perfiles en config.json (bloque profiles)"
		return
	}
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if name == "" {
		effective := config.ProfileForWorker(worker.ID)
		if effective == "" {
			effective = "claude por defecto"
		}
		app.message = "perfiles: " + available + "  (" + worker.Ticket + " usa: " + effective + ")"
		return
	}
	if name == "-" {
		delete(config.WorkerProfiles, config.WorkerProfileKey(worker.ID))
		config.SaveState()
		app.message = worker.Ticket + " vuelve al perfil del workspace"
		return
	}
	for _, profile := range config.AgentProfiles {
		if profile.Name == name {
			config.WorkerProfiles[config.WorkerProfileKey(worker.ID)] = name
			config.SaveState()
			app.message = "el agente de " + worker.Ticket + " se lanzara con " + name
			return
		}
	}
	app.message = "perfil desconocido: " + name + "  (hay: " + available + ")"
}

// createWorktree crea un universo paralelo del proyecto: git worktree con su
// propia rama en <root>-workers/<nombre>. Solo en workspaces sin vel (en
// workspaces con manifiesto lo hace su gestor de worktrees).
func (app *tuiApp) createWorktree(name string) {
	if config.ManifestPath != "" {
		app.message = "este workspace usa vel: crea el worker con /wt-new"
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		app.message = "uso: :wt <nombre-del-universo>"
		return
	}
	dest := filepath.Join(filepath.Dir(config.PrincipalDir), filepath.Base(config.PrincipalDir)+"-workers", name)
	app.message = "creando worktree " + name + "..."
	go func() {
		out, err := execx.RunTimeout(60*time.Second, config.PrincipalDir, "git", "worktree", "add", "-b", name, dest)
		if err != nil && strings.Contains(out, "already exists") {
			out, err = execx.RunTimeout(60*time.Second, config.PrincipalDir, "git", "worktree", "add", dest, name)
		}
		if err != nil {
			app.message = "worktree fallo: " + strings.TrimSpace(out)
			return
		}
		app.message = "universo " + name + " listo en " + dest
		select {
		case app.wsEvents <- ws.List():
		default:
		}
	}()
}

// stopAllConfirm pide X dos veces: tumba todos los servicios del worker.
func (app *tuiApp) stopAllConfirm() {
	if app.detail == nil {
		return
	}
	if time.Since(app.pendingStopAll) > 3*time.Second {
		app.pendingStopAll = time.Now()
		app.message = "presiona [X] otra vez para detener TODOS los servicios de " + app.detail.Ticket
		return
	}
	app.pendingStopAll = time.Time{}
	app.stopAllNow()
}

func (app *tuiApp) stopAllNow() {
	if app.detail == nil {
		return
	}
	root := app.detail.Root
	var running []string
	for _, service := range app.detail.Services {
		if service.Running {
			running = append(running, service.Name)
		}
	}
	if len(running) == 0 {
		app.message = "no hay servicios corriendo en este worker"
		return
	}
	app.message = fmt.Sprintf("deteniendo %d servicios...", len(running))
	go func() {
		failed := 0
		for _, name := range running {
			if _, err := ws.VelCommand(root, "stop", name); err != nil {
				failed++
			}
		}
		if failed > 0 {
			app.message = fmt.Sprintf("detenidos %d/%d servicios (%d fallaron)", len(running)-failed, len(running), failed)
		} else {
			app.message = fmt.Sprintf("detenidos los %d servicios", len(running))
		}
		app.requestDetail(false)
	}()
}

// openShell abre (o retoma) el shell del worker y lo deja enfocado como
// pestaña en el panel, sin invadir la pantalla.
func (app *tuiApp) openShell() {
	if app.detail == nil {
		return
	}
	id := app.detail.ID
	session := ws.ShellSession(id)
	if !tmuxx.HasSession(session) {
		if _, err := tmuxx.Run("new-session", "-d", "-s", session, "-c", app.detail.Root); err != nil {
			app.message = "no se pudo abrir el shell: " + err.Error()
			return
		}
	}
	app.activeTab[id] = session
	app.exitDetail()
	app.rightSess = session
	app.focusRight = true
}

// handleLineKey edita la linea de : o de /. En busqueda el filtro se aplica en
// vivo con cada tecla; Escape lo cancela, Enter lo deja fijo.
func (app *tuiApp) handleLineKey(key byte, more <-chan byte) bool {
	switch key {
	case 13, 10:
		line := string(app.input)
		wasSearch := app.mode == modeSearch
		app.mode = modeNormal
		app.input = nil
		if wasSearch {
			app.filter = line
			app.selected = 0
			return false
		}
		return app.executeCommand(line)
	case 27:
		if readByteTimeout(more) == 0 { // Escape solo, no una flecha
			if app.mode == modeSearch {
				app.filter = ""
			}
			app.mode = modeNormal
			app.input = nil
			app.clampSelection()
		}
	case 127, 8: // backspace: quitar la ultima runa, no el ultimo byte
		runes := []rune(string(app.input))
		if len(runes) > 0 {
			app.input = []byte(string(runes[:len(runes)-1]))
		}
	case 3: // Ctrl-C cancela la linea
		app.mode = modeNormal
		app.input = nil
	default:
		if key >= 32 && key != 127 {
			app.input = append(app.input, key)
		}
	}
	if app.mode == modeSearch {
		app.filter = string(app.input)
		app.clampSelection()
	}
	return false
}

func (app *tuiApp) executeCommand(line string) (quit bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	name, argument, _ := strings.Cut(line, " ")
	argument = strings.TrimSpace(argument)

	switch name {
	case "q", "qa", "quit":
		return true
	case "a", "attach":
		app.attachAgent()
	case "st", "start":
		app.startAgent(argument)
	case "send":
		app.sendToAgent(argument)
	case "esc", "i", "int":
		app.interruptAgent()
	case "stop":
		app.message = "el agente puede estar trabajando: usa :stop! para confirmar"
	case "stop!":
		app.stopAgentNow()
	case "stopall":
		app.message = "usa :stopall! para detener todos los servicios del worker"
	case "stopall!":
		if app.viewMode == viewDetail {
			app.stopAllNow()
		} else {
			app.message = ":stopall! solo dentro de un worker ([l] para entrar)"
		}
	case "sh", "shell":
		if app.viewMode == viewDetail {
			app.openShell()
		} else {
			app.message = ":shell solo dentro de un worker ([l] para entrar)"
		}
	case "r", "refresh":
		if app.viewMode == viewDetail {
			app.requestDetail(false)
		} else {
			app.refreshWorkspace()
		}
		app.message = "refrescando..."
	case "wt":
		app.createWorktree(argument)
	case "prof", "profile":
		app.selectProfile(argument)
	case "window", "win":
		app.externalTerminal()
	case "d", "diff":
		app.toggleDiff()
	case "leer", "read":
		app.speakLastReply()
	case "help", "ayuda":
		app.helpMode = true
	case "noh", "nohl":
		app.filter = ""
		app.clampSelection()
	default:
		app.message = "comando desconocido: :" + name
	}
	return false
}

func readByteTimeout(more <-chan byte) byte {
	select {
	case b := <-more:
		return b
	case <-time.After(50 * time.Millisecond):
		return 0
	}
}

func (app *tuiApp) moveSelection(delta int) {
	count := len(app.visibleWorkers())
	if count == 0 {
		return
	}
	app.selected = (app.selected + delta + count) % count
}

// attachSession cede la terminal a tmux y la recupera al salir. Dentro de
// tmux cambia de cliente en vez de anidar sesiones.
func (app *tuiApp) attachSession(session string) {
	if os.Getenv("TMUX") != "" {
		if _, err := tmuxx.Run("switch-client", "-t", session); err != nil {
			app.message = "switch-client fallo: " + err.Error()
		}
		return
	}
	// Letrero de escape: quien entre a la sesion sabe como volver al panel.
	_, _ = tmuxx.Run("set-option", "-t", session, "status-right", " Ctrl+b d: volver al panel ")
	app.reader.pause()
	app.restoreTerminal()
	cmd := exec.Command("tmux", "attach", "-t", session)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
	_, _ = sttyCommand("raw", "-echo")
	fmt.Print("\x1b[?1049h\x1b[?25l")
	app.reader.unpause()
}

// attachAgent ya NO invade la pantalla: enfoca la pestaña del agente dentro
// del panel (mismo flujo que Tab). Pantalla completa queda en [F] (ventana).
func (app *tuiApp) attachAgent() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if !worker.Agent.Running {
		app.message = "el agente esta detenido: [Ctrl+s] o :start lo inician"
		return
	}
	app.activeTab[worker.ID] = worker.Agent.Session
	app.viewMode = viewList
	app.rightSess = worker.Agent.Session
	app.focusRight = true
}

// startAgent lanza un agente; si ya hay, crea otra pestaña (agent-x-2, -3).
func (app *tuiApp) startAgent(prompt string) {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	base := ws.AgentSession(worker.ID)
	sess := base
	for i := 2; tmuxx.HasSession(sess); i++ {
		sess = fmt.Sprintf("%s-%d", base, i)
	}
	if err := ws.AgentStart(sess, worker.Root, prompt, "", worker.ID); err != nil {
		app.message = err.Error()
		return
	}
	app.activeTab[worker.ID] = sess
	app.message = "agente lanzado en " + sess
	if prompt != "" {
		app.message += " con prompt"
	}
}

// newShellTab abre otra pestaña con shell en la raiz del worker.
func (app *tuiApp) newShellTab() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	base := ws.ShellSession(worker.ID)
	sess := base
	for i := 2; tmuxx.HasSession(sess); i++ {
		sess = fmt.Sprintf("%s-%d", base, i)
	}
	if _, err := tmuxx.Run("new-session", "-d", "-s", sess, "-c", worker.Root); err != nil {
		app.message = "no se pudo abrir el shell: " + err.Error()
		return
	}
	app.activeTab[worker.ID] = sess
	app.message = "pestaña shell nueva: " + sess
}

func (app *tuiApp) sendToAgent(text string) {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if text == "" {
		app.message = "uso: :send <texto para el agente>"
		return
	}
	if err := ws.AgentSend(worker.ID, text); err != nil {
		app.message = err.Error()
		return
	}
	app.message = "enviado al agente de " + worker.Ticket
}

func (app *tuiApp) interruptAgent() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if err := ws.AgentInterrupt(worker.ID); err != nil {
		app.message = err.Error()
		return
	}
	app.message = "Escape enviado al agente"
}

// stopAgentConfirm pide la tecla dos veces seguidas: matar una sesion con
// trabajo en curso no debe pasar por un dedazo. :stop! salta la confirmacion.
func (app *tuiApp) stopAgentConfirm() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if time.Since(app.pendingStop) > 3*time.Second {
		app.pendingStop = time.Now()
		app.message = "presiona [x] otra vez para detener el agente de " + worker.Ticket
		return
	}
	app.pendingStop = time.Time{}
	app.stopAgentNow()
}

func (app *tuiApp) stopAgentNow() {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	if err := ws.AgentStop(worker.ID); err != nil {
		app.message = err.Error()
		return
	}
	app.message = "agente de " + worker.Ticket + " detenido"
}

// runeWidth aproxima el ancho en celdas: emojis y CJK ocupan dos.
func runeWidth(r rune) int {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF, r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFF00 && r <= 0xFF60, r == 0x26A1, r >= 0x2600 && r <= 0x27BF:
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	width := 0
	for _, r := range tmuxx.StripSGR(s) {
		width += runeWidth(r)
	}
	return width
}

// wrapPlain corta texto plano en lineas de a lo sumo width columnas (corte
// por palabra), maximo maxLines; si queda texto afuera, la ultima linea
// termina en "…".
func wrapPlain(text string, width, maxLines int) []string {
	words := strings.Fields(text)
	if len(words) == 0 || width < 4 || maxLines < 1 {
		return nil
	}
	var lines []string
	current := ""
	for _, word := range words {
		for displayWidth(word) > width && len(lines) < maxLines {
			if current != "" {
				lines = append(lines, current)
				current = ""
				continue
			}
			runes := []rune(word)
			cut := width
			if cut > len(runes) {
				cut = len(runes)
			}
			lines = append(lines, string(runes[:cut]))
			word = string(runes[cut:])
		}
		if len(lines) >= maxLines {
			break
		}
		if word == "" {
			continue
		}
		switch {
		case current == "":
			current = word
		case displayWidth(current+" "+word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
			if len(lines) >= maxLines {
				break
			}
		}
	}
	if current != "" && len(lines) < maxLines {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return nil
	}
	if displayWidth(strings.Join(lines, " ")) < displayWidth(strings.Join(words, " ")) {
		lines[len(lines)-1] += "…"
	}
	return lines
}

// Cursor-nave: el cursor de hardware solo puede ser bloque/raya/barra, asi
// que se dibuja un glifo propio (cohete, escape unicode por
// .claude/rules/no-unicode-in-code.md) en la celda del cursor del pane.
const shipGlyph = "\x1b[0m\U0001F680"

// overlayCursor sobreescribe la celda en la columna visible col con el glifo,
// dejando intactas las secuencias de color del resto de la linea.
func overlayCursor(line string, col int, glyph string) string {
	var out strings.Builder
	width := displayWidth(glyph)
	visible := 0
	placed := false
	skip := 0
	inEscape := false
	for _, r := range line {
		if inEscape {
			out.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if r == 0x1b {
			inEscape = true
			out.WriteRune(r)
			continue
		}
		if !placed && visible >= col {
			out.WriteString(glyph)
			placed = true
			skip = width
		}
		if skip > 0 {
			// Comerse los caracteres que quedaron debajo del glifo.
			skip -= runeWidth(r)
			visible += runeWidth(r)
			continue
		}
		out.WriteRune(r)
		visible += runeWidth(r)
	}
	if !placed {
		if pad := col - visible; pad > 0 {
			out.WriteString(strings.Repeat(" ", pad))
		}
		out.WriteString(glyph)
	}
	return out.String()
}

// clipANSI corta una linea al ancho visible sin partir secuencias de color.
func clipANSI(line string, max int) string {
	var out strings.Builder
	visible := 0
	inEscape := false
	for _, r := range line {
		if inEscape {
			out.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if r == 0x1b {
			inEscape = true
			out.WriteRune(r)
			continue
		}
		if visible+runeWidth(r) > max {
			break
		}
		out.WriteRune(r)
		visible += runeWidth(r)
	}
	out.WriteString(ansiReset)
	return out.String()
}

// ctxLabel pinta el porcentaje de contexto del agente: ambar desde 60,
// rojo desde 85 (cerca del auto-compact).
func ctxLabel(pct int) string {
	if pct <= 0 {
		return ""
	}
	color := ansiDim
	switch {
	case pct >= 85:
		color = ansiRed
	case pct >= 60:
		color = ansiYellow
	}
	return color + fmt.Sprintf(" ctx %d%%", pct) + ansiReset
}

func humanMem(kb int64) string {
	switch {
	case kb <= 0:
		return ""
	case kb < 1024:
		return fmt.Sprintf("%dK", kb)
	case kb < 1024*1024:
		return fmt.Sprintf("%dM", kb/1024)
	default:
		return fmt.Sprintf("%.1fG", float64(kb)/(1024*1024))
	}
}

// memLabel pinta la memoria en ambar desde 2G y en rojo desde 4G.
func memLabel(kb int64) string {
	text := humanMem(kb)
	if text == "" {
		return ""
	}
	color := ansiDim
	switch {
	case kb >= 4*1024*1024:
		color = ansiRed
	case kb >= 2*1024*1024:
		color = ansiYellow
	}
	return color + " mem " + text + ansiReset
}

func padRight(s string, width int) string {
	if len(s) > width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

func agentStatusLabel(agent ws.AgentState, frame int) string {
	switch agent.Status {
	case "working":
		label := "working"
		if agent.Activity != "" {
			label += " " + agent.Activity
		}
		if agent.ElapsedSec > 0 {
			label += fmt.Sprintf(" %ds", agent.ElapsedSec)
		}
		return agentSpinner(frame) + " " + ansiGreen + label + ansiReset
	case "waiting":
		return ansiYellow + pulse(frame) + fmt.Sprintf(" waiting %ds", agent.IdleSec) + ansiReset
	case "sin agente":
		return ansiRed + "sesion sin agente" + ansiReset
	default:
		return ansiDim + "stopped" + ansiReset
	}
}

type frameWriter struct {
	builder strings.Builder
	rows    int
	cols    int
	used    int
}

func (fw *frameWriter) line(content string) {
	if fw.used >= fw.rows-1 {
		return
	}
	fw.builder.WriteString(clipANSI(content, fw.cols))
	fw.builder.WriteString("\x1b[K\r\n")
	fw.used++
}

func (fw *frameWriter) separator() {
	fw.line(ansiSep + strings.Repeat("-", fw.cols) + ansiReset)
}

func (app *tuiApp) render() {
	rows, cols := app.size()
	fw := &frameWriter{rows: rows, cols: cols}
	// Frame atomico: cursor oculto durante el repintado y modo de salida
	// sincronizada (DEC 2026). Sin esto el cursor visible "salta" por la
	// pantalla siguiendo la escritura de cada frame.
	fw.builder.WriteString("\x1b[?2026h\x1b[?25l\x1b[H")

	if app.helpMode {
		app.monitor.watch("", false)
		app.renderHelp(fw)
	} else if app.viewMode == viewDetail {
		app.monitor.watch("", false)
		app.renderDetail(fw)
	} else {
		app.renderList(fw)
	}

	fw.builder.WriteString("\x1b[J\x1b[" + fmt.Sprint(rows) + ";1H")
	var footer string
	switch app.mode {
	case modeCommand:
		footer = ":" + string(app.input) + ansiCursor
	case modeSearch:
		footer = "/" + string(app.input) + ansiCursor
	default:
		if app.helpMode {
			footer = ansiDim + "[j/k C-d C-u] scroll  [g/G] extremos  [q/esc/?] cerrar" + ansiReset
		} else if app.viewMode == viewDetail {
			footer = ansiDim + "[j/k] servicio  [enter] attach  [s/R/x] start/restart/stop  [X][X] parar todo  [a] agente  [F] ventana  [t] shell  [p] PRs  [h] volver" + ansiReset
		} else if app.diffMode {
			footer = ansiMagenta + "explorador" + ansiReset + ansiDim + "  [j/k] archivo  [l/enter] carpeta  [J/K u f b] codigo  [a] rama/todo  [q/C-d/esc] cerrar" + ansiReset
		} else if app.focusRight {
			footer = ansiMagenta + "terminal" + ansiReset + ansiDim + "  las teclas van al agente  [C-j/k] worker  [C-h/l] pestañas  [C-s] agente  [C-d] archivos  [C-q] lista" + ansiReset
		} else {
			footer = ansiDim + "[j/k] mover  [h/l] pestañas  [Tab] escribir  [d] diff  [z] zoom  [enter] detalle  [/] filtrar  [C-s] iniciar  [x][x] detener  [:] comando  [?] ayuda  [q] salir" + ansiReset
		}
		if app.message != "" {
			footer = ansiYellow + app.message + ansiReset
		}
	}
	fw.builder.WriteString(clipANSI(footer, cols))
	fw.builder.WriteString("\x1b[K")
	if app.cursorSeq != "" {
		fw.builder.WriteString(app.cursorSeq)
	}
	fw.builder.WriteString("\x1b[?2026l")
	os.Stdout.WriteString(fw.builder.String())
}

// handleFocusKey reenvia el teclado a la terminal derecha en bytes crudos.
// Ctrl+q devuelve el foco a la lista; Ctrl+h / Ctrl+l cambian de pestaña sin
// soltar el foco (por eso esos bytes NO llegan a la terminal de adentro).
func (app *tuiApp) handleFocusKey(key byte, more <-chan byte) bool {
	switch key {
	case 17: // Ctrl-q
		app.focusRight = false
		return false
	case 8: // Ctrl-h: pestaña anterior sin salir del modo terminal
		app.cycleTab(-1)
		return false
	case 12: // Ctrl-l: pestaña siguiente
		app.cycleTab(1)
		return false
	case 11: // Ctrl-k: worker anterior sin soltar el foco
		app.moveSelection(-1)
		return false
	case 10: // Ctrl-j: worker siguiente (Enter manda 13 en modo raw, no choca)
		app.moveSelection(1)
		return false
	case 19: // Ctrl-s: iniciar el agente del worker sin soltar el foco
		app.startAgent("")
		return false
	case 4: // Ctrl-d: explorador flotante de archivos del worker
		app.toggleDiff()
		return false
	}
	if app.rightSess == "" {
		// Sin terminal viva no hay a donde mandar las teclas: no se crea nada
		// solo, se espera el Ctrl+s explicito.
		app.message = "este worker no tiene terminal: [Ctrl+s] inicia el agente  [Ctrl+q] lista"
		return false
	}
	buf := []byte{key}
drain:
	for len(buf) < 32 {
		select {
		case extra := <-more:
			if extra == 4 || extra == 8 || extra == 10 || extra == 11 || extra == 12 || extra == 17 || extra == 19 {
				// Tecla de control del panel en medio de la rafaga: mandar lo
				// acumulado y atender el control por separado.
				app.forwardToRight(buf)
				return app.handleFocusKey(extra, more)
			}
			buf = append(buf, extra)
		default:
			break drain
		}
	}
	app.forwardToRight(buf)
	return false
}

func (app *tuiApp) forwardToRight(buf []byte) {
	args := []string{"send-keys", "-H", "-t", app.rightSess}
	for _, b := range buf {
		args = append(args, fmt.Sprintf("%02x", b))
	}
	if _, err := tmuxx.Run(args...); err != nil {
		app.message = "la terminal del worker murio: [Ctrl+s] otro agente"
		return
	}
	// Capturar ya mismo: el eco de la tecla no debe esperar al proximo sondeo.
	app.monitor.pokeNow()
}

func (app *tuiApp) toggleDiff() {
	if app.diffMode {
		app.diffMode = false
		app.viewerRel = ""
		return
	}
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	root := worker.Root
	app.diffFiles = ws.BranchChanges(worker.ID)
	app.changed = map[string]string{}
	app.dirtyDirs = map[string]bool{}
	for _, file := range app.diffFiles {
		rel, err := filepath.Rel(root, filepath.Join(file.Dir, file.Path))
		if err != nil {
			continue
		}
		app.changed[rel] = file.Status
		for dir := filepath.Dir(rel); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			app.dirtyDirs[dir] = true
		}
	}
	app.treeRootPath = root
	app.treeAll = len(app.changed) == 0
	if app.treeAll {
		app.message = "la rama no tiene cambios: mostrando el arbol completo"
	}
	app.rebuildTree()
	app.treeSel = 0
	app.branches = map[string]string{}
	app.diffMode = true
	app.computeDiffStats()
	app.previewSelected()
}

var shortstatRe = regexp.MustCompile(`(\d+) insertion|(\d+) deletion`)

// computeDiffStats suma las lineas insertadas/borradas de la rama contra su
// base (worktree incluido), un git diff --shortstat por repo.
func (app *tuiApp) computeDiffStats() {
	app.diffAdds, app.diffDels = 0, 0
	seen := map[string]bool{}
	for _, file := range app.diffFiles {
		if file.Base == "" || seen[file.Dir] {
			continue
		}
		seen[file.Dir] = true
		raw, err := ws.GitOut(file.Dir, "diff", "--shortstat", file.Base)
		if err != nil {
			continue
		}
		for _, match := range shortstatRe.FindAllStringSubmatch(raw, -1) {
			if match[1] != "" {
				n, _ := strconv.Atoi(match[1])
				app.diffAdds += n
			}
			if match[2] != "" {
				n, _ := strconv.Atoi(match[2])
				app.diffDels += n
			}
		}
	}
}

// branchFor devuelve la rama actual del repo, cacheada mientras la ventana
// del explorador este abierta (un git por repo, no por frame).
func (app *tuiApp) branchFor(dir string) string {
	if cached, ok := app.branches[dir]; ok {
		return cached
	}
	branch, _ := ws.GitOut(dir, "branch", "--show-current")
	app.branches[dir] = branch
	return branch
}

// rebuildTree arma el arbol segun el modo: solo los archivos de la rama
// (default) o el worktree completo.
func (app *tuiApp) rebuildTree() {
	if app.treeAll {
		app.treeRoot = newTree(app.treeRootPath)
		app.treeRoot.load()
	} else {
		app.treeRoot = buildChangedTree(app.treeRootPath, app.changed)
	}
	app.treeFlat = flattenTree(app.treeRoot)
	if app.treeSel >= len(app.treeFlat) {
		app.treeSel = 0
	}
}

// handleDiffKey maneja la ventana flotante del explorador: el arbol siempre
// tiene el foco (j/k) y el archivo bajo el cursor se previsualiza solo a la
// derecha; J/K/u/f/b mueven el codigo.
func (app *tuiApp) handleDiffKey(key byte, more <-chan byte) bool {
	switch key {
	case 'q', 'd', 4, 3: // q, d, Ctrl-d o Ctrl-c cierran la ventana
		app.toggleDiff()
	case 27:
		switch readByteTimeout(more) {
		case 0:
			app.toggleDiff()
		case '[':
			switch readByteTimeout(more) {
			case 'A':
				app.moveTreeSel(-1)
			case 'B':
				app.moveTreeSel(1)
			case 'C':
				app.treeOpen()
			case 'D':
				app.treeCollapse()
			}
		}
	case 'k':
		app.moveTreeSel(-1)
	case 'j':
		app.moveTreeSel(1)
	case 'g':
		app.treeSel = 0
		app.previewSelected()
	case 'G':
		if len(app.treeFlat) > 0 {
			app.treeSel = len(app.treeFlat) - 1
			app.previewSelected()
		}
	case 'h':
		app.treeCollapse()
	case 13, 10, 'l':
		app.treeOpen()
	case 'a': // alternar solo-cambios / arbol completo
		app.treeAll = !app.treeAll
		app.rebuildTree()
		app.previewSelected()
	case 'K':
		app.scrollViewer(-3)
	case 'J':
		app.scrollViewer(3)
	case 21, 'u': // Ctrl-u / u: media pagina arriba
		app.scrollViewer(-app.viewerPage() / 2)
	case 'b':
		app.scrollViewer(-app.viewerPage())
	case 'f', ' ':
		app.scrollViewer(app.viewerPage())
	case ':':
		app.mode = modeCommand
		app.input = nil
	}
	return false
}

func (app *tuiApp) viewerPage() int {
	if app.floatViewH > 0 {
		return app.floatViewH
	}
	return 10
}

func (app *tuiApp) scrollViewer(delta int) {
	app.viewerOffset += delta
	if max := len(app.viewerLines) - app.viewerPage(); app.viewerOffset > max {
		app.viewerOffset = max
	}
	if app.viewerOffset < 0 {
		app.viewerOffset = 0
	}
}

// previewSelected carga en el visor el archivo bajo el cursor del arbol; en
// carpetas limpia el visor.
func (app *tuiApp) previewSelected() {
	node := app.selectedTreeNode()
	if node == nil || node.isDir {
		app.viewerRel = ""
		app.viewerAbs = ""
		return
	}
	app.openViewer(node)
}

// refreshViewer recarga el archivo previsualizado si cambio en disco: la IA
// edita mientras la ventana esta abierta y el codigo debe verse al dia.
func (app *tuiApp) refreshViewer() {
	if !app.diffMode || app.viewerAbs == "" {
		return
	}
	info, err := os.Stat(app.viewerAbs)
	if err != nil || info.ModTime().Equal(app.viewerMtime) {
		return
	}
	node := app.selectedTreeNode()
	if node == nil || node.abs != app.viewerAbs {
		return
	}
	offset := app.viewerOffset
	app.openViewer(node)
	app.viewerOffset = offset
	app.scrollViewer(0) // reclamp por si el archivo se acorto
	// El archivo cambio: los contadores +/- del titulo tambien.
	app.computeDiffStats()
}

func (app *tuiApp) moveTreeSel(delta int) {
	count := len(app.treeFlat)
	if count == 0 {
		return
	}
	app.treeSel = (app.treeSel + delta + count) % count
	app.previewSelected()
}

func (app *tuiApp) selectedTreeNode() *treeNode {
	if app.treeSel < 0 || app.treeSel >= len(app.treeFlat) {
		return nil
	}
	return app.treeFlat[app.treeSel].node
}

// treeOpen expande la carpeta o abre el archivo en el visor integrado.
func (app *tuiApp) treeOpen() {
	node := app.selectedTreeNode()
	if node == nil {
		return
	}
	if node.isDir {
		node.load()
		node.expanded = !node.expanded
		app.treeFlat = flattenTree(app.treeRoot)
		return
	}
	app.openViewer(node)
	// Enter en un archivo tambien centra el visor al inicio.
	app.viewerOffset = 0
}

// treeCollapse cierra la carpeta, o salta al padre si es un archivo.
func (app *tuiApp) treeCollapse() {
	node := app.selectedTreeNode()
	if node == nil {
		return
	}
	if node.isDir && node.expanded {
		node.expanded = false
		app.treeFlat = flattenTree(app.treeRoot)
		return
	}
	parent := filepath.Dir(node.rel)
	for index, item := range app.treeFlat {
		if item.node.isDir && item.node.rel == parent {
			app.treeSel = index
			return
		}
	}
}

const viewerMaxBytes = 2 * 1024 * 1024

// openViewer carga el archivo en el visor integrado (solo lectura, sin
// dependencias externas).
func (app *tuiApp) openViewer(node *treeNode) {
	raw, err := os.ReadFile(node.abs)
	if err != nil {
		app.message = "no se pudo leer " + node.rel + ": " + err.Error()
		return
	}
	if len(raw) > viewerMaxBytes {
		raw = raw[:viewerMaxBytes]
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	app.viewerLines = hl.Lines(strings.Split(content, "\n"), filepath.Ext(node.name))
	app.viewerRel = node.rel
	app.viewerAbs = node.abs
	app.viewerOffset = 0
	if info, err := os.Stat(node.abs); err == nil {
		app.viewerMtime = info.ModTime()
	}
	app.viewerMarks, app.viewerAllNew = app.computeViewerMarks(node.rel)
}

var hunkHeaderRe = regexp.MustCompile(`^@@ .*\+(\d+)(?:,\d+)? @@`)

// computeViewerMarks marca que lineas del archivo las trajo esta rama, para
// resaltarlas en el mismo visor (sin ventana doble): se parsean los hunks del
// diff contra la base y se apuntan los numeros de linea nuevos.
func (app *tuiApp) computeViewerMarks(rel string) (map[int]bool, bool) {
	worker := app.currentWorker()
	if worker == nil {
		return nil, false
	}
	for _, file := range app.diffFiles {
		fileRel, err := filepath.Rel(worker.Root, filepath.Join(file.Dir, file.Path))
		if err != nil || fileRel != rel {
			continue
		}
		if file.Status == "A" || file.Base == "" || !file.InBase {
			return nil, true // archivo nuevo: todo es de la rama
		}
		raw, err := ws.GitOut(file.Dir, "diff", file.Base, "--", file.Path)
		if err != nil {
			return nil, false
		}
		marks := map[int]bool{}
		newLine := 0
		for _, line := range strings.Split(raw, "\n") {
			if match := hunkHeaderRe.FindStringSubmatch(line); match != nil {
				newLine, _ = strconv.Atoi(match[1])
				continue
			}
			if newLine == 0 || line == "" {
				continue
			}
			switch line[0] {
			case '+':
				marks[newLine] = true
				newLine++
			case '-':
			default:
				newLine++
			}
		}
		return marks, false
	}
	return nil, false
}

// treeLines pinta el explorador: carpetas expandibles y los archivos que
// trae la rama resaltados por estado (A verde, M ambar, D rojo).
func (app *tuiApp) treeLines(width int) []string {
	out := make([]string, 0, len(app.treeFlat))
	for index, item := range app.treeFlat {
		node := item.node
		marker := "  "
		if index == app.treeSel {
			marker = ansiMagenta + "> " + ansiReset
		}
		indent := strings.Repeat("  ", item.depth)
		line := ""
		icon := fileIcon(node)
		if node.isDir {
			dirty := ""
			if app.dirtyDirs[node.rel] {
				dirty = ansiYellow + " *" + ansiReset
			}
			line = marker + indent + icon + " " + ansiCyan + node.name + "/" + ansiReset + dirty
		} else {
			color := ansiText
			tag := ""
			switch app.changed[node.rel] {
			case "M":
				color = ansiYellow
				tag = ansiYellow + " M" + ansiReset
			case "A":
				color = ansiGreen
				tag = ansiGreen + " A" + ansiReset
			case "D":
				color = ansiRed
				tag = ansiRed + " D" + ansiReset
			}
			line = marker + indent + icon + " " + color + node.name + ansiReset + tag
		}
		out = append(out, clipANSI(line, width))
	}
	if len(out) == 0 {
		out = append(out, ansiDim+"carpeta vacia"+ansiReset)
	}
	return out
}

// miniStatus: animacion + contexto de UN agente, para el renglon por agente.
func miniStatus(agent ws.AgentState, frame int) string {
	switch agent.Status {
	case "working":
		return agentSpinner(frame) + ctxLabel(agent.CtxPct)
	case "waiting":
		return ansiYellow + pulse(frame) + ansiReset + ctxLabel(agent.CtxPct)
	default:
		return ansiDim + "-" + ansiReset
	}
}

// workerLines arma las filas de un worker: cabecera + un renglon por agente
// (animacion, ctx y tarea de cada uno) o el formato compacto de dos lineas.
// cardTint: color base del fondo interior de la tarjeta segun el estado.
// Tonos oscuros para que el texto siga legible encima.
func cardTint(status string, selected bool) (r, g, b int, ok bool) {
	if selected {
		return 46, 12, 42, true // magenta profundo
	}
	switch status {
	case "working":
		return 8, 50, 58, true // teal
	case "waiting":
		return 54, 38, 8, true // ambar
	case "sin agente":
		return 54, 14, 20, true // rojo vino
	}
	return 0, 0, 0, false // apagada: sin fondo
}

// waveTable: intensidades del degradado que recorre la tarjeta de arriba a
// abajo; el frame lo desplaza y el fondo "respira" en movimiento.
var waveTable = []int{55, 62, 72, 84, 94, 100, 94, 84, 72, 62}

func waveBG(baseR, baseG, baseB, frame, row int) string {
	factor := waveTable[(row*2+frame/2)%len(waveTable)]
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm",
		baseR*factor/100, baseG*factor/100, baseB*factor/100)
}

func (app *tuiApp) workerLines(worker ws.WorkerSummary, index, leftW int) []string {
	selected := index == app.selected
	marker := "  "
	nameColor := ansiText
	if selected {
		marker = ansiMagenta + "> " + ansiReset
		nameColor = ansiBold + ansiCyan
	}
	speakTag := ""
	if label, on := voice.CurrentSpeaking(); on && label != "" && label == worker.Ticket {
		speakTag = ansiBold + ansiMagenta + " " + speakGlyph(app.frame) + ansiReset
	}

	if app.zoomed {
		glyph := ansiDim + "-" + ansiReset
		switch worker.Agent.Status {
		case "working":
			glyph = agentSpinner(app.frame)
		case "waiting":
			glyph = ansiYellow + pulse(app.frame) + ansiReset
		case "sin agente":
			glyph = ansiRed + "!" + ansiReset
		default:
			if len(worker.Externals) > 0 {
				glyph = ansiCyan + pulse(app.frame) + ansiReset
			}
		}
		if count := len(worker.Agents); count > 1 {
			glyph += ansiCyan + fmt.Sprintf("x%d", count) + ansiReset
		}
		return []string{fmt.Sprintf("%s%s%s%s %s%s", marker, nameColor, padRight(worker.Ticket, 10), ansiReset, glyph, speakTag)}
	}

	// Tarjeta: bordes redondeados con el titulo sobre el borde superior y
	// "sombra" (borde inferior y derecho siempre en el gris del marco, como
	// luz desde arriba-izquierda). El color del marco cuenta el estado:
	// cyan trabajando, ambar esperando, rojo anomalo, magenta la
	// seleccionada; las apagadas quedan opacas.
	frameColor := ansiSep
	faded := true
	switch worker.Agent.Status {
	case "working":
		frameColor = ansiCyan
		faded = false
	case "waiting":
		frameColor = ansiYellow
		faded = false
	case "sin agente":
		frameColor = ansiRed
		faded = false
	default:
		if len(worker.Externals) > 0 {
			faded = false
		}
	}
	if selected {
		frameColor = ansiBold + ansiMagenta
	}
	if faded && !selected {
		nameColor = ansiDim
	}
	inner := leftW - 4

	var content []string
	statusStr := compactStatus(worker, app.frame)
	if len(worker.Agents) > 1 {
		extra := ""
		if count := len(worker.Externals); count > 0 {
			extra = ansiCyan + fmt.Sprintf(" +%d suelto", count) + ansiReset
		}
		statusStr = ansiCyan + fmt.Sprintf("%d agentes", len(worker.Agents)) + ansiReset + extra
		for i, agent := range worker.Agents {
			task := agent.Task
			if task == "" {
				task = agent.Activity
			}
			if task == "" {
				task = "sin tarea visible"
			}
			// La tarea puede ocupar hasta 3 renglones (corte por palabra): el
			// primero sigue al estado y los demas arrancan indentados.
			head := ansiDim + fmt.Sprintf("IA%d ", i+1) + ansiReset +
				miniStatus(agent, app.frame) + " "
			availFirst := inner - displayWidth(head)
			words := strings.Fields(task)
			first := ""
			next := 0
			for ; next < len(words); next++ {
				candidate := words[next]
				if first != "" {
					candidate = first + " " + candidate
				}
				if displayWidth(candidate) > availFirst {
					break
				}
				first = candidate
			}
			content = append(content, clipANSI(head+ansiCyan+first, inner))
			rest := strings.Join(words[next:], " ")
			for _, seg := range wrapPlain(rest, inner-4, 2) {
				content = append(content, clipANSI("    "+ansiCyan+seg, inner))
			}
		}
	} else {
		title := worker.Description
		titleColor := ansiDim
		if worker.Agent.Running && worker.Agent.Task != "" {
			title = worker.Agent.Task
			titleColor = ansiCyan
		}
		if faded && !selected {
			titleColor = ansiDim
		}
		for _, seg := range wrapPlain(title, inner, 2) {
			content = append(content, titleColor+seg+ansiReset)
		}
	}

	cardTitle := " " + marker + nameColor + worker.Ticket + ansiReset + " " + statusStr + speakTag + " "
	cardTitle = clipANSI(cardTitle, leftW-5)
	fill := leftW - 3 - displayWidth(cardTitle)
	if fill < 0 {
		fill = 0
	}
	tintR, tintG, tintB, tinted := cardTint(worker.Agent.Status, selected)
	out := []string{frameColor + boxTL + boxH + ansiReset + cardTitle +
		frameColor + strings.Repeat(boxH, fill) + boxTR + ansiReset}
	for i, line := range content {
		row := frameColor + boxV + ansiReset
		if tinted {
			// Fondo interior con el degradado en movimiento: los resets del
			// contenido reactivan el fondo para que no se corte a mitad de
			// linea, y el relleno hasta el borde tambien queda tintado.
			bg := waveBG(tintR, tintG, tintB, app.frame, i)
			body := bg + strings.ReplaceAll(clipANSI(line, inner), ansiReset, ansiReset+bg)
			if pad := inner - displayWidth(body); pad > 0 {
				body += strings.Repeat(" ", pad)
			}
			row += bg + " " + body + " " + ansiBGOff
		} else {
			row += " " + padANSI(line, inner) + " "
		}
		row += ansiSep + boxV + ansiReset
		out = append(out, row)
	}
	out = append(out, ansiSep+boxBL+strings.Repeat(boxH, leftW-2)+boxBR+ansiReset)
	return out
}

func compactStatus(worker ws.WorkerSummary, frame int) string {
	agent := worker.Agent
	// Claudes corriendo en terminales sueltas (fuera de tmux): el panel los ve
	// por su cwd aunque no pueda adjuntarse a ellos.
	extra := ""
	if count := len(worker.Externals); count > 0 {
		extra = ansiCyan + fmt.Sprintf(" +%d suelto", count) + ansiReset
	}
	switch agent.Status {
	case "working":
		return agentSpinner(frame) + ansiGreen + " work" + ansiReset + ctxLabel(agent.CtxPct) + extra
	case "waiting":
		return ansiYellow + pulse(frame) + " wait" + ansiReset + ctxLabel(agent.CtxPct) + extra
	case "sin agente":
		return ansiRed + "sin agente" + ansiReset + extra
	default:
		if extra != "" {
			return ansiCyan + pulse(frame) + fmt.Sprintf(" %d claude suelto", len(worker.Externals)) + ansiReset
		}
		return ansiDim + "-" + ansiReset
	}
}

// padANSI rellena hasta el ancho visible (ignorando codigos de color).
func padANSI(s string, width int) string {
	visible := displayWidth(s)
	if visible >= width {
		return clipANSI(s, width)
	}
	return s + strings.Repeat(" ", width-visible)
}

func wrapRunes(s string, width, lines int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 && len(out) < lines {
		n := width
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

// ensureTermSize ajusta la ventana tmux al tamano del panel derecho para que
// el TUI de adentro se dibuje completo.
func (app *tuiApp) ensureTermSize(sess string, w, h int) {
	if app.sizedSess == sess && app.sizedW == w && app.sizedH == h {
		return
	}
	_, _ = tmuxx.Run("resize-window", "-t", sess, "-x", fmt.Sprint(w), "-y", fmt.Sprint(h))
	app.sizedSess, app.sizedW, app.sizedH = sess, w, h
}

// rightPaneSession resuelve que sesion se muestra a la derecha: el agente si
// vive, el shell del worker si existe, o nada.
func (app *tuiApp) rightPaneSession(worker *ws.WorkerSummary) string {
	if worker == nil {
		return ""
	}
	sessions := ws.WorkerSessions(worker.ID)
	if len(sessions) == 0 {
		return ""
	}
	if active := app.activeTab[worker.ID]; active != "" {
		for _, sess := range sessions {
			if sess == active {
				return sess
			}
		}
	}
	if worker.Agent.Running && worker.Agent.Session != "" {
		return worker.Agent.Session
	}
	return sessions[0]
}

// cycleTab pasa a la pestaña siguiente/anterior de sesiones del worker.
func (app *tuiApp) cycleTab(delta int) {
	worker := app.currentWorker()
	if worker == nil {
		return
	}
	sessions := ws.WorkerSessions(worker.ID)
	if len(sessions) < 2 {
		app.message = "este worker tiene una sola pestaña ([s] agente nuevo, [n] shell nuevo)"
		return
	}
	current := app.rightSess
	index := 0
	for i, sess := range sessions {
		if sess == current {
			index = i
			break
		}
	}
	index = (index + delta + len(sessions)) % len(sessions)
	app.activeTab[worker.ID] = sessions[index]
}

// tabsBar: pestañas literales arriba de la terminal, estilo editor. La
// activa en cyan solido, las demas en gris sobre fondo oscuro, y un [+]
// recordando que n abre otra.
const (
	tabActiveStyle   = "\x1b[48;2;0;240;255m\x1b[38;2;5;6;13m"
	tabInactiveStyle = "\x1b[48;2;16;26;48m\x1b[38;2;95;112;153m"
)

func (app *tuiApp) tabsBar(worker *ws.WorkerSummary, active string) string {
	sessions := ws.WorkerSessions(worker.ID)
	if len(sessions) == 0 {
		return ""
	}
	agentBase := ws.AgentSession(worker.ID)
	var bar strings.Builder
	agentCount, shellCount := 0, 0
	for _, sess := range sessions {
		label := ""
		if sess == agentBase || strings.HasPrefix(sess, agentBase+"-") {
			agentCount++
			label = fmt.Sprintf("IA %d", agentCount)
		} else {
			shellCount++
			label = fmt.Sprintf("sh %d", shellCount)
		}
		if sess == active {
			bar.WriteString(tabActiveStyle + ansiBold + " " + label + " " + ansiReset + ansiBGOff + " ")
		} else {
			bar.WriteString(tabInactiveStyle + " " + label + " " + ansiReset + ansiBGOff + " ")
		}
	}
	bar.WriteString(tabInactiveStyle + " + " + ansiReset + ansiBGOff)
	return bar.String()
}

func (app *tuiApp) handleHelpKey(key byte, more <-chan byte) bool {
	page := app.cachedRows - 4
	if page < 5 {
		page = 5
	}
	switch key {
	case 'q', '?', 13, 10:
		app.helpMode = false
		app.helpOffset = 0
	case 27:
		switch readByteTimeout(more) {
		case 0:
			app.helpMode = false
			app.helpOffset = 0
		case '[':
			switch readByteTimeout(more) {
			case 'A':
				app.helpOffset--
			case 'B':
				app.helpOffset++
			}
		}
	case 'k':
		app.helpOffset--
	case 'j':
		app.helpOffset++
	case 4: // Ctrl-d
		app.helpOffset += page / 2
	case 21: // Ctrl-u
		app.helpOffset -= page / 2
	case 'g':
		app.helpOffset = 0
	case 'G':
		app.helpOffset = 1 << 20
	}
	if app.helpOffset < 0 {
		app.helpOffset = 0
	}
	return false
}

func (app *tuiApp) renderHelp(fw *frameWriter) {
	var lines []string
	section := func(title string) {
		lines = append(lines, "", ansiBold+ansiMagenta+title+ansiReset)
	}
	item := func(keys, desc string) {
		lines = append(lines, "  "+ansiCyan+padRight(keys, 16)+ansiReset+ansiText+desc+ansiReset)
	}

	section("LISTA (tras Ctrl+q; Tab vuelve a la terminal)")
	item("j/k  flechas", "moverse entre workers")
	item("gg / G", "primer / ultimo worker")
	item("/", "filtrar workers en vivo (enter fija, esc limpia)")
	item("Tab", "pasar el teclado a la terminal del worker (hablar con la IA)")
	item("h/l  [ ]", "cambiar de pestaña entre las sesiones del worker")
	item("Ctrl+s", "iniciar el agente; si ya hay uno, otro en pestaña nueva")
	item("n", "nueva pestaña con shell en la raiz del worker")
	item("z", "zoom: la conversacion toma casi todo el ancho")
	item("d", "arbol de cambios de la rama del worker")
	item("enter", "detalle del worker: acciones de servicios, repos, PRs")
	item("a", "enfocar la pestaña del agente (dentro del panel, sin invadir)")
	item("F", "abrir la sesion en una ventana del sistema")
	item("i", "interrumpir al agente (le manda Escape)")
	item("x x", "detener el agente (dos veces seguidas)")
	item("r", "refrescar")
	item("v", "voz: leer en voz alta la respuesta de cada agente al terminar")
	item("V", "voz: leer el ultimo resultado del worker seleccionado")
	item("?", "esta ayuda")
	item("q / Ctrl+c", "salir del panel")

	section("TERMINAL (modo por defecto: el foco siempre vive aqui)")
	item("(todas)", "cada tecla va a la sesion del agente, cursor real incluido")
	item("Ctrl+j / Ctrl+k", "worker siguiente / anterior sin soltar el foco")
	item("Ctrl+h / Ctrl+l", "pestaña anterior / siguiente del worker")
	item("Ctrl+s", "iniciar el agente (nada se crea solo sin esta tecla)")
	item("Ctrl+q", "bajar a la lista (comandos, filtro, diff, detalle)")

	section("EXPLORADOR FLOTANTE (Ctrl+d desde la terminal, d desde la lista)")
	item("j/k  g G", "moverse por el arbol: el archivo se previsualiza solo")
	item("l/enter  h", "expandir / colapsar carpetas")
	item("J/K u f b", "mover el codigo (el visor sigue los cambios de la IA en vivo)")
	item("a", "alternar: solo archivos de la rama (default) / arbol completo")
	item("q / esc / C-d", "cerrar la ventana y seguir con los agentes")

	section("DETALLE (tras l/enter)")
	item("j/k", "moverse entre servicios")
	item("enter / l", "attach a la sesion tmux del servicio (logs en vivo)")
	item("s / R / x", "start / restart / stop del servicio")
	item("X X", "detener TODOS los servicios del worker")
	item("a", "attach al agente   t: shell en la raiz   F: ventana sistema")
	item("p", "refrescar PRs")
	item("h / esc", "volver a la lista")

	section("COMANDOS (:)")
	item(":q :qa", "salir   :r refrescar   :noh limpiar filtro")
	item(":start [texto]", "iniciar agente, con prompt inicial opcional")
	item(":send <texto>", "mandarle un mensaje al agente sin salir del panel")
	item(":esc :stop!", "interrumpir / detener agente   :stopall! todos los servicios")
	item(":prof [nombre]", "cuenta del worker (los perfiles del config.json); :prof - quita")
	item(":wt <nombre>", "crear universo paralelo (git worktree) en workspaces sin vel")
	item(":d :window :sh", "arbol de cambios / ventana sistema / shell del worker")
	item(":leer", "leer en voz alta el ultimo resultado del worker seleccionado")
	item(":help", "esta ayuda")

	section("TMUX (dentro de un attach)")
	item("Ctrl+b  d", "detach: volver al panel dejando la sesion viva")

	avail := fw.rows - 3
	if avail < 5 {
		avail = 5
	}
	maxOffset := len(lines) - avail
	if maxOffset < 0 {
		maxOffset = 0
	}
	if app.helpOffset > maxOffset {
		app.helpOffset = maxOffset
	}
	position := ""
	if maxOffset > 0 {
		position = ansiDim + fmt.Sprintf("  (%d-%d de %d)", app.helpOffset+1, app.helpOffset+avail, len(lines)) + ansiReset
	}
	fw.line(ansiBold + ansiCyan + "VERSECAM" + ansiReset + ansiDim + "  atajos de teclado" + ansiReset + position)
	fw.separator()
	for i := app.helpOffset; i < len(lines) && i < app.helpOffset+avail; i++ {
		fw.line(lines[i])
	}
}

// toggleVoice enciende la lectura en voz alta de las respuestas (kokoro via
// el sidecar de voz local).
func (app *tuiApp) toggleVoice() {
	if voice.On.Load() {
		voice.On.Store(false)
		app.message = "voz apagada"
		return
	}
	if !voice.Healthy() {
		app.message = "sidecar de voz apagado (se espera HTTP en :9111, ver VERSECAM_VOICE_URL)"
		return
	}
	voice.On.Store(true)
	app.message = "voz encendida: leere las respuestas al terminar cada turno"
	voice.SpeakAsync("", "voz encendida")
}

// speakGlyph: nota musical alternante mientras suena la voz.
func speakGlyph(frame int) string {
	if frame/3%2 == 0 {
		return "♪"
	}
	return "♫"
}

// speakLastReply lee en voz alta el ultimo resultado de la pestaña activa
// del worker seleccionado (la misma que se ve en el panel derecho), sin
// necesidad de tener el toggle de voz encendido.
func (app *tuiApp) speakLastReply() {
	worker := app.currentWorker()
	if worker == nil {
		app.message = "no hay worker seleccionado"
		return
	}
	if !voice.Healthy() {
		app.message = "sidecar de voz apagado (se espera HTTP en :9111, ver VERSECAM_VOICE_URL)"
		return
	}
	sess := app.rightPaneSession(worker)
	if sess == "" {
		app.message = "este worker no tiene sesiones que leer"
		return
	}
	reply := ws.ParseReply(tmuxx.Capture(sess, 400))
	if reply == "" {
		app.message = "la pestaña activa no tiene respuesta que leer"
		return
	}
	app.message = "leyendo el ultimo resultado de " + worker.Ticket
	voice.SpeakNow(worker.Ticket, reply)
}

// voiceService busca el estado del sidecar de voz. Cada worker lista su
// propia variante (vel-voice-<worker>), casi siempre apagada: la que vale es
// la que ESTE corriendo (normalmente la global del principal, sin sufijo).
func (app *tuiApp) voiceService() (ws.ServiceState, bool) {
	var found ws.ServiceState
	ok := false
	for _, worker := range app.view.Workers {
		for _, svc := range worker.Services {
			if svc.Name != "voice" && svc.Name != "voz" {
				continue
			}
			if svc.Running {
				return svc, true
			}
			if !ok {
				found, ok = svc, true
			}
		}
	}
	return found, ok
}

// servicesSection pinta los servicios corriendo del worker en la zona
// inferior izquierda (reemplazo del viejo resumen del worker). Solo aparece
// si hay algo corriendo. El sidecar de voz no se lista: su estado vive en el
// header.
func (app *tuiApp) servicesSection(worker *ws.WorkerSummary, width int) []string {
	if worker == nil {
		return nil
	}
	var running []ws.ServiceState
	total := 0
	for _, svc := range worker.Services {
		if svc.Name == "voice" || svc.Name == "voz" {
			continue
		}
		total++
		if svc.Running {
			running = append(running, svc)
		}
	}
	if len(running) == 0 {
		return nil
	}
	out := []string{
		ansiSep + strings.Repeat("-", width) + ansiReset,
		ansiBold + ansiCyan + "SERVICIOS" + ansiReset +
			ansiDim + fmt.Sprintf(" %d/%d corriendo", len(running), total) + ansiReset,
	}
	const maxRows = 10
	for index, svc := range running {
		if index >= maxRows {
			out = append(out, ansiDim+fmt.Sprintf("  + %d corriendo mas", len(running)-maxRows)+ansiReset)
			break
		}
		out = append(out, " "+ansiGreen+pulse(app.frame+index)+ansiReset+" "+
			padRight(svc.Name, 20)+ansiGreen+" running"+ansiReset+memLabel(svc.MemKB))
	}
	if stopped := total - len(running); stopped > 0 {
		out = append(out, ansiDim+fmt.Sprintf(" + %d apagados  [C-q][enter] detalle", stopped)+ansiReset)
	}
	return out
}

func (app *tuiApp) renderList(fw *frameWriter) {
	now := time.Now().Format("15:04:05")
	header := ansiBold + ansiCyan + "VERSECAM" + ansiMagenta + " " + strings.ToUpper(config.ActiveWSName) + ansiReset + ansiDim + "  " + now + ansiReset
	if config.ActiveProfile != "" {
		header += ansiDim + "  agente:" + ansiReset + ansiCyan + config.ActiveProfile + ansiReset
	}
	// El servicio de voz vive arriba, no en la seccion de servicios: es el
	// sidecar del panel, no un servicio del workspace.
	if svc, ok := app.voiceService(); ok {
		if svc.Running {
			header += "  " + ansiGreen + "voice " + pulse(app.frame) + ansiReset + memLabel(svc.MemKB)
		} else {
			header += "  " + ansiDim + "voice off" + ansiReset
		}
	}
	if voice.On.Load() {
		header += ansiMagenta + "  voz" + ansiReset
	}
	if label, on := voice.CurrentSpeaking(); on {
		header += ansiBold + ansiMagenta + "  " + speakGlyph(app.frame) + " leyendo"
		if label != "" {
			header += " " + label
		}
		header += ansiReset
	}
	if app.filter != "" {
		header += ansiMagenta + fmt.Sprintf("  /%s (%d)", app.filter, len(app.visibleWorkers())) + ansiReset
	}
	fw.line(header)
	fw.separator()

	leftW := 44
	if fw.cols < 110 {
		leftW = fw.cols * 2 / 5
	}
	if app.zoomed {
		leftW = 15
	}
	bodyH := fw.rows - 4

	// columna izquierda: workers compactos + servicios corriendo abajo
	var left []string
	visible := app.visibleWorkers()
	var bottom []string
	if !app.zoomed {
		bottom = app.servicesSection(app.currentWorker(), leftW)
	}
	reserved := len(bottom)
	if app.zoomed {
		reserved = 1
	}
	availRows := bodyH - reserved
	if availRows < 6 {
		availRows = 6
	}
	// Filas de altura variable (un renglon por agente): se arma todo y se
	// recorta una ventana que mantenga visible el bloque del seleccionado.
	var entries []string
	var owner []int
	for index, worker := range visible {
		for _, line := range app.workerLines(worker, index, leftW) {
			entries = append(entries, line)
			owner = append(owner, index)
		}
	}
	startLine := 0
	if len(entries) > availRows {
		selStart, selEnd := -1, -1
		for i, ownerIdx := range owner {
			if ownerIdx == app.selected {
				if selStart < 0 {
					selStart = i
				}
				selEnd = i
			}
		}
		if selStart < 0 {
			selStart, selEnd = 0, 0
		}
		block := selEnd - selStart + 1
		startLine = selStart - (availRows-block)/2
		if startLine < 0 {
			startLine = 0
		}
		if startLine+availRows > len(entries) {
			startLine = len(entries) - availRows
		}
	}
	for i := startLine; i < len(entries) && i < startLine+availRows; i++ {
		left = append(left, entries[i])
	}
	if len(visible) == 0 {
		left = append(left, ansiDim+"cargando workers..."+ansiReset)
	}

	left = append(left, bottom...)

	// tres columnas: workers | conversacion | explorador (cuando esta abierto)
	app.cursorSeq = ""
	worker := app.currentWorker()

	midW := fw.cols - leftW - 3

	var midTitle string
	var midBody []string
	app.rightSess = app.rightPaneSession(worker)
	if app.rightSess != "" {
		termH := bodyH - 1
		app.ensureTermSize(app.rightSess, midW, termH)
		hint := "Tab: escribir"
		hintColor := ansiSep
		if app.focusRight {
			hint = "Ctrl+q: volver"
			hintColor = ansiMagenta
		}
		midTitle = app.tabsBar(worker, app.rightSess) + "  " + hintColor + hint + ansiReset
		app.monitor.watch(app.rightSess, app.focusRight && !app.diffMode)
		// El monitor captura en background; solo al cambiar de sesion se
		// captura aqui mismo para no pintar la pantalla de la anterior.
		snap := app.snap
		if snap == nil || snap.session != app.rightSess {
			lines, x, y, visible := tmuxx.ScreenAndCursor(app.rightSess)
			snap = &paneSnapshot{session: app.rightSess, lines: lines, curX: x, curY: y, curVis: visible}
			app.snap = snap
		}
		midBody = make([]string, len(snap.lines))
		for i, line := range snap.lines {
			midBody[i] = paneContrast.Replace(line)
		}
		if app.focusRight && !app.diffMode && snap.curVis && snap.curY < termH && snap.curX < midW && snap.curY < len(midBody) {
			midBody[snap.curY] = overlayCursor(midBody[snap.curY], snap.curX, shipGlyph)
		}
	} else {
		app.monitor.watch("", false)
		midTitle = ansiDim + "sin terminal" + ansiReset
		if worker != nil && len(worker.Externals) > 0 {
			midTitle = ansiCyan + fmt.Sprintf("%d claude corriendo en terminales sueltas", len(worker.Externals)) + ansiReset +
				ansiDim + "  (fuera de tmux: no adjuntables)" + ansiReset
		}
		midBody = []string{"", ansiDim + "  [Ctrl+s] inicia el agente de este worker" + ansiReset,
			ansiDim + "  [Ctrl+j/k] moverse entre workers" + ansiReset,
			ansiDim + "  [Ctrl+q] lista: [n] shell  [enter] detalle" + ansiReset}
	}

	overlayRows, overlayTop, overlayLeft := app.floatExplorer(fw.cols, bodyH)

	midBorder := ansiSep + "|" + ansiReset
	if app.focusRight {
		midBorder = ansiMagenta + "|" + ansiReset
	}

	for i := 0; i < bodyH; i++ {
		l := ""
		if i < len(left) {
			l = left[i]
		}
		m := ""
		if i == 0 {
			m = midTitle
		} else if i-1 < len(midBody) {
			m = strings.ReplaceAll(midBody[i-1], "\t", "    ")
		}
		row := padANSI(l, leftW) + " " + midBorder + " " + m
		// La ventana flotante se compone ENCIMA de la fila: se recorta la fila
		// base hasta el borde izquierdo del box y se pega el contenido.
		if overlayRows != nil && i >= overlayTop && i-overlayTop < len(overlayRows) {
			row = padANSI(clipANSI(row, overlayLeft-1), overlayLeft-1) + overlayRows[i-overlayTop]
		}
		fw.line(row)
	}
}

// floatExplorer arma la ventana flotante del explorador: arbol de archivos a
// la izquierda y el codigo del archivo bajo el cursor a la derecha, con el
// contenido siempre al dia (refreshViewer lo recarga si la IA lo cambia).
func (app *tuiApp) floatExplorer(cols, bodyH int) (rows []string, top, left int) {
	if !app.diffMode {
		return nil, 0, 0
	}
	width := cols * 5 / 6
	if width > 170 {
		width = 170
	}
	if width < 40 {
		width = cols - 2
	}
	height := bodyH - 4
	if height < 6 {
		height = bodyH
	}
	left = (cols-width)/2 + 1
	if left < 1 {
		left = 1
	}
	top = (bodyH - height) / 2

	treeW := 34
	if treeW > width/3 {
		treeW = width / 3
	}
	codeW := width - treeW - 7 // "| " + " | " + " |"
	viewH := height - 4        // bordes + titulo + separador
	app.floatViewH = viewH

	border := ansiMagenta
	horizontal := border + "+" + strings.Repeat("-", width-2) + "+" + ansiReset
	side := border + "|" + ansiReset

	// Resumen estilo git diff: archivos y lineas +verdes/-rojas.
	mode := ansiDim + fmt.Sprintf("%d archivos", len(app.diffFiles)) + ansiReset
	if app.treeAll {
		mode = ansiDim + "arbol completo" + ansiReset
	}
	if app.diffAdds > 0 || app.diffDels > 0 {
		mode += " " + ansiGreen + fmt.Sprintf("+%d", app.diffAdds) + ansiReset +
			" " + ansiRed + fmt.Sprintf("-%d", app.diffDels) + ansiReset
	}
	// Contexto: en que worktree estoy y de que proyecto (repo) es el archivo.
	where := ""
	if worker := app.currentWorker(); worker != nil {
		project := filepath.Base(worker.Root)
		repoDir := app.treeRootPath
		if worker.RepoCount > 1 && strings.Contains(app.viewerRel, "/") {
			// Workspace multi-repo: el primer segmento de la ruta es el repo.
			project, _, _ = strings.Cut(app.viewerRel, "/")
			repoDir = filepath.Join(app.treeRootPath, project)
		}
		where = ansiBold + ansiMagenta + worker.Ticket + ansiReset +
			ansiDim + " · " + ansiReset + ansiCyan + project + ansiReset
		if branch := app.branchFor(repoDir); branch != "" {
			where += ansiDim + " · " + ansiReset + ansiGreen + branch + ansiReset
		}
		where += "  "
	}
	title := ansiBold + ansiCyan + " EXPLORADOR " + ansiReset + where + mode + ansiDim + "  [a] alterna" + ansiReset
	if app.viewerRel != "" {
		status := app.changed[app.viewerRel]
		tag := ""
		if status != "" {
			tag = ansiYellow + " [" + status + "]" + ansiReset
		}
		title += "  " + ansiText + clipANSI(app.viewerRel, width-displayWidth(title)-8) + ansiReset + tag +
			ansiGreen + " en vivo" + ansiReset
	}

	// Arbol con ventana centrada en la seleccion.
	treeRows := app.treeLines(treeW)
	if len(treeRows) > viewH {
		start := app.treeSel - viewH/2
		if start < 0 {
			start = 0
		}
		if start+viewH > len(treeRows) {
			start = len(treeRows) - viewH
		}
		treeRows = treeRows[start : start+viewH]
	}

	// Codigo del archivo previsualizado (con las lineas de la rama en verde).
	var codeRows []string
	if app.viewerRel == "" {
		codeRows = []string{"", ansiDim + "  muevete con j/k: el archivo se muestra aqui" + ansiReset,
			ansiDim + "  [l/enter] abre carpetas  [J/K u f b] mueve el codigo" + ansiReset}
	} else {
		numWidth := len(fmt.Sprint(len(app.viewerLines)))
		for i := app.viewerOffset; i < len(app.viewerLines) && len(codeRows) < viewH; i++ {
			content := strings.ReplaceAll(app.viewerLines[i], "\t", "    ")
			if app.viewerAllNew || app.viewerMarks[i+1] {
				// Linea traida por la rama: fondo verde en toda la linea.
				line := ansiGreen + fmt.Sprintf("%*d + ", numWidth, i+1) + ansiReset + content
				line = ansiMarkBG + strings.ReplaceAll(line, ansiReset, ansiReset+ansiMarkBG)
				line = clipANSI(line, codeW)
				if pad := codeW - displayWidth(line); pad > 0 {
					line += ansiMarkBG + strings.Repeat(" ", pad)
				}
				codeRows = append(codeRows, line+ansiBGOff)
				continue
			}
			gutter := ansiDim + fmt.Sprintf("%*d   ", numWidth, i+1) + ansiReset
			codeRows = append(codeRows, gutter+content)
		}
	}

	rows = make([]string, 0, height)
	rows = append(rows, horizontal)
	rows = append(rows, side+padANSI(title, width-2)+side)
	rows = append(rows, border+"+"+strings.Repeat("-", treeW+2)+"+"+strings.Repeat("-", width-treeW-5)+"+"+ansiReset)
	for i := 0; i < viewH; i++ {
		tl := ""
		if i < len(treeRows) {
			tl = treeRows[i]
		}
		cl := ""
		if i < len(codeRows) {
			cl = codeRows[i]
		}
		rows = append(rows, side+" "+padANSI(tl, treeW)+" "+side+" "+padANSI(cl, codeW)+" "+side)
	}
	rows = append(rows, horizontal)
	return rows, top, left
}

func (app *tuiApp) renderDetail(fw *frameWriter) {
	detail := app.detail
	if detail == nil {
		fw.line(ansiDim + "cargando detalle..." + ansiReset)
		return
	}

	header := ansiBold + ansiCyan + detail.Ticket + ansiReset + " " + ansiText + detail.Description + ansiReset
	if label, on := voice.CurrentSpeaking(); on {
		header += ansiBold + ansiMagenta + "  " + speakGlyph(app.frame) + " leyendo"
		if label != "" && label != detail.Ticket {
			header += " " + label
		}
		header += ansiReset
	}
	fw.line(header)
	fw.separator()

	fw.line(ansiDim + "root: " + detail.Root + ansiReset)
	info := ""
	if detail.PortBase > 0 {
		info = fmt.Sprintf("ports: %d+  php: %d  ", detail.PortBase, detail.PHPPort)
	}
	if detail.DBName != "" {
		info += "db: " + detail.DBName
		if !detail.DBReady {
			info += ansiRed + " FALTA" + ansiReset
		}
	}
	info += memLabel(detail.MemKB)
	if info != "" {
		fw.line(ansiDim + info + ansiReset)
	}
	agentLine := "agente: " + agentStatusLabel(detail.Agent, app.frame) + ctxLabel(detail.Agent.CtxPct)
	if profile := config.ProfileForWorker(detail.ID); profile != "" {
		agentLine += ansiDim + "  perfil: " + ansiReset + ansiMagenta + profile + ansiReset
	}
	if detail.Agent.Task != "" {
		agentLine += ansiDim + "  task: " + ansiReset + detail.Agent.Task
	}
	fw.line(agentLine)

	fw.separator()
	fw.line(ansiBold + "SERVICIOS" + ansiReset)
	if len(detail.Services) == 0 {
		fw.line(ansiDim + "  sin servicios aplicables en este worker" + ansiReset)
	}
	for index, service := range detail.Services {
		marker := "  "
		if index == app.svcSelected {
			marker = ansiMagenta + "> " + ansiReset
		}
		state := ansiDim + "[--]" + ansiReset
		extra := ""
		if service.Running {
			// Orbital lento en lima: la senal de vida del servicio.
			state = ansiSep + "[" + ansiGreen + orbit(app.frame/3+index) + ansiSep + "]" + ansiReset + " "
			if len(service.Ports) > 0 {
				var urls []string
				for _, port := range service.Ports {
					urls = append(urls, fmt.Sprintf("http://localhost:%d", port))
				}
				extra = "  " + ansiCyan + strings.Join(urls, "  ") + ansiReset +
					ansiDim + "  pid " + service.PID + ansiReset + memLabel(service.MemKB)
			} else {
				extra = ansiYellow + "  sesion viva, nada en escucha (revisa logs)" + ansiReset +
					ansiDim + "  pid " + service.PID + ansiReset + memLabel(service.MemKB)
			}
		}
		fw.line(fmt.Sprintf("%s%s %s%s%s%s", marker, state, ansiText, padRight(service.Name, 24), ansiReset, extra))
	}

	fw.separator()
	fw.line(ansiBold + "REPOS" + ansiReset)
	if len(detail.Repos) == 0 {
		fw.line(ansiDim + "  cargando repos..." + ansiReset)
	}
	for _, repo := range detail.Repos {
		if repo.Error != "" {
			fw.line("  " + padRight(repo.Name, 24) + ansiDim + repo.Error + ansiReset)
			continue
		}
		gitState := ""
		if repo.Dirty > 0 {
			gitState += ansiYellow + fmt.Sprintf(" dirty %d", repo.Dirty) + ansiReset
		}
		if repo.Ahead > 0 {
			gitState += ansiMagenta + fmt.Sprintf(" ahead %d", repo.Ahead) + ansiReset
		}
		if repo.HasRemote && repo.Pushed && repo.Dirty == 0 {
			gitState = ansiGreen + " pushed" + ansiReset
		}
		fw.line("  " + padRight(repo.Name, 24) + ansiCyan + repo.Branch + ansiReset + gitState)
	}

	if len(detail.PRs) > 0 {
		fw.separator()
		fw.line(ansiBold + "PRS" + ansiReset)
		for _, pr := range detail.PRs {
			stateColor := ansiDim
			switch pr.State {
			case "OPEN":
				stateColor = ansiGreen
			case "MERGED":
				stateColor = ansiMagenta
			}
			fw.line(fmt.Sprintf("  %s#%d %s%s %s%s %s",
				stateColor, pr.Number, pr.State, ansiReset,
				ansiDim+pr.Checks+ansiReset, "", padRight(pr.Repo+": "+pr.Title, fw.cols-30)))
		}
	}
}
