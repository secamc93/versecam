package ws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
)

type RepoState struct {
	Name      string `json:"name"`
	Branch    string `json:"branch"`
	Mode      string `json:"mode"`
	Dirty     int    `json:"dirty"`
	Ahead     int    `json:"ahead"`
	Pushed    bool   `json:"pushed"`
	HasRemote bool   `json:"has_remote"`
	Error     string `json:"error,omitempty"`
}

func GitOut(dir string, args ...string) (string, error) {
	out, err := execx.RunTimeout(20*time.Second, dir, "git", args...)
	return strings.TrimSpace(out), err
}

func repoState(root string, project config.WorktreeProject) RepoState {
	dir := filepath.Join(root, project.Name)
	st := RepoState{Name: project.Name, Mode: project.Mode, Branch: project.Branch}
	if project.Name == "." {
		st.Name = filepath.Base(root)
	}

	if _, err := os.Stat(dir); err != nil {
		st.Error = "no clonado en el worker"
		return st
	}

	if branch, err := GitOut(dir, "branch", "--show-current"); err == nil && branch != "" {
		st.Branch = branch
	}

	if status, err := GitOut(dir, "status", "--porcelain"); err == nil && status != "" {
		st.Dirty = len(strings.Split(status, "\n"))
	}

	upstream := "origin/" + st.Branch
	if _, err := GitOut(dir, "rev-parse", "--verify", upstream); err == nil {
		st.HasRemote = true
		if count, err := GitOut(dir, "rev-list", "--count", upstream+"..HEAD"); err == nil {
			st.Ahead, _ = strconv.Atoi(count)
		}
	}
	st.Pushed = st.HasRemote && st.Ahead == 0
	return st
}

// repoBranch cachea la rama actual de un repo ~30s: la lista de workers se
// refresca cada 3s y no vale un git por worker por tick.
type branchEntry struct {
	name string
	at   time.Time
}

var (
	branchMu    sync.Mutex
	branchCache = map[string]branchEntry{}
)

func repoBranch(dir string) string {
	branchMu.Lock()
	cached, ok := branchCache[dir]
	branchMu.Unlock()
	if ok && time.Since(cached.at) < 30*time.Second {
		return cached.name
	}
	name, _ := GitOut(dir, "branch", "--show-current")
	branchMu.Lock()
	branchCache[dir] = branchEntry{name: name, at: time.Now()}
	branchMu.Unlock()
	return name
}

// GraphCommit: un commit con sus padres y refs, para que la app dibuje el
// arbol de versiones (carriles, merges) al estilo git graph.
type GraphCommit struct {
	Hash    string   `json:"hash"`
	Short   string   `json:"short"`
	Parents []string `json:"parents"`
	Refs    []string `json:"refs,omitempty"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
}

type RepoGraph struct {
	Repo    string        `json:"repo"`
	Commits []GraphCommit `json:"commits"`
}

// CommitGraphs: historial topo-ordenado de todas las ramas de cada repo del
// worker. Los campos van separados por \x1f y los registros por \x1e porque
// el subject puede traer cualquier cosa.
func CommitGraphs(workerID string, limit int) []RepoGraph {
	entries, err := AllEntries()
	if err != nil {
		return nil
	}
	entry, ok := entries[workerID]
	if !ok {
		return nil
	}
	base := repoRoot(workerID, entry)
	var out []RepoGraph
	for _, project := range entry.Projects {
		dir := filepath.Join(base, project.Name)
		name := project.Name
		if project.Name == "." {
			dir = base
			name = filepath.Base(base)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		raw, err := GitOut(dir, "log", "--all", "--topo-order", "-n", strconv.Itoa(limit),
			"--date=format:%d/%m/%y %H:%M",
			"--pretty=%H\x1f%h\x1f%P\x1f%D\x1f%an\x1f%ad\x1f%s\x1e")
		if err != nil || raw == "" {
			continue
		}
		var commits []GraphCommit
		for _, record := range strings.Split(raw, "\x1e") {
			fields := strings.Split(strings.TrimSpace(record), "\x1f")
			if len(fields) < 7 {
				continue
			}
			commits = append(commits, GraphCommit{
				Hash:    fields[0],
				Short:   fields[1],
				Parents: strings.Fields(fields[2]),
				Refs:    splitRefs(fields[3]),
				Author:  fields[4],
				Date:    fields[5],
				Subject: fields[6],
			})
		}
		if len(commits) > 0 {
			out = append(out, RepoGraph{Repo: name, Commits: commits})
		}
	}
	return out
}

func splitRefs(decoration string) []string {
	if decoration == "" {
		return nil
	}
	var refs []string
	for _, ref := range strings.Split(decoration, ", ") {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

type PullRequest struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Checks string `json:"checks"`
	URL    string `json:"url"`
}

type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	URL         string `json:"url"`
	StatusCheck []struct {
		State      string `json:"state"`
		Conclusion string `json:"conclusion"`
	} `json:"statusCheckRollup"`
}

type prCacheEntry struct {
	prs   []PullRequest
	saved time.Time
}

var (
	prMu    sync.Mutex
	prCache = map[string]prCacheEntry{}
)

// pullRequests consulta gh por repo. Se cachea porque cada llamada sale a la
// red y el panel refresca seguido.
func pullRequests(worker string, repos []RepoState, force bool) []PullRequest {
	prMu.Lock()
	if entry, ok := prCache[worker]; ok && !force && time.Since(entry.saved) < 3*time.Minute {
		prMu.Unlock()
		return entry.prs
	}
	prMu.Unlock()

	var out []PullRequest
	for _, repo := range repos {
		if repo.Branch == "" || repo.Error != "" {
			continue
		}
		raw, err := execx.RunTimeout(25*time.Second, "", "gh", "pr", "list",
			"--repo", config.GithubOrg+"/"+repo.Name,
			"--head", repo.Branch,
			"--state", "all",
			"--limit", "5",
			"--json", "number,title,state,url,statusCheckRollup")
		if err != nil {
			continue
		}
		var parsed []ghPR
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			continue
		}
		for _, pr := range parsed {
			out = append(out, PullRequest{
				Repo:   repo.Name,
				Number: pr.Number,
				Title:  pr.Title,
				State:  pr.State,
				URL:    pr.URL,
				Checks: rollupChecks(pr),
			})
		}
	}

	prMu.Lock()
	prCache[worker] = prCacheEntry{prs: out, saved: time.Now()}
	prMu.Unlock()
	return out
}

func rollupChecks(pr ghPR) string {
	if len(pr.StatusCheck) == 0 {
		return "sin checks"
	}
	var failed, pending int
	for _, check := range pr.StatusCheck {
		switch {
		case check.Conclusion == "FAILURE" || check.Conclusion == "TIMED_OUT" || check.Conclusion == "CANCELLED":
			failed++
		case check.State == "PENDING" || check.Conclusion == "":
			pending++
		}
	}
	switch {
	case failed > 0:
		return strconv.Itoa(failed) + " fallando"
	case pending > 0:
		return strconv.Itoa(pending) + " en curso"
	default:
		return "verde"
	}
}
