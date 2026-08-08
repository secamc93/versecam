package webui

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"versecam/internal/ws"
)

const (
	streamInterval  = 700 * time.Millisecond
	streamHeartbeat = 15 * time.Second
)

type streamFrame struct {
	State ws.AgentState `json:"state"`
	// Output solo viaja cuando la pantalla cambio.
	Output string `json:"output,omitempty"`
	// Reply se llena unicamente en el frame donde el agente termino su turno.
	// Es lo que se lee en voz alta: mandarlo en cada frame haria que la voz
	// repitiera lo mismo mientras el agente sigue escribiendo.
	Reply string `json:"reply,omitempty"`
}

// handleAgentStream empuja la pantalla del agente por SSE. Solo manda el
// contenido cuando cambio: el TUI repinta seguido y reenviar 11 KB por tick
// sin necesidad satura el navegador para nada.
func handleAgentStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := resolveWorker(id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("el servidor no soporta streaming"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(streamInterval)
	defer ticker.Stop()

	var lastOutput, lastState, lastReply, prevStatus string
	lastSent := time.Now()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}

		state := ws.AgentStateFor(id)
		output := ""
		if state.Running {
			output = ws.AgentOutputColor(id, 400)
		}

		outputHash := hashOf(output)
		stateHash := hashOf(fmt.Sprint(state.Status, state.Task, state.Activity, state.Running))

		changed := outputHash != lastOutput || stateHash != lastState
		if !changed && time.Since(lastSent) < streamHeartbeat {
			continue
		}

		frame := streamFrame{State: state}
		if outputHash != lastOutput {
			frame.Output = output
		}

		// El turno termino cuando el agente pasa de trabajar a esperar.
		if prevStatus == "working" && state.Status == "waiting" {
			if reply := ws.ParseReply(output); reply != "" && reply != lastReply {
				frame.Reply = reply
				lastReply = reply
			}
		}
		prevStatus = state.Status

		payload, err := json.Marshal(frame)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return
		}
		flusher.Flush()

		lastOutput, lastState, lastSent = outputHash, stateHash, time.Now()
	}
}

func hashOf(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
