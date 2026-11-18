package hrender

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func Test_RenderW(t *testing.T) {
	tests := []struct {
		name          string
		mockFS        fstest.MapFS
		data          H
		page          string
		layout        string
		customFuncs   template.FuncMap
		expectedError bool
		expectedBody  string
	}{
		{
			name: "simple page",
			mockFS: fstest.MapFS{
				"index.html": {
					Data: []byte(`<h1>hello world</h1>`),
				},
			},
			page:         "index.html",
			expectedBody: "<h1>hello world</h1>",
		},
		{
			name: "simple page with layout",
			mockFS: fstest.MapFS{
				"index.html": {
					Data: []byte(`<h1>hello world</h1>`),
				},
				"main-layout.html": {
					Data: []byte(`<main>{{ embed }}</main>`),
				},
			},
			page:         "index.html",
			layout:       "main-layout.html",
			expectedBody: "<main><h1>hello world</h1></main>",
		},
		{
			name: "simple page with fragment",
			mockFS: fstest.MapFS{
				"index.html": {
					Data: []byte(`<h1>{{ template "fragments/header" . }}</h1>`),
				},
				"fragments/header.html": {
					Data: []byte(`Hello Header`),
				},
			},
			page:         "index.html",
			expectedBody: "<h1>Hello Header</h1>",
		},
		{
			name: "simple page with layout and fragment",
			mockFS: fstest.MapFS{
				"layout.html": {
					Data: []byte(`<body>{{ embed }}</body>`),
				},
				"index.html": {
					Data: []byte(`<div>{{ template "fragments/footer" . }}</div>`),
				},
				"fragments/footer.html": {
					Data: []byte(`Footer Content`),
				},
			},
			page:         "index.html",
			layout:       "layout.html",
			expectedBody: "<body><div>Footer Content</div></body>",
		},
		{
			name: "simple page with Dict function and fragment",
			mockFS: fstest.MapFS{
				"index.html": {
					Data: []byte(`<button {{ template "fragments/button_attrs" (dict "ID" "myButton" "Class" "btn btn-primary") }}>Click Me</button>`),
				},
				"fragments/button_attrs.html": {
					Data: []byte(`id="{{.ID}}" class="{{.Class}}"`),
				},
			},
			page:         "index.html",
			expectedBody: `<button id="myButton" class="btn btn-primary">Click Me</button>`,
		},
		{
			name: "auto-append .html to page",
			mockFS: fstest.MapFS{
				"about.html": {
					Data: []byte(`<h1>About</h1>`),
				},
			},
			page:         "about",
			expectedBody: "<h1>About</h1>",
		},
		{
			name: "auto-append .html to layout",
			mockFS: fstest.MapFS{
				"contact.html": {
					Data: []byte(`<p>Contact</p>`),
				},
				"base.html": {
					Data: []byte(`<div>{{ embed }}</div>`),
				},
			},
			page:         "contact.html",
			layout:       "base",
			expectedBody: "<div><p>Contact</p></div>",
		},
		{
			name: "error page not found",
			mockFS: fstest.MapFS{
				"index.html": {
					Data: []byte(`<h1>Exists</h1>`),
				},
			},
			page:          "missing.html",
			expectedError: true,
			expectedBody:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewHTMLRender(tt.mockFS, false)

			for name, fn := range tt.customFuncs {
				r.AddFunc(name, fn)
			}

			rec := httptest.NewRecorder()

			var layouts []string
			if tt.layout != "" {
				layouts = append(layouts, tt.layout)
			}

			err := r.RenderW(rec, 200, tt.page, tt.data, layouts...)
			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if got := strings.TrimSpace(rec.Body.String()); got != tt.expectedBody {
					t.Errorf("expected body %q, got %q", tt.expectedBody, got)
				}
			}
		})
	}
}

