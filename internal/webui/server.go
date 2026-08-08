// Package webui: el panel web de versecam. Sirve los assets estaticos y la
// API HTTP/SSE que consume web/index.html.
package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"versecam/internal/config"
	"versecam/internal/tmuxx"
	"versecam/internal/ws"
)

// Serve levanta el servidor web del panel con los assets embebidos.
func Serve(addr string, static fs.FS) error {
	log.Printf("versecam escuchando en http://%s", addr)
	return http.ListenAndServe(addr, Mux(static))
}

// Mux arma el router completo (assets + API). Lo comparte el modo -web y la
// app de escritorio (Wails lo usa como asset server).
func Mux(static fs.FS) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))

	mux.HandleFunc("GET /api/projects", handleProjects)
	mux.HandleFunc("POST /api/projects/open", handleProjectOpen)
	mux.HandleFunc("GET /api/projects/open", handleProjectOpen)
	mux.HandleFunc("GET /api/projects/browse", handleProjectBrowse)

	mux.HandleFunc("GET /api/workers", handleWorkers)
	mux.HandleFunc("GET /api/workers/{id}", handleWorkerDetail)
	mux.HandleFunc("GET /api/jira", handleJira)
	mux.HandleFunc("GET /api/jira/{key}", handleJiraDetail)

	mux.HandleFunc("GET /api/voice", handleVoiceHealth)
	mux.HandleFunc("GET /api/voice/toggle", handleVoiceToggle)
	mux.HandleFunc("GET /api/workers/{id}/sessions/{sess}/speak", handleSessionSpeak)
	mux.HandleFunc("POST /api/voice/stt", handleVoiceSTT)
	mux.HandleFunc("POST /api/voice/tts", handleVoiceTTS)

	mux.HandleFunc("GET /api/workers/{id}/agent", handleAgentOutput)
	mux.HandleFunc("GET /api/workers/{id}/agent/stream", handleAgentStream)
	mux.HandleFunc("POST /api/workers/{id}/agent/start", handleAgentStart)
	mux.HandleFunc("GET /api/workers/{id}/agent/start", handleAgentStart)
	mux.HandleFunc("POST /api/workers/{id}/agent/send", handleAgentSend)
	mux.HandleFunc("POST /api/workers/{id}/agent/key", handleAgentKey)
	mux.HandleFunc("POST /api/workers/{id}/agent/interrupt", handleAgentInterrupt)
	mux.HandleFunc("POST /api/workers/{id}/agent/stop", handleAgentStop)

	mux.HandleFunc("GET /api/profiles", handleProfiles)
	mux.HandleFunc("GET /api/worktrees/create", handleWorktreeCreate)
	mux.HandleFunc("GET /api/workers/{id}/tickets/add", handleWorkerTicketAdd)
	mux.HandleFunc("GET /api/workers/{id}/shell/start", handleShellStart)

	mux.HandleFunc("GET /api/workers/{id}/services/{svc}/logs", handleServiceLogs)
	mux.HandleFunc("POST /api/workers/{id}/services/{svc}/{action}", handleServiceAction)
	mux.HandleFunc("GET /api/workers/{id}/services/{svc}/do/{action}", handleServiceAction)

	mux.HandleFunc("GET /api/workers/{id}/github", handleWorkerGithub)
	mux.HandleFunc("GET /api/workers/{id}/changes", handleWorkerChanges)
	mux.HandleFunc("GET /api/workers/{id}/ls", handleWorkerLS)
	mux.HandleFunc("GET /api/workers/{id}/file", handleWorkerFile)

	mux.HandleFunc("GET /api/workers/{id}/sessions", handleWorkerSessions)
	mux.HandleFunc("GET /api/workers/{id}/sessions/{sess}/screen", handleSessionScreen)
	mux.HandleFunc("POST /api/workers/{id}/sessions/{sess}/keys", handleSessionKeys)
	mux.HandleFunc("GET /api/workers/{id}/sessions/{sess}/keys", handleSessionKeys)

	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// resolveWorker valida contra el manifiesto antes de usar el id en un nombre
