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
		baseTmpl    *template.Template
		baseOnce    sync.Once
	}
)

const (
	ContentType         string = "Content-Type"
	ContentTextHTMLUTF8 string = "text/html; charset=utf-8"
)

// NewHTMLRender initializes a new HRender instance, setting up the template engine.
// It configures the file system to load templates from, enables or disables caching,
// and registers a default "dict" function for template use.
//
// fileSystem: The `fs.FS` implementation from which templates will be loaded (e.g., `embed.FS` or `os.DirFS`).
// enableCache: If `true`, compiled templates will be stored in an in-memory cache for faster retrieval on subsequent renders.
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

// AddFunc registers a custom function with the template's FuncMap.
// This method must be called before any templates are rendered for the function
// to be available within the templates.
//
// name: The name by which the function will be called within the template.
// fn: The Go function to be registered. Its signature must be compatible with `html/template` expectations.
func (r *HRender) AddFunc(name string, fn any) {
	r.funcMap[name] = fn
}

// RenderW executes the specified template (pageName) with the provided data and writes the
// resulting HTML to the `http.ResponseWriter`. It can optionally apply a layout template.
//
// This function handles template compilation (with caching if enabled) and execution,
// ensuring that any errors during execution do not result in partial responses.
//
// w: The `http.ResponseWriter` to which the rendered HTML will be written.
// pageName: The base name of the page template to render (e.g., "pages/login.html").
// data: A map of data (`H`) to be passed into the template for dynamic content generation.
// layoutName: (Optional) The name of the layout template to wrap the page content (e.g., "layouts/base.html").
//
//	The page content will be embedded where `{{ embed }}` or `{{embed}}` is found in the layout.
func (r *HRender) RenderW(w http.ResponseWriter, pageName string, data H, layoutName ...string) error {
	tmpl, err := r.getTemplateInstance(pageName, layoutName...)
	if err != nil {
		return err
	}

	return r.execute(w, tmpl, data)
}

// RenderS executes the specified template (pageName) with the provided data and
// returns the resulting HTML as a string. It can optionally apply a layout template.
//
// This function is useful for scenarios where the rendered HTML needs to be further processed,
// sent as part of an SSE stream, or returned as a plain string rather than directly written
// to an `http.ResponseWriter`.
//
// pageName: The base name of the page template to render (e.g., "pages/login.html").
// data: A map of data (`H`) to be passed into the template for dynamic content generation.
// layoutName: (Optional) The name of the layout template to wrap the page content (e.g., "layouts/base.html").
//
//	The page content will be embedded where `{{ embed }}` or `{{embed}}` is found in the layout.
//
// Returns:
//
//	A string containing the rendered HTML.
//	An error if template compilation or execution fails.
func (r *HRender) RenderS(pageName string, data H, layoutName ...string) (string, error) {
	tmpl, err := r.getTemplateInstance(pageName, layoutName...)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// getTemplateInstance retrieves a compiled template. It first checks if the template
// is available in the cache (if caching is enabled). If not found or caching is disabled,
// it proceeds to build the template by combining the page and optional layout, and
// parsing all associated fragments. If caching is enabled, the newly built template
// is stored in the cache before being returned.
//
// pageName: The name of the main page template.
// layoutName: (Optional) The name of the layout template.
//
// Returns:
//
//	A pointer to `template.Template` instance ready for execution.
//	An error if template files cannot be read or parsing fails.
func (r *HRender) getTemplateInstance(pageName string, layoutName ...string) (*template.Template, error) {
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
			return cachedTmpl, nil // Retorna si está en caché
		}

		r.cacheMu.Lock()
		defer r.cacheMu.Unlock()
		if cachedTmpl, ok = r.cache[cacheKey]; ok {
			return cachedTmpl, nil
		}

		tmpl, err = r.buildTemplate(pageName, layout)
		if err != nil {
			return nil, err
		}

		r.cache[cacheKey] = tmpl
		slog.Debug("template compiled and cached", "key", cacheKey)
		return tmpl, nil

	} else {
		return r.buildTemplate(pageName, layout)
	}
}

// execute renders the provided template with data into an internal buffer.
// If successful, it writes the buffer's content to the `http.ResponseWriter`.
// This prevents partial HTTP responses in case of template execution errors.
//
// w: The `http.ResponseWriter` to write the rendered content to.
// tmpl: The `*template.Template` instance to execute.
// data: The data map (`H`) to pass to the template.
//
// Returns:
//
//	An error if template execution or writing to the response writer fails.
func (r *HRender) execute(w http.ResponseWriter, tmpl *template.Template, data H) error {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// buildTemplate constructs a new template instance by cloning a pre-loaded base template
// (containing shared components/fragments) and parsing the specific page and layout.
// It ensures shared templates are loaded only once.
func (r *HRender) buildTemplate(pageName, layoutName string) (*template.Template, error) {
	var initErr error
	r.baseOnce.Do(func() {
		initErr = r.loadSharedTemplates()
	})
	if initErr != nil {
		return nil, fmt.Errorf("failed to load shared templates: %w", initErr)
	}

	tmpl, err := r.baseTmpl.Clone()
	if err != nil {
		return nil, fmt.Errorf("failed to clone base template: %w", err)
	}

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

	if _, err := tmpl.Parse(finalContent); err != nil {
		return nil, fmt.Errorf("error parsing main content: %w", err)
	}

	return tmpl, nil
}

// loadSharedTemplates scans the file system for templates in "components/" and "fragments/"
// directories and parses them into a base template instance.
func (r *HRender) loadSharedTemplates() error {
	r.baseTmpl = template.New("root").Funcs(r.funcMap)

	err := fs.WalkDir(r.fs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".html") && !strings.HasSuffix(path, ".gohtml") {
			return nil
		}

		pathSlash := filepath.ToSlash(path)
		if !strings.HasPrefix(pathSlash, "components/") && !strings.HasPrefix(pathSlash, "fragments/") {
			return nil
		}

		content, err := fs.ReadFile(r.fs, path)
		if err != nil {
			return err
		}

		name := pathSlash
		name = strings.TrimSuffix(name, filepath.Ext(name))

		_, err = r.baseTmpl.New(name).Parse(string(content))
		return err
	})
	if err != nil {
		return fmt.Errorf("error loading shared templates: %w", err)
	}

	return nil
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
