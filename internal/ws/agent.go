package ws

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
	"versecam/internal/tmuxx"
)

// Un agente es una sesion tmux con Claude Code corriendo dentro del worker.
// Se usa tmux por la misma razon que vel: el proceso sobrevive al turno, el
// usuario puede hacer attach y el scrollback queda disponible.

func AgentSession(worker string) string {
	return config.AgentPrefix + "-" + worker
}

type activityRecord struct {
	hash string
	seen time.Time
}

var (
	activityMu sync.Mutex
	activity   = map[string]activityRecord{}
	// lastTask sobrevive al scroll: cuando la respuesta es larga, la linea del
	// prompt sale de la pantalla y el panel dejaria de saber en que anda.
	lastTask = map[string]string{}
	// lastCtx sobrevive a los dialogos: mientras Claude muestra un menu de
	// permisos no pinta la statusline con el ctx.
	lastCtx = map[string]int{}
)

func rememberCtx(session string, pct int) {
	activityMu.Lock()
	lastCtx[session] = pct
	activityMu.Unlock()
}

func recalledCtx(session string) int {
	activityMu.Lock()
	defer activityMu.Unlock()
	return lastCtx[session]
}

func rememberTask(session, task string) {
	if strings.TrimSpace(task) == "" {
		return
	}
	activityMu.Lock()
	lastTask[session] = task
	activityMu.Unlock()
}

func recalledTask(session string) string {
	activityMu.Lock()
	defer activityMu.Unlock()
	return lastTask[session]
}

// idleSeconds indica hace cuanto que la pantalla del agente no cambia. Sirve
// para distinguir "trabajando" de "esperando instrucciones".
func idleSeconds(session, screen string) int {
	sum := sha1.Sum([]byte(strings.TrimRight(screen, " \n")))
	h := hex.EncodeToString(sum[:])

	activityMu.Lock()
	defer activityMu.Unlock()

	rec, ok := activity[session]
	now := time.Now()
	if !ok || rec.hash != h {
		activity[session] = activityRecord{hash: h, seen: now}
		return 0
	}
	return int(now.Sub(rec.seen).Seconds())
}

func forgetActivity(session string) {
	activityMu.Lock()
	delete(activity, session)
	activityMu.Unlock()
}

// agentProcsCached lista los pids de CLIs de agente (claude, kimi) con un
// cache corto: agentState corre por worker en cada refresco.
var (
	agentProcsMu   sync.Mutex
	agentProcsSeen []int
	agentProcsAt   time.Time
)

