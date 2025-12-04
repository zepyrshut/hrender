package hrender

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

type (
	H map[string]any

	HRender struct {
		fs          fs.FS
		enableCache bool
		funcMap     template.FuncMap
		cache       map[string]*template.Template
		cacheMu     sync.RWMutex
	}
)

const (
	ContentType         string = "Content-Type"
	ContentTextHTMLUTF8 string = "text/html; charset=utf-8"
)

// NewHTMLRender initializes a new Render instance.
// fileSystem: The file system containing the templates (usually embed.FS or os.DirFS).
// enableCache: If true, templates will be compiled once and cached for future use.
func NewHTMLRender(fileSystem fs.FS, enableCache bool) *HRender {
	return &HRender{
		fs:          fileSystem,
		enableCache: enableCache,
		cache:       make(map[string]*template.Template),
		funcMap: template.FuncMap{
			"dict": Dict,
		},
	}
}

// AddFunc registers a custom function to the template's FuncMap.
// This must be called before rendering any templates if you want the function to be available.
func (r *HRender) AddFunc(name string, fn any) {
	r.funcMap[name] = fn
}

// Render executes the template for the given pageName and writes the result to w.
// pageName: The path to the page template (e.g., "pages/login.html").
// data: The data map to be passed to the template.
// layoutName: (Optional) The path to the layout template (e.g., "layouts/base.html").
//
// If caching is enabled, it checks the cache first. If not found (or cache disabled),
// it compiles the template (merging layout and page) and caches it if appropriate.
func (r *HRender) Render(w http.ResponseWriter, pageName string, data H, layoutName ...string) error {
	if !strings.HasSuffix(pageName, ".html") {
		pageName += ".html"
	}

	var layout string
	if len(layoutName) > 0 && layoutName[0] != "" {
		layout = layoutName[0]
		if !strings.HasSuffix(layout, ".html") {
			layout += ".html"
		}
	}

	cacheKey := pageName
	if layout != "" {
		cacheKey = layout + "::" + pageName
	}

	var tmpl *template.Template
	var err error

	if r.enableCache {
		r.cacheMu.RLock()
		cachedTmpl, ok := r.cache[cacheKey]
		r.cacheMu.RUnlock()
		if ok {
			return r.execute(w, cachedTmpl, data)
		}

		r.cacheMu.Lock()
		if cachedTmpl, ok = r.cache[cacheKey]; ok {
			r.cacheMu.Unlock()
			return r.execute(w, cachedTmpl, data)
		}

		tmpl, err = r.buildTemplate(pageName, layout)
		if err != nil {
			r.cacheMu.Unlock()
			return err
		}

		r.cache[cacheKey] = tmpl
		slog.Debug("template compiled and cached", "key", cacheKey)
		r.cacheMu.Unlock()

	} else {
		tmpl, err = r.buildTemplate(pageName, layout)
		if err != nil {
			return err
		}
	}

	return r.execute(w, tmpl, data)
}

// execute runs the template into a buffer first to catch any execution errors
// before writing to the response writer. This prevents partial writes on error.
func (r *HRender) execute(w http.ResponseWriter, tmpl *template.Template, data H) error {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// buildTemplate constructs a new template instance by:
// 1. Reading the page content.
// 2. Reading the layout content (if provided) and embedding the page content into it using {{ embed }}.
// 3. Walking the file system to parse all other available templates (fragments/components) so they can be invoked.
func (r *HRender) buildTemplate(pageName, layoutName string) (*template.Template, error) {
	pageContent, err := fs.ReadFile(r.fs, pageName)
	if err != nil {
		return nil, fmt.Errorf("page not found: %w", err)
	}

	var finalContent string

	if layoutName != "" {
		layoutBytes, err := fs.ReadFile(r.fs, layoutName)
		if err != nil {
			return nil, fmt.Errorf("layout not found: %w", err)
		}

		layoutStr := string(layoutBytes)

		layoutStr = strings.ReplaceAll(layoutStr, "{{ embed }}", string(pageContent))
		layoutStr = strings.ReplaceAll(layoutStr, "{{embed}}", string(pageContent))

		finalContent = layoutStr
	} else {
		finalContent = string(pageContent)
	}

	tmpl := template.New("root").Funcs(r.funcMap)

	if _, err := tmpl.Parse(finalContent); err != nil {
		return nil, fmt.Errorf("error parsing main content: %w", err)
	}

	err = fs.WalkDir(r.fs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".html") && !strings.HasSuffix(path, ".gohtml") {
			return nil
		}

		if path == pageName || path == layoutName {
			return nil
		}

		content, err := fs.ReadFile(r.fs, path)
		if err != nil {
			return err
		}

		name := filepath.ToSlash(path)
		name = strings.TrimSuffix(name, filepath.Ext(name))

		_, err = tmpl.New(name).Parse(string(content))
		return err
	})
	if err != nil {
		return nil, err
	}

	return tmpl, nil
}

// Dict creates a map[string]any from a list of key-value pairs.
// It expects an even number of arguments, where every odd argument is a string key
// and the following argument is its value.
//
// This is particularly useful for passing multiple pieces of data to a sub-template
// or fragment, as Go templates normally only accept a single pipeline argument.
//
// Usage in template:
//
//	{{ template "fragments/input" dict "Label" "Username" "Value" .User.Name "Type" "text" }}
//
// Returns:
//   - A map[string]any containing the key-value pairs.
//   - A map with an "error" key if the number of arguments is odd or a key is not a string.
func Dict(values ...any) map[string]any {
	if len(values)%2 != 0 {
		return map[string]any{
			"error": "invalid dict call",
		}
	}
	dict := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return map[string]any{
				"error": "dict keys must be strings",
			}
		}
		dict[key] = values[i+1]
	}
	return dict
}
