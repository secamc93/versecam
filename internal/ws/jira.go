package ws

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"versecam/internal/config"
)

type JiraIssue struct {
	Key         string `json:"key"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	StatusCat   string `json:"status_cat"` // new | indeterminate | done: la fase
	Priority    string `json:"priority"`
	Updated     string `json:"updated"`
	URL         string `json:"url"`
	HasWork     bool   `json:"has_worker"`
	Sprint      string `json:"sprint,omitempty"`       // nombre del sprint del ticket
	SprintState string `json:"sprint_state,omitempty"` // active | future | closed
}

// El sprint vive en un custom field cuyo id varia por instancia: se descubre
// una vez contra /rest/api/3/field (schema gh-sprint) y se cachea.
var (
	sprintFieldMu   sync.Mutex
	sprintFieldVal  string
	sprintFieldDone bool
)

func sprintFieldID(base, email, token string) string {
	sprintFieldMu.Lock()
	defer sprintFieldMu.Unlock()
	if sprintFieldDone {
		return sprintFieldVal
	}
	sprintFieldDone = true
	body, err := jiraGet(base+"/rest/api/3/field", email, token)
	if err != nil {
		return ""
	}
	var fields []struct {
		ID     string `json:"id"`
		Schema struct {
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	for _, field := range fields {
		if strings.HasSuffix(field.Schema.Custom, ":gh-sprint") {
			sprintFieldVal = field.ID
			break
		}
	}
	return sprintFieldVal
}

type jiraResponse struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Updated string `json:"updated"`
			Status  struct {
				Name           string `json:"name"`
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"status"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
		} `json:"fields"`
	} `json:"issues"`
}

type jiraCacheEntry struct {
	issues []JiraIssue
	saved  time.Time
	err    error
}

var (
	jiraMu    sync.Mutex
	jiraCache jiraCacheEntry
)

// ResetCaches bota lo que quedo del proyecto anterior al cambiar de workspace:
// lo global (Jira) y lo que va por worker, cuyo id SI se repite entre
// proyectos ("principal" esta en todos). Los de ramas y PRs van por ruta, que
// nunca se repite.
func ResetCaches() {
	jiraMu.Lock()
	jiraCache = jiraCacheEntry{}
	jiraMu.Unlock()
	changesMu.Lock()
	changesCache = map[string]changesEntry{}
	changesMu.Unlock()
}

func JiraIssues(force bool) ([]JiraIssue, error) {
	jiraMu.Lock()
	if !force && time.Since(jiraCache.saved) < 5*time.Minute && jiraCache.saved.Unix() > 0 {
		defer jiraMu.Unlock()
		return jiraCache.issues, jiraCache.err
	}
	jiraMu.Unlock()

	issues, err := fetchJira()

	jiraMu.Lock()
	jiraCache = jiraCacheEntry{issues: issues, saved: time.Now(), err: err}
	jiraMu.Unlock()
	return issues, err
}

func fetchJira() ([]JiraIssue, error) {
	env := config.MCPEnv("jira")
	site := env["ATLASSIAN_SITE_NAME"]
	email := env["ATLASSIAN_USER_EMAIL"]
	token := env["ATLASSIAN_API_TOKEN"]
	if site == "" || email == "" || token == "" {
		return nil, fmt.Errorf("faltan credenciales de Jira en .mcp.json")
	}
	base := site
	if !strings.HasPrefix(base, "http") {
		base = "https://" + site + ".atlassian.net"
	}

	sprintField := sprintFieldID(base, email, token)
	fields := "summary,status,priority,updated"
	if sprintField != "" {
		fields += "," + sprintField
	}
	query := url.Values{}
	query.Set("jql", config.JiraJQL)
	query.Set("maxResults", "50")
	query.Set("fields", fields)

	endpoint := base + "/rest/api/3/search/jql?" + query.Encode()
	body, err := jiraGet(endpoint, email, token)
	if err != nil {
		// Instancias viejas siguen exponiendo el endpoint anterior.
		body, err = jiraGet(base+"/rest/api/3/search?"+query.Encode(), email, token)
		if err != nil {
			return nil, err
		}
	}

	var parsed jiraResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("respuesta de Jira ilegible: %w", err)
	}
	sprints := parseSprints(body, sprintField)

	out := make([]JiraIssue, 0, len(parsed.Issues))
	for _, issue := range parsed.Issues {
		item := JiraIssue{
			Key:       issue.Key,
			Summary:   issue.Fields.Summary,
			Status:    issue.Fields.Status.Name,
			StatusCat: issue.Fields.Status.StatusCategory.Key,
			Priority:  issue.Fields.Priority.Name,
			Updated:   issue.Fields.Updated,
			URL:       base + "/browse/" + issue.Key,
		}
		if sprint, ok := sprints[issue.Key]; ok {
			item.Sprint, item.SprintState = sprint[0], sprint[1]
		}
		out = append(out, item)
	}
	return out, nil
}

// parseSprints saca el sprint de cada issue del cuerpo crudo: el campo es
// dinamico (customfield_XXXXX) asi que se reparsea generico. Si el ticket
// arrastra varios sprints, gana el activo; si no, el ultimo.
func parseSprints(body []byte, sprintField string) map[string][2]string {
	out := map[string][2]string{}
	if sprintField == "" {
		return out
	}
	var generic struct {
		Issues []struct {
			Key    string                     `json:"key"`
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"issues"`
	}
	if json.Unmarshal(body, &generic) != nil {
		return out
	}
	for _, issue := range generic.Issues {
		raw, ok := issue.Fields[sprintField]
		if !ok || len(raw) == 0 {
			continue
		}
		var list []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
			continue
		}
		pick := list[len(list)-1]
		for _, sprint := range list {
			if sprint.State == "active" {
				pick = sprint
				break
			}
		}
		out[issue.Key] = [2]string{pick.Name, pick.State}
	}
	return out
}

