package config

// Selector de proyecto (estilo "recent projects" de VSCode): la app de
// escritorio, cuando la abren desde el buscador de aplicaciones, no tiene un
// cwd util del que deducir el workspace, asi que pregunta cual abrir. Aqui
// viven la lista de candidatos y los ultimos usados, que se persisten en
// state.json.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxRecentProjects = 5

// RecentProject es una entrada del historial: nombre visible, raiz y cuando se
// abrio por ultima vez.
type RecentProject struct {
	Name string `json:"name"`
	Root string `json:"root"`
	At   int64  `json:"at"`
}

var recentProjects []RecentProject

// switchMu serializa el cambio de proyecto en caliente contra los lectores de
// fondo (el anunciador de voz), que son los unicos que tocan el contexto del
// workspace sin pasar por una peticion del panel.
var switchMu sync.Mutex

// Ready indica si ya hay un proyecto abierto. Mientras es false el panel
// muestra el selector y no consulta nada del workspace.
func Ready() bool { return WSRoot != "" }

// Workspaces lista los proyectos declarados en config.json.
func Workspaces() []Workspace {
	cfg := loadGlobalConfig()
	if cfg == nil {
		return nil
	}
	return cfg.Workspaces
}

// RecentProjects devuelve el historial, del mas reciente al mas viejo,
// saltando los que ya no existen en disco.
func RecentProjects() []RecentProject {
	out := make([]RecentProject, 0, len(recentProjects))
	for _, project := range recentProjects {
		if info, err := os.Stat(project.Root); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, project)
	}
	return out
}

// TouchProject sube el proyecto al tope del historial (maximo 5) y persiste.
func TouchProject(ws Workspace) {
	if ws.Root == "" {
		return
	}
	updated := []RecentProject{{Name: ws.Name, Root: ws.Root, At: time.Now().Unix()}}
	for _, project := range recentProjects {
		if project.Root == ws.Root {
			continue
		}
		if len(updated) >= maxRecentProjects {
			break
		}
		updated = append(updated, project)
	}
	recentProjects = updated
	SaveState()
}

// FindProject resuelve lo que eligio el usuario en el selector: un workspace
// del config.json (gana por nombre, luego por raiz) o una carpeta suelta.
func FindProject(name, root string) (Workspace, error) {
	for _, ws := range Workspaces() {
		if name != "" && ws.Name == name {
			return ws, nil
		}
	}
	for _, ws := range Workspaces() {
		if root != "" && ws.Root == root {
			return ws, nil
		}
	}
	if root == "" {
		return Workspace{}, fmt.Errorf("el proyecto %q no esta en %s", name, configPath())
	}
	abs, err := filepath.Abs(expandHome(root))
	if err != nil {
		return Workspace{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("no se puede abrir %s: %w", abs, err)
	}
	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("%s no es una carpeta", abs)
	}
	return AdHocWorkspace(abs), nil
}

// AdHocWorkspace arma un workspace para una carpeta que no esta en config.json:
// sin manifiesto ni servicios, con prefijo de sesiones propio para no chocar
// con las de otros proyectos.
func AdHocWorkspace(root string) Workspace {
	base := filepath.Base(root)
	return Workspace{Name: base, Root: root, AgentPrefix: "agent-" + base}
}

// expandHome acepta rutas escritas a mano con ~.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// OpenProject cambia el workspace activo en caliente y lo marca como reciente.
func OpenProject(ws Workspace) {
	switchMu.Lock()
	defer switchMu.Unlock()
	InitWorkspace(ws)
	TouchProject(ws)
}

// HoldProject corre fn con el proyecto congelado: lo usan los bucles de fondo
// para no leer el contexto a medio cambiar.
func HoldProject(fn func()) {
	switchMu.Lock()
	defer switchMu.Unlock()
	fn()
}

// SubDirs lista las carpetas de dir para el "abrir carpeta..." del selector.
// Se omiten las ocultas: nadie abre ~/.cache como proyecto.
func SubDirs(dir string) (string, []string, error) {
	abs, err := filepath.Abs(expandHome(dir))
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		out = append(out, entry.Name())
	}
	return abs, out, nil
}

// IsGitRepo marca en el selector cuales carpetas son repos (las que valen como
// proyecto).
func IsGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
