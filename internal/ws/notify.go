package ws

// Notificaciones de escritorio via notify-send (libnotify). El panel avisa
// cuando un agente pasa de working a waiting (termino y espera instrucciones)
// o cuando su sesion muere. Corren en el goroutine de refresco, asi que
// tambien se disparan mientras el usuario esta attacheado a otra sesion.

import (
	"sync"
	"time"
	"versecam/internal/execx"
)

var NotificationsEnabled = true

var (
	notifyMu   sync.Mutex
	notifyLast = map[string]time.Time{}
)

func notifySend(urgency, summary, body string) {
	_, _ = execx.RunTimeout(5*time.Second, "", "notify-send",
		"-a", "versecam", "-i", "utilities-terminal",
		"-u", urgency, summary, body)
}

// notifyCooldown evita spam cuando el estado del agente parpadea: una pausa
// larga de Claude a mitad de turno puede leerse como waiting por un momento.
func notifyCooldown(key string, cooldown time.Duration) bool {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if last, ok := notifyLast[key]; ok && time.Since(last) < cooldown {
		return false
	}
	notifyLast[key] = time.Now()
	return true
}

func AgentStatuses(view WorkspaceView) map[string]string {
	out := map[string]string{}
	for _, worker := range view.Workers {
		out[worker.ID] = worker.Agent.Status
	}
	return out
}

func NotifyTransitions(previous map[string]string, view WorkspaceView) {
	if previous == nil || !NotificationsEnabled {
		return
	}
	for _, worker := range view.Workers {
		before, known := previous[worker.ID]
		now := worker.Agent.Status
		if !known || before == now {
			continue
		}
		switch {
		case before == "working" && now == "waiting":
			if !notifyCooldown(worker.ID+":waiting", time.Minute) {
				continue
			}
			body := worker.Agent.Task
			if body == "" {
				body = "listo para instrucciones"
			}
			notifySend("normal", worker.Ticket+": agente en espera", body)
		case before == "working" && (now == "stopped" || now == "sin agente"):
			if !notifyCooldown(worker.ID+":stopped", time.Minute) {
				continue
			}
			notifySend("critical", worker.Ticket+": el agente se detuvo", "la sesion quedo sin proceso de agente")
		}
	}
}
