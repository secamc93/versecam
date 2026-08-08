package ws

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"versecam/internal/config"
	"versecam/internal/execx"
	"versecam/internal/tmuxx"
)

type ServiceDef struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Cmd  string `json:"cmd"`
	Desc string `json:"desc"`
}

func LoadServiceDefs() []ServiceDef {
	f, err := os.Open(config.ServicesConf)
	if err != nil {
		return nil
	}
	defer f.Close()

	var defs []ServiceDef
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":::")
		if len(parts) < 4 {
			continue
		}
		defs = append(defs, ServiceDef{
			Name: parts[0], Dir: parts[1], Cmd: parts[2], Desc: parts[3],
		})
	}
	return defs
}

type ServiceState struct {
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Running bool   `json:"running"`
	Session string `json:"session"`
	PID     string `json:"pid"`
	Ports   []int  `json:"ports,omitempty"`
	MemKB   int64  `json:"mem_kb,omitempty"`
	// Los de infraestructura (contenedores detectados solos) viajan por la
	// misma lista: el panel los distingue por Kind y los maneja por ID.
	ID    string `json:"id,omitempty"`    // vacio = el propio Name (proceso del services_conf)
	Kind  string `json:"kind,omitempty"`  // postgres, redis, rabbit, nats, docker...
	State string `json:"state,omitempty"` // estado crudo del contenedor
	Image string `json:"image,omitempty"`
	// Para la vista de diagrama: en que capa va y de que depende.
	Tier  string   `json:"tier,omitempty"`  // app, servicio, infra
	Links []string `json:"links,omitempty"` // ids de los que consume
	// dependsOn viaja solo dentro del paquete: son nombres de servicios del
	// compose, que linkServices traduce a ids.
	dependsOn []string
}

func velSession(worker, service string) string {
	if worker == "" {
		return config.VelPrefix + "-" + service
	}
	return config.VelPrefix + "-" + service + "-" + worker
}

// servicesForWorker solo reporta los servicios cuyo directorio existe dentro
// del worker: un worktree agrega repos bajo demanda, no los tiene todos.
func ServicesForWorker(root string, worker string, defs []ServiceDef, live map[string]bool) []ServiceState {
	var out []ServiceState
	dirs := map[string]string{} // servicio -> su carpeta, para leer su configuracion
	for _, def := range defs {
		repo := strings.SplitN(def.Dir, "/", 2)[0]
		if _, err := os.Stat(filepath.Join(root, repo)); err != nil {
			continue
		}
		sess := velSession(worker, def.Name)
		st := ServiceState{
			Name: def.Name, Desc: def.Desc, Session: sess, Running: live[sess],
			Tier: tierFor(def),
		}
		if st.Running {
			st.PID = tmuxx.PanePID(sess)
		}
		dirs[def.Name] = filepath.Join(root, def.Dir)
		out = append(out, st)
	}
	out = append(out, infraStates(root)...)
	return linkServices(out, dirs)
}

// tierFor decide en que capa del diagrama va un proceso. El comando es la
// pista mas fiable (los bundlers de front son inconfundibles); la ruta
// desempata.
func tierFor(def ServiceDef) string {
	haystack := strings.ToLower(def.Cmd + " " + def.Dir + " " + def.Desc)
	for _, marker := range []string{"next", "vite", "astro", "nuxt", "ng serve", "react", "front", "web"} {
		if strings.Contains(haystack, marker) {
			return "app"
		}
	}
	return "servicio"
}

// linkServices conecta cada proceso con la infraestructura que consume. Nadie
// declara esas dependencias: se deducen de los puertos que menciona la
// configuracion del servicio y del depends_on de los compose.
func linkServices(services []ServiceState, dirs map[string]string) []ServiceState {
	byPort := map[int]string{}
	byName := map[string]string{}
	tiers := map[string]string{}
	for _, service := range services {
		id := serviceID(service)
		tiers[id] = service.Tier
		for _, port := range service.Ports {
			byPort[port] = id
		}
		// Los procesos todavia no tienen puerto medido aqui (eso sale del
		// arbol de procesos, mas tarde): se usa el que anuncia su descripcion,
		// que es como un front encuentra a su backend.
		if service.Kind == "" {
			for _, match := range descPortRe.FindAllStringSubmatch(service.Desc, -1) {
				if port, err := strconv.Atoi(match[1]); err == nil {
					byPort[port] = id
				}
			}
		}
		byName[service.Name] = id
	}
	for i, service := range services {
		id := serviceID(service)
		if service.Kind == "" {
			seen := map[string]bool{}
			for port := range envPorts(dirs[service.Name]) {
				target, ok := byPort[port]
				if !ok || target == id || seen[target] {
					continue
				}
				// Las referencias van en los dos sentidos (el backend tambien
				// conoce la URL del front), pero el diagrama solo dibuja hacia
				// abajo: de lo contrario deja de leerse como jerarquia.
				if tierRank(tiers[target]) < tierRank(service.Tier) {
					continue
				}
				seen[target] = true
				services[i].Links = append(services[i].Links, target)
			}
			sort.Strings(services[i].Links)
			continue
		}
		for _, dependency := range service.dependsOn {
			if target, ok := byName[dependency]; ok && target != id {
				services[i].Links = append(services[i].Links, target)
			}
		}
	}
	return services
}

