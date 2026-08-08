package ws

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
	"versecam/internal/tmuxx"
)

type WorkerSummary struct {
	ID          string         `json:"id"`
	Ticket      string         `json:"ticket"`
	Description string         `json:"description"`
	Root        string         `json:"root"`
	IsPrincipal bool           `json:"is_principal"`
	PortBase    int            `json:"port_base"`
	PHPPort     int            `json:"php_port"`
	DBName      string         `json:"db_name"`
	DBReady     bool           `json:"db_ready"`
	RepoCount   int            `json:"repo_count"`
	Branch      string         `json:"branch,omitempty"`
	ServicesUp  int            `json:"services_up"`
	ServicesAll int            `json:"services_all"`
	MemKB       int64          `json:"mem_kb"`
	Agents      []AgentState   `json:"agents,omitempty"`
	LastAct     int64          `json:"last_activity"`
	Services    []ServiceState `json:"services,omitempty"`
	Agent       AgentState     `json:"agent"`
	Externals   []ClaudeProc   `json:"externals"`
}

// workerMemKB suma el RSS de todo lo que el worker tiene vivo: el agente, los
// servicios vel y los claudes sueltos abiertos en su raiz.
func workerMemKB(snap *procSnapshot, agent AgentState, services []ServiceState, externals []ClaudeProc) int64 {
	var total int64
	if agent.PID != "" {
		total += snap.treeRSSKB(agent.PID)
	}
	for _, service := range services {
		if service.Running && service.PID != "" {
			total += snap.treeRSSKB(service.PID)
		}
	}
	for _, proc := range externals {
		total += snap.treeRSSKB(strconv.Itoa(proc.PID))
	}
	return total
}

type WorkerDetail struct {
	WorkerSummary
	Repos    []RepoState    `json:"repos"`
	Services []ServiceState `json:"services"`
	PRs      []PullRequest  `json:"prs"`
}

// PrincipalID identifica el workspace principal, que no esta en el manifiesto
// pero tambien se mueve: ahi se corren agentes sin ticket asignado.
const PrincipalID = "principal"

// ErrNotFound: el id de worker no existe en el manifiesto.
var ErrNotFound = errors.New("worker no encontrado")

var workerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CreateTicketWorktree crea el universo de un ticket. Con manifiesto vel se
// agrega la entrada con la raiz creada y SIN proyectos: el worktree nace
// vacio y los repos se agregan cuando el analisis aclare cuales toca. Sin
// manifiesto, git worktree con rama nueva (el repo es el proyecto).
func CreateTicketWorktree(name string) error {
	if !workerNameRe.MatchString(name) {
		return errors.New("nombre de worker invalido: " + name)
	}
	entries, err := AllEntries()
	if err == nil {
		if _, exists := entries[name]; exists {
			return errors.New("el worker " + name + " ya existe")
		}
	}
	if config.ManifestPath != "" {
		return createManifestWorktree(name)
	}
	dest := filepath.Join(filepath.Dir(config.PrincipalDir),
		filepath.Base(config.PrincipalDir)+"-workers", name)
	out, err := execx.RunTimeout(60*time.Second, config.PrincipalDir,
		"git", "worktree", "add", "-b", name, dest)
	if err != nil && strings.Contains(out, "already exists") {
		out, err = execx.RunTimeout(60*time.Second, config.PrincipalDir,
			"git", "worktree", "add", dest, name)
	}
	if err != nil {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

// createManifestWorktree agrega la entrada al manifiesto vel preservando
// intactas las demas (se parsea como RawMessage: solo se toca la nueva).
func createManifestWorktree(name string) error {
	entries := map[string]json.RawMessage{}
	raw, err := os.ReadFile(config.ManifestPath)
	if err == nil {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return errors.New("manifiesto ilegible: " + err.Error())
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	root := filepath.Join(config.WorkersDir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	entry, err := json.Marshal(map[string]any{"root": root, "projects": []any{}})
	if err != nil {
		return err
	}
	entries[name] = entry
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(config.ManifestPath, out, 0o644)
}

func splitWorkerID(id string) (ticket, description string) {
	if id == PrincipalID {
		return "PRINCIPAL", "workspace sin ticket"
	}
	if config.ManifestPath == "" {
		return id, ""
	}
	parts := strings.SplitN(id, "-", 3)
	if len(parts) >= 3 {
		return parts[0] + "-" + parts[1], strings.ReplaceAll(parts[2], "-", " ")
	}
	return id, ""
}

// principalEntry deja al workspace principal con la misma forma que un worker
// para que el resto del panel no tenga que tratarlo como caso especial.
func principalEntry() config.WorktreeEntry {
	entry := config.WorktreeEntry{Root: config.WSRoot}
	names, err := os.ReadDir(config.PrincipalDir)
	if err != nil {
		return entry
	}
	for _, item := range names {
		if !item.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(config.PrincipalDir, item.Name(), ".git")); err != nil {
			continue
		}
		entry.Projects = append(entry.Projects, config.WorktreeProject{Name: item.Name(), Mode: "rw"})
	}
	// Si el root mismo es un repo (workspace de un solo repo),
	// se lista como proyecto "." para ver su rama y estado git.
	if _, err := os.Stat(filepath.Join(config.PrincipalDir, ".git")); err == nil {
		entry.Projects = append(entry.Projects, config.WorktreeProject{Name: ".", Mode: "rw"})
	}
	return entry
}

// repoRoot es donde viven los repos del contexto: en principal cuelgan de
// principal/, en un worker cuelgan de la raiz del worker.
func repoRoot(id string, entry config.WorktreeEntry) string {
	if id == PrincipalID {
		return config.PrincipalDir
	}
	return entry.Root
}

func AllEntries() (map[string]config.WorktreeEntry, error) {
	manifest, err := config.LoadManifest()
	if err != nil {
		return nil, err
	}
	manifest[PrincipalID] = principalEntry()
	// Sin manifiesto vel, los universos paralelos son los git worktrees del
	// repo: cada uno es una copia completa del proyecto con su propia rama.
	if config.ManifestPath == "" {
		for id, entry := range gitWorktreeEntries() {
			manifest[id] = entry
		}
	}
	return manifest, nil
}

func gitWorktreeEntries() map[string]config.WorktreeEntry {
	out := map[string]config.WorktreeEntry{}
	if _, err := os.Stat(filepath.Join(config.PrincipalDir, ".git")); err != nil {
		return out
	}
	raw, err := execx.RunTimeout(10*time.Second, config.PrincipalDir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return out
	}
	var path, branch string
	flush := func() {
		if path != "" && path != config.PrincipalDir {
			out[filepath.Base(path)] = config.WorktreeEntry{
				Root:     path,
				Projects: []config.WorktreeProject{{Name: ".", Mode: "rw", Branch: branch}},
			}
		}
		path, branch = "", ""
	}
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch refs/heads/"):
			branch = strings.TrimPrefix(strings.TrimSpace(line), "branch refs/heads/")
		}
	}
	flush()
	return out
}

// localDatabases lista las databases del contenedor local de una sola vez.
// Consultar una por worker seria un docker exec por fila.
func localDatabases() map[string]bool {
	out := map[string]bool{}
	if config.DBContainer == "" {
		return out
	}
	raw, err := execx.RunTimeout(15*time.Second, "", "docker", "exec", config.DBContainer,
		"mysql", "-u", config.DBUser, "-p"+config.DBPass, "-N", "-B", "-e", "SHOW DATABASES")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(raw, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

type WorkspaceView struct {
	Workers   []WorkerSummary `json:"workers"`
	Externals []ClaudeProc    `json:"externals"`
}

func List() WorkspaceView {
	// Sin proyecto abierto (el panel esta en el selector) no hay nada que
	// listar: mirar el manifiesto con el contexto vacio inventaria un worker
	// "principal" apuntando a la nada.
	if !config.Ready() {
		return WorkspaceView{}
	}
	entries, err := AllEntries()
	if err != nil {
		return WorkspaceView{}
	}
	reassignMigratedSessions(entries)
	defs := LoadServiceDefs()
	live := LiveSessions()
	databases := localDatabases()
	snap := snapshotProcs()

	roots := map[string]string{}
	for id, entry := range entries {
		roots[id] = entry.Root
	}
	byWorker, loose := classifyProcs(scanClaudeProcs(), roots)

	out := make([]WorkerSummary, 0, len(entries))
	for id, entry := range entries {
		ticket, description := splitWorkerID(id)

		// En principal las sesiones de vel no llevan sufijo de ticket.
		sessionKey := id
		if id == PrincipalID {
			sessionKey = ""
		}
		services := ServicesForWorker(repoRoot(id, entry), sessionKey, defs, live)
		// Memoria por servicio: la usa el panel derecho de servicios del TUI.
		for i := range services {
			if services[i].Running && services[i].PID != "" {
				services[i].MemKB = snap.treeRSSKB(services[i].PID)
			}
		}

		up := 0
		for _, svc := range services {
			if svc.Running {
				up++
			}
		}

		summary := WorkerSummary{
			ID:          id,
			Ticket:      ticket,
			Description: description,
			Root:        entry.Root,
			IsPrincipal: id == PrincipalID,
			PortBase:    entry.PortBase,
			DBName:      entry.DBName,
			DBReady:     entry.DBName == "" || databases[entry.DBName],
			RepoCount:   len(entry.Projects),
			ServicesUp:  up,
			ServicesAll: len(services),
			Externals:   byWorker[id],
		}
		summary.Agents = workerAgents(id)
		if len(summary.Agents) > 0 {
			summary.Agent = summary.Agents[0]
		} else {
			summary.Agent = AgentStateFor(id)
		}
		if entry.PortBase > 0 {
			summary.PHPPort = entry.PortBase + 8
		}
		if summary.Description == "" && len(entry.Projects) > 0 && entry.Projects[0].Branch != "" {
			summary.Description = entry.Projects[0].Branch
		}
		// La rama del primer repo del worker (la del ticket): para la
		// etiqueta de la tarjeta.
		if len(entry.Projects) > 0 {
			dir := filepath.Join(repoRoot(id, entry), entry.Projects[0].Name)
			if entry.Projects[0].Name == "." {
				dir = repoRoot(id, entry)
			}
			if branch := repoBranch(dir); branch != "" {
				summary.Branch = branch
			} else if entry.Projects[0].Branch != "" {
				summary.Branch = entry.Projects[0].Branch
			}
		}
		// El % de la statusline del agente es el exacto (sabe la ventana
		// real); el transcript solo es respaldo cuando no esta en pantalla.
		if summary.Agent.Running && summary.Agent.CtxPct == 0 {
			summary.Agent.CtxPct = contextPct(entry.Root)
		}
		summary.Services = services
		summary.MemKB = workerMemKB(snap, summary.Agent, services, summary.Externals)
		// Ultima actividad de IA: la sesion tmux del agente o el transcript
		// mas reciente escrito en ese root.
		if summary.Agent.Running {
			summary.LastAct = tmuxx.SessionActivity(summary.Agent.Session)
		}
		if transcriptAct := lastAIActivity(repoRoot(id, entry)); transcriptAct > summary.LastAct {
			summary.LastAct = transcriptAct
		}
		out = append(out, summary)
	}

	// El worker con IA activa mas reciente arriba; empate: principal y ticket.
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastAct != out[j].LastAct {
			return out[i].LastAct > out[j].LastAct
		}
		if out[i].IsPrincipal != out[j].IsPrincipal {
			return out[i].IsPrincipal
		}
		return out[i].ID < out[j].ID
	})
	return WorkspaceView{Workers: out, Externals: loose}
}

func Detail(id string, refreshPRs bool) (*WorkerDetail, error) {
	entries, err := AllEntries()
	if err != nil {
		return nil, err
	}
	entry, ok := entries[id]
	if !ok {
		return nil, ErrNotFound
	}

	ticket, description := splitWorkerID(id)
	base := repoRoot(id, entry)
	sessionKey := id
	if id == PrincipalID {
		sessionKey = ""
	}

	defs := LoadServiceDefs()
	services := ServicesForWorker(base, sessionKey, defs, LiveSessions())
	EnrichServicePorts(services)

	repos := make([]RepoState, len(entry.Projects))
	var wg sync.WaitGroup
	for i, project := range entry.Projects {
		wg.Add(1)
		go func(idx int, p config.WorktreeProject) {
			defer wg.Done()
			repos[idx] = repoState(base, p)
		}(i, project)
	}
	wg.Wait()

	up := 0
	for _, svc := range services {
		if svc.Running {
			up++
		}
	}

	roots := map[string]string{}
	for key, value := range entries {
		roots[key] = value.Root
	}
	byWorker, _ := classifyProcs(scanClaudeProcs(), roots)

	detail := &WorkerDetail{
		WorkerSummary: WorkerSummary{
			ID:          id,
			Ticket:      ticket,
			Description: description,
			Root:        entry.Root,
			IsPrincipal: id == PrincipalID,
			PortBase:    entry.PortBase,
			DBName:      entry.DBName,
			DBReady:     entry.DBName == "" || localDatabases()[entry.DBName],
			RepoCount:   len(entry.Projects),
			ServicesUp:  up,
			ServicesAll: len(services),
			Externals:   byWorker[id],
		},
		Repos:    repos,
		Services: services,
		PRs:      pullRequests(id, repos, refreshPRs),
	}
	detail.Agents = workerAgents(id)
	if len(detail.Agents) > 0 {
		detail.Agent = detail.Agents[0]
	} else {
		detail.Agent = AgentStateFor(id)
	}
	if entry.PortBase > 0 {
		detail.PHPPort = entry.PortBase + 8
	}
	detail.MemKB = workerMemKB(snapshotProcs(), detail.Agent, services, detail.Externals)
	if detail.Agent.Running && detail.Agent.CtxPct == 0 {
		detail.Agent.CtxPct = contextPct(entry.Root)
	}
	return detail, nil
}
