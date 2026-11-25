package onboard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CallSite records a source code location flagged or discovered by the AST doctor.
type CallSite struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Func     string `json:"func,omitempty"`
	Symbol   string `json:"symbol"`
	Category string `json:"category,omitempty"`
	Adapter  string `json:"adapter,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// HandlerSite records an HTTP/Kafka/gRPC handler or processor entrypoint.
type HandlerSite struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // http, kafka, grpc, handler
	Route   string `json:"route,omitempty"`
	Method  string `json:"method,omitempty"`
	Topic   string `json:"topic,omitempty"`
}

// DoctorReport is the output of ScanRepo, used by `kavach doctor` and `kavach init`.
type DoctorReport struct {
	Dir              string        `json:"dir"`
	Service          string        `json:"service"`
	Language         string        `json:"language"`
	Module           string        `json:"module,omitempty"`
	Manifests        []string      `json:"manifests,omitempty"`
	FilesScanned     int           `json:"files_scanned"`
	DurationMS       float64       `json:"duration_ms"`
	RecommendedMode  string        `json:"recommended_mode"` // "driver" or "scoped"
	Drivers          []Driver      `json:"drivers"`
	InitSites        []CallSite    `json:"init_sites"`
	ClockCalls       []CallSite    `json:"clock_calls"`
	RandCalls        []CallSite    `json:"rand_calls"`
	DetachedContexts []CallSite    `json:"detached_contexts"`
	Handlers         []HandlerSite `json:"handlers"`
	Topics           []string      `json:"topics,omitempty"`
	Routes           []string      `json:"routes,omitempty"`
	SuggestedSnippet string        `json:"suggested_snippet"`
	Warnings         []string      `json:"warnings,omitempty"`
}

type driverInitMatcher struct {
	pkgPrefixes []string
	funcs       map[string]bool
	driver      string
	adapter     string
	category    string
}

var goDriverInitMatchers = []driverInitMatcher{
	{
		pkgPrefixes: []string{"github.com/jackc/pgx/v5/pgxpool", "github.com/jackc/pgx/v5/stdlib", "github.com/jackc/pgx/v5", "github.com/jackc/pgx"},
		funcs:       map[string]bool{"New": true, "NewWithConfig": true, "Connect": true, "ConnectConfig": true, "OpenDBFromPool": true, "OpenDB": true},
		driver:      "pgx/v5", adapter: "kavachpgx", category: "database",
	},
	{
		pkgPrefixes: []string{"database/sql"},
		funcs:       map[string]bool{"Open": true, "OpenDB": true},
		driver:      "database/sql", adapter: "kavachpgx", category: "database",
	},
	{
		pkgPrefixes: []string{"gorm.io/gorm", "gorm.io/driver/postgres"},
		funcs:       map[string]bool{"Open": true, "New": true},
		driver:      "gorm", adapter: "kavachpgx", category: "database",
	},
	{
		pkgPrefixes: []string{"github.com/redis/go-redis/v9", "github.com/redis/go-redis", "github.com/go-redis/redis"},
		funcs:       map[string]bool{"NewClient": true, "NewUniversalClient": true, "NewClusterClient": true, "NewFailoverClient": true, "NewRing": true, "ParseURL": true},
		driver:      "go-redis/v9", adapter: "kavachredis", category: "cache",
	},
	{
		pkgPrefixes: []string{"github.com/twmb/franz-go/pkg/kgo"},
		funcs:       map[string]bool{"NewClient": true},
		driver:      "franz-go", adapter: "kavachkgo", category: "queue",
	},
	{
		pkgPrefixes: []string{"github.com/segmentio/kafka-go"},
		funcs:       map[string]bool{"NewReader": true, "NewWriter": true, "DialLeader": true},
		driver:      "kafka-go", adapter: "kavachkgo", category: "queue",
	},
	{
		pkgPrefixes: []string{"github.com/IBM/sarama", "github.com/Shopify/sarama"},
		funcs:       map[string]bool{"NewSyncProducer": true, "NewAsyncProducer": true, "NewConsumerGroup": true, "NewClient": true},
		driver:      "sarama", adapter: "kavachkgo", category: "queue",
	},
	{
		pkgPrefixes: []string{"net/http"},
		funcs:       map[string]bool{"ListenAndServe": true, "ListenAndServeTLS": true, "NewServeMux": true, "HandleFunc": true, "Handle": true},
		driver:      "net/http", adapter: "kavachhttp", category: "http",
	},
	{
		pkgPrefixes: []string{"github.com/gin-gonic/gin"},
		funcs:       map[string]bool{"Default": true, "New": true},
		driver:      "gin", adapter: "kavachhttp", category: "http",
	},
	{
		pkgPrefixes: []string{"github.com/go-chi/chi"},
		funcs:       map[string]bool{"NewRouter": true, "NewMux": true},
		driver:      "chi", adapter: "kavachhttp", category: "http",
	},
	{
		pkgPrefixes: []string{"github.com/labstack/echo"},
		funcs:       map[string]bool{"New": true},
		driver:      "echo", adapter: "kavachhttp", category: "http",
	},
	{
		pkgPrefixes: []string{"google.golang.org/grpc"},
		funcs:       map[string]bool{"NewServer": true, "NewClient": true, "Dial": true, "DialContext": true},
		driver:      "grpc", adapter: "kavachgrpc", category: "rpc",
	},
}

var bareTimeFuncs = map[string]bool{
	"Now":   true,
	"Since": true,
	"Until": true,
}

var mathRandFuncs = map[string]bool{
	"Int": true, "Intn": true, "IntN": true, "Int31": true, "Int31n": true,
	"Int63": true, "Int63n": true, "Uint32": true, "Uint64": true,
	"Float32": true, "Float64": true, "NormFloat64": true, "ExpFloat64": true,
	"Read": true, "Perm": true, "Shuffle": true, "New": true, "NewSource": true,
}

var cryptoRandFuncs = map[string]bool{
	"Read": true, "Int": true, "Prime": true,
}

var uuidRandFuncs = map[string]bool{
	"New": true, "NewString": true, "NewRandom": true, "NewV4": true, "NewV7": true, "NewV1": true, "NewUUID": true,
}

var routeRegisterMethods = map[string]string{
	"HandleFunc": "ANY",
	"Handle":     "ANY",
	"GET":        "GET", "Get": "GET",
	"POST": "POST", "Post": "POST",
	"PUT": "PUT", "Put": "PUT",
	"DELETE": "DELETE", "Delete": "DELETE",
	"PATCH": "PATCH", "Patch": "PATCH",
}

// ScanRepo performs manifest detection and fast Go AST static analysis on dir.
func ScanRepo(dir string) (*DoctorReport, error) {
	start := time.Now()
	det, err := DetectRepo(dir)
	if err != nil {
		return nil, err
	}

	rep := &DoctorReport{
		Dir:              det.Dir,
		Service:          det.Service,
		Language:         det.Language,
		Module:           det.Module,
		Manifests:        det.Manifests,
		Drivers:          []Driver{},
		InitSites:        []CallSite{},
		ClockCalls:       []CallSite{},
		RandCalls:        []CallSite{},
		DetachedContexts: []CallSite{},
		Handlers:         []HandlerSite{},
	}

	seenDrivers := map[string]Driver{}
	for _, d := range det.Drivers {
		mergeDriver(seenDrivers, d)
	}

	topicSet := map[string]bool{}
	routeSet := map[string]bool{}
	var goFiles []string

	err = filepath.WalkDir(det.Dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == det.Dir {
				return walkErr
			}
			return nil
		}
		if d.IsDir() {
			if path != det.Dir && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(goFiles)

	if len(goFiles) > 0 && rep.Language == LangUnknown {
		rep.Language = LangGo
	}

	for _, fullPath := range goFiles {
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, fullPath, nil, 0)
		if parseErr != nil {
			rel, _ := filepath.Rel(det.Dir, fullPath)
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("skipped unparseable Go file %s: %v", filepath.ToSlash(rel), parseErr))
			continue
		}
		rep.FilesScanned++
		rel, err := filepath.Rel(det.Dir, fullPath)
		if err != nil {
			rel = filepath.Base(fullPath)
		}
		rel = filepath.ToSlash(rel)

		scanGoFile(fset, file, rel, seenDrivers, rep, topicSet, routeSet)
	}

	if err := scanTSRepo(det.Dir, seenDrivers, rep, topicSet, routeSet); err != nil {
		return nil, err
	}

	rep.Drivers = sortedDrivers(seenDrivers)
	rep.Topics = sortedKeys(topicSet)
	rep.Routes = sortedKeys(routeSet)
	sortCallSites(rep.InitSites)
	sortCallSites(rep.ClockCalls)
	sortCallSites(rep.RandCalls)
	sortCallSites(rep.DetachedContexts)
	sort.Slice(rep.Handlers, func(i, j int) bool {
		if rep.Handlers[i].File != rep.Handlers[j].File {
			return rep.Handlers[i].File < rep.Handlers[j].File
		}
		return rep.Handlers[i].Line < rep.Handlers[j].Line
	})

	if len(rep.Drivers) > 0 {
		rep.RecommendedMode = "driver"
	} else {
		rep.RecommendedMode = "scoped"
	}

	rep.SuggestedSnippet = buildSuggestedSnippet(rep)
	rep.DurationMS = float64(time.Since(start).Microseconds()) / 1000.0
	return rep, nil
}

func scanGoFile(
	fset *token.FileSet,
	file *ast.File,
	relPath string,
	seenDrivers map[string]Driver,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) {
	imports := map[string]string{} // local identifier -> import path
	for _, imp := range file.Imports {
		pkgPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		var local string
		if imp.Name != nil {
			local = imp.Name.Name
		} else {
			local = defaultImportName(pkgPath)
		}
		if local != "_" && local != "." && local != "" {
			imports[local] = pkgPath
		}
		// Register stdlib & third-party drivers observed via AST imports.
		if d, ok := goStdlibDrivers[pkgPath]; ok {
			mergeDriver(seenDrivers, d)
		} else if d, ok := matchGoDriver(pkgPath, "", "ast"); ok {
			mergeDriver(seenDrivers, d)
		}
	}

	fileInHandlerPkg := pathLooksLikeHandler(relPath)

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		fnName := formatFuncName(fn)
		fnLine := fset.Position(fn.Pos()).Line
		fnEnd := fset.Position(fn.End()).Line

		// Check whether this function declaration is a handler/processor entrypoint.
		if kind := classifyHandlerFunc(fn, imports, fileInHandlerPkg); kind != "" {
			rep.Handlers = append(rep.Handlers, HandlerSite{
				File:    relPath,
				Line:    fnLine,
				EndLine: fnEnd,
				Name:    fnName,
				Kind:    kind,
			})
		}

		if fn.Body == nil {
			continue
		}

		isHandlerContext := fn.Name.Name != "main" && fn.Name.Name != "init" &&
			(funcAcceptsContext(fn) || classifyHandlerFunc(fn, imports, fileInHandlerPkg) != "" || fileInHandlerPkg)

		walkFuncBody(fset, fn.Body, fnName, relPath, imports, isHandlerContext, 0, rep, topicSet, routeSet)
	}
}

func walkFuncBody(
	fset *token.FileSet,
	node ast.Node,
	fnName, relPath string,
	imports map[string]string,
	isHandlerContext bool,
	goroutineDepth int,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) {
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch x := n.(type) {
		case *ast.GoStmt:
			// Walk goroutine body with goroutineDepth+1 so any context.Background()
			// or context.TODO() spawned inside is flagged.
			walkFuncBody(fset, x.Call, fnName, relPath, imports, isHandlerContext, goroutineDepth+1, rep, topicSet, routeSet)
			return false

		case *ast.CompositeLit:
			// Detect &http.Server{...}
			if sel, ok := x.Type.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] == "net/http" && sel.Sel.Name == "Server" {
					rep.InitSites = append(rep.InitSites, CallSite{
						File:     relPath,
						Line:     fset.Position(x.Pos()).Line,
						Func:     fnName,
						Symbol:   "http.Server",
						Category: "net/http",
						Adapter:  "kavachhttp",
						Detail:   "Wrap Handler with kavachhttp.Wrap(kv, handler)",
					})
				}
				// Detect kgo.Record{Topic: "..."}
				if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(imports[id.Name], "github.com/twmb/franz-go") && sel.Sel.Name == "Record" {
					if topic := extractStructStringField(x, "Topic"); topic != "" {
						topicSet[topic] = true
					}
				}
			}

		case *ast.CallExpr:
			line := fset.Position(x.Pos()).Line
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				method := sel.Sel.Name
				if id, ok := sel.X.(*ast.Ident); ok {
					pkgPath := imports[id.Name]
					if pkgPath != "" {
						inspectPkgCall(pkgPath, id.Name, method, x, line, fnName, relPath, isHandlerContext, goroutineDepth, rep, topicSet, routeSet)
					}
				}
				// Method-level detection (routes, Kafka poll/produce, gRPC register) regardless of receiver type.
				inspectMethodCall(sel, method, x, line, fnName, relPath, rep, topicSet, routeSet)
			}
		}
		return true
	})
}

func inspectPkgCall(
	pkgPath, pkgAlias, method string,
	call *ast.CallExpr,
	line int,
	fnName, relPath string,
	isHandlerContext bool,
	goroutineDepth int,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) {
	// 1. Driver initialization sites.
	for _, m := range goDriverInitMatchers {
		if m.funcs[method] && matchAnyPrefix(pkgPath, m.pkgPrefixes) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File:     relPath,
				Line:     line,
				Func:     fnName,
				Symbol:   pkgAlias + "." + method,
				Category: m.driver,
				Adapter:  m.adapter,
			})
			break
		}
	}

	// 2. Bare clock reads.
	if pkgPath == "time" && bareTimeFuncs[method] {
		rep.ClockCalls = append(rep.ClockCalls, CallSite{
			File:     relPath,
			Line:     line,
			Func:     fnName,
			Symbol:   "time." + method,
			Category: "clock",
			Detail:   "Replace with kv.Now(ctx) or run with clock interposer",
		})
	}

	// 3. Bare random / UUID generation.
	switch {
	case (pkgPath == "math/rand" || pkgPath == "math/rand/v2") && mathRandFuncs[method]:
		rep.RandCalls = append(rep.RandCalls, CallSite{
			File:     relPath,
			Line:     line,
			Func:     fnName,
			Symbol:   pkgPath + "." + method,
			Category: "rand",
			Detail:   "Use kv.Rand(ctx) for deterministic replay",
		})
	case pkgPath == "crypto/rand" && cryptoRandFuncs[method]:
		rep.RandCalls = append(rep.RandCalls, CallSite{
			File:     relPath,
			Line:     line,
			Func:     fnName,
			Symbol:   "crypto/rand." + method,
			Category: "rand",
			Detail:   "Use kv.Rand(ctx) for deterministic replay",
		})
	case (strings.HasPrefix(pkgPath, "github.com/google/uuid") || strings.HasPrefix(pkgPath, "github.com/gofrs/uuid")) && uuidRandFuncs[method]:
		rep.RandCalls = append(rep.RandCalls, CallSite{
			File:     relPath,
			Line:     line,
			Func:     fnName,
			Symbol:   "uuid." + method,
			Category: "uuid",
			Detail:   "Seed or generate via kv.Rand(ctx) for deterministic replay",
		})
	}

	// 4. Detached context.Background() / context.TODO() inside handlers or goroutines.
	if pkgPath == "context" && (method == "Background" || method == "TODO") {
		if goroutineDepth > 0 || isHandlerContext {
			reason := "inside handler/processor"
			if goroutineDepth > 0 {
				reason = "inside goroutine"
			}
			rep.DetachedContexts = append(rep.DetachedContexts, CallSite{
				File:     relPath,
				Line:     line,
				Func:     fnName,
				Symbol:   "context." + method,
				Category: "context",
				Detail:   "Detached context " + reason + " drops Kavach step propagation; pass caller ctx",
			})
		}
	}

	// 5. Kafka topic option literals (franz-go).
	if strings.HasPrefix(pkgPath, "github.com/twmb/franz-go/pkg/kgo") {
		if method == "ConsumeTopics" || method == "DefaultProduceTopic" {
			for _, arg := range call.Args {
				if s, ok := stringLit(arg); ok && s != "" {
					topicSet[s] = true
				}
			}
		}
	}

	// 6. net/http top-level route registration.
	if pkgPath == "net/http" && (method == "HandleFunc" || method == "Handle") && len(call.Args) >= 1 {
		if pattern, ok := stringLit(call.Args[0]); ok && strings.HasPrefix(pattern, "/") {
			route := "ANY " + pattern
			routeSet[route] = true
			rep.Handlers = append(rep.Handlers, HandlerSite{
				File:   relPath,
				Line:   line,
				Name:   handlerArgName(call.Args, pattern),
				Kind:   "http",
				Route:  pattern,
				Method: "ANY",
			})
		}
	}
}

func inspectMethodCall(
	sel *ast.SelectorExpr,
	method string,
	call *ast.CallExpr,
	line int,
	fnName, relPath string,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) {
	// Route registration on mux/router (chi, gin, echo, ServeMux): r.Get("/path", h)
	if httpMethod, ok := routeRegisterMethods[method]; ok && len(call.Args) >= 2 {
		// Skip if already handled as package-level http.HandleFunc.
		if id, isIdent := sel.X.(*ast.Ident); isIdent && id.Name == "http" {
			return
		}
		if pattern, ok := stringLit(call.Args[0]); ok && strings.HasPrefix(pattern, "/") {
			route := httpMethod + " " + pattern
			routeSet[route] = true
			rep.Handlers = append(rep.Handlers, HandlerSite{
				File:   relPath,
				Line:   line,
				Name:   handlerArgName(call.Args, pattern),
				Kind:   "http",
				Route:  pattern,
				Method: httpMethod,
			})
		}
	}

	// gRPC service registration: pb.RegisterFooServer(srv, impl)
	if strings.HasPrefix(method, "Register") && strings.HasSuffix(method, "Server") && len(call.Args) >= 2 {
		rep.Handlers = append(rep.Handlers, HandlerSite{
			File: relPath,
			Line: line,
			Name: method,
			Kind: "grpc",
		})
	}

	// Kafka topic hints from env defaults or helper calls like GetEnvOrDefault("...TOPIC...", "events_enriched")
	if len(call.Args) >= 2 {
		if key, ok := stringLit(call.Args[0]); ok && strings.Contains(strings.ToUpper(key), "TOPIC") {
			if def, ok := stringLit(call.Args[1]); ok && looksLikeTopicName(def) {
				topicSet[def] = true
			}
		}
	}
}

func classifyHandlerFunc(fn *ast.FuncDecl, imports map[string]string, inHandlerPkg bool) string {
	if fn.Name == nil || fn.Name.Name == "main" || fn.Name.Name == "init" {
		return ""
	}
	if fn.Type != nil && fn.Type.Params != nil {
		var hasRespWriter, hasHTTPReq bool
		for _, field := range fn.Type.Params.List {
			ts := exprTypeString(field.Type)
			switch {
			case strings.HasSuffix(ts, "http.ResponseWriter"):
				hasRespWriter = true
			case strings.HasSuffix(ts, "http.Request"):
				hasHTTPReq = true
			case strings.HasSuffix(ts, "gin.Context"), strings.HasSuffix(ts, "echo.Context"):
				return "http"
			case strings.HasSuffix(ts, "kgo.Record"), strings.HasSuffix(ts, "kafka.Message"), strings.HasSuffix(ts, "sarama.ConsumerMessage"):
				return "kafka"
			}
		}
		if hasRespWriter && hasHTTPReq {
			return "http"
		}
	}
	name := fn.Name.Name
	if inHandlerPkg && fn.Name.IsExported() {
		for _, prefix := range []string{"Handle", "Process", "Consume", "StartProcessing", "Serve"} {
			if strings.HasPrefix(name, prefix) {
				if strings.Contains(strings.ToLower(name), "event") || strings.Contains(strings.ToLower(name), "consume") || strings.Contains(strings.ToLower(name), "process") {
					return "kafka"
				}
				return "handler"
			}
		}
	}
	return ""
}

func funcAcceptsContext(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		if strings.HasSuffix(exprTypeString(field.Type), "context.Context") {
			return true
		}
	}
	return false
}

func pathLooksLikeHandler(relPath string) bool {
	lower := strings.ToLower(relPath)
	for _, seg := range []string{"handler", "processor", "consumer", "worker", "api/", "http/", "service", "controller", "server"} {
		if strings.Contains(lower, seg) {
			return true
		}
	}
	return false
}

func defaultImportName(pkgPath string) string {
	parts := strings.Split(pkgPath, "/")
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	// Handle version suffixes like github.com/jackc/pgx/v5 -> pgx
	if len(last) >= 2 && last[0] == 'v' && isAllDigits(last[1:]) && len(parts) >= 2 {
		last = parts[len(parts)-2]
	}
	// Strip hyphen prefixes like go-redis -> redis, franz-go -> kgo
	if idx := strings.LastIndexByte(last, '-'); idx >= 0 {
		last = last[idx+1:]
	}
	return last
}

func matchAnyPrefix(pkgPath string, prefixes []string) bool {
	for _, p := range prefixes {
		if pkgPath == p || strings.HasPrefix(pkgPath, p+"/") {
			return true
		}
	}
	return false
}

func formatFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := strings.TrimPrefix(exprTypeString(fn.Recv.List[0].Type), "*")
		if recv != "" {
			return recv + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

func exprTypeString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprTypeString(x.X)
	case *ast.SelectorExpr:
		return exprTypeString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprTypeString(x.Elt)
	case *ast.IndexExpr:
		return exprTypeString(x.X)
	default:
		return ""
	}
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func extractStructStringField(lit *ast.CompositeLit, field string) string {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
			if s, ok := stringLit(kv.Value); ok {
				return s
			}
		}
	}
	return ""
}

func handlerArgName(args []ast.Expr, fallback string) string {
	if len(args) >= 2 {
		switch fn := args[len(args)-1].(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			return exprTypeString(fn)
		}
	}
	return fallback
}

func looksLikeTopicName(s string) bool {
	if len(s) < 2 || len(s) > 128 || strings.ContainsAny(s, " \t\n/\\:") {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func sortCallSites(sites []CallSite) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		if sites[i].Line != sites[j].Line {
			return sites[i].Line < sites[j].Line
		}
		return sites[i].Symbol < sites[j].Symbol
	})
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func buildSuggestedSnippet(rep *DoctorReport) string {
	if rep.Language == LangTypeScript {
		return buildTypeScriptSnippet(rep)
	}
	if rep.Language == LangRust {
		return fmt.Sprintf(`// Rust adapter pack (Phase 4) or scoped host protocol:
