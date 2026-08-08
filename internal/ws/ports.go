package ws

// Deteccion de puertos reales de cada servicio: se cruza el arbol de procesos
// del pane de tmux con los sockets TCP en LISTEN. Es la evidencia de que el
// servicio escucha, no la convencion de offsets del worktree.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"versecam/internal/execx"
)

var (
	ssListenLine = regexp.MustCompile(`:(\d+)\s+\S+\s+users:\((.+)\)`)
	ssPIDField   = regexp.MustCompile(`pid=(\d+)`)
)

// listeningPortsByPID mapea pid -> puertos TCP en LISTEN con una sola pasada
// de ss. Sin sudo solo se ven los procesos propios, que es lo que se necesita.
func listeningPortsByPID() map[int][]int {
	out, err := execx.RunTimeout(5*time.Second, "", "ss", "-tlnp")
	if err != nil {
		return nil
	}
	result := map[int][]int{}
	for _, line := range strings.Split(out, "\n") {
		match := ssListenLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		port, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		for _, pidMatch := range ssPIDField.FindAllStringSubmatch(match[2], -1) {
			pid, _ := strconv.Atoi(pidMatch[1])
			result[pid] = append(result[pid], port)
		}
	}
	return result
}

// procSnapshot es una foto de todos los procesos: arbol padre-hijo y RSS.
// Una sola pasada de ps sirve para puertos y para memoria.
type procSnapshot struct {
	children map[int][]int
	rssKB    map[int]int64
}

func snapshotProcs() *procSnapshot {
	snap := &procSnapshot{children: map[int][]int{}, rssKB: map[int]int64{}}
	out, err := execx.RunTimeout(5*time.Second, "", "ps", "-eo", "pid=,ppid=,rss=")
	if err != nil {
		return snap
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		parent, err2 := strconv.Atoi(fields[1])
		rss, err3 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		snap.children[parent] = append(snap.children[parent], pid)
		snap.rssKB[pid] = rss
	}
	return snap
}

// tree devuelve el pid raiz y todos sus descendientes (shell -> wgo -> go run
// -> binario).
func (snap *procSnapshot) tree(rootPID int) []int {
	seen := map[int]bool{}
	queue := []int{rootPID}
	var out []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
		queue = append(queue, snap.children[pid]...)
	}
	return out
}

func (snap *procSnapshot) treeRSSKB(panePID string) int64 {
	root, err := strconv.Atoi(strings.TrimSpace(panePID))
	if err != nil {
		return 0
	}
	var total int64
	for _, pid := range snap.tree(root) {
		total += snap.rssKB[pid]
	}
	return total
}

func (snap *procSnapshot) treePorts(panePID string, listening map[int][]int) []int {
	root, err := strconv.Atoi(strings.TrimSpace(panePID))
	if err != nil {
		return nil
	}
	var ports []int
	for _, pid := range snap.tree(root) {
		ports = append(ports, listening[pid]...)
	}
	sort.Ints(ports)
	deduped := ports[:0]
	for i, port := range ports {
		if i == 0 || port != ports[i-1] {
			deduped = append(deduped, port)
		}
	}
	return deduped
}

func EnrichServicePorts(services []ServiceState) {
	listening := listeningPortsByPID()
	snap := snapshotProcs()
	for i := range services {
		if !services[i].Running || services[i].PID == "" {
			continue
		}
		services[i].Ports = snap.treePorts(services[i].PID, listening)
		services[i].MemKB = snap.treeRSSKB(services[i].PID)
	}
}
