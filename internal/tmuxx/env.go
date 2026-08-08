package tmuxx

// Higiene del entorno GLOBAL del servidor tmux.
//
// Cada pane nuevo hereda el entorno global del servidor, que queda fijado con
// el de la shell que levanto el servidor la primera vez. Si esa shell traia un
// proxy que ya no existe (caso tipico: un sandbox que apunta HTTP_PROXY a un
// puerto muerto para cortar la red), TODO lo que lance el panel adentro nace
// sin salida a internet: el agente arranca pero no llega a su API
// ("Unable to connect to API (ConnectionRefused)") y npm/go tampoco bajan nada.
//
// Al arrancar se quitan del entorno global SOLO los proxys que no tienen a
// nadie escuchando: un proxy real (corporativo, mitmproxy) se respeta.

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

var proxyVars = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "all_proxy",
}

var defaultProxyPort = map[string]string{
	"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080",
}

// tmuxGlobalEnv lee el entorno global del servidor tmux. Las lineas con un
// "-" delante son variables marcadas como removidas.
func tmuxGlobalEnv() map[string]string {
	out, err := Run("show-environment", "-g")
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env
}

// proxyIsDead: nadie escucha del otro lado, asi que cualquier proceso que lo
// use se va a comer un ConnectionRefused.
func proxyIsDead(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	port := parsed.Port()
	if port == "" {
		port = defaultProxyPort[parsed.Scheme]
	}
	if port == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(parsed.Hostname(), port), 400*time.Millisecond)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// sanitizeTmuxEnv devuelve los proxys muertos que saco del entorno global.
func SanitizeEnv() []string {
	env := tmuxGlobalEnv()
	var removed []string
	for _, key := range proxyVars {
		value, ok := env[key]
		if !ok || !proxyIsDead(value) {
			continue
		}
		if _, err := Run("set-environment", "-g", "-u", key); err == nil {
			removed = append(removed, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return removed
}
