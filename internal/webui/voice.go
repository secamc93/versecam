package webui

// Proxy hacia el sidecar de voz para el panel web: health, STT y TTS.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"versecam/internal/voice"
)

func handleVoiceHealth(w http.ResponseWriter, r *http.Request) {
	resp, err := voice.Client.Get(voice.BaseURL + "/health")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"on":        voice.On.Load(),
			"hint":      "sidecar apagado: vel start voice",
		})
		return
	}
	defer resp.Body.Close()

	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "on": voice.On.Load()})
		return
	}
	health["available"] = true
	health["on"] = voice.On.Load()
	writeJSON(w, http.StatusOK, health)
}

// handleVoiceToggle prende/apaga la voz en vivo (leer cada respuesta al
// terminar el turno), igual que la tecla v del TUI.
func handleVoiceToggle(w http.ResponseWriter, r *http.Request) {
	if voice.On.Load() {
		voice.On.Store(false)
		writeJSON(w, http.StatusOK, map[string]any{"on": false})
		return
	}
	if !voice.Healthy() {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("sidecar de voz apagado (vel start voice)"))
		return
	}
	voice.On.Store(true)
	voice.SpeakAsync("", "voz encendida")
	writeJSON(w, http.StatusOK, map[string]any{"on": true})
}

func handleVoiceSTT(w http.ResponseWriter, r *http.Request) {
	proxyVoice(w, r, "/stt", "application/octet-stream")
}

func handleVoiceTTS(w http.ResponseWriter, r *http.Request) {
	proxyVoice(w, r, "/tts", "text/plain; charset=utf-8")
}

func proxyVoice(w http.ResponseWriter, r *http.Request, path, contentType string) {
	// El cuerpo se lee entero antes de reenviarlo: con un reader de tamano
	// desconocido Go usa transfer-encoding chunked, y el sidecar (que es un
	// BaseHTTPRequestHandler) solo entiende Content-Length.
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 40<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, voice.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", contentType)

	resp, err := voice.Client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "el sidecar de voz no responde (vel start voice)",
		})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
