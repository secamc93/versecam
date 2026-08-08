package ws

// Infraestructura del proyecto: los postgres, redis, rabbit, nats... que
// acompañan al worker. No hay que declararlos en el services_conf; se detectan
// solos por dos vias:
//
//   - los contenedores que docker ya conoce, con las etiquetas de compose que
//     dicen de que carpeta salieron (asi se sabe a que worker pertenecen);
//   - los servicios declarados en los docker-compose del repo que todavia no
//     existen como contenedor, para poder levantarlos desde el panel.
//
// Lo que no encaje en ninguna imagen conocida igual aparece, como "docker": es
// preferible mostrar de mas que esconder algo que el proyecto necesita.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"versecam/internal/config"
	"versecam/internal/execx"
)

// InfraKind: la familia del servicio, para que el panel pinte icono y nombre
// legible. La clave es un prefijo de la imagen.
var infraKinds = []struct{ match, kind string }{
	{"postgis", "postgres"}, {"postgres", "postgres"}, {"timescale", "postgres"},
	{"mariadb", "mysql"}, {"mysql", "mysql"},
	{"mongo", "mongo"},
	{"valkey", "redis"}, {"redis", "redis"},
	{"rabbitmq", "rabbit"},
	{"nats", "nats"},
	{"redpanda", "kafka"}, {"kafka", "kafka"}, {"zookeeper", "kafka"},
	{"opensearch", "search"}, {"elasticsearch", "search"},
	{"clickhouse", "clickhouse"},
	{"cassandra", "cassandra"},
	{"memcached", "cache"},
	{"minio/mc", "docker"}, // el cliente de minio: tareas de un solo uso, no un servicio
	{"minio", "storage"},
	{"localstack", "aws"}, {"elasticmq", "aws"},
	{"mailhog", "mail"}, {"mailpit", "mail"},
}

// KindForImage clasifica por la imagen. Devuelve "docker" para lo que no
// reconoce: sigue siendo parte del proyecto.
func KindForImage(image string) string {
	lower := strings.ToLower(image)
	for _, candidate := range infraKinds {
		if strings.Contains(lower, candidate.match) {
			return candidate.kind
		}
	}
	return "docker"
}

type dockerContainer struct {
	Name    string
	Image   string
	State   string // running, exited, restarting, created, paused
	Status  string // "Up 3 hours"
	Ports   []int
	Service string // nombre en el compose
	WorkDir string // carpeta desde la que se levanto el compose
	File    string // primer archivo compose
}

const (
	dockerTTL  = 4 * time.Second
	composeTTL = 60 * time.Second
)

var (
	dockerMu    sync.Mutex
	dockerCache []dockerContainer
	dockerAt    time.Time
	dockerOK    bool
)

var hostPortRe = regexp.MustCompile(`:(\d+)->`)

// dockerContainers lista TODOS los contenedores (una sola llamada, cacheada):
// el panel refresca cada 3s y consulta esto por cada worker.
func dockerContainers() []dockerContainer {
	dockerMu.Lock()
	defer dockerMu.Unlock()
	if dockerOK && time.Since(dockerAt) < dockerTTL {
		return dockerCache
	}
	const format = `{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Status}}\t{{.Ports}}\t` +
		`{{.Label "com.docker.compose.service"}}\t{{.Label "com.docker.compose.project.working_dir"}}\t` +
		`{{.Label "com.docker.compose.project.config_files"}}`
	raw, err := execx.RunTimeout(8*time.Second, "", "docker", "ps", "-a", "--no-trunc", "--format", format)
	dockerAt = time.Now()
	dockerOK = true
	if err != nil {
		dockerCache = nil // sin docker (o sin permisos): el panel sigue con lo demas
		return nil
	}
	var out []dockerContainer
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 8 || fields[0] == "" {
			continue
		}
		container := dockerContainer{
			Name: fields[0], Image: fields[1], State: fields[2], Status: fields[3],
			Service: fields[5], WorkDir: fields[6],
			File: strings.SplitN(fields[7], ",", 2)[0],
		}
		seen := map[int]bool{}
		for _, match := range hostPortRe.FindAllStringSubmatch(fields[4], -1) {
			port, err := strconv.Atoi(match[1])
			if err != nil || seen[port] {
				continue
			}
			seen[port] = true
			container.Ports = append(container.Ports, port)
		}
		sort.Ints(container.Ports)
		out = append(out, container)
	}
	dockerCache = out
	return out
}

