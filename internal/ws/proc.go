package ws

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
	"versecam/internal/tmuxx"
)

// ClaudeProc es un proceso claude vivo en la maquina, lo haya lanzado el panel
// o no. El usuario abre agentes a mano en terminales sueltas y esos tambien
// mueven codigo: si el panel no los muestra, miente.
type ClaudeProc struct {
	PID       int    `json:"pid"`
	CWD       string `json:"cwd"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	UptimeSec int64  `json:"uptime_sec"`
	Managed   bool   `json:"managed"`
}

func scanClaudeProcs() []ClaudeProc {
	raw, err := execx.RunTimeout(10*time.Second, "", "pgrep", "-x", "claude")
	if err != nil {
		return nil
	}

	managed := managedPIDs()

	var out []ClaudeProc
	for _, line := range strings.Split(raw, "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			continue
		}
		out = append(out, ClaudeProc{
			PID:       pid,
			CWD:       cwd,
			UptimeSec: procUptime(pid),
			Managed:   isDescendantOf(pid, managed),
		})
	}
	return out
}

// managedPIDs son los pids de los panes de las sesiones agent-* que creo el
// panel. Sirven para no listar dos veces el mismo agente.
func managedPIDs() map[int]bool {
	out := map[int]bool{}
	shellPrefix := "shell-" + config.ActiveWSName + "-"
	for _, sess := range tmuxx.Sessions() {
		// Cuentan como gestionadas las sesiones agent-* del panel y los
		// shells por worker: un claude ahi ya aparece como agente principal.
		if !strings.HasPrefix(sess, config.AgentPrefix+"-") && !strings.HasPrefix(sess, shellPrefix) {
			continue
		}
		if pid, err := strconv.Atoi(tmuxx.PanePID(sess)); err == nil {
			out[pid] = true
		}
	}
	return out
}

func isDescendantOf(pid int, ancestors map[int]bool) bool {
	for depth := 0; pid > 1 && depth < 12; depth++ {
		if ancestors[pid] {
			return true
		}
		pid = parentPID(pid)
	}
	return false
}

func parentPID(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// El campo 4 es el PPid, pero el campo 2 (comm) puede traer espacios y
	// parentesis: se corta despues del ultimo ')'.
	text := string(raw)
	idx := strings.LastIndex(text, ")")
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(text[idx+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

func procUptime(pid int) int64 {
	info, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	return int64(time.Since(info.ModTime()).Seconds())
}

// classifyProcs reparte los procesos entre los workers, principal y el resto.
func classifyProcs(procs []ClaudeProc, workerRoots map[string]string) (map[string][]ClaudeProc, []ClaudeProc) {
	byWorker := map[string][]ClaudeProc{}
	var loose []ClaudeProc

	for _, proc := range procs {
		if proc.Managed {
			continue
		}
		assigned := ""
		for id, root := range workerRoots {
			if root != "" && underPath(proc.CWD, root) {
				// Gana la raiz mas especifica: principal es prefijo de todo.
				if assigned == "" || len(workerRoots[id]) > len(workerRoots[assigned]) {
					assigned = id
				}
			}
		}
		if assigned == "" {
			proc.Scope = "externo"
			proc.Label = filepath.Base(proc.CWD)
			loose = append(loose, proc)
			continue
		}
		proc.Scope = assigned
		proc.Label = filepath.Base(proc.CWD)
		byWorker[assigned] = append(byWorker[assigned], proc)
	}
	return byWorker, loose
}

func underPath(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// SessionAgentInfo devuelve el CLAUDE_CONFIG_DIR y el binario del proceso de
// agente que corre DENTRO de la sesion, leyendo /proc del proceso real: asi
// se sabe con que cuenta corre cada pestaña, sin importar como se lanzo.
func SessionAgentInfo(sess string) (configDir, bin string) {
	root, err := strconv.Atoi(strings.TrimSpace(tmuxx.PanePID(sess)))
	if err != nil {
		return "", ""
	}
	ancestors := map[int]bool{root: true}
	for _, pid := range agentProcs() {
		if !isDescendantOf(pid, ancestors) {
			continue
		}
		proc := "/proc/" + strconv.Itoa(pid)
		if raw, err := os.ReadFile(proc + "/environ"); err == nil {
			for _, kv := range strings.Split(string(raw), "\x00") {
				if value, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
					configDir = value
				}
			}
		}
		if raw, err := os.ReadFile(proc + "/comm"); err == nil {
			bin = strings.TrimSpace(string(raw))
		}
		return configDir, bin
	}
	return "", ""
}

// agentCwdForPane encuentra el cwd real del proceso de agente que vive bajo
// un pane de tmux.
func agentCwdForPane(panePID string) string {
	root, err := strconv.Atoi(strings.TrimSpace(panePID))
	if err != nil {
		return ""
	}
	ancestors := map[int]bool{root: true}
	for _, pid := range agentProcs() {
		if isDescendantOf(pid, ancestors) {
			if cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
				return cwd
			}
		}
	}
	return ""
}

// reassignMigratedSessions: si el claude de una sesion se movio a otro
// worktree (ej. desde principal creo un worktree y siguio trabajando alla),
// la sesion pasa a pertenecer a ese worker.
func reassignMigratedSessions(entries map[string]config.WorktreeEntry) {
	roots := map[string]string{}
	for id, entry := range entries {
		roots[id] = entry.Root
	}
	for id := range entries {
		for _, sess := range []string{AgentSession(id), ShellSession(id)} {
			if !tmuxx.HasSession(sess) {
				continue
			}
			cwd := agentCwdForPane(tmuxx.PanePID(sess))
			if cwd == "" {
				continue
			}
			best := ""
			for otherID, root := range roots {
				if root != "" && underPath(cwd, root) {
					if best == "" || len(root) > len(roots[best]) {
						best = otherID
					}
				}
			}
			if best == "" || best == id {
				continue
			}
			target := ShellSession(best)
			if !tmuxx.HasSession(target) {
				_, _ = tmuxx.Run("rename-session", "-t", sess, target)
			}
		}
	}
}
