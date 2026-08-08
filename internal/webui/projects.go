package webui

// Selector de proyecto: la app de escritorio arranca sin workspace cuando la
// abren desde el buscador de aplicaciones (no hay cwd que valga), pide uno por
// aqui y recuerda los ultimos 5.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"versecam/internal/config"
	"versecam/internal/ws"
)

type projectInfo struct {
	Name       string `json:"name"`
	Root       string `json:"root"`
	At         int64  `json:"at,omitempty"`
	Configured bool   `json:"configured"` // declarado en config.json (trae servicios, jira, etc.)
}

// handleProjects: lo que el selector necesita para pintarse.
func handleProjects(w http.ResponseWriter, r *http.Request) {
	configured := map[string]bool{}
	var workspaces []projectInfo
	for _, project := range config.Workspaces() {
		configured[project.Root] = true
		workspaces = append(workspaces, projectInfo{Name: project.Name, Root: project.Root, Configured: true})
	}

	var recent []projectInfo
	for _, project := range config.RecentProjects() {
		recent = append(recent, projectInfo{
			Name: project.Name, Root: project.Root, At: project.At,
			Configured: configured[project.Root],
		})
	}

	var active *projectInfo
	if config.Ready() {
		active = &projectInfo{
			Name: config.ActiveWSName, Root: config.WSRoot,
			Configured: configured[config.WSRoot],
		}
	}

	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":     active,
		"recent":     recent,
		"workspaces": workspaces,
		"home":       home,
	})
}

// handleProjectOpen abre el proyecto elegido: cambia el workspace activo en
// caliente, lo sube a los recientes y limpia las caches del anterior. El panel
// recarga la vista despues, asi no queda nada del proyecto viejo en pantalla.
func handleProjectOpen(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	root := r.URL.Query().Get("root")
	if r.Method == http.MethodPost {
		var body struct{ Name, Root string }
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if body.Name != "" {
				name = body.Name
			}
			if body.Root != "" {
				root = body.Root
			}
		}
	}
	if name == "" && root == "" {
		writeError(w, http.StatusBadRequest, errors.New("falta name o root"))
		return
	}
	project, err := config.FindProject(name, root)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	config.OpenProject(project)
	ws.ResetCaches()
	configured := false
	for _, known := range config.Workspaces() {
		if known.Root == project.Root {
			configured = true
			break
		}
	}
	writeJSON(w, http.StatusOK, projectInfo{Name: project.Name, Root: project.Root, Configured: configured})
}

// handleProjectBrowse lista carpetas para el "abrir carpeta..." del selector:
// solo nombres de directorios visibles, marcando cuales son repos git.
func handleProjectBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	abs, names, err := config.SubDirs(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	type dirInfo struct {
		Name string `json:"name"`
		Root string `json:"root"`
		Git  bool   `json:"git"`
	}
	dirs := make([]dirInfo, 0, len(names))
	for _, name := range names {
		root := filepath.Join(abs, name)
		dirs = append(dirs, dirInfo{Name: name, Root: root, Git: config.IsGitRepo(root)})
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": abs, "parent": parent, "dirs": dirs})
}
