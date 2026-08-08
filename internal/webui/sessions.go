package webui

// API de sesiones tmux para la app de escritorio: las pestañas del worker,
// la pantalla del pane (con cursor) y el teclado crudo. Es el mismo modelo
// del TUI (capturar/reenviar sobre tmux), servido por HTTP.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"versecam/internal/config"
	"versecam/internal/tmuxx"
	"versecam/internal/voice"
	"versecam/internal/ws"
)

type sessionInfo struct {
	Session string `json:"session"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`              // agent | shell
	Profile string `json:"profile,omitempty"` // perfil de claude del worker (pestañas de agente)
	CLI     string `json:"cli,omitempty"`     // claude | kimi: para el icono de la pestaña
}

// handleWorkerSessions lista las pestañas del worker con la misma etiqueta
// del TUI (IA 1, IA 2, sh 1...).
func handleWorkerSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	agentBase := ws.AgentSession(id)
	out := []sessionInfo{}
	agents, shells := 0, 0
	for _, sess := range ws.WorkerSessions(id) {
		if sess == agentBase || strings.HasPrefix(sess, agentBase+"-") {
			agents++
			profile, cli := sessionProfile(id, sess)
			out = append(out, sessionInfo{Session: sess, Label: fmt.Sprintf("IA %d", agents), Kind: "agent", Profile: profile, CLI: cli})
		} else {
			shells++
			out = append(out, sessionInfo{Session: sess, Label: fmt.Sprintf("sh %d", shells), Kind: "shell"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionProfile detecta la cuenta REAL de la sesion leyendo el entorno del
// proceso vivo (CLAUDE_CONFIG_DIR): funciona aunque el agente se haya lanzado
// a mano o con otro perfil que el actual del worker. Si no hay proceso, cae
// al perfil configurado del worker.
func sessionProfile(workerID, sess string) (profile, cli string) {
	dir, bin := ws.SessionAgentInfo(sess)
	cli = "claude"
	if strings.Contains(bin, "kimi") {
		return "kimi", "kimi"
	}
	if bin == "" {
		// Sin proceso de agente detectable: el perfil configurado del worker.
		profile = config.ProfileForWorker(workerID)
		if cmd, _ := config.ResolveProfile(workerID); strings.Contains(cmd, "kimi") {
			cli = "kimi"
		}
		return profile, cli
	}
	if dir == "" {
		return "claude", cli // cuenta por defecto (~/.claude)
	}
	// Nombre del perfil: el que declare ese config dir, o el sufijo del
	// directorio (~/.claude-empresa -> empresa).
	for _, candidate := range config.AgentProfiles {
		if candidate.Env["CLAUDE_CONFIG_DIR"] == dir {
			return candidate.Name, cli
		}
	}
	return strings.TrimPrefix(filepath.Base(dir), ".claude-"), cli
}

// validSession: el nombre que llega por URL solo se usa si es una sesion
// real del worker (nunca se confia en el valor crudo).
func validSession(workerID, sess string) bool {
	for _, candidate := range ws.WorkerSessions(workerID) {
		if candidate == sess {
			return true
		}
	}
	return false
}

// sizedSessions cachea el ultimo resize por sesion para no llamar tmux en
// cada captura.
var (
	sizedMu       sync.Mutex
	sizedSessions = map[string]string{}
)

func ensureSize(sess string, cols, rows int) {
	if cols <= 10 || rows <= 4 {
		return
	}
	key := fmt.Sprintf("%dx%d", cols, rows)
	sizedMu.Lock()
	previous := sizedSessions[sess]
	sizedSessions[sess] = key
	sizedMu.Unlock()
	if previous != key {
		_, _ = tmuxx.Run("resize-window", "-t", sess, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	}
}

// handleSessionScreen devuelve la pantalla visible del pane con colores y la
// posicion del cursor. ?cols=&rows= ajusta el tamano del window de tmux al
// del visor.
func handleSessionScreen(w http.ResponseWriter, r *http.Request) {
	id, sess := r.PathValue("id"), r.PathValue("sess")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !validSession(id, sess) {
		writeError(w, http.StatusNotFound, errors.New("sesion desconocida"))
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	ensureSize(sess, cols, rows)
	lines, x, y, visible := tmuxx.ScreenAndCursor(sess)
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines, "x": x, "y": y, "cursor": visible,
	})
}

// handleSessionSpeak lee en voz alta el ultimo mensaje del agente de la
// pestaña (igual que V / :leer en el TUI): kokoro corre en esta maquina, el
// audio suena local.
func handleSessionSpeak(w http.ResponseWriter, r *http.Request) {
	id, sess := r.PathValue("id"), r.PathValue("sess")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !validSession(id, sess) {
		writeError(w, http.StatusNotFound, errors.New("sesion desconocida"))
		return
	}
	if !voice.Healthy() {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("sidecar de voz apagado (vel start voice)"))
		return
	}
	reply := ws.ParseReply(tmuxx.Capture(sess, 400))
	if reply == "" {
		writeError(w, http.StatusNotFound, errors.New("la pestaña no tiene respuesta que leer"))
		return
	}
	voice.SpeakNow(id, reply)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "leyendo"})
}

type keysRequest struct {
	Hex []string `json:"hex"`
}

// handleSessionKeys reenvia bytes crudos del teclado a la sesion, igual que
// el modo terminal del TUI (tmux send-keys -H). Acepta el JSON {"hex":[...]}
// por POST o ?hex=aa,bb por GET: el webview de la app de escritorio pierde
// cuerpos de POST hacia su esquema interno en algunas versiones de WebKit.
func handleSessionKeys(w http.ResponseWriter, r *http.Request) {
	id, sess := r.PathValue("id"), r.PathValue("sess")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !validSession(id, sess) {
		writeError(w, http.StatusNotFound, errors.New("sesion desconocida"))
		return
	}
	var body keysRequest
	if query := r.URL.Query().Get("hex"); query != "" {
		body.Hex = strings.Split(query, ",")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if len(body.Hex) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("faltan bytes"))
		return
	}
	if len(body.Hex) > 512 {
		body.Hex = body.Hex[:512]
	}
	args := []string{"send-keys", "-H", "-t", sess}
	for _, hex := range body.Hex {
		if _, err := strconv.ParseUint(hex, 16, 8); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("byte invalido: "+hex))
			return
		}
		args = append(args, hex)
	}
	if _, err := tmuxx.Run(args...); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "enviado"})
}