type composeService struct {
	File      string
	Dir       string
	Service   string
	Image     string
	Container string   // container_name declarado, si lo hay
	DependsOn []string // otros servicios del mismo compose
}

var (
	composeMu    sync.Mutex
	composeCache = map[string]composeEntry{}
)

type composeEntry struct {
	services []composeService
	at       time.Time
}

// composeServicesFor busca los docker-compose del worker y saca los servicios
// que declaran. La busqueda es corta a proposito (3 niveles, sin carpetas de
// dependencias): un repo grande no puede costar un recorrido completo.
func composeServicesFor(root string) []composeService {
	composeMu.Lock()
	cached, ok := composeCache[root]
	composeMu.Unlock()
	if ok && time.Since(cached.at) < composeTTL {
		return cached.services
	}
	var out []composeService
	for _, file := range composeFiles(root) {
		out = append(out, parseComposeServices(file)...)
	}
	composeMu.Lock()
	composeCache[root] = composeEntry{services: out, at: time.Now()}
	composeMu.Unlock()
	return out
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	".next": true, "target": true, "venv": true, ".venv": true, "__pycache__": true,
}

func composeFiles(root string) []string {
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if depth > 0 && !skipDirs[name] && !strings.HasPrefix(name, ".") {
					walk(filepath.Join(dir, name), depth-1)
				}
				continue
			}
			lower := strings.ToLower(name)
			if !strings.HasSuffix(lower, ".yml") && !strings.HasSuffix(lower, ".yaml") {
				continue
			}
			if strings.HasPrefix(lower, "docker-compose") || strings.HasPrefix(lower, "compose") {
				out = append(out, filepath.Join(dir, name))
			}
		}
	}
	walk(root, 3)
	sort.Strings(out)
	return out
}

// parseComposeServices lee el bloque services: de un compose. Es un lector
// minimo a proposito: el modulo raiz no tiene dependencias y de aqui solo hace
// falta el nombre de cada servicio, su imagen y su container_name.
func parseComposeServices(path string) []composeService {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []composeService
	dir := filepath.Dir(path)
	inServices := false
	serviceIndent := -1
	current := -1
	dependsIndent := -1 // >=0 mientras se leen las lineas de un depends_on
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)

		if indent == 0 {
			inServices = trimmed == "services:"
			serviceIndent = -1
			current = -1
			dependsIndent = -1
			continue
		}
		if !inServices {
			continue
		}
		// Primer nivel dentro de services: cada clave es un servicio.
		if serviceIndent < 0 || indent == serviceIndent {
			if !strings.HasSuffix(trimmed, ":") {
				continue
			}
			serviceIndent = indent
			dependsIndent = -1
			out = append(out, composeService{
				File: path, Dir: dir, Service: strings.TrimSuffix(trimmed, ":"),
			})
			current = len(out) - 1
			continue
		}
		if current < 0 || indent <= serviceIndent {
			continue
		}
		// depends_on acepta lista ("- redis") o mapa ("redis:" con condition):
		// de las dos solo interesa el nombre.
		if dependsIndent >= 0 {
			if indent > dependsIndent {
				name := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
				name = strings.TrimSuffix(name, ":")
				if name != "" && !strings.Contains(name, ":") {
					out[current].DependsOn = append(out[current].DependsOn, name)
				}
				continue
			}
			dependsIndent = -1
		}
		switch {
		case strings.HasPrefix(trimmed, "image:"):
			out[current].Image = composeValue(trimmed[len("image:"):])
		case strings.HasPrefix(trimmed, "container_name:"):
			out[current].Container = composeValue(trimmed[len("container_name:"):])
		case trimmed == "depends_on:":
			dependsIndent = indent
		}
	}
	return out
}

var envFileNames = []string{".env", ".env.local", ".env.development", ".env.dev", ".env.example"}