// See https://github.com/kavachlabs/kavach/blob/main/sdk/rust/README.md
// Service: %s`, rep.Service)
	}

	adapters := map[string]bool{}
	for _, d := range rep.Drivers {
		if d.Language == LangGo && d.Adapter != "" {
			adapters[d.Adapter] = true
		}
	}

	if len(adapters) == 0 {
		return fmt.Sprintf(`kv := kavach.Init(kavach.Config{
    Service: %q,
    PII:     kavach.ScrubStandard,
})
defer kv.Close()
// No supported I/O drivers detected — run 'kavach init --mode scoped' or wrap your handler with kv.ServeOrReplay(runner).`, rep.Service)
	}

	var imports []string
	imports = append(imports, `    "github.com/kavachlabs/kavach-go"`)
	for _, a := range []string{"kavachhttp", "kavachkgo", "kavachpgx", "kavachredis", "kavachgrpc"} {
		if adapters[a] {
			imports = append(imports, fmt.Sprintf(`    "github.com/kavachlabs/kavach-go/adapters/%s"`, a))
		}
	}

	var wraps []string
	if adapters["kavachpgx"] {
		hasGorm := hasDriver(rep.Drivers, "gorm")
		hasSQL := hasDriver(rep.Drivers, "database/sql")
		switch {
		case hasGorm:
			wraps = append(wraps, `sqlDB := kavachpgx.OpenDB(kv, "pgx", dsn) // pass to gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}))`)
		case hasSQL && !hasDriver(rep.Drivers, "pgx/v5"):
			wraps = append(wraps, `sqlDB := kavachpgx.OpenDB(kv, "postgres", dsn)`)
		default:
			wraps = append(wraps, `pool := kavachpgx.Wrap(kv, rawPgxPool)`)
		}
	}
	if adapters["kavachredis"] {
		wraps = append(wraps, `rdb := kavachredis.Wrap(kv, rawRedisClient)`)
	}
	if adapters["kavachkgo"] {
		wraps = append(wraps, `kafka := kavachkgo.Wrap(kv, rawKafkaClient)`)
	}
	if adapters["kavachhttp"] {
		wraps = append(wraps, `mux := kavachhttp.Wrap(kv, appRouter)`)
	}
	if adapters["kavachgrpc"] {
		wraps = append(wraps, `grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(kavachgrpc.UnaryServerInterceptor(kv)))`)
	}

	serveTarget := "handler"
	if adapters["kavachhttp"] {
		serveTarget = "mux"
	} else if adapters["kavachkgo"] {
		serveTarget = "kafka"
	}

	var b strings.Builder
	b.WriteString("import (\n")
	b.WriteString(strings.Join(imports, "\n"))
	b.WriteString("\n)\n\n")
	fmt.Fprintf(&b, "kv := kavach.Init(kavach.Config{\n    Service: %q,\n    PII:     kavach.ScrubStandard,\n})\ndefer kv.Close()\n\n", rep.Service)
	b.WriteString("// 1-line driver wraps at client initialization — zero business-logic edits:\n")
	b.WriteString(strings.Join(wraps, "\n"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "kv.ServeOrReplay(%s)", serveTarget)
	return b.String()
}

func hasDriver(drivers []Driver, name string) bool {
	for _, d := range drivers {
		if d.Name == name {
			return true
		}
	}
	return false
}