// --- Detalle de un ticket ---------------------------------------------------

type JiraComment struct {
	Author  string `json:"author"`
	Created string `json:"created"`
	Text    string `json:"text"`
}

type JiraDetail struct {
	Key         string        `json:"key"`
	Summary     string        `json:"summary"`
	Status      string        `json:"status"`
	StatusCat   string        `json:"status_cat"`
	Priority    string        `json:"priority"`
	Assignee    string        `json:"assignee"`
	Updated     string        `json:"updated"`
	URL         string        `json:"url"`
	Labels      []string      `json:"labels"`
	Description string        `json:"description"`
	Comments    []JiraComment `json:"comments"`
}

var jiraKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)

// JiraIssueDetail trae un ticket completo y legible: la descripcion y los
// comentarios (formato ADF de Atlassian) se aplanan a texto plano.
func JiraIssueDetail(key string) (*JiraDetail, error) {
	if !jiraKeyRe.MatchString(key) {
		return nil, fmt.Errorf("ticket invalido: %s", key)
	}
	env := config.MCPEnv("jira")
	site := env["ATLASSIAN_SITE_NAME"]
	email := env["ATLASSIAN_USER_EMAIL"]
	token := env["ATLASSIAN_API_TOKEN"]
	if site == "" || email == "" || token == "" {
		return nil, fmt.Errorf("faltan credenciales de Jira en .mcp.json")
	}
	base := site
	if !strings.HasPrefix(base, "http") {
		base = "https://" + site + ".atlassian.net"
	}

	endpoint := base + "/rest/api/3/issue/" + key +
		"?fields=summary,description,status,priority,assignee,updated,labels,comment"
	body, err := jiraGet(endpoint, email, token)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Fields struct {
			Summary     string          `json:"summary"`
			Description json.RawMessage `json:"description"`
			Updated     string          `json:"updated"`
			Labels      []string        `json:"labels"`
			Status      struct {
				Name           string `json:"name"`
				StatusCategory struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"status"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
			Assignee struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Comment struct {
				Comments []struct {
					Created string `json:"created"`
					Author  struct {
						DisplayName string `json:"displayName"`
					} `json:"author"`
					Body json.RawMessage `json:"body"`
				} `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("respuesta de Jira ilegible: %w", err)
	}

	detail := &JiraDetail{
		Key:         key,
		Summary:     parsed.Fields.Summary,
		Status:      parsed.Fields.Status.Name,
		StatusCat:   parsed.Fields.Status.StatusCategory.Key,
		Priority:    parsed.Fields.Priority.Name,
		Assignee:    parsed.Fields.Assignee.DisplayName,
		Updated:     parsed.Fields.Updated,
		Labels:      parsed.Fields.Labels,
		URL:         base + "/browse/" + key,
		Description: adfToHTML(parsed.Fields.Description),
	}
	for _, comment := range parsed.Fields.Comment.Comments {
		detail.Comments = append(detail.Comments, JiraComment{
			Author:  comment.Author.DisplayName,
			Created: comment.Created,
			Text:    adfToHTML(comment.Body),
		})
	}
	return detail, nil
}

// adfNode es el arbol del Atlassian Document Format.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
	Attrs   struct {
		Text string `json:"text"` // menciones, emojis y status traen su texto aqui
	} `json:"attrs"`
	Marks []struct {
		Type string `json:"type"`
	} `json:"marks"`
}

// adfToHTML convierte ADF a HTML simple y seguro (todo el texto escapado)
// para que el ticket se lea como en Jira: titulos, negritas, listas, tablas
// y bloques de codigo.
func adfToHTML(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var root adfNode
	if err := json.Unmarshal(raw, &root); err != nil {
		// Instancias viejas devuelven texto plano directamente.
		var plain string
		if json.Unmarshal(raw, &plain) == nil {
			return "<p>" + html.EscapeString(plain) + "</p>"
		}
		return ""
	}
	var out strings.Builder
	var walk func(node adfNode)
	children := func(node adfNode) {
		for _, child := range node.Content {
			walk(child)
		}
	}
	wrap := func(node adfNode, open, close string) {
		out.WriteString(open)
		children(node)
		out.WriteString(close)
	}
	walk = func(node adfNode) {
		switch node.Type {
		case "text":
			text := html.EscapeString(node.Text)
			for _, mark := range node.Marks {
				switch mark.Type {
				case "strong":
					text = "<b>" + text + "</b>"
				case "em":
					text = "<i>" + text + "</i>"
				case "code":
					text = "<code>" + text + "</code>"
				case "underline":
					text = "<u>" + text + "</u>"
				case "strike":
					text = "<s>" + text + "</s>"
				}
			}
			out.WriteString(text)
		case "hardBreak":
			out.WriteString("<br>")
		case "mention", "emoji", "status":
			out.WriteString(`<span class="mnt">` + html.EscapeString(node.Attrs.Text) + `</span>`)
		case "paragraph":
			wrap(node, "<p>", "</p>")
		case "heading":
			wrap(node, "<h4>", "</h4>")
		case "bulletList":
			wrap(node, "<ul>", "</ul>")
		case "orderedList":
			wrap(node, "<ol>", "</ol>")
		case "listItem":
			wrap(node, "<li>", "</li>")
		case "codeBlock":
			wrap(node, "<pre>", "</pre>")
		case "blockquote":
			wrap(node, "<blockquote>", "</blockquote>")
		case "rule":
			out.WriteString("<hr>")
		case "table":
			wrap(node, "<table>", "</table>")
		case "tableRow":
			wrap(node, "<tr>", "</tr>")
		case "tableHeader":
			wrap(node, "<th>", "</th>")
		case "tableCell":
			wrap(node, "<td>", "</td>")
		case "mediaGroup", "mediaSingle", "media":
			out.WriteString(`<span class="mnt">(adjunto)</span>`)
			// sin recorrer hijos: los nodos media no traen texto util
		default:
			children(node)
		}
	}
	walk(root)
	return out.String()
}

func jiraGet(endpoint, email, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Jira respondio %d", resp.StatusCode)
	}
	return body, nil
}