// descPortRe: el puerto que el servicio anuncia en su descripcion, "(:3060)".
var descPortRe = regexp.MustCompile(`:(\d{3,5})\b`)

// tierRank ordena las capas de arriba hacia abajo en el diagrama.
func tierRank(tier string) int {
	switch tier {
	case "app":
		return 0
	case "infra":
		return 2
	default:
		return 1
	}
}

// serviceID: los de infraestructura se identifican por su id; los procesos,
// por su nombre.
func serviceID(service ServiceState) string {
	if service.ID != "" {
		return service.ID
	}
	return service.Name
}

// infraStates traduce la infraestructura detectada (postgres, redis, rabbit,
// nats...) al mismo tipo que consume el panel.
func infraStates(root string) []ServiceState {
	var out []ServiceState
	for _, service := range InfraForWorker(root) {
		desc := service.Image
		if service.Status != "" {
			desc += " · " + service.Status
		}
		out = append(out, ServiceState{
			Name:      service.Name,
			Desc:      desc,
			Running:   service.State == "running",
			Ports:     service.Ports,
			ID:        service.ID,
			Kind:      service.Kind,
			State:     service.State,
			Image:     service.Image,
			Tier:      "infra",
			dependsOn: service.DependsOn,
		})
	}
	return out
}

// KnownService valida un nombre de servicio contra el manifiesto: los ids que
// llegan por URL nunca se usan sin validar.
func KnownService(name string) bool {
	for _, def := range LoadServiceDefs() {
		if def.Name == name {
			return true
		}
	}
	return false
}

func LiveSessions() map[string]bool {
	out := map[string]bool{}
	for _, s := range tmuxx.Sessions() {
		out[s] = true
	}
	return out
}

// velCommand ejecuta vel dentro del worker para que herede el contexto
// (puertos, BD y sufijo de sesion) que vel infiere del cwd. Sin vel_bin,
// versecam gestiona el servicio directo sobre tmux (mismo patron: la sesion
// sobrevive, hay scrollback y se puede hacer attach).
func VelCommand(root string, args ...string) (string, error) {
	if config.VelBin == "" {
		if len(args) != 2 {
			return "", fmt.Errorf("accion invalida")
		}
		return nativeService(root, args[0], args[1])
	}
	return execx.RunTimeout(60*time.Second, root, config.VelBin, args...)
}

func nativeService(root, action, name string) (string, error) {
	var def *ServiceDef
	for _, candidate := range LoadServiceDefs() {
		if candidate.Name == name {
			found := candidate
			def = &found
			break
		}
	}
	if def == nil {
		return "", fmt.Errorf("servicio desconocido: %s", name)
	}
	sess := velSession(workerIDForRoot(root), name)
	dir := filepath.Join(root, def.Dir)

	switch action {
	case "start":
		if tmuxx.HasSession(sess) {
			return "", fmt.Errorf("%s ya esta corriendo", name)
		}
		return "", nativeStart(sess, dir, def.Cmd)
	case "stop":
		if !tmuxx.HasSession(sess) {
			return "", fmt.Errorf("%s no esta corriendo", name)
		}
		return "", tmuxx.Kill(sess)
	case "restart":
		if tmuxx.HasSession(sess) {
			_ = tmuxx.Kill(sess)
		}
		return "", nativeStart(sess, dir, def.Cmd)
	}
	return "", fmt.Errorf("accion no permitida: %s", action)
}

func nativeStart(sess, dir, cmd string) error {
	if _, err := tmuxx.Run("new-session", "-d", "-s", sess, "-c", dir, "-x", "220", "-y", "50"); err != nil {
		return err
	}
	tmuxx.SessionsInvalidate()
	_, _ = tmuxx.Run("set-option", "-t", sess, "history-limit", "50000")
	if _, err := tmuxx.Run("send-keys", "-t", sess, "-l", cmd); err != nil {
		return err
	}
	_, err := tmuxx.Run("send-keys", "-t", sess, "Enter")
	return err
}

func workerIDForRoot(root string) string {
	entries, err := AllEntries()
	if err != nil {
		return ""
	}
	for id, entry := range entries {
		if entry.Root == root {
			if id == PrincipalID {
				return ""
			}
			return id
		}
	}
	return ""
}

func ServiceLogs(worker, service string, lines int) string {
	// Las sesiones de servicios del principal no llevan sufijo de worker
	// (vel-api, no vel-api-principal). Con colores: los logs son para ojos
	// humanos.
	if worker == PrincipalID {
		worker = ""
	}
	return tmuxx.CaptureColor(velSession(worker, service), lines)
}