// de sesion o en un path: nunca se confia en el valor que llega por URL.
func resolveWorker(id string) (config.WorktreeEntry, error) {
	entries, err := ws.AllEntries()
	if err != nil {
		return config.WorktreeEntry{}, err
	}
	entry, ok := entries[id]
	if !ok {
		return config.WorktreeEntry{}, ws.ErrNotFound
	}
	return entry, nil
}

func handleWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ws.List())
}

func handleWorkerDetail(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh_prs") == "1"
	detail, err := ws.Detail(r.PathValue("id"), refresh)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func handleJira(w http.ResponseWriter, r *http.Request) {
	issues, err := ws.JiraIssues(r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	entries, _ := ws.AllEntries()
	for i := range issues {
		for id := range entries {
			if strings.HasPrefix(id, issues[i].Key+"-") || id == issues[i].Key {
				issues[i].HasWork = true
				break
			}
			// Tickets asociados a mano: un worktree resuelve varios tickets.
			for _, ticket := range config.TicketsForWorker(id) {
				if ticket == issues[i].Key {
					issues[i].HasWork = true
					break
				}
			}
			if issues[i].HasWork {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, issues)
}

// handleWorktreeCreate crea el worktree de un ticket (?name=LOG-123-slug).
// Nace sin proyectos: el analisis decide despues cuales involucra.
func handleWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, errors.New("falta el nombre"))
		return
	}
	if err := ws.CreateTicketWorktree(name); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "worktree creado", "worker": name})
}

// handleWorkerTicketAdd asocia otro ticket a un worker existente.
func handleWorkerTicketAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, errors.New("falta el ticket"))
		return
	}
	config.AddTicketToWorker(id, ticket)
	writeJSON(w, http.StatusOK, map[string]string{"ok": ticket + " asociado a " + id})
}

// handleWorkerGithub: el estado git/GitHub del worker para el panel Ctrl+G:
// repos con rama y estado, PRs con checks (gh) y commits recientes.
func handleWorkerGithub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	detail, err := ws.Detail(id, refresh)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org":    config.GithubOrg,
		"repos":  detail.Repos,
		"prs":    detail.PRs,
		"graphs": ws.CommitGraphs(id, 60),
	})
}

// handleJiraDetail: un ticket completo y legible (descripcion y comentarios
// aplanados desde ADF), con las credenciales del .mcp.json del workspace.
func handleJiraDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := ws.JiraIssueDetail(r.PathValue("key"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func handleAgentOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 5000 {
			lines = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":  ws.AgentStateFor(id),
		"output": ws.AgentOutput(id, lines),
	})
}

type agentStartRequest struct {
	Prompt string `json:"prompt"`
	Flags  string `json:"flags"`
}

func handleAgentStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := resolveWorker(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var body agentStartRequest
	if r.Method == http.MethodGet {
		// La app de escritorio usa GET: el webview pierde cuerpos de POST.
		body.Prompt = r.URL.Query().Get("prompt")
		body.Flags = r.URL.Query().Get("flags")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	// Igual que el TUI: si ya hay un agente corriendo, se abre OTRA pestaña
	// (agent-x-2, -3...) en vez de fallar con "la sesion ya existe".
	base := ws.AgentSession(id)
	sess := base
	for i := 2; tmuxx.HasSession(sess); i++ {
		sess = fmt.Sprintf("%s-%d", base, i)
	}

	// ?profile=<nombre>: lanzar con ese perfil en vez del efectivo del worker.
	if name := r.URL.Query().Get("profile"); name != "" {
		cmd, env, ok := profileByName(name)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("perfil desconocido: "+name))
			return
		}
		if err := ws.AgentStartWith(sess, entry.Root, body.Prompt, body.Flags, cmd, env); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "agente lanzado", "session": sess})
		return
	}
	if err := ws.AgentStart(sess, entry.Root, body.Prompt, body.Flags, id); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "agente lanzado", "session": sess})
}

