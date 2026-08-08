package ws

// Porcentaje de contexto del agente: se lee de la cola del transcript JSONL
// de la sesion de Claude mas reciente del worker (el TUI no lo expone). El
// total de la ultima respuesta (input + cache + output) contra la ventana.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"versecam/internal/config"
)

const defaultContextWindow = 200000

// claudeConfigDirs junta el config dir por defecto y los declarados en los
// perfiles: el agente pudo lanzarse con cualquiera de ellos.
func claudeConfigDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dirs := []string{filepath.Join(home, ".claude")}
	seen := map[string]bool{dirs[0]: true}
	for _, profile := range config.AgentProfiles {
		if dir := profile.Env["CLAUDE_CONFIG_DIR"]; dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if dir := config.AgentEnv["CLAUDE_CONFIG_DIR"]; dir != "" && !seen[dir] {
		dirs = append(dirs, dir)
	}
	return dirs
}

func newestTranscript(root string) string {
	munged := strings.ReplaceAll(root, "/", "-")
	best := ""
	var bestTime time.Time
	for _, dir := range claudeConfigDirs() {
		matches, err := filepath.Glob(filepath.Join(dir, "projects", munged, "*.jsonl"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.ModTime().After(bestTime) {
				bestTime = info.ModTime()
				best = path
			}
		}
	}
	return best
}

// lastAIActivity: epoch del transcript mas reciente del root, la senial de
// "aqui se trabajo con IA por ultima vez".
func lastAIActivity(root string) int64 {
	path := newestTranscript(root)
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

type transcriptLine struct {
	Message struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// contextPct devuelve el porcentaje de la ventana de contexto usado por la
// sesion mas reciente del worker, o 0 si no hay transcript legible.
func contextPct(root string) int {
	path := newestTranscript(root)
	if path == "" {
		return 0
	}
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0
	}
	const tailBytes = 128 * 1024
	offset := info.Size() - tailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return 0
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return 0
	}

	lines := strings.Split(string(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(lines[i], `"usage"`) {
			continue
		}
		var parsed transcriptLine
		if err := json.Unmarshal([]byte(lines[i]), &parsed); err != nil {
			continue
		}
		usage := parsed.Message.Usage
		total := usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens + usage.OutputTokens
		if total == 0 {
			continue
		}
		// La ventana depende del modelo: Fable corre con 1M.
		window := int64(defaultContextWindow)
		if strings.Contains(parsed.Message.Model, "fable") {
			window = 1000000
		}
		pct := int(total * 100 / window)
		if pct > 100 {
			pct = 100
		}
		return pct
	}
	return 0
}