// portRefRe: puertos citados en la configuracion, sea "DB_PORT=5444" o
// "amqp://user:pass@127.0.0.1:5674/".
var portRefRe = regexp.MustCompile(`[:=]\s*(\d{3,5})\b`)

var (
	envMu    sync.Mutex
	envCache = map[string]envEntry{}
)

type envEntry struct {
	ports map[int]bool
	at    time.Time
}

// envPorts lee la configuracion de un servicio y devuelve los puertos que
// menciona. Cruzados con los puertos de la infraestructura, dicen a que se
// conecta cada proceso sin que nadie lo declare en ningun lado.
func envPorts(dir string) map[int]bool {
	envMu.Lock()
	cached, ok := envCache[dir]
	envMu.Unlock()
	if ok && time.Since(cached.at) < composeTTL {
		return cached.ports
	}
	ports := map[int]bool{}
	for _, name := range envFileNames {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if len(raw) > 256*1024 {
			raw = raw[:256*1024]
		}
		for _, match := range portRefRe.FindAllStringSubmatch(string(raw), -1) {
			if port, err := strconv.Atoi(match[1]); err == nil {
				ports[port] = true
			}
		}
	}
	envMu.Lock()
	envCache[dir] = envEntry{ports: ports, at: time.Now()}
	envMu.Unlock()
	return ports
}

// composeValue limpia el valor de una clave: comillas, comentario al final y
// espacios.
func composeValue(raw string) string {
	value := strings.TrimSpace(raw)
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return strings.Trim(value, `"'`)
}

// InfraService es un servicio de infraestructura listo para el panel.
type InfraService struct {
	ID        string
	Name      string
	Kind      string
	Image     string
	Container string
	State     string // running, exited, restarting, created, ausente
	Status    string
	Ports     []int
	File      string
	Dir       string
	Service   string
	DependsOn []string // nombres de servicios del mismo compose
}

