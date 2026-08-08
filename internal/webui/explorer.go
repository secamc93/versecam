package webui

// API del explorador de archivos para la app de escritorio: los cambios de
// la rama, listado perezoso de carpetas y contenido de archivos con
// resaltado + las lineas que trajo la rama. Espejo del explorador del TUI.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"versecam/internal/hl"
	"versecam/internal/ws"
)

// cleanRel valida una ruta relativa que llega por URL: nada de absolutas ni
// de escaparse del root con "..".
func cleanRel(rel string) (string, bool) {
	rel = filepath.Clean("/" + rel)[1:] // normaliza y corta el "/" inicial
	if rel == "" || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return rel, true
}

var shortstatRe = regexp.MustCompile(`(\d+) insertion|(\d+) deletion`)

type changedFile struct {
	Rel    string `json:"rel"`
	Status string `json:"status"`
	Repo   string `json:"repo"`
}

// handleWorkerChanges: los archivos que trae la rama del worker + el resumen
// estilo git diff (+insertadas -borradas) + la rama de cada repo.
func handleWorkerChanges(w http.ResponseWriter, r *http.Request) {
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

	files := []changedFile{}
	adds, dels := 0, 0
	branches := map[string]string{}
	seenDirs := map[string]bool{}
	for _, file := range ws.BranchChanges(id) {
		rel, err := filepath.Rel(root, filepath.Join(file.Dir, file.Path))
		if err != nil {
			continue
		}
		files = append(files, changedFile{Rel: rel, Status: file.Status, Repo: file.Repo})
		if seenDirs[file.Dir] {
			continue
		}
		seenDirs[file.Dir] = true
		if branch, err := ws.GitOut(file.Dir, "branch", "--show-current"); err == nil {
			branches[file.Repo] = branch
		}
		if file.Base == "" {
			continue
		}
		raw, err := ws.GitOut(file.Dir, "diff", "--shortstat", file.Base)
		if err != nil {
			continue
		}
		for _, match := range shortstatRe.FindAllStringSubmatch(raw, -1) {
			if match[1] != "" {
				n, _ := strconv.Atoi(match[1])
				adds += n
			}
			if match[2] != "" {
				n, _ := strconv.Atoi(match[2])
				dels += n
			}
		}
	}
	// La rama del repo raiz, para worktrees de un solo repo sin cambios.
	if rootBranch, err := ws.GitOut(root, "branch", "--show-current"); err == nil && rootBranch != "" {
		branches[filepath.Base(root)] = rootBranch
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "adds": adds, "dels": dels, "branches": branches,
	})
}

type dirEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// handleWorkerLS lista una carpeta del worker (carga perezosa del arbol
// completo). Se salta .git, como el TUI.
func handleWorkerLS(w http.ResponseWriter, r *http.Request) {
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
	dir := root
	if raw := r.URL.Query().Get("rel"); raw != "" {
		rel, ok := cleanRel(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("ruta invalida"))
			return
		}
		dir = filepath.Join(root, rel)
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	entries := []dirEntry{}
	for _, item := range items {
		if item.Name() == ".git" {
			continue
		}
		entries = append(entries, dirEntry{Name: item.Name(), Dir: item.IsDir()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

const explorerMaxBytes = 2 * 1024 * 1024

// handleWorkerFile: el archivo resaltado (ANSI) + las lineas de la rama en
// verde + mtime para que el visor recargue cuando la IA lo cambie.
func handleWorkerFile(w http.ResponseWriter, r *http.Request) {
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
	rel, ok := cleanRel(r.URL.Query().Get("rel"))
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("ruta invalida"))
		return
	}
	abs := filepath.Join(root, rel)
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, errors.New("no es un archivo legible"))
		return
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if len(raw) > explorerMaxBytes {
		raw = raw[:explorerMaxBytes]
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := hl.Lines(strings.Split(content, "\n"), filepath.Ext(rel))
	marks, allNew := ws.BranchMarks(id, rel)
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines, "marks": marks, "all_new": allNew, "mtime": info.ModTime().UnixNano(),
	})
}
