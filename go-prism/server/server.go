package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brianvoe/gofakeit/v6"
	"github.com/fsnotify/fsnotify"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
)

// Options defines server configuration
type Options struct {
	Port           int
	Dynamic        bool
	IgnoreExamples bool
	FillProperties bool
	Watch          bool
}

// Server represents mock server
type Server struct {
	specPath string
	opts     Options

	mu   sync.RWMutex
	doc  *openapi3.T
	rtr  routers.Router
	srv  *http.Server
	stop chan struct{}
}

// New creates server instance
func New(spec string, opts Options) (*Server, error) {
	s := &Server{specPath: spec, opts: opts, stop: make(chan struct{})}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) load() error {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile(s.specPath)
	if err != nil {
		return err
	}
	if err := doc.Validate(context.Background()); err != nil {
		return err
	}

	if ext, ok := doc.Extensions["x-json-schema-faker"]; ok {
		if m, ok := ext.(map[string]any); ok {
			if fill, ok := m["fillProperties"].(bool); ok {
				s.opts.FillProperties = fill
			}
		}
	}
	rtr, err := legacyrouter.NewRouter(doc)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.doc = doc
	s.rtr = rtr
	s.mu.Unlock()
	return nil
}

// Start runs the http server
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = &http.Server{Addr: fmt.Sprintf(":%d", s.opts.Port), Handler: mux}

	if s.opts.Watch {
		go s.watch()
	}

	go func() {
		if err := s.srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
		}
	}()

	<-s.stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher error: %v", err)
		return
	}
	defer watcher.Close()

	file := s.specPath
	dir := filepath.Dir(file)
	watcher.Add(dir)

	for {
		select {
		case ev := <-watcher.Events:
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 && filepath.Base(ev.Name) == filepath.Base(file) {
				log.Println("reloading spec")
				if err := s.load(); err != nil {
					log.Printf("reload error: %v", err)
				}
			}
		case err := <-watcher.Errors:
			log.Printf("watcher error: %v", err)
		case <-s.stop:
			return
		}
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rtr := s.rtr
	s.mu.RUnlock()

	route, pathParams, err := rtr.FindRoute(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	input := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route:      route,
	}
	if err := openapi3filter.ValidateRequest(r.Context(), input); err != nil {
		code := http.StatusUnprocessableEntity
		if route.Operation.Responses.Status(code) == nil && route.Operation.Responses.Status(http.StatusBadRequest) != nil {
			code = http.StatusBadRequest
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	status, body := s.generate(route.Operation, r)
	if route.Operation.Deprecated {
		w.Header().Set("Deprecation", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (s *Server) generate(op *openapi3.Operation, r *http.Request) (int, any) {
	prefer := parsePrefer(r.Header.Get("Prefer"))

	dynamic := s.opts.Dynamic
	if val, ok := prefer["dynamic"]; ok {
		dynamic = val == "true"
	}

	code := 200
	if val, ok := prefer["code"]; ok {
		if c, err := strconv.Atoi(val); err == nil {
			code = c
		}
	}

	var resp *openapi3.Response
	if res := op.Responses.Status(code); res != nil {
		resp = res.Value
	} else {
		for k, v := range op.Responses.Map() {
			if i, err := strconv.Atoi(k); err == nil {
				code = i
			}
			resp = v.Value
			break
		}
	}
	if resp == nil {
		return http.StatusNotImplemented, nil
	}

	mt := negotiate(resp.Content, r.Header.Get("Accept"))
	if mt == nil {
		return code, nil
	}

	// choose example
	if !dynamic && !s.opts.IgnoreExamples {
		if key, ok := prefer["example"]; ok {
			if ex, ok := mt.Examples[key]; ok {
				return code, ex.Value.Value
			}
		}
		if mt.Example != nil {
			return code, mt.Example
		}
		for _, ex := range mt.Examples {
			return code, ex.Value.Value
		}
	}

	return code, generateFromSchema(mt.Schema, dynamic, s.opts.FillProperties)
}

func negotiate(content openapi3.Content, accept string) *openapi3.MediaType {
	if mt, ok := content[accept]; ok {
		return mt
	}
	if mt, ok := content["application/json"]; ok {
		return mt
	}
	for _, mt := range content {
		return mt
	}
	return nil
}

type schemaOpts struct {
	dynamic bool
	fill    bool
}

func generateFromSchema(ref *openapi3.SchemaRef, dynamic bool, fill bool) any {
	if ref == nil || ref.Value == nil {
		return nil
	}
	s := ref.Value

	if dynamic {
		if faker, ok := s.Extensions["x-faker"]; ok {
			switch v := faker.(type) {
			case string:
				if val, err := callFaker(v, nil); err == nil {
					return val
				}
			case map[string]any:
				for name, arg := range v {
					if val, err := callFaker(name, arg); err == nil {
						return val
					}
				}
			}
		}
	}

	if !dynamic && s.Example != nil {
		return s.Example
	}
	if !dynamic && s.Default != nil {
		return s.Default
	}

	var t string
	if s.Type != nil && len(*s.Type) > 0 {
		t = (*s.Type)[0]
	}

	switch t {
	case openapi3.TypeString:
		switch s.Format {
		case "email":
			return gofakeit.Email()
		case "uuid":
			return gofakeit.UUID()
		default:
			return gofakeit.Word()
		}
	case openapi3.TypeInteger:
		return gofakeit.Int64()
	case openapi3.TypeNumber:
		return gofakeit.Float64()
	case openapi3.TypeBoolean:
		return gofakeit.Bool()
	case openapi3.TypeArray:
		if s.Items != nil {
			return []any{generateFromSchema(s.Items, dynamic, fill)}
		}
		return []any{}
	case openapi3.TypeObject:
		obj := map[string]any{}
		for name, prop := range s.Properties {
			required := false
			for _, r := range s.Required {
				if r == name {
					required = true
					break
				}
			}
			if !fill && !required {
				continue
			}
			obj[name] = generateFromSchema(prop, dynamic, fill)
		}
		return obj
	default:
		return nil
	}
}

func callFaker(name string, arg any) (any, error) {
	lookup := strings.ReplaceAll(strings.ToLower(name), ".", "")
	info := gofakeit.GetFuncLookup(lookup)
	if info == nil {
		return nil, errors.New("unknown faker")
	}
	params := gofakeit.MapParams{}
	switch v := arg.(type) {
	case map[string]any:
		for k, val := range v {
			params.Add(k, fmt.Sprintf("%v", val))
		}
	case []any:
		for i, val := range v {
			params.Add(fmt.Sprintf("%d", i), fmt.Sprintf("%v", val))
		}
	case string:
		params.Add("0", v)
	case nil:
	default:
		params.Add("0", fmt.Sprintf("%v", v))
	}
	faker := gofakeit.New(0)
	val, err := info.Generate(faker.Rand, &params, info)
	if err != nil {
		return nil, err
	}
	return val, nil
}

func parsePrefer(header string) map[string]string {
	res := map[string]string{}
	parts := strings.Split(header, ",")
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			res[strings.ToLower(kv[0])] = strings.Trim(kv[1], "\"")
		}
	}
	return res
}
