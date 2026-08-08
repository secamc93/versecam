package hl

// Resaltado de sintaxis del visor integrado: comentarios, strings, numeros y
// palabras clave por familia de lenguaje. Sin dependencias: suficiente para
// leer codigo comodo, no pretende ser un parser.

import (
	"regexp"
	"strings"
)

const (
	hlKeyword = ansiMagenta
	hlString  = ansiGreen
	hlComment = ansiDim
	hlNumber  = ansiYellow
	hlKey     = ansiCyan
)

type hlFamily struct {
	lineComment string
	blockOn     string
	blockOff    string
	keywords    *regexp.Regexp
	dataKeys    bool
}

var numberRe = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
var dataKeyRe = regexp.MustCompile(`^(\s*"?[A-Za-z0-9_.\-]+"?)(\s*:)`)
var mdHeaderRe = regexp.MustCompile(`^#{1,6}\s.*$`)

func keywordRe(words string) *regexp.Regexp {
	return regexp.MustCompile(`\b(` + strings.ReplaceAll(words, " ", "|") + `)\b`)
}

var hlFamilies = map[string]*hlFamily{}

func init() {
	cKeywords := keywordRe("func function return if else elseif for foreach while do switch case default break continue " +
		"type struct interface class enum extends implements new this self static public private protected " +
		"var let const nil null undefined true false void int int32 int64 uint float float64 double string bool byte rune error " +
		"map chan go defer select range import package export from use namespace echo try catch finally throw async await yield")
	cfam := &hlFamily{lineComment: "//", blockOn: "/*", blockOff: "*/", keywords: cKeywords}
	for _, ext := range []string{".go", ".js", ".jsx", ".ts", ".tsx", ".php", ".dart", ".kt", ".java", ".c", ".h", ".cpp", ".rs", ".css", ".scss"} {
		hlFamilies[ext] = cfam
	}

	pyFam := &hlFamily{lineComment: "#", keywords: keywordRe("def return if elif else for while in not and or import from as " +
		"class try except finally with lambda yield None True False pass raise global nonlocal async await print self")}
	hlFamilies[".py"] = pyFam

	shFam := &hlFamily{lineComment: "#", keywords: keywordRe("if then else elif fi for do done while until case esac function " +
		"echo export local return exit source set read shift break continue")}
	for _, ext := range []string{".sh", ".zsh", ".bash"} {
		hlFamilies[ext] = shFam
	}

	sqlFam := &hlFamily{lineComment: "--", blockOn: "/*", blockOff: "*/", keywords: keywordRe("select from where insert update delete " +
		"into values join left right inner outer on group by order limit offset and or not null is as create table alter drop " +
		"index primary key foreign references distinct having union set begin commit rollback")}
	hlFamilies[".sql"] = sqlFam

	dataFam := &hlFamily{lineComment: "#", dataKeys: true, keywords: keywordRe("true false null")}
	for _, ext := range []string{".yml", ".yaml", ".json", ".toml", ".conf", ".ini", ".env"} {
		hlFamilies[ext] = dataFam
	}
}

// highlightLines colorea el archivo completo linea a linea, manteniendo el
// estado de comentarios de bloque entre lineas.
// Lines resalta sintaxis por familia de lenguaje (ANSI truecolor).
func Lines(lines []string, ext string) []string {
	if ext == ".md" || ext == ".markdown" {
		return highlightMarkdown(lines)
	}
	family, ok := hlFamilies[ext]
	if !ok || len(lines) > 8000 {
		return lines
	}
	out := make([]string, len(lines))
	inBlock := false
	for i, line := range lines {
		out[i] = highlightLine(line, family, &inBlock)
	}
	return out
}

func highlightMarkdown(lines []string) []string {
	out := make([]string, len(lines))
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			inFence = !inFence
			out[i] = hlComment + line + ansiReset
		case inFence:
			out[i] = hlString + line + ansiReset
		case mdHeaderRe.MatchString(trimmed):
			out[i] = ansiBold + hlKey + line + ansiReset
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* "):
			out[i] = hlKeyword + "-" + ansiReset + line[strings.Index(line, trimmed)+1:]
		default:
			out[i] = line
		}
	}
	return out
}

// colorCode aplica numeros y keywords a un tramo de codigo puro (sin strings
// ni comentarios). Los numeros van primero: las secuencias ANSI insertadas
// traen digitos y colorearlos despues las romperia.
func colorCode(code string, family *hlFamily) string {
	if code == "" {
		return code
	}
	if family.dataKeys {
		code = dataKeyRe.ReplaceAllString(code, hlKey+"$1"+ansiReset+"$2")
	}
	code = numberRe.ReplaceAllString(code, hlNumber+"$0"+ansiReset)
	code = family.keywords.ReplaceAllString(code, hlKeyword+"$0"+ansiReset)
	return code
}

func highlightLine(line string, family *hlFamily, inBlock *bool) string {
	var out strings.Builder
	i := 0

	if *inBlock {
		end := strings.Index(line, family.blockOff)
		if end < 0 {
			return hlComment + line + ansiReset
		}
		out.WriteString(hlComment + line[:end+len(family.blockOff)] + ansiReset)
		i = end + len(family.blockOff)
		*inBlock = false
	}

	codeStart := i
	flush := func(until int) {
		out.WriteString(colorCode(line[codeStart:until], family))
	}
	for i < len(line) {
		rest := line[i:]
		switch {
		case family.lineComment != "" && strings.HasPrefix(rest, family.lineComment):
			flush(i)
			out.WriteString(hlComment + rest + ansiReset)
			return out.String()
		case family.blockOn != "" && strings.HasPrefix(rest, family.blockOn):
			flush(i)
			end := strings.Index(rest[len(family.blockOn):], family.blockOff)
			if end < 0 {
				out.WriteString(hlComment + rest + ansiReset)
				*inBlock = true
				return out.String()
			}
			stop := i + len(family.blockOn) + end + len(family.blockOff)
			out.WriteString(hlComment + line[i:stop] + ansiReset)
			i = stop
			codeStart = i
		case rest[0] == '"' || rest[0] == '\'' || rest[0] == '`':
			flush(i)
			quote := rest[0]
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == quote {
					j++
					break
				}
				j++
			}
			if j > len(line) {
				j = len(line)
			}
			out.WriteString(hlString + line[i:j] + ansiReset)
			i = j
			codeStart = i
		default:
			i++
		}
	}
	flush(len(line))
	return out.String()
}

// Paleta local (truecolor): identica a la del TUI para que el visor se vea
// igual en terminal y en la app.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[38;2;95;112;153m"
	ansiGreen   = "\x1b[38;2;166;255;43m"
	ansiYellow  = "\x1b[38;2;255;179;0m"
	ansiCyan    = "\x1b[38;2;0;240;255m"
	ansiMagenta = "\x1b[38;2;255;43;214m"
)
