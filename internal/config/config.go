package config

// Configuracion global de versecam: ~/.config/versecam/config.json declara
// los workspaces y el panel se apunta a uno por
// flag -ws o por el directorio actual. Sin config, el workspace es el cwd.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"versecam/internal/execx"
)

type Workspace struct {
	Name         string            `json:"name"`
	Root         string            `json:"root"`
	PrincipalDir string            `json:"principal_dir,omitempty"` // relativo al root; vacio = el root mismo
	WorkersDir   string            `json:"workers_dir,omitempty"`   // relativo al root
	Manifest     string            `json:"manifest,omitempty"`      // manifiesto de worktrees (formato vel)
	ServicesConf string            `json:"services_conf,omitempty"` // servicios estilo .vel-services.conf
	VelBin       string            `json:"vel_bin,omitempty"`       // gestor de servicios (vel)
	MCPConf      string            `json:"mcp_conf,omitempty"`      // .mcp.json para credenciales Jira
	GithubOrg    string            `json:"github_org,omitempty"`
	DBContainer  string            `json:"db_container,omitempty"`
	DBUser       string            `json:"db_user,omitempty"`
	DBPass       string            `json:"db_pass,omitempty"`
	AgentPrefix  string            `json:"agent_prefix,omitempty"`  // prefijo de sesiones tmux de agentes
	AgentEnv     map[string]string `json:"agent_env,omitempty"`     // env para lanzar el agente (ej. CLAUDE_CONFIG_DIR)
	AgentProfile string            `json:"agent_profile,omitempty"` // perfil por defecto del workspace
	VelPrefix    string            `json:"vel_prefix,omitempty"`    // prefijo de sesiones de servicios
	JiraJQL      string            `json:"jira_jql,omitempty"`
}

// AgentProfile es una forma de lanzar el agente: cuenta de Claude (via
// CLAUDE_CONFIG_DIR) u otro CLI compatible (kimi code).
type AgentProfile struct {
	Name string            `json:"name"`
	Cmd  string            `json:"cmd,omitempty"` // default: claude
	Env  map[string]string `json:"env,omitempty"`
}

type globalConfig struct {
	Profiles   []AgentProfile `json:"profiles,omitempty"`
	Workspaces []Workspace    `json:"workspaces"`
}

// Variables de contexto del workspace activo. Todo el resto del codigo las
// consume para no arrastrar el contexto por todas las firmas.
var (
	WSRoot        string
	WorkersDir    string
	PrincipalDir  string
	ManifestPath  string
	ServicesConf  string
	mcpConfPath   string
	VelBin        string
	GithubOrg     string
	AgentPrefix   = "agent"
	VelPrefix     = "vel"
	JiraJQL       = "assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC"
	ActiveWSName  = ""
	DBContainer   = ""
	DBUser        = ""
	DBPass        = ""
	AgentEnv      map[string]string
	AgentProfiles []AgentProfile
	ActiveProfile = ""
)

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "versecam", "config.json")
}

func loadGlobalConfig() *globalConfig {
	raw, err := os.ReadFile(configPath())
	if err != nil {
		return nil
	}
	var parsed globalConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fmt.Fprintln(os.Stderr, "config.json ilegible:", err)
		return nil
	}
	AgentProfiles = parsed.Profiles
	return &parsed
}

// resolveWorkspace escoge el workspace: por nombre si vino -ws, por el cwd si
// cae dentro del root de alguno (gana el mas especifico), o el primero.
func ResolveWorkspace(name string) Workspace {
	cfg := loadGlobalConfig()
	cwd, _ := os.Getwd()

	if cfg == nil || len(cfg.Workspaces) == 0 {
		return Workspace{Name: filepath.Base(cwd), Root: cwd}
	}
	if name != "" {
		for _, ws := range cfg.Workspaces {
			if ws.Name == name {
				return ws
			}
		}
		fmt.Fprintf(os.Stderr, "workspace %q no esta en %s\n", name, configPath())
		os.Exit(1)
	}
	best := -1
	for i, ws := range cfg.Workspaces {
		if underPath(cwd, ws.Root) && (best < 0 || len(ws.Root) > len(cfg.Workspaces[best].Root)) {
			best = i
		}
	}
	if best >= 0 {
		return cfg.Workspaces[best]
	}
	// Fuera de todo workspace configurado: si el cwd esta dentro de un repo
	// git, ese repo es el workspace (ad-hoc). Si no, el primero del config.
	if top, err := execx.RunTimeout(5*time.Second, cwd, "git", "rev-parse", "--show-toplevel"); err == nil {
		root := strings.TrimSpace(top)
		if root != "" {
			return AdHocWorkspace(root)
		}
	}
	return cfg.Workspaces[0]
}

