package ws

import (
	"regexp"
	"strconv"
	"strings"
	"versecam/internal/tmuxx"
)

// El TUI de Claude Code no expone un API de estado, asi que el panel lee la
// pantalla. Dos anclas estables:
//   - la ultima linea de prompt (el chevron U+276F seguido del texto) es la
//     tarea que se le pidio al agente
//   - la linea de trabajo ("Doodling..." con los segundos entre parentesis)
//     solo existe mientras el agente esta pensando
var (
	promptLine = regexp.MustCompile(`(?m)^\s*\x{276f}\s+(\S.*?)\s*$`)
	// La statusline de Claude pinta el contexto exacto: "ctx:[...] 19% 1/1000.0k".
	ctxLine     = regexp.MustCompile(`ctx:\[[^\]]*\]\s*(\d+)%`)
	workingLine = regexp.MustCompile(`(?m)^\s*\S{1,3}\s+([A-Za-z]+)\x{2026}\s*\((\d+)s`)
	doneLine    = regexp.MustCompile(`(?m)^\s*\S{1,3}\s+[A-Za-z]+ for (\d+)s`)
)

const interruptHint = "esc to interrupt"

// Glifos del TUI, en escapes para no meter unicode crudo en el fuente
// (.claude/rules/no-unicode-in-code.md).
const bulletGlyph = "\u25cf"

var chromePrefixes = []string{
	"\u2500", // linea horizontal
	"\u2501", // linea horizontal gruesa
	"\u256d", // esquina superior izquierda
	"\u2570", // esquina inferior izquierda
	"\u2502", // linea vertical
	"\u276f", // chevron del prompt
	"\u23bf", // resultado de herramienta
	"\u2937", // continuacion
}

// parseReply devuelve el ultimo mensaje del agente. En el TUI cada respuesta
// arranca con una vineta (U+25CF); lo que sigue son sus lineas de continuacion
// hasta el spinner, un separador o la caja del prompt.
func ParseReply(screen string) string {
	lines := strings.Split(tmuxx.StripSGR(screen), "\n")

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), bulletGlyph) {
			start = i
		}
	}
	if start < 0 {
		return ""
	}

	var collected []string
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isChrome(trimmed) {
			break
		}
		trimmed = strings.TrimPrefix(trimmed, bulletGlyph)
		collected = append(collected, strings.TrimSpace(trimmed))
	}

	reply := strings.Join(collected, " ")
	return strings.Join(strings.Fields(reply), " ")
}

// isChrome reconoce el marco del TUI: separadores, spinner, barra de estado y
// la linea del prompt. Nada de eso es parte de la respuesta.
func isChrome(line string) bool {
	for _, prefix := range chromePrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	switch {
	case workingLine.MatchString(line), doneLine.MatchString(line):
		return true
	case strings.Contains(line, "auto mode on"), strings.Contains(line, "ctx:["):
		return true
	}
	return false
}

type screenInfo struct {
	Task       string
	Activity   string
	ElapsedSec int
	Working    bool
	CtxPct     int
}

func parseScreen(screen string) screenInfo {
	info := screenInfo{}
	if screen == "" {
		return info
	}

	for _, match := range promptLine.FindAllStringSubmatch(screen, -1) {
		text := strings.TrimSpace(match[1])
		if text != "" {
			info.Task = text
		}
	}
	if len(info.Task) > 160 {
		info.Task = info.Task[:160] + "..."
	}

	if match := ctxLine.FindStringSubmatch(screen); match != nil {
		info.CtxPct, _ = strconv.Atoi(match[1])
	}

	if match := workingLine.FindStringSubmatch(screen); match != nil {
		info.Working = true
		info.Activity = match[1]
		info.ElapsedSec, _ = strconv.Atoi(match[2])
		return info
	}
	if strings.Contains(screen, interruptHint) {
		info.Working = true
		info.Activity = "trabajando"
		return info
	}

	// Al terminar, el TUI reemplaza el spinner por "<verbo> for Ns".
	if match := doneLine.FindStringSubmatch(screen); match != nil {
		info.Activity = "listo"
		info.ElapsedSec, _ = strconv.Atoi(match[1])
	}
	return info
}