func agentProcs() []int {
	agentProcsMu.Lock()
	defer agentProcsMu.Unlock()
	if time.Since(agentProcsAt) < 2*time.Second {
		return agentProcsSeen
	}
	var pids []int
	for _, name := range []string{"claude", "kimi"} {
		raw, err := execx.RunTimeout(5*time.Second, "", "pgrep", "-x", name)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(raw, "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	agentProcsSeen, agentProcsAt = pids, time.Now()
	return pids
}

func paneHasAgentProc(panePID string) bool {
	root, err := strconv.Atoi(strings.TrimSpace(panePID))
	if err != nil {
		return false
	}
	ancestors := map[int]bool{root: true}
	for _, pid := range agentProcs() {
		if isDescendantOf(pid, ancestors) {
			return true
		}
	}
	return false
}

type AgentState struct {
	Running    bool   `json:"running"`
	CtxPct     int    `json:"ctx_pct"`
	Session    string `json:"session"`
	PID        string `json:"pid"`
	UptimeSec  int64  `json:"uptime_sec"`
	IdleSec    int    `json:"idle_sec"`
	Status     string `json:"status"`
	Task       string `json:"task"`
	Activity   string `json:"activity"`
	ElapsedSec int    `json:"elapsed_sec"`
}

// liveAgentSession busca en que sesion del worker vive realmente el proceso
// del agente: la agent-* del panel o el shell del worker si el usuario lanzo
// claude a mano ahi.
// shellSession lleva el nombre del workspace: el mismo worker id (ej.
// "principal") existe en varios workspaces y sin prefijo las sesiones de un
// workspace se cuelan en el otro.
func ShellSession(worker string) string {
	return "shell-" + config.ActiveWSName + "-" + worker
}

// workerSessions lista TODAS las sesiones del worker: agentes y shells,
// incluidas las pestañas extra con sufijo numerico (agent-x-2, shell-x-2).
func WorkerSessions(worker string) []string {
	bases := []string{AgentSession(worker), ShellSession(worker)}
	var out []string
	for _, base := range bases {
		for _, sess := range tmuxx.Sessions() {
			if sess == base || strings.HasPrefix(sess, base+"-") {
				out = append(out, sess)
			}
		}
	}
	sort.Strings(out)
	return out
}

func liveAgentSession(worker string) string {
	for _, candidate := range WorkerSessions(worker) {
		if paneHasAgentProc(tmuxx.PanePID(candidate)) {
			return candidate
		}
	}
	return ""
}

func AgentStateFor(worker string) AgentState {
	base := AgentSession(worker)
	sess := liveAgentSession(worker)
	if sess == "" {
		st := AgentState{Session: base, Status: "idle"}
		if !tmuxx.HasSession(base) {
			forgetActivity(base)
			st.Status = "stopped"
			return st
		}
		// Sesion viva sin proceso de agente: el CLI murio o alguien uso la
		// sesion para otra cosa (ej. correr el panel adentro).
		st.Running = true
		st.PID = tmuxx.PanePID(base)
		st.Status = "sin agente"
		return st
	}
	return agentStateForSession(sess)
}

// workerAgents: el estado de CADA sesion del worker que tenga un agente vivo
// (varios claude/kimi en paralelo sobre el mismo worktree).
func workerAgents(worker string) []AgentState {
	var out []AgentState
	for _, sess := range WorkerSessions(worker) {
		if paneHasAgentProc(tmuxx.PanePID(sess)) {
			out = append(out, agentStateForSession(sess))
		}
	}
	return out
}

func agentStateForSession(sess string) AgentState {
	st := AgentState{Session: sess, Status: "idle"}
	st.Running = true
	st.PID = tmuxx.PanePID(sess)
	if started := tmuxx.SessionStarted(sess); started > 0 {
		st.UptimeSec = time.Now().Unix() - started
	}

	screen := tmuxx.Capture(sess, 60)
	st.IdleSec = idleSeconds(sess, screen)

	info := parseScreen(screen)
	if info.Task != "" {
		rememberTask(sess, info.Task)
	}
	st.Task = info.Task
	if st.Task == "" {
		st.Task = recalledTask(sess)
	}
	st.Activity = info.Activity
	st.ElapsedSec = info.ElapsedSec
	if info.CtxPct > 0 {
		rememberCtx(sess, info.CtxPct)
	}
	st.CtxPct = info.CtxPct
	if st.CtxPct == 0 {
		st.CtxPct = recalledCtx(sess)
	}

	// La linea de trabajo es la senal confiable; el cambio de pantalla solo
	// se usa como respaldo cuando el TUI no la esta pintando.
	switch {
	case info.Working:
		st.Status = "working"
	case st.IdleSec < 3:
		st.Status = "working"
	default:
		st.Status = "waiting"
	}
	return st
}

// AgentStart lanza el agente con el perfil efectivo del worker.
func AgentStart(sess, root, prompt, flags, profileWorker string) error {
	cmd, env := config.ResolveProfile(profileWorker)
	return AgentStartWith(sess, root, prompt, flags, cmd, env)
}

// AgentStartWith lanza el agente con un CLI y entorno explicitos (perfil
// elegido a mano en el selector de la app).
func AgentStartWith(sess, root, prompt, flags, cmd string, env map[string]string) error {
	if tmuxx.HasSession(sess) {
		return fmt.Errorf("la sesion %s ya existe", sess)
	}
	if _, err := tmuxx.Run("new-session", "-d", "-s", sess, "-x", "220", "-y", "50"); err != nil {
		return fmt.Errorf("no se pudo crear la sesion tmux: %w", err)
	}
	tmuxx.SessionsInvalidate()
	if _, err := tmuxx.Run("set-option", "-t", sess, "history-limit", "50000"); err != nil {
		return err
	}
	if cmd == "" {
		cmd = "claude"
	}
	if strings.TrimSpace(flags) != "" {
		cmd += " " + strings.TrimSpace(flags)
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		prefix := ""
		for _, key := range keys {
			prefix += key + "=" + shellQuote(env[key]) + " "
		}
		cmd = prefix + cmd
	}
	if _, err := tmuxx.Run("send-keys", "-t", sess, fmt.Sprintf("cd %s && %s", shellQuote(root), cmd), "Enter"); err != nil {
		return err
	}

	if strings.TrimSpace(prompt) != "" {
		// Claude Code tarda un momento en pintar el prompt interactivo.
		go func() {
			time.Sleep(4 * time.Second)
			text := strings.TrimRight(prompt, "\n")
			if _, err := tmuxx.Run("send-keys", "-t", sess, "-l", text); err == nil {
				time.Sleep(150 * time.Millisecond)
				_, _ = tmuxx.Run("send-keys", "-t", sess, "Enter")
			}
		}()
	}
	return nil
}

func AgentSend(worker, text string) error {
	sess := liveAgentSession(worker)
	if sess == "" {
		sess = AgentSession(worker)
	}
	if !tmuxx.HasSession(sess) {
		return fmt.Errorf("el agente de %s no esta corriendo", worker)
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return fmt.Errorf("mensaje vacio")
	}
	// -l manda el texto literal: evita que tmux interprete palabras como
	// nombres de tecla (por ejemplo "Enter" dentro del prompt).
	if _, err := tmuxx.Run("send-keys", "-t", sess, "-l", text); err != nil {
		return err
	}
	rememberTask(sess, strings.SplitN(text, "\n", 2)[0])
	time.Sleep(150 * time.Millisecond)
	_, err := tmuxx.Run("send-keys", "-t", sess, "Enter")
	return err
}

// allowedKeys son las teclas que el panel puede mandar al agente. Existe una
// lista blanca porque send-keys interpreta cualquier nombre de tecla y no se
// quiere exponer combinaciones arbitrarias desde el navegador.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Space": true, "Tab": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	"C-c": true, "C-d": true,
}

func AgentKey(worker, key string) error {
	sess := AgentSession(worker)
	if !tmuxx.HasSession(sess) {
		return fmt.Errorf("el agente de %s no esta corriendo", worker)
	}
	if !allowedKeys[key] {
		return fmt.Errorf("tecla no permitida: %s", key)
	}
	_, err := tmuxx.Run("send-keys", "-t", sess, key)
	return err
}

func AgentInterrupt(worker string) error {
	sess := liveAgentSession(worker)
	if sess == "" {
		sess = AgentSession(worker)
	}
	if !tmuxx.HasSession(sess) {
		return fmt.Errorf("el agente de %s no esta corriendo", worker)
	}
	_, err := tmuxx.Run("send-keys", "-t", sess, "Escape")
	return err
}

func AgentStop(worker string) error {
	sess := liveAgentSession(worker)
	if sess == "" {
		sess = AgentSession(worker)
	}
	if !tmuxx.HasSession(sess) {
		return fmt.Errorf("el agente de %s no esta corriendo", worker)
	}
	forgetActivity(sess)
	activityMu.Lock()
	delete(lastTask, sess)
	activityMu.Unlock()
	return tmuxx.Kill(sess)
}

func AgentOutput(worker string, lines int) string {
	sess := liveAgentSession(worker)
	if sess == "" {
		sess = AgentSession(worker)
	}
	return tmuxx.Capture(sess, lines)
}

func AgentOutputColor(worker string, lines int) string {
	return tmuxx.CaptureColor(AgentSession(worker), lines)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