// InfraForWorker: la infraestructura que le corresponde al worker. Los
// contenedores se atribuyen por la carpeta desde la que se levanto el compose;
// asi cada worktree ve LO SUYO y no la infra de los demas.
func InfraForWorker(root string) []InfraService {
	var out []InfraService
	claimed := map[string]bool{} // compose file + servicio ya cubierto por un contenedor

	for _, container := range dockerContainers() {
		owned := container.WorkDir != "" && underPath(container.WorkDir, root)
		// El contenedor de base de datos declarado en el workspace cuenta
		// aunque no venga de un compose del repo.
		if !owned && config.DBContainer != "" && container.Name == config.DBContainer {
			owned = true
		}
		if !owned {
			continue
		}
		name := container.Service
		if name == "" {
			name = container.Name
		}
		out = append(out, InfraService{
			ID:        "docker:" + container.Name,
			Name:      name,
			Kind:      KindForImage(container.Image),
			Image:     container.Image,
			Container: container.Name,
			State:     container.State,
			Status:    container.Status,
			Ports:     container.Ports,
			File:      container.File,
			Dir:       container.WorkDir,
			Service:   container.Service,
		})
		if container.File != "" && container.Service != "" {
			claimed[container.File+"#"+container.Service] = true
		}
		// El depends_on vive en el compose, no en el contenedor: se busca ahi.
		for _, declared := range composeServicesFor(root) {
			if declared.File == container.File && declared.Service == container.Service {
				out[len(out)-1].DependsOn = declared.DependsOn
				break
			}
		}
	}

	// Declarados en el compose del repo pero sin contenedor todavia. Aqui si
	// se filtra: solo infraestructura reconocida (un backend declarado que
	// nadie ha levantado no es un servicio del panel) y una sola vez por
	// familia, que el mismo postgres suele estar en varios compose.
	seen := map[string]bool{}
	for _, service := range out {
		seen[service.Kind+"/"+service.Name] = true
	}
	for _, service := range composeServicesFor(root) {
		if service.Image == "" || claimed[service.File+"#"+service.Service] {
			continue
		}
		kind := KindForImage(service.Image)
		if kind == "docker" || isDeployManifest(service.File) {
			continue
		}
		if seen[kind+"/"+service.Service] {
			continue
		}
		seen[kind+"/"+service.Service] = true
		if service.Container != "" && containerExists(service.Container) {
			continue
		}
		out = append(out, InfraService{
			ID:        "compose:" + service.File + "#" + service.Service,
			Name:      service.Service,
			Kind:      kind,
			Image:     service.Image,
			State:     "ausente",
			Status:    "declarado en " + filepath.Base(service.File),
			File:      service.File,
			Dir:       service.Dir,
			Service:   service.Service,
			DependsOn: service.DependsOn,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if (out[i].State == "running") != (out[j].State == "running") {
			return out[i].State == "running"
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// isDeployManifest: los compose de despliegue (aws, produccion, staging) no
// describen la maquina del desarrollador; declararlos como servicios locales
// solo llena el panel de cosas que nadie va a levantar aqui.
func isDeployManifest(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	for _, marker := range []string{"aws", "prod", "staging", "deploy", "ci"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// composeCmd resuelve como se invoca compose aqui: el plugin de docker
// (docker compose) o el binario suelto (docker-compose). No todas las maquinas
// tienen los dos, y asumir uno deja el boton de levantar servicios muerto.
var composeOnce struct {
	sync.Once
	name string
	args []string
}

func composeCmd() (string, []string) {
	composeOnce.Do(func() {
		if _, err := execx.RunTimeout(10*time.Second, "", "docker", "compose", "version"); err == nil {
			composeOnce.name, composeOnce.args = "docker", []string{"compose"}
			return
		}
		if _, err := exec.LookPath("docker-compose"); err == nil {
			composeOnce.name = "docker-compose"
		}
	})
	return composeOnce.name, append([]string(nil), composeOnce.args...)
}

func containerExists(name string) bool {
	for _, container := range dockerContainers() {
		if container.Name == name {
			return true
		}
	}
	return false
}

// findInfra valida el id que llega por URL contra lo detectado: nunca se arma
// un comando docker con un valor que no salio de aqui.
func findInfra(root, id string) (InfraService, bool) {
	for _, service := range InfraForWorker(root) {
		if service.ID == id {
			return service, true
		}
	}
	return InfraService{}, false
}

// IsInfraID distingue los servicios de infraestructura de los procesos de
// desarrollo del services_conf.
func IsInfraID(id string) bool {
	return strings.HasPrefix(id, "docker:") || strings.HasPrefix(id, "compose:")
}

// InfraAction enciende, apaga o reinicia un contenedor. Un servicio "ausente"
// solo se puede levantar, y para eso hace falta el compose que lo declara.
func InfraAction(root, id, action string) (string, error) {
	service, ok := findInfra(root, id)
	if !ok {
		return "", ErrNotFound
	}
	invalidate := func() {
		dockerMu.Lock()
		dockerOK = false
		dockerMu.Unlock()
	}
	defer invalidate()

	if service.Container == "" {
		if action != "start" {
			return "", ErrNotFound
		}
		name, args := composeCmd()
		if name == "" {
			return "", fmt.Errorf("no hay docker compose en esta maquina")
		}
		args = append(args, "-f", service.File, "up", "-d", service.Service)
		return execx.RunTimeout(180*time.Second, service.Dir, name, args...)
	}
	switch action {
	case "start", "stop", "restart":
		return execx.RunTimeout(120*time.Second, "", "docker", action, service.Container)
	}
	return "", ErrNotFound
}

// InfraLogs devuelve los ultimos logs del contenedor (stdout+stderr juntos,
// como los muestra docker).
func InfraLogs(root, id string, lines int) string {
	service, ok := findInfra(root, id)
	if !ok {
		return ""
	}
	if service.Container == "" {
		return "(sin contenedor todavia: [s] lo levanta con docker compose)\n\n" +
			"declarado en " + service.File
	}
	out, err := execx.RunTimeout(15*time.Second, "", "docker", "logs", "--tail", strconv.Itoa(lines), service.Container)
	if err != nil && strings.TrimSpace(out) == "" {
		return "(no se pudieron leer los logs: " + err.Error() + ")"
	}
	return out
}