// profileByName resuelve cmd y env de un perfil declarado o autodetectado;
// "claude" pelado siempre vale (cuenta por defecto).
func profileByName(name string) (cmd string, env map[string]string, ok bool) {
	for _, profile := range config.AgentProfiles {
		if profile.Name == name {
			cmd = profile.Cmd
			if cmd == "" {
				cmd = "claude"
			}
			return cmd, profile.Env, true
		}
	}
	if name == "claude" {
		return "claude", nil, true
	}
	return "", nil, false
}

// handleProfiles lista los perfiles disponibles para el selector de la app.
func handleProfiles(w http.ResponseWriter, r *http.Request) {
	type profileInfo struct {
		Name string `json:"name"`
		CLI  string `json:"cli"`
	}
	out := []profileInfo{}
	seen := map[string]bool{}
	for _, profile := range config.AgentProfiles {
		cli := "claude"
		if strings.Contains(profile.Cmd, "kimi") {
			cli = "kimi"
		}
		out = append(out, profileInfo{Name: profile.Name, CLI: cli})
		seen[profile.Name] = true
	}
	if !seen["claude"] {
		out = append(out, profileInfo{Name: "claude", CLI: "claude"})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShellStart abre una pestaña de shell en la raiz del worker (sin
// agente: el usuario decide que corre ahi).
func handleShellStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	root, err := ws.RepoRootFor(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	base := ws.ShellSession(id)
	sess := base
	for i := 2; tmuxx.HasSession(sess); i++ {
		sess = fmt.Sprintf("%s-%d", base, i)
	}
	if _, err := tmuxx.Run("new-session", "-d", "-s", sess, "-c", root); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	tmuxx.SessionsInvalidate()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "shell abierto", "session": sess})
}

type agentSendRequest struct {
	Text string `json:"text"`
}

func handleAgentSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var body agentSendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := ws.AgentSend(id, body.Text); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "enviado"})
}

type agentKeyRequest struct {
	Key string `json:"key"`
}

func handleAgentKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var body agentKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := ws.AgentKey(id, body.Key); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": body.Key})
}

func handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := ws.AgentInterrupt(id); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "interrumpido"})
}

func handleAgentStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := ws.AgentStop(id); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "agente detenido"})
}

func handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	id, svc := r.PathValue("id"), r.PathValue("svc")
	entry, err := resolveWorker(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	// La infraestructura detectada (contenedores) lleva su propio id y sus
	// logs salen de docker, no de la sesion tmux del servicio.
	if ws.IsInfraID(svc) {
		writeJSON(w, http.StatusOK, map[string]string{"output": ws.InfraLogs(entry.Root, svc, 400)})
		return
	}
	if !ws.KnownService(svc) {
		writeError(w, http.StatusNotFound, errors.New("servicio desconocido"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": ws.ServiceLogs(id, svc, 400)})
}

func handleServiceAction(w http.ResponseWriter, r *http.Request) {
	id, svc, action := r.PathValue("id"), r.PathValue("svc"), r.PathValue("action")
	entry, err := resolveWorker(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	switch action {
	case "start", "restart", "stop":
	default:
		writeError(w, http.StatusBadRequest, errors.New("accion no permitida"))
		return
	}
	if ws.IsInfraID(svc) {
		out, err := ws.InfraAction(entry.Root, svc, action)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "output": out})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": action + " " + svc, "output": out})
		return
	}
	if !ws.KnownService(svc) {
		writeError(w, http.StatusNotFound, errors.New("servicio desconocido"))
		return
	}

	out, err := ws.VelCommand(entry.Root, action, svc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "output": out})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": action + " " + svc, "output": out})
}
