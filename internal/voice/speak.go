package voice

// Voz local: cuando un agente termina su turno, el panel lee la respuesta en
// voz alta usando el sidecar de voz (kokoro TTS, POST /tts -> WAV). Todo
// corre en la maquina; si el sidecar no esta arriba, la voz simplemente no
// suena y el panel avisa al activarla.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"versecam/internal/ws"
)

var (
	On       atomic.Bool
	speaking atomic.Bool
	// speakingLabel: ticket del worker cuya respuesta esta sonando ("" si es
	// un anuncio generico). Solo vale mientras speaking este en true.
	speakingLabel atomic.Value
)

// currentSpeaking devuelve el ticket que se esta leyendo en voz alta, o ""
// si no hay nada sonando.
func CurrentSpeaking() (string, bool) {
	if !speaking.Load() {
		return "", false
	}
	label, _ := speakingLabel.Load().(string)
	return label, true
}

var speakClient = &http.Client{Timeout: 120 * time.Second}

func Healthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(BaseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 300
}

func voiceTTS(text string) ([]byte, error) {
	resp, err := speakClient.Post(BaseURL+"/tts", "text/plain; charset=utf-8", strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tts respondio %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func findPlayer() (string, []string) {
	candidates := []struct {
		bin  string
		args []string
	}{
		{"paplay", nil},
		{"aplay", []string{"-q"}},
		{"ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet"}},
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate.bin); err == nil {
			return candidate.bin, candidate.args
		}
	}
	return "", nil
}

// speakAsync sintetiza y reproduce en background solo si el toggle de voz
// esta encendido (anuncios automaticos al terminar cada turno). label es el
// ticket del worker que se lee ("" para anuncios genericos).
func SpeakAsync(label, text string) {
	if !On.Load() {
		return
	}
	SpeakNow(label, text)
}

// speakNow sintetiza y reproduce sin mirar el toggle: para lecturas pedidas a
// mano ([V] / :leer). Si ya hay algo sonando se descarta: mejor perder un
// anuncio que encolar un monologo.
func SpeakNow(label, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !speaking.CompareAndSwap(false, true) {
		return
	}
	speakingLabel.Store(label)
	go func() {
		defer speaking.Store(false)
		const maxChars = 600
		if len(text) > maxChars {
			text = text[:maxChars]
		}
		audio, err := voiceTTS(text)
		if err != nil || len(audio) == 0 {
			return
		}
		tmp, err := os.CreateTemp("", "versecam-voz-*.wav")
		if err != nil {
			return
		}
		defer os.Remove(tmp.Name())
		_, _ = tmp.Write(audio)
		_ = tmp.Close()
		player, args := findPlayer()
		if player == "" {
			return
		}
		_ = exec.Command(player, append(args, tmp.Name())...).Run()
	}()
}

// voiceAnnounce lee en voz alta la respuesta de los agentes que acaban de
// terminar su turno (working -> waiting).
func Announce(previous map[string]string, view ws.WorkspaceView) {
	if previous == nil || !On.Load() {
		return
	}
	for _, worker := range view.Workers {
		before, known := previous[worker.ID]
		if !known || before != "working" || worker.Agent.Status != "waiting" {
			continue
		}
		reply := ws.ParseReply(ws.AgentOutput(worker.ID, 400))
		if reply == "" {
			reply = "termino y espera instrucciones"
		}
		SpeakAsync(worker.Ticket, worker.Ticket+". "+reply)
	}
}
