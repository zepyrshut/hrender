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
	r := &HRender{
		fs:          fileSystem,
		enableCache: enableCache,
		cache:       make(map[string]*template.Template),
	}

	r.funcMap = template.FuncMap{
		"dict":    Dict,
		"list":    List,
		"merge":   Merge,
		"include": r.Include,
		"attrs":   Attrs,
	}

	return r
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
// status: The `http status` to write header in r.execute.
// pageName: The base name of the page template to render (e.g., "pages/login.html").
// data: A map of data (`H`) to be passed into the template for dynamic content generation.
// layoutName: (Optional) The name of the layout template to wrap the page content (e.g., "layouts/base.html").
//
//	The page content will be embedded where `{{ embed }}` or `{{embed}}` is found in the layout.
func (r *HRender) RenderW(w http.ResponseWriter, status int, pageName string, data any, layoutName ...string) error {
	tmpl, err := r.getTemplateInstance(pageName, layoutName...)
	if err != nil {
		return err
	}

	return r.execute(w, status, tmpl, data)
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
func (r *HRender) RenderS(pageName string, data any, layoutName ...string) (string, error) {
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
// status: The `http status` to to write headers to.
// tmpl: The `*template.Template` instance to execute.
// data: The data map (`H`) to pass to the template.
//
// Returns:
//
//	An error if template execution or writing to the response writer fails.
func (r *HRender) execute(w http.ResponseWriter, status int, tmpl *template.Template, data any) error {
	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return err // Si falla, NO hemos tocado 'w'. Podemos mandar un 500 arriba.
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	_, err := buf.WriteTo(w)
	return err
}

// parsePageAndLayoutIntoTemplate reads the page and optional layout content and parses them
// into the provided template instance.
//
// This helper function abstracts the file reading and template parsing logic for pages and layouts.
//
// tmpl: The target `*template.Template` instance where the content will be parsed.
// pageName: The filename of the page template.
// layoutName: The filename of the layout template (can be empty).
//
// Returns:
//
//	An error if reading the files or parsing the content fails.
func (r *HRender) parsePageAndLayoutIntoTemplate(tmpl *template.Template, pageName, layoutName string) error {
	pageContent, err := fs.ReadFile(r.fs, pageName)
	if err != nil {
		return fmt.Errorf("page not found: %w", err)
	}

	var finalContent string

	if layoutName != "" {
		layoutBytes, err := fs.ReadFile(r.fs, layoutName)
		if err != nil {
			return fmt.Errorf("layout not found: %w", err)
		}

		layoutStr := string(layoutBytes)
		layoutStr = strings.ReplaceAll(layoutStr, "{{ embed }}", string(pageContent))
		layoutStr = strings.ReplaceAll(layoutStr, "{{embed}}", string(pageContent))

		finalContent = layoutStr
	} else {
		finalContent = string(pageContent)
	}

	if _, err := tmpl.Parse(finalContent); err != nil {
		return fmt.Errorf("error parsing main content: %w", err)
	}
	return nil
}

// buildTemplate constructs a new template instance for the specified page and layout.
//
// If caching is enabled (`enableCache` is true), it clones a pre-loaded base template
// (containing shared components) and parses the page/layout into the clone.
// If caching is disabled, it creates a fresh template instance, parses all shared components,
// and then parses the page/layout.
//
// pageName: The filename of the page template.
// layoutName: The filename of the layout template.
//
// Returns:
//
//	A pointer to the constructed `*template.Template`.
//	An error if any part of the template loading or parsing fails.
func (r *HRender) buildTemplate(pageName, layoutName string) (*template.Template, error) {
	if !r.enableCache {
		tmpl := template.New("root").Funcs(r.funcMap)
		if err := r.parseSharedTemplatesInto(tmpl); err != nil {
			return nil, fmt.Errorf("failed to parse shared templates for non-cached template: %w", err)
		}
		if err := r.parsePageAndLayoutIntoTemplate(tmpl, pageName, layoutName); err != nil {
			return nil, err
		}
		return tmpl, nil
	}

	var initErr error
	r.baseOnce.Do(func() {
		initErr = r.loadBaseTemplate()
	})
	if initErr != nil {
		return nil, fmt.Errorf("failed to load shared templates into base: %w", initErr)
	}

	tmpl, err := r.baseTmpl.Clone() // This is the line user wants to disable when enableCache is false
	if err != nil {
		return nil, fmt.Errorf("failed to clone base template: %w", err)
	}

	if err := r.parsePageAndLayoutIntoTemplate(tmpl, pageName, layoutName); err != nil {
		return nil, err
	}

	return tmpl, nil
}

// parseSharedTemplatesInto scans the file system for shared templates and parses them.
//
// It looks for templates in "components/" and "fragments/" directories and parses them
// into the provided template instance.
//
// tmpl: The `*template.Template` instance to populate with shared templates.
//
// Returns:
//
//	An error if walking the directory or parsing a template fails.
func (r *HRender) parseSharedTemplatesInto(tmpl *template.Template) error {
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

		_, err = tmpl.New(name).Parse(string(content))
		return err
	})
	if err != nil {
		return fmt.Errorf("error loading shared templates: %w", err)
	}

	return nil
}

// loadBaseTemplate initializes the base template with shared components.
//
// It creates a new "root" template, registers the function map, and populates it
// with all shared templates found in the file system. This is typically used
// to initialize the cached base template.
//
// Returns:
//
//	An error if loading the shared templates fails.
func (r *HRender) loadBaseTemplate() error {
	r.baseTmpl = template.New("root").Funcs(r.funcMap)
	return r.parseSharedTemplatesInto(r.baseTmpl)
}

func (r *HRender) Include(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	var err error

	if r.enableCache {
		r.baseOnce.Do(func() { _ = r.loadBaseTemplate() })

		if r.baseTmpl == nil {
			return "", fmt.Errorf("base template not initialized")
		}

		err = r.baseTmpl.ExecuteTemplate(&buf, name, data)
	} else {
		t := template.New("temp_include").Funcs(r.funcMap)

		if parseErr := r.parseSharedTemplatesInto(t); parseErr != nil {
			return "", fmt.Errorf("failed to parse shared templates for include: %w", parseErr)
		}

		err = t.ExecuteTemplate(&buf, name, data)
	}

	if err != nil {
		return "", fmt.Errorf("include error (%s): %w", name, err)
	}

	return template.HTML(buf.String()), nil
}

func List(values ...any) []any {
	return values
}

func Merge(base any, overlays ...map[string]any) map[string]any {
	out := make(map[string]any)

	if baseMap, ok := base.(map[string]any); ok {
		for k, v := range baseMap {
			out[k] = v
		}
	} else if baseH, ok := base.(H); ok { // Soporte para tu tipo H
		for k, v := range baseH {
			out[k] = v
		}
	}

	for _, overlay := range overlays {
		for k, v := range overlay {
			out[k] = v
		}
	}
	return out
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

func Attrs(m map[string]string) template.HTMLAttr {
	var sb strings.Builder
	for k, v := range m {
		if v != "" {
			sb.WriteString(fmt.Sprintf(` %s="%s"`, k, v))
		}
	}
	return template.HTMLAttr(sb.String())
}
