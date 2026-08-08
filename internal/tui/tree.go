package tui

// Explorador de archivos del worker, estilo editor: arbol completo del
// worktree (carga perezosa por carpeta), con los archivos que trae la rama
// pintados por estado. No depende de herramientas externas.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type treeNode struct {
	name     string
	rel      string
	abs      string
	isDir    bool
	expanded bool
	loaded   bool
	children []*treeNode
}

var skipDirNames = map[string]bool{".git": true}

func newTree(root string) *treeNode {
	return &treeNode{name: filepath.Base(root), abs: root, isDir: true, expanded: true}
}

func (node *treeNode) load() {
	if node.loaded {
		return
	}
	node.loaded = true
	items, err := os.ReadDir(node.abs)
	if err != nil {
		return
	}
	var dirs, files []*treeNode
	for _, item := range items {
		if skipDirNames[item.Name()] {
			continue
		}
		child := &treeNode{
			name:  item.Name(),
			abs:   filepath.Join(node.abs, item.Name()),
			isDir: item.IsDir(),
		}
		if node.rel == "" {
			child.rel = item.Name()
		} else {
			child.rel = node.rel + "/" + item.Name()
		}
		if child.isDir {
			dirs = append(dirs, child)
		} else {
			files = append(files, child)
		}
	}
	byName := func(list []*treeNode) {
		sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	}
	byName(dirs)
	byName(files)
	node.children = append(dirs, files...)
}

// expandDirty abre de entrada las carpetas que contienen cambios de la rama,
// para que lo pintado quede visible sin buscarlo.
func (node *treeNode) expandDirty(dirty map[string]bool) {
	if !node.isDir {
		return
	}
	if node.rel != "" && !dirty[node.rel] {
		return
	}
	node.load()
	node.expanded = true
	for _, child := range node.children {
		child.expandDirty(dirty)
	}
}

type treeItem struct {
	node  *treeNode
	depth int
}

func flattenTree(root *treeNode) []treeItem {
	var out []treeItem
	var walk func(node *treeNode, depth int)
	walk = func(node *treeNode, depth int) {
		for _, child := range node.children {
			out = append(out, treeItem{child, depth})
			if child.isDir && child.expanded {
				walk(child, depth+1)
			}
		}
	}
	if root.expanded {
		walk(root, 0)
	}
	return out
}

// fileIcon: iconos por tipo, en escapes unicode para no meter emoji crudo en
// el fuente (no-unicode-in-code).
func fileIcon(node *treeNode) string {
	if node.isDir {
		if node.expanded {
			return "\U0001F4C2"
		}
		return "\U0001F4C1"
	}
	lower := node.name
	for i := 0; i < len(lower); i++ {
		if lower[i] >= 'A' && lower[i] <= 'Z' {
			lower = lower[:i] + string(lower[i]+32) + lower[i+1:]
		}
	}
	if containsAny(lower, "_test.", ".test.", ".spec.") {
		return "\U0001F9EA"
	}
	if containsAny(lower, "dockerfile", "docker-compose") {
		return "\U0001F433"
	}
	switch filepath.Ext(lower) {
	case ".go":
		return "\U0001F537"
	case ".js", ".jsx":
		return "\U0001F7E8"
	case ".ts", ".tsx":
		return "\U0001F7E6"
	case ".py":
		return "\U0001F40D"
	case ".php":
		return "\U0001F418"
	case ".md", ".txt":
		return "\U0001F4DD"
	case ".json", ".yml", ".yaml", ".toml", ".conf", ".ini", ".env":
		return "\U0001F527"
	case ".sql":
		return "\U0001F4BE"
	case ".sh", ".zsh", ".bash":
		return "\u26A1"
	case ".png", ".jpg", ".jpeg", ".svg", ".gif", ".ico", ".css", ".scss":
		return "\U0001F3A8"
	case ".html", ".vue", ".astro":
		return "\U0001F310"
	case ".lock", ".sum", ".mod":
		return "\U0001F4E6"
	case ".dart":
		return "\U0001F3AF"
	case ".kt", ".java":
		return "\U0001F916"
	case ".pdf", ".xlsx", ".csv":
		return "\U0001F4CA"
	}
	return "\U0001F4C4"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// buildChangedTree arma el arbol SOLO con los archivos que trae la rama, en
// su estructura de carpetas, todo expandido: lo que se toco, sin ruido.
func buildChangedTree(rootPath string, changed map[string]string) *treeNode {
	root := newTree(rootPath)
	root.loaded = true
	index := map[string]*treeNode{"": root}
	var rels []string
	for rel := range changed {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		parts := strings.Split(rel, "/")
		parent := root
		parentRel := ""
		for i, part := range parts {
			childRel := part
			if parentRel != "" {
				childRel = parentRel + "/" + part
			}
			node, ok := index[childRel]
			if !ok {
				node = &treeNode{
					name:   part,
					rel:    childRel,
					abs:    filepath.Join(rootPath, childRel),
					isDir:  i < len(parts)-1,
					loaded: true,
				}
				index[childRel] = node
				parent.children = append(parent.children, node)
			}
			parent = node
			parentRel = childRel
		}
	}
	sortTree(root)
	compressTree(root)
	return root
}

// compressTree une cadenas de carpetas con un solo hijo carpeta
// ("back/central/services/") como hacen los editores, para no gastar
// una fila y un nivel de indentacion por cada una.
func compressTree(node *treeNode) {
	for _, child := range node.children {
		for child.isDir && len(child.children) == 1 && child.children[0].isDir {
			grandchild := child.children[0]
			child.name = child.name + "/" + grandchild.name
			child.rel = grandchild.rel
			child.abs = grandchild.abs
			child.children = grandchild.children
		}
		compressTree(child)
	}
}

func sortTree(node *treeNode) {
	sort.Slice(node.children, func(i, j int) bool {
		a, b := node.children[i], node.children[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		return a.name < b.name
	})
	for _, child := range node.children {
		sortTree(child)
	}
}
