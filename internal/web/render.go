package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// renderer holds the parsed page templates.
//
// Each page is parsed together with the layout and any partials, so a page has
// exactly the templates it needs and a name collision between pages is
// impossible.
type renderer struct {
	mu    sync.RWMutex
	pages map[string]*template.Template
	dev   bool
}

func newRenderer(dev bool) (*renderer, error) {
	r := &renderer{pages: map[string]*template.Template{}, dev: dev}
	if err := r.parse(); err != nil {
		return nil, err
	}
	return r, nil
}

// parse builds one template set per page.
func (r *renderer) parse() error {
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("web: glob templates: %w", err)
	}

	const layout = "templates/layout.html"

	var shared []string
	var pages []string
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		switch {
		case e == layout:
			continue
		case strings.HasPrefix(base, "_"):
			shared = append(shared, e)
		default:
			pages = append(pages, e)
		}
	}

	parsed := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		files := append([]string{layout}, shared...)
		files = append(files, page)

		t, err := template.New("layout.html").Funcs(templateFuncs()).ParseFS(templateFS, files...)
		if err != nil {
			return fmt.Errorf("web: parse template %s: %w", page, err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(page, "templates/"), ".html")
		parsed[name] = t
	}

	r.mu.Lock()
	r.pages = parsed
	r.mu.Unlock()
	return nil
}

// render writes a page. In dev mode templates are reparsed first so edits show
// up without a restart.
func (r *renderer) render(w http.ResponseWriter, status int, page string, data any) {
	if r.dev {
		if err := r.parse(); err != nil {
			slog.Error("template reparse failed", "err", err)
		}
	}

	r.mu.RLock()
	t, ok := r.pages[page]
	r.mu.RUnlock()

	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Render into a buffer first: a template error midway through would
	// otherwise emit a half-written page with a 200 already committed.
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		slog.Error("template render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		slog.Debug("write response failed", "page", page, "err", err)
	}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// hasPrefix drives active-tab highlighting in the nav.
		"hasPrefix": strings.HasPrefix,
	}
}