func Test_RenderS(t *testing.T) {
	mockFS := fstest.MapFS{
		"index.html": {
			Data: []byte(`<h1>{{ .Title }}</h1>`),
		},
	}

	r := NewHTMLRender(mockFS, false)
	data := H{"Title": "Render String"}

	got, err := r.RenderS("index.html", data)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	expected := "<h1>Render String</h1>"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func Test_SharedTemplatesIsolation(t *testing.T) {
	// This test ensures that ONLY components/ and fragments/ are loaded into the base template
	mockFS := fstest.MapFS{
		"pages/home.html": {
			Data: []byte(`
				{{ template "components/btn" . }}
				{{ template "fragments/tbl" . }}
				{{ template "other/ignored" . }}
			`),
		},
		"components/btn.html": {
			Data: []byte(`[BUTTON]`),
		},
		"fragments/tbl.html": {
			Data: []byte(`[TABLE]`),
		},
		"other/ignored.html": {
			Data: []byte(`[IGNORED]`),
		},
	}

	r := NewHTMLRender(mockFS, false)

	// Attempt to render home. It should fail because "other/ignored" is not available in the base template,
	// and "pages/home.html" only tries to invoke it, it doesn't define it.
	err := r.RenderW(httptest.NewRecorder(), 200, "pages/home.html", nil)
	if err == nil {
		t.Fatal("expected error due to missing 'other/ignored' template, but got nil")
	}

	// Now a valid test case checking correct loading of components and fragments
	mockFSValid := fstest.MapFS{
		"pages/valid.html": {
			Data: []byte(`{{ template "components/btn" . }} - {{ template "fragments/tbl" . }}`),
		},
		"components/btn.html": {
			Data: []byte(`[BUTTON]`),
		},
		"fragments/tbl.html": {
			Data: []byte(`[TABLE]`),
		},
	}

	r2 := NewHTMLRender(mockFSValid, false)
	rec := httptest.NewRecorder()
	err = r2.RenderW(rec, 200, "pages/valid.html", nil)
	if err != nil {
		t.Fatalf("unexpected error rendering valid templates: %v", err)
	}

	expected := "[BUTTON] - [TABLE]"
	if got := rec.Body.String(); got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func Test_Dict(t *testing.T) {
	tests := []struct {
		name     string
		given    []any
		expected map[string]any
	}{
		{
			name:  "valid - all strings",
			given: []any{"key", "value"},
			expected: map[string]any{
				"key": "value",
			},
		},
		{
			name:  "valid - key string value int",
			given: []any{"age", 15},
			expected: map[string]any{
				"age": 15,
			},
		},
		{
			name:  "error invalid dict call",
			given: []any{"error"},
			expected: map[string]any{
				"error": "invalid dict call",
			},
		},
		{
			name:  "error dict keys must be strings",
			given: []any{1, "error"},
			expected: map[string]any{
				"error": "dict keys must be strings",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Dict(tt.given...)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func Test_Cache(t *testing.T) {
	mockFS := fstest.MapFS{
		"index.html": {
			Data: []byte(`<h1>Hello Cached World</h1>`),
		},
		"main-layout.html": {
			Data: []byte(`<main>{{ embed }}</main>`),
		},
	}

	var buf bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(originalLogger)

	r := NewHTMLRender(mockFS, true)

	rec1 := httptest.NewRecorder()
	err1 := r.RenderW(rec1, 200, "index.html", nil, "main-layout.html")
	if err1 != nil {
		t.Errorf("unexpected error: %v", err1)
	}
	expected := "<main><h1>Hello Cached World</h1></main>"
	if got := strings.TrimSpace(rec1.Body.String()); got != expected {
		t.Errorf("expected body %q, got %q", expected, got)
	}

	if !strings.Contains(buf.String(), "template compiled and cached") {
		t.Error("expected 'template compiled and cached' message on first render")
	}

	buf.Reset()

	rec2 := httptest.NewRecorder()
	err2 := r.RenderW(rec2, 200, "index.html", nil, "main-layout.html")
	if err2 != nil {
		t.Errorf("unexpected error: %v", err2)
	}
	if got := strings.TrimSpace(rec2.Body.String()); got != expected {
		t.Errorf("expected body %q, got %q", expected, got)
	}

	if strings.Contains(buf.String(), "template compiled and cached") {
		t.Error("did not expect 'template compiled and cached' message on second render")
	}
}

func Test_IncludeWithCacheKeepsBaseClonable(t *testing.T) {
	mockFS := fstest.MapFS{
		"components/icon.html":    {Data: []byte(`<i></i>`)},
		"pages/with-include.html": {Data: []byte(`{{ include "components/icon" nil }}`)},
		"pages/other.html":        {Data: []byte(`<p>other</p>`)},
	}
	r := NewHTMLRender(mockFS, true)

	if _, err := r.RenderS("pages/with-include", nil); err != nil {
		t.Fatalf("rendering the page with include: %v", err)
	}
	got, err := r.RenderS("pages/other", nil)
	if err != nil {
		t.Fatalf("rendering a page after an include: %v", err)
	}
	if got != "<p>other</p>" {
		t.Errorf("got %q, want %q", got, "<p>other</p>")
	}
}
