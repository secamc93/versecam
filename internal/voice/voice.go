// Package voice: cliente del sidecar de voz local (kokoro TTS). El panel no
// carga los modelos: habla por HTTP con el sidecar, que los mantiene en
// memoria. Si el sidecar no esta arriba, la voz simplemente se reporta
// apagada.
package voice

import (
	"net/http"
	"os"
	"time"
)

// BaseURL apunta al sidecar (VERSECAM_VOICE_URL lo cambia).
var BaseURL = envOr("VERSECAM_VOICE_URL", envOr("PANEL_VOICE_URL", "http://127.0.0.1:9111"))

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Client es el cliente HTTP hacia el sidecar; el timeout largo cubre la
// sintesis de textos grandes. Lo comparte el proxy del modo web.
var Client = &http.Client{Timeout: 120 * time.Second}
