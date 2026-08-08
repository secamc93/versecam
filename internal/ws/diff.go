package ws

// Arbol de cambios de la rama del worker: lo que trae la rama contra su base
// (merge-base con develop/main), commiteado o no, por cada repo del worker.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DiffEntry struct {
	Repo   string
	Dir    string // directorio del repo
	Path   string // relativo al repo
	Status string // M A D R
	InBase bool   // existe en la base: se puede abrir en diff
	Base   string // hash del merge-base
}

func diffBase(dir string) string {
	for _, candidate := range []string{"origin/develop", "develop", "origin/main", "main"} {
		out, err := GitOut(dir, "merge-base", candidate, "HEAD")
		if err == nil && out != "" {
			return out
		}
	}
	return ""
}

// RepoRootFor devuelve la raiz de repos del worker (en principal difiere del
// Root de la entry): la usa la API del explorador.
func RepoRootFor(workerID string) (string, error) {
	entries, err := AllEntries()
	if err != nil {
		return "", err
	}
	entry, ok := entries[workerID]
	if !ok {
		return "", ErrNotFound
	}
	return repoRoot(workerID, entry), nil
}

var hunkHeaderRe = regexp.MustCompile(`^@@ .*\+(\d+)(?:,\d+)? @@`)

// BranchMarks devuelve que lineas del archivo (1-indexadas) las trajo la rama
// del worker, o allNew si el archivo es nuevo completo. Mismo parseo de hunks
// que el visor del TUI.
func BranchMarks(workerID, rel string) (marks []int, allNew bool) {
	root, err := RepoRootFor(workerID)
	if err != nil {
		return nil, false
	}
	for _, file := range BranchChanges(workerID) {
		fileRel, err := filepath.Rel(root, filepath.Join(file.Dir, file.Path))
		if err != nil || fileRel != rel {
			continue
		}
		if file.Status == "A" || file.Base == "" || !file.InBase {
			return nil, true
		}
		raw, err := GitOut(file.Dir, "diff", file.Base, "--", file.Path)
		if err != nil {
			return nil, false
		}
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
				marks = append(marks, newLine)
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

// Cache corto de los cambios de la rama. El explorador pide esto al abrirse,
// en CADA archivo que seleccionas (para pintar las lineas nuevas) y en el
// refresco de 1.5s; recalcularlo cada vez deja la vista esperando a git.
type changesEntry struct {
	files []DiffEntry
	at    time.Time
}

const changesTTL = 4 * time.Second

var (
	changesMu    sync.Mutex
	changesCache = map[string]changesEntry{}
)

func BranchChanges(workerID string) []DiffEntry {
	changesMu.Lock()
	cached, ok := changesCache[workerID]
	changesMu.Unlock()
	if ok && time.Since(cached.at) < changesTTL {
		return cached.files
	}
	files := branchChanges(workerID)
	changesMu.Lock()
	changesCache[workerID] = changesEntry{files: files, at: time.Now()}
	changesMu.Unlock()
	return files
}

func branchChanges(workerID string) []DiffEntry {
	entries, err := AllEntries()
	if err != nil {
		return nil
	}
	entry, ok := entries[workerID]
	if !ok {
		return nil
	}
	base := repoRoot(workerID, entry)

	var out []DiffEntry
	for _, project := range entry.Projects {
		dir := filepath.Join(base, project.Name)
		repoName := project.Name
		if project.Name == "." {
			dir = base
			repoName = filepath.Base(base)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		mergeBase := diffBase(dir)
		seen := map[string]int{}
		add := func(path, status string) {
			if path == "" {
				return
			}
			if idx, ok := seen[path]; ok {
				out[idx].Status = status
				return
			}
			seen[path] = len(out)
			out = append(out, DiffEntry{Repo: repoName, Dir: dir, Path: path, Status: status, Base: mergeBase})
		}
		if mergeBase != "" {
			if raw, err := GitOut(dir, "diff", "--name-status", mergeBase, "HEAD"); err == nil {
				for _, line := range strings.Split(raw, "\n") {
					fields := strings.Split(line, "\t")
					if len(fields) < 2 {
						continue
					}
					add(fields[len(fields)-1], fields[0][:1])
				}
			}
		}
		if raw, err := GitOut(dir, "status", "--porcelain"); err == nil {
			for _, line := range strings.Split(raw, "\n") {
				if len(line) < 4 {
					continue
				}
				status := strings.TrimSpace(line[:2])
				path := strings.TrimSpace(line[3:])
				if idx := strings.Index(path, " -> "); idx >= 0 {
					path = path[idx+4:]
				}
				if status == "??" {
					status = "A"
				} else {
					status = status[:1]
				}
				add(path, status)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	// Que archivos existen en la base: UN ls-tree por repo. Antes era un
	// "git cat-file -e" por archivo, y un repo con cientos de archivos tocados
	// (borrados masivos, por ejemplo) gastaba segundos solo en lanzar procesos.
	trees := map[string]map[string]bool{}
	for i := range out {
		if out[i].Base == "" {
			continue
		}
		key := out[i].Dir + "\x00" + out[i].Base
		paths, ok := trees[key]
		if !ok {
			paths = pathsInTree(out[i].Dir, out[i].Base)
			trees[key] = paths
		}
		out[i].InBase = paths[out[i].Path]
	}
	return out
}

// pathsInTree lista los archivos de un commit. Con -z para no lidiar con el
// escapado que git aplica a los nombres raros.
func pathsInTree(dir, ref string) map[string]bool {
	paths := map[string]bool{}
	raw, err := GitOut(dir, "ls-tree", "-r", "--name-only", "-z", ref)
	if err != nil {
		return paths
	}
	for _, path := range strings.Split(raw, "\x00") {
		if path != "" {
			paths[path] = true
		}
	}
	return paths
}