// underPath: copia local del helper de rutas (tambien existe en ws; config no
// puede importarlo sin crear un ciclo).
func underPath(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

func InitWorkspace(ws Workspace) {
	joinRoot := func(rel, fallback string) string {
		if rel == "" {
			rel = fallback
		}
		if rel == "" {
			return ""
		}
		if filepath.IsAbs(rel) {
			return rel
		}
		return filepath.Join(ws.Root, rel)
	}

	ActiveWSName = ws.Name
	WSRoot = ws.Root
	PrincipalDir = joinRoot(ws.PrincipalDir, ".")
	WorkersDir = joinRoot(ws.WorkersDir, "workers")
	ManifestPath = joinRoot(ws.Manifest, "")
	ServicesConf = joinRoot(ws.ServicesConf, "")
	mcpConfPath = joinRoot(ws.MCPConf, ".mcp.json")
	VelBin = joinRoot(ws.VelBin, "")
	GithubOrg = ws.GithubOrg
	DBContainer = ws.DBContainer
	DBUser = ws.DBUser
	DBPass = ws.DBPass
	if ws.AgentPrefix != "" {
		AgentPrefix = ws.AgentPrefix
	}
	if ws.VelPrefix != "" {
		VelPrefix = ws.VelPrefix
	}
	if ws.JiraJQL != "" {
		JiraJQL = ws.JiraJQL
	}
	AgentEnv = ws.AgentEnv
	ActiveProfile = ws.AgentProfile
	mergeAutoProfiles()
}

// mergeAutoProfiles detecta cuentas de Claude por convencion, sin tener que
// declararlas en config.json: cada directorio ~/.claude-<nombre> que parezca
// un config dir es un perfil <nombre> (CLAUDE_CONFIG_DIR apuntando ahi), y si
// el CLI de kimi esta instalado aparece el perfil "kimi". Los perfiles
// explicitos del config ganan por nombre.
func mergeAutoProfiles() {
	seen := map[string]bool{}
	for _, profile := range AgentProfiles {
		seen[profile.Name] = true
	}
	for _, profile := range autoProfiles() {
		if !seen[profile.Name] {
			AgentProfiles = append(AgentProfiles, profile)
		}
	}
}

func autoProfiles() []AgentProfile {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude-*"))
	var out []AgentProfile
	for _, dir := range matches {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if !looksLikeClaudeConfig(dir) {
			continue
		}
		name := strings.TrimPrefix(filepath.Base(dir), ".claude-")
		out = append(out, AgentProfile{
			Name: name,
			Env:  map[string]string{"CLAUDE_CONFIG_DIR": dir},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// La app de escritorio no siempre hereda el PATH de la shell: se busca
	// tambien en la ruta de instalacion tipica de kimi.
	if _, err := exec.LookPath("kimi"); err == nil {
		out = append(out, AgentProfile{Name: "kimi", Cmd: "kimi"})
	} else if _, err := os.Stat(filepath.Join(home, ".kimi-code", "bin", "kimi")); err == nil {
		out = append(out, AgentProfile{Name: "kimi", Cmd: filepath.Join(home, ".kimi-code", "bin", "kimi")})
	}
	return out
}

func looksLikeClaudeConfig(dir string) bool {
	for _, marker := range []string{".credentials.json", ".claude.json", "settings.json", "projects", "statsig", "daemon"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

type WorktreeProject struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Branch string `json:"branch"`
}

type WorktreeEntry struct {
	Root     string            `json:"root"`
	PortBase int               `json:"port_base"`
	DBName   string            `json:"db_name"`
	Projects []WorktreeProject `json:"projects"`
}

// loadManifest tolera la ausencia del manifiesto: un workspace sin worktrees
// solo muestra su principal.
func LoadManifest() (map[string]WorktreeEntry, error) {
	out := map[string]WorktreeEntry{}
	if ManifestPath == "" {
		return out, nil
	}
	raw, err := os.ReadFile(ManifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for key, entry := range out {
		if entry.Root == "" {
			entry.Root = filepath.Join(WorkersDir, key)
			out[key] = entry
		}
	}
	return out, nil
}

type mcpFile struct {
	MCPServers map[string]struct {
		Env map[string]string `json:"env"`
	} `json:"mcpServers"`
}

// mcpEnv devuelve las variables de entorno declaradas para un servidor MCP.
// Se reutilizan las credenciales ya configuradas en .mcp.json en vez de pedir
// al usuario que las duplique en otro archivo.
func MCPEnv(server string) map[string]string {
	raw, err := os.ReadFile(mcpConfPath)
	if err != nil {
		return nil
	}
	var parsed mcpFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	entry, ok := parsed.MCPServers[server]
	if !ok {
		return nil
	}
	return entry.Env
}

// Perfil por worker: cada worktree puede lanzar su agente con una cuenta
// distinta. Se persiste en state.json para sobrevivir al panel.
var WorkerProfiles = map[string]string{}

// Tickets asociados por worker: un worktree puede resolver VARIOS tickets
// (el que le dio nombre + los asociados a mano). Persistido en state.json.
var WorkerTickets = map[string][]string{}

// TicketsForWorker devuelve los tickets extra asociados al worker.
func TicketsForWorker(worker string) []string {
	return WorkerTickets[WorkerProfileKey(worker)]
}

// AddTicketToWorker asocia un ticket a un worker (idempotente) y persiste.
func AddTicketToWorker(worker, ticket string) {
	key := WorkerProfileKey(worker)
	for _, existing := range WorkerTickets[key] {
		if existing == ticket {
			return
		}
	}
	WorkerTickets[key] = append(WorkerTickets[key], ticket)
	SaveState()
}

func statePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "versecam", "state.json")
}

type stateFile struct {
	WorkerProfiles map[string]string   `json:"worker_profiles"`
	WorkerTickets  map[string][]string `json:"worker_tickets,omitempty"`
	RecentProjects []RecentProject     `json:"recent_projects,omitempty"`
}

func LoadState() {
	raw, err := os.ReadFile(statePath())
	if err != nil {
		return
	}
	var parsed stateFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return
	}
	if parsed.WorkerProfiles != nil {
		WorkerProfiles = parsed.WorkerProfiles
	}
	if parsed.WorkerTickets != nil {
		WorkerTickets = parsed.WorkerTickets
	}
	recentProjects = parsed.RecentProjects
}

func SaveState() {
	raw, err := json.MarshalIndent(stateFile{
		WorkerProfiles: WorkerProfiles,
		WorkerTickets:  WorkerTickets,
		RecentProjects: recentProjects,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(statePath(), raw, 0o644)
}

func WorkerProfileKey(worker string) string {
	return ActiveWSName + "/" + worker
}

// profileForWorker: el override del worker gana; si no hay, el default del
// workspace.
func ProfileForWorker(worker string) string {
	if name, ok := WorkerProfiles[WorkerProfileKey(worker)]; ok {
		return name
	}
	return ActiveProfile
}

// profileNames lista los perfiles disponibles para el mensaje de :prof.
func ProfileNames() []string {
	out := make([]string, 0, len(AgentProfiles))
	for _, profile := range AgentProfiles {
		out = append(out, profile.Name)
	}
	return out
}

// resolveProfile devuelve comando y entorno del perfil efectivo del worker.
// Sin perfil, claude pelado con el agent_env del workspace (compatibilidad).
func ResolveProfile(worker string) (cmd string, env map[string]string) {
	wanted := ProfileForWorker(worker)
	for _, profile := range AgentProfiles {
		if profile.Name == wanted {
			cmd = profile.Cmd
			if cmd == "" {
				cmd = "claude"
			}
			return cmd, profile.Env
		}
	}
	return "claude", AgentEnv
}
