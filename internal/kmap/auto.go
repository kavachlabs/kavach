package kmap

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/internal/kconfig"
)

// AutoOptions configures static AST map synthesis via AutoSpecFromDir.
type AutoOptions struct {
	Service        string
	Title          string
	InvariantsPath string
	Invariants     []AutoInvariant
}

// AutoInvariant describes a declarative invariant rule to include as a map node.
type AutoInvariant struct {
	Name  string
	Kind  string
	Sink  string
	Sinks []string
	Key   string
	Field string
}

type autoScanData struct {
	service    string
	language   string
	files      int
	routes     []autoRoute
	handlers   []autoHandler
	gateways   map[string]*autoGateway
	sinks      map[string]*autoSink
	invariants []AutoInvariant
}

type autoRoute struct {
	method  string
	path    string
	handler string
	code    string
}

type autoHandler struct {
	id         string
	name       string
	kind       string // http, kafka, grpc, handler
	code       string
	readsDB    bool
	writesDB   bool
	usesRedis  bool
	usesKafkaP bool
	usesHTTP   bool
	topics     []string
}

type autoGateway struct {
	id       string
	kind     string // state | external
	title    string
	plain    string
	codeRefs []string
}

type autoSink struct {
	id       string
	sink     string
	title    string
	plain    string
	codeRefs []string
}

// AutoSpecFromDir statically scans a repository's Go AST (and manifests /
// .kavach/invariants.yaml) to synthesize a valid kmap.Spec on Day 0.
func AutoSpecFromDir(dir string, opts AutoOptions) (*Spec, error) {
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	sd := &autoScanData{
		service:  opts.Service,
		gateways: map[string]*autoGateway{},
		sinks:    map[string]*autoSink{},
	}

	detectAutoManifests(absDir, sd)
	if sd.service == "" {
		sd.service = cleanAutoName(filepath.Base(absDir))
		if sd.service == "" {
			sd.service = "service"
		}
	}

	// Scan Go and TypeScript/JavaScript source files for handlers, routes, DB/Redis/Kafka/HTTP calls.
	if err := scanAutoGoFiles(absDir, sd); err != nil {
		return nil, err
	}
	if err := scanAutoTSFiles(absDir, sd); err != nil {
		return nil, err
	}

	// Load declarative invariants from explicit options or .kavach/invariants.yaml.
	sd.invariants = append(sd.invariants, opts.Invariants...)
	if len(sd.invariants) == 0 {
		invPath := opts.InvariantsPath
		if invPath == "" {
			cand := filepath.Join(absDir, ".kavach", "invariants.yaml")
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				invPath = cand
			}
		}
		if invPath != "" {
			if parsed, err := parseAutoInvariantsYAML(invPath); err == nil {
				sd.invariants = append(sd.invariants, parsed...)
			}
		}
	}

	return buildAutoSpec(sd, opts), nil
}

// WriteSpecFile serializes a Spec as clean formatted map.json (atomic-safe).
func WriteSpecFile(path string, spec *Spec) error {
	cp := cloneSpec(spec)
	if err := cp.NormalizeAndValidate(); err != nil {
		return err
	}
	// Strip derived Links so map.json stays clean and minimal; LoadSpec rebuilds them.
	for i := range cp.Nodes {
		cp.Nodes[i].Links = nil
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func detectAutoManifests(root string, sd *autoScanData) {
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		sd.language = "go"
		text := string(data)
		for _, line := range strings.Split(text, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "module ") && sd.service == "" {
				fields := strings.Fields(t)
				if len(fields) >= 2 {
					parts := strings.Split(strings.Trim(fields[1], `"`), "/")
					sd.service = cleanAutoName(parts[len(parts)-1])
				}
			}
			if strings.Contains(line, "// indirect") {
				continue
			}
			switch {
			case strings.Contains(t, "github.com/jackc/pgx") || strings.Contains(t, "gorm.io/gorm") || strings.Contains(t, "gorm.io/driver/postgres"):
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL", "Primary SQL store (pgx/GORM)", "")
			case strings.Contains(t, "github.com/redis/go-redis") || strings.Contains(t, "github.com/go-redis/redis"):
				addAutoGateway(sd, "gw_redis", "state", "Redis", "Cache / coordination state (go-redis)", "")
			case strings.Contains(t, "github.com/twmb/franz-go") || strings.Contains(t, "github.com/segmentio/kafka-go") || strings.Contains(t, "sarama"):
				addAutoGateway(sd, "gw_kafka", "external", "Kafka Broker", "Event stream consumer & producer", "")
			case strings.Contains(t, "google.golang.org/grpc"):
				addAutoGateway(sd, "gw_grpc", "external", "gRPC", "RPC transport", "")
			}
		}
		return
	}
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		sd.language = "typescript"
		var pkg struct {
			Name                 string            `json:"name"`
			Dependencies         map[string]string `json:"dependencies"`
			DevDependencies      map[string]string `json:"devDependencies"`
			PeerDependencies     map[string]string `json:"peerDependencies"`
			OptionalDependencies map[string]string `json:"optionalDependencies"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			if sd.service == "" && pkg.Name != "" {
				sd.service = cleanAutoName(pkg.Name)
			}
			all := map[string]bool{}
			for _, m := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
				for k := range m {
					all[k] = true
				}
			}
			if all["pg"] || all["@kavach/pg"] || all["prisma"] || all["@prisma/client"] || all["drizzle-orm"] {
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL", "SQL database client", "")
			}
			if all["ioredis"] || all["@kavach/ioredis"] || all["redis"] {
				addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache & coordination client", "")
			}
			if all["kafkajs"] || all["@kavach/kafkajs"] {
				addAutoGateway(sd, "gw_kafka", "external", "Kafka Broker", "kafkajs client", "")
			}
			if all["express"] || all["fastify"] || all["hono"] || all["next"] || all["@kavach/http"] {
				addAutoSink(sd, "http:response", "http:response", "HTTP response emitted to caller", "")
			}
		}
		return
	}
	if data, err := os.ReadFile(filepath.Join(root, "Cargo.toml")); err == nil {
		sd.language = "rust"
		text := string(data)
		if strings.Contains(text, "sqlx") {
			addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL", "sqlx database pool", "")
		}
		if strings.Contains(text, "redis") {
			addAutoGateway(sd, "gw_redis", "state", "Redis", "redis client", "")
		}
		if strings.Contains(text, "rdkafka") {
			addAutoGateway(sd, "gw_kafka", "external", "Kafka Broker", "rdkafka client", "")
		}
		if strings.Contains(text, "axum") {
			addAutoSink(sd, "http:response", "http:response", "HTTP response emitted by axum router", "")
		}
	}
}

func scanAutoGoFiles(root string, sd *autoScanData) error {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if d.IsDir() {
			if path != root && autoSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	if len(files) > 0 && sd.language == "" {
		sd.language = "go"
	}

	routeSeen := map[string]bool{}
	consumeTopics := map[string]string{} // topic -> codeRef
	produceTopics := map[string]string{} // topic -> codeRef
	allTopics := map[string]string{}

	for _, fullPath := range files {
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, fullPath, nil, 0)
		if parseErr != nil {
			continue
		}
		sd.files++
		rel, _ := filepath.Rel(root, fullPath)
		rel = filepath.ToSlash(rel)

		imports := map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			var alias string
			if imp.Name != nil {
				alias = imp.Name.Name
			} else {
				parts := strings.Split(p, "/")
				alias = parts[len(parts)-1]
				if len(alias) >= 2 && alias[0] == 'v' && isDigits(alias[1:]) && len(parts) >= 2 {
					alias = parts[len(parts)-2]
				}
				if i := strings.LastIndexByte(alias, '-'); i >= 0 {
					alias = alias[i+1:]
				}
			}
			if alias != "" && alias != "_" && alias != "." {
				imports[alias] = p
			}
			switch {
			case p == "database/sql" || strings.HasPrefix(p, "github.com/jackc/pgx") || strings.HasPrefix(p, "gorm.io/"):
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", "")
			case strings.HasPrefix(p, "github.com/redis/go-redis") || strings.HasPrefix(p, "github.com/go-redis/redis"):
				addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache and coordination reads/writes", "")
			case strings.HasPrefix(p, "github.com/twmb/franz-go") || strings.HasPrefix(p, "github.com/segmentio/kafka-go") || strings.Contains(p, "sarama"):
				addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", "")
			}
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			startLine := fset.Position(fn.Pos()).Line
			endLine := fset.Position(fn.End()).Line
			fnCode := fmt.Sprintf("%s:%d-%d", rel, startLine, endLine)
			fnName := autoFuncName(fn)

			hKind := classifyAutoHandler(fn, rel)
			h := autoHandler{
				name: fnName,
				kind: hKind,
				code: fnCode,
			}

			if fn.Body != nil {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CompositeLit:
						if sel, ok := x.Type.(*ast.SelectorExpr); ok {
							if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(imports[id.Name], "github.com/twmb/franz-go") && sel.Sel.Name == "Record" {
								if topic := autoStructStringField(x, "Topic"); topic != "" {
									ref := fmt.Sprintf("%s:%d", rel, fset.Position(x.Pos()).Line)
									produceTopics[topic] = ref
									allTopics[topic] = ref
									h.usesKafkaP = true
									h.topics = append(h.topics, topic)
								}
							}
						}
					case *ast.CallExpr:
						sel, ok := x.Fun.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						method := sel.Sel.Name
						line := fset.Position(x.Pos()).Line
						ref := fmt.Sprintf("%s:%d", rel, line)

						// Package calls.
						if id, ok := sel.X.(*ast.Ident); ok {
							pkgPath := imports[id.Name]
							switch {
							case (pkgPath == "database/sql" && (method == "Open" || method == "OpenDB")) ||
								(strings.HasPrefix(pkgPath, "github.com/jackc/pgx") && (method == "New" || method == "NewWithConfig" || method == "Connect")) ||
								(strings.HasPrefix(pkgPath, "gorm.io/") && (method == "Open" || method == "New")):
								addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
							case strings.HasPrefix(pkgPath, "github.com/redis/go-redis") && strings.HasPrefix(method, "New"):
								addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache and coordination reads/writes", ref)
							case strings.HasPrefix(pkgPath, "github.com/twmb/franz-go") && method == "ConsumeTopics":
								for _, arg := range x.Args {
									if s, ok := autoStringLit(arg); ok && s != "" {
										consumeTopics[s] = ref
										allTopics[s] = ref
									}
								}
							case strings.HasPrefix(pkgPath, "github.com/twmb/franz-go") && method == "DefaultProduceTopic":
								for _, arg := range x.Args {
									if s, ok := autoStringLit(arg); ok && s != "" {
										produceTopics[s] = ref
										allTopics[s] = ref
									}
								}
							case pkgPath == "net/http" && (method == "Get" || method == "Post" || method == "Do"):
								addAutoGateway(sd, "gw_http", "external", "Outbound HTTP", "External HTTP API calls", ref)
								h.usesHTTP = true
							}
						}

						// Route registration.
						if httpMethod, ok := autoRouteMethods[method]; ok && len(x.Args) >= 2 {
							if pattern, ok := autoStringLit(x.Args[0]); ok && strings.HasPrefix(pattern, "/") {
								key := httpMethod + " " + pattern
								if !routeSeen[key] {
									routeSeen[key] = true
									sd.routes = append(sd.routes, autoRoute{
										method:  httpMethod,
										path:    pattern,
										handler: autoHandlerArg(x.Args),
										code:    ref,
									})
								}
								addAutoSink(sd, "http:response", "http:response", "HTTP response emitted to caller", "")
							}
						}

						// SQL / DB operations.
						switch method {
						case "Query", "QueryRow", "QueryContext", "QueryRowContext", "Find", "First", "Take", "Count", "Raw":
							h.readsDB = true
							addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
						case "Exec", "ExecContext", "Create", "Save", "Update", "Updates", "Delete":
							h.writesDB = true
							addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
							addAutoSink(sd, "pg:exec", "pg:exec", "SQL write statements (INSERT/UPDATE/DELETE)", ref)
						}

						// Redis operations (check receiver name hints to avoid false positives on map/http Get).
						recvLower := strings.ToLower(autoExprString(sel.X))
						if strings.Contains(recvLower, "redis") || strings.Contains(recvLower, "rdb") || strings.Contains(recvLower, "cache") || method == "ZAdd" || method == "SetNX" || method == "HGetAll" {
							switch method {
							case "Get", "Set", "SetNX", "HGet", "HSet", "HGetAll", "Del", "Incr", "IncrBy", "SAdd", "SIsMember", "ZAdd", "ZRem", "ZRange", "Exists", "Expire", "Pipeline", "TxPipeline", "Eval":
								h.usesRedis = true
								addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache and coordination reads/writes", ref)
							}
						}

						// Kafka produce/consume calls.
						switch method {
						case "Produce", "ProduceSync", "WriteMessages", "SendMessage":
							h.usesKafkaP = true
							addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
						case "PollFetches", "FetchMessage", "ConsumeClaim":
							addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
						}

						// Topic defaults in env helper calls: getEnv("...TOPIC...", "topic_name")
						if len(x.Args) >= 2 {
							if envKey, ok := autoStringLit(x.Args[0]); ok && strings.Contains(strings.ToUpper(envKey), "TOPIC") {
								if defVal, ok := autoStringLit(x.Args[1]); ok && autoLooksLikeTopic(defVal) {
									upperKey := strings.ToUpper(envKey)
									allTopics[defVal] = ref
									if strings.Contains(upperKey, "CONSUME") || strings.Contains(upperKey, "INPUT") || strings.Contains(upperKey, "INCOMING") {
										consumeTopics[defVal] = ref
									} else {
										produceTopics[defVal] = ref
									}
								}
							}
						}
					}
					return true
				})
			}

			if h.kind != "" {
				sd.handlers = append(sd.handlers, h)
			}
		}
	}

	// Register discovered produce topics as sinks (and fallback to allTopics if no explicit distinction).
	if len(produceTopics) == 0 && len(allTopics) > 0 {
		for t, ref := range allTopics {
			produceTopics[t] = ref
		}
	}
	for _, topic := range autoSortedMapKeys(produceTopics) {
		sinkName := "kafka:" + topic
		addAutoSink(sd, sinkName, sinkName, "Kafka produce topic "+topic, produceTopics[topic])
	}
	// If Kafka gateway exists without literal topic names, still add a generic Kafka sink node.
	if _, hasKafka := sd.gateways["gw_kafka"]; hasKafka && len(produceTopics) == 0 {
		addAutoSink(sd, "kafka:produce", "kafka:produce", "Kafka producer output stream", "")
	}

	// Record consume topics as synthetic routes with method KAFKA so buildAutoSpec emits entry nodes.
	for _, topic := range autoSortedMapKeys(consumeTopics) {
		sd.routes = append(sd.routes, autoRoute{
			method: "KAFKA",
			path:   topic,
			code:   consumeTopics[topic],
		})
	}

	return nil
}

func buildAutoSpec(sd *autoScanData, opts AutoOptions) *Spec {
	title := opts.Title
	if title == "" {
		title = sd.service + " topology"
	}

	spec := &Spec{
		Version: 1,
		Title:   title,
		Summary: fmt.Sprintf("Auto-generated Day-0 topology for %s (%s, %d source files scanned)", sd.service, orUnknown(sd.language), sd.files),
		Service: sd.service,
		Groups: []Group{
			{ID: "entries", Title: "Inputs & Entrypoints", Plain: "HTTP routes, Kafka topics, and service entrypoints"},
			{ID: "handlers", Title: "Handlers & Processors", Plain: "Request handlers and event processors discovered via AST"},
			{ID: "gateways", Title: "Data Stores & Dependencies", Plain: "External databases, caches, and RPC gateways intercepted on replay"},
			{ID: "sinks", Title: "Outputs & Sinks", Plain: "Durable side effects captured and asserted on every step"},
		},
	}

	nodeSet := map[string]bool{}
	addNode := func(n Node) string {
		if nodeSet[n.ID] {
			return n.ID
		}
		nodeSet[n.ID] = true
		spec.Nodes = append(spec.Nodes, n)
		return n.ID
	}

	edgeSet := map[string]bool{}
	addEdge := func(from, to, kind, label string) {
		if from == "" || to == "" || !nodeSet[from] || !nodeSet[to] || from == to {
			return
		}
		key := from + "\x00" + to + "\x00" + kind
		if edgeSet[key] {
			return
		}
		edgeSet[key] = true
		spec.Edges = append(spec.Edges, Edge{From: from, To: to, Kind: kind, Label: label, Auto: true})
	}

	// 1. Entrypoint nodes (cap at 12 for crisp canvas readability).
	var entryIDs []string
	entryHandlerTarget := map[string]string{}
	sort.Slice(sd.routes, func(i, j int) bool {
		if sd.routes[i].method != sd.routes[j].method {
			return sd.routes[i].method < sd.routes[j].method
		}
		return sd.routes[i].path < sd.routes[j].path
	})
	for i, r := range sd.routes {
		if i >= 12 {
			break
		}
		id := makeAutoID("in", r.method+"_"+r.path)
		var code []string
		if r.code != "" {
			code = []string{r.code}
		}
		if r.method == "KAFKA" {
			source := "kafka:" + r.path
			addNode(Node{
				ID:    id,
				Group: "entries",
				Kind:  "entry",
				Title: source,
				Plain: "Inbound Kafka topic " + r.path,
				Code:  code,
				Match: []Rule{{On: "input", Source: source}},
				Auto:  true,
			})
		} else {
			label := r.method + " " + r.path
			source := "http:" + label
			addNode(Node{
				ID:    id,
				Group: "entries",
				Kind:  "entry",
				Title: label,
				Plain: "Inbound HTTP route " + label,
				Code:  code,
				Match: []Rule{{On: "input", Source: source}},
				Auto:  true,
			})
			if r.handler != "" {
				entryHandlerTarget[id] = r.handler
			}
		}
		entryIDs = append(entryIDs, id)
	}
	if len(entryIDs) == 0 {
		source := "service:" + sd.service
		plain := "Service entrypoint for " + sd.service
		if _, hasKafka := sd.gateways["gw_kafka"]; hasKafka {
			source = "kafka:events"
			plain = "Inbound Kafka event stream"
		} else if _, hasHTTP := sd.sinks["http:response"]; hasHTTP {
			source = "http:request"
			plain = "Inbound HTTP request"
		}
		id := addNode(Node{
			ID:    "in_primary",
			Group: "entries",
			Kind:  "entry",
			Title: source,
			Plain: plain,
			Match: []Rule{{On: "input", Source: source}},
			Auto:  true,
		})
		entryIDs = append(entryIDs, id)
	}

	// 2. Handler nodes (cap at 14, prioritizing handlers that touch DB/Redis/Kafka).
	sort.SliceStable(sd.handlers, func(i, j int) bool {
		si := handlerScore(sd.handlers[i])
		sj := handlerScore(sd.handlers[j])
		if si != sj {
			return si > sj
		}
		return sd.handlers[i].name < sd.handlers[j].name
	})
	var handlerIDs []string
	handlerByName := map[string]string{}
	for i := range sd.handlers {
		if len(handlerIDs) >= 14 {
			break
		}
		h := &sd.handlers[i]
		id := makeAutoID("fn", h.name)
		if nodeSet[id] {
			continue
		}
		h.id = id
		addNode(Node{
			ID:    id,
			Group: "handlers",
			Kind:  "function",
			Title: h.name,
			Plain: fmt.Sprintf("%s handler (%s)", strings.ToUpper(h.kind), h.code),
			Code:  []string{h.code},
			Auto:  true,
		})
		handlerIDs = append(handlerIDs, id)
		handlerByName[h.name] = id
		if dot := strings.LastIndexByte(h.name, '.'); dot >= 0 {
			handlerByName[h.name[dot+1:]] = id
		}
	}
	if len(handlerIDs) == 0 {
		id := addNode(Node{
			ID:    "fn_handler",
			Group: "handlers",
			Kind:  "function",
			Title: sd.service + ".Handle",
			Plain: "Primary service request/event handler",
			Auto:  true,
		})
		handlerIDs = append(handlerIDs, id)
	}

	// 3. Gateway / data store nodes.
	var gwIDs []string
	for _, k := range autoSortedGatewayKeys(sd.gateways) {
		gw := sd.gateways[k]
		addNode(Node{
			ID:    gw.id,
			Group: "gateways",
			Kind:  gw.kind,
			Title: gw.title,
			Plain: gw.plain,
			Code:  limitStrings(gw.codeRefs, 5),
			Auto:  true,
		})
		gwIDs = append(gwIDs, gw.id)
	}

	// Ensure any sinks referenced by invariants exist in sd.sinks.
	for _, inv := range sd.invariants {
		if inv.Sink != "" {
			addAutoSink(sd, inv.Sink, inv.Sink, "Output sink guarded by "+inv.Name, "")
		}
		for _, s := range inv.Sinks {
			if s != "" {
				addAutoSink(sd, s, s, "Output sink guarded by "+inv.Name, "")
			}
		}
	}

	// 4. Sink nodes.
	var sinkIDs []string
	sinkIDByName := map[string]string{}
	for _, k := range autoSortedSinkKeys(sd.sinks) {
		sk := sd.sinks[k]
		addNode(Node{
			ID:    sk.id,
			Group: "sinks",
			Kind:  "sink",
			Title: sk.title,
			Plain: sk.plain,
			Code:  limitStrings(sk.codeRefs, 5),
			Match: []Rule{{On: "output", Sink: sk.sink}},
			Auto:  true,
		})
		sinkIDs = append(sinkIDs, sk.id)
		sinkIDByName[sk.sink] = sk.id
	}
	if len(sinkIDs) == 0 {
		id := addNode(Node{
			ID:    "sink_output",
			Group: "sinks",
			Kind:  "sink",
			Title: "service:output",
			Plain: "Emitted service output",
			Match: []Rule{{On: "output", Sink: "service:output"}},
			Auto:  true,
		})
		sinkIDs = append(sinkIDs, id)
		sinkIDByName["service:output"] = id
	}

	// 5. Invariant nodes from .kavach/invariants.yaml.
	var invIDs []string
	if len(sd.invariants) > 0 {
		spec.Groups = append(spec.Groups, Group{
			ID:    "invariants",
			Title: "Invariants",
			Plain: "Declarative rules from .kavach/invariants.yaml checked on every replay & variant",
		})
		for _, inv := range sd.invariants {
			if inv.Name == "" {
				continue
			}
			id := makeAutoID("inv", inv.Name)
			plain := fmt.Sprintf("Declarative %s rule", inv.Kind)
			if inv.Sink != "" {
				plain += " on " + inv.Sink
			} else if len(inv.Sinks) > 0 {
				plain += " on " + strings.Join(inv.Sinks, ", ")
			}
			if inv.Key != "" {
				plain += " (key " + inv.Key + ")"
			}
			addNode(Node{
				ID:        id,
				Group:     "invariants",
				Kind:      "invariant",
				Title:     inv.Name,
				Plain:     plain,
				Code:      []string{".kavach/invariants.yaml"},
				Invariant: inv.Name,
				Auto:      true,
			})
			invIDs = append(invIDs, id)

			wired := false
			if inv.Sink != "" {
				if sid := sinkIDByName[inv.Sink]; sid != "" {
					addEdge(sid, id, "checks", inv.Kind)
					wired = true
				}
			}
			for _, s := range inv.Sinks {
				if sid := sinkIDByName[s]; sid != "" {
					addEdge(sid, id, "checks", inv.Kind)
					wired = true
				}
			}
			if !wired {
				for _, sid := range sinkIDs {
					addEdge(sid, id, "checks", inv.Kind)
				}
			}
		}
	}

	// 6. Wire Entry -> Handler edges.
	wiredHandler := map[string]bool{}
	for _, eid := range entryIDs {
		targetName := entryHandlerTarget[eid]
		if targetName != "" {
			if hid := handlerByName[targetName]; hid != "" {
				addEdge(eid, hid, "calls", "")
				wiredHandler[hid] = true
				continue
			}
			if dot := strings.LastIndexByte(targetName, '.'); dot >= 0 {
				if hid := handlerByName[targetName[dot+1:]]; hid != "" {
					addEdge(eid, hid, "calls", "")
					wiredHandler[hid] = true
					continue
				}
			}
		}
		// Connect to primary handler(s).
		addEdge(eid, handlerIDs[0], "calls", "")
		wiredHandler[handlerIDs[0]] = true
	}
	// If we only had a generic/fallback entry, fan out to remaining top handlers so the canvas stays connected.
	if len(entryIDs) == 1 {
		for _, hid := range handlerIDs {
			if !wiredHandler[hid] {
				addEdge(entryIDs[0], hid, "calls", "")
				wiredHandler[hid] = true
			}
		}
	}

	// 7. Wire Handler -> Gateway & Handler -> Sink edges.
	gwConnected := map[string]bool{}
	sinkConnected := map[string]bool{}
	handlerHasOut := map[string]bool{}
	for _, h := range sd.handlers {
		if h.id == "" {
			continue
		}
		if h.readsDB {
			addEdge(h.id, "gw_postgres", "reads", "SQL")
			gwConnected["gw_postgres"] = true
			handlerHasOut[h.id] = true
		}
		if h.writesDB {
			addEdge(h.id, "gw_postgres", "writes", "SQL")
			gwConnected["gw_postgres"] = true
			if sid := sinkIDByName["pg:exec"]; sid != "" {
				addEdge(h.id, sid, "emits", "")
				sinkConnected[sid] = true
			}
			handlerHasOut[h.id] = true
		}
		if h.usesRedis {
			addEdge(h.id, "gw_redis", "reads", "Redis")
			gwConnected["gw_redis"] = true
			handlerHasOut[h.id] = true
		}
		if h.usesHTTP {
			addEdge(h.id, "gw_http", "calls", "HTTP")
			gwConnected["gw_http"] = true
			handlerHasOut[h.id] = true
		}
		if h.usesKafkaP {
			addEdge(h.id, "gw_kafka", "writes", "Kafka")
			gwConnected["gw_kafka"] = true
			for _, t := range h.topics {
				if sid := sinkIDByName["kafka:"+t]; sid != "" {
					addEdge(h.id, sid, "emits", "")
					sinkConnected[sid] = true
				}
			}
			handlerHasOut[h.id] = true
		}
		if h.kind == "http" {
			if sid := sinkIDByName["http:response"]; sid != "" {
				addEdge(h.id, sid, "emits", "")
				sinkConnected[sid] = true
				handlerHasOut[h.id] = true
			}
		}
	}
	// Ensure every gateway and sink has at least one incoming edge from a handler.
	for _, gwid := range gwIDs {
		if !gwConnected[gwid] {
			addEdge(handlerIDs[0], gwid, "reads", "")
		}
	}
	for _, sid := range sinkIDs {
		if !sinkConnected[sid] {
			addEdge(handlerIDs[0], sid, "emits", "")
		}
	}

	// 8. Build a Day-0 guided walkthrough flow.
	var flowSteps []FlowStep
	flowSteps = append(flowSteps, FlowStep{
		Node: entryIDs[0],
		Text: "Inbound event or request enters the wrapped entrypoint boundary.",
	})
	flowSteps = append(flowSteps, FlowStep{
		Node: handlerIDs[0],
		Text: "Business logic executes unchanged inside the step context.",
	})
	if len(gwIDs) > 0 {
		flowSteps = append(flowSteps, FlowStep{
			Node: gwIDs[0],
			Text: "Gateway reads are recorded once and replayed deterministically offline (with fault injection on retry).",
		})
	}
	if len(sinkIDs) > 0 {
		flowSteps = append(flowSteps, FlowStep{
			Node: sinkIDs[0],
			Text: "Durable outputs are captured in memory and compared against baseline + invariants.",
		})
	}
	if len(invIDs) > 0 {
		flowSteps = append(flowSteps, FlowStep{
			Node: invIDs[0],
			Text: "Declarative invariants from .kavach/invariants.yaml are verified after every step and mutated variant.",
		})
	}
	spec.Flows = []Flow{{
		ID:    "day0-flow",
		Title: "Primary service flow",
		Plain: "Auto-synthesized end-to-end path from entrypoint through gateways and sinks to invariants.",
		Steps: flowSteps,
	}}

	_ = spec.NormalizeAndValidate()
	return spec
}

func addAutoGateway(sd *autoScanData, id, kind, title, plain, codeRef string) {
	gw, ok := sd.gateways[id]
	if !ok {
		gw = &autoGateway{id: id, kind: kind, title: title, plain: plain}
		sd.gateways[id] = gw
	}
	if codeRef != "" && !containsStr(gw.codeRefs, codeRef) {
		gw.codeRefs = append(gw.codeRefs, codeRef)
	}
}

func addAutoSink(sd *autoScanData, sink, title, plain, codeRef string) {
	sk, ok := sd.sinks[sink]
	if !ok {
		sk = &autoSink{id: makeAutoID("sink", sink), sink: sink, title: title, plain: plain}
		sd.sinks[sink] = sk
	}
	if codeRef != "" && !containsStr(sk.codeRefs, codeRef) {
		sk.codeRefs = append(sk.codeRefs, codeRef)
	}
}

// parseAutoInvariantsYAML reads declarative invariant rules from
// .kavach/invariants.yaml using the shared internal/kconfig parser.
func parseAutoInvariantsYAML(path string) ([]AutoInvariant, error) {
	invFile, err := kconfig.LoadInvariants(path)
	if err != nil {
		return nil, err
	}
	out := make([]AutoInvariant, 0, len(invFile.Invariants))
	for _, r := range invFile.Invariants {
		out = append(out, AutoInvariant{
			Name:  r.Name,
			Kind:  r.Kind,
			Sink:  r.Sink,
			Sinks: append([]string(nil), r.Sinks...),
			Key:   r.Key,
			Field: r.Field,
		})
	}
	return out, nil
}

var autoRouteMethods = map[string]string{
	"HandleFunc": "ANY", "Handle": "ANY",
	"GET": "GET", "Get": "GET",
	"POST": "POST", "Post": "POST",
	"PUT": "PUT", "Put": "PUT",
	"DELETE": "DELETE", "Delete": "DELETE",
	"PATCH": "PATCH", "Patch": "PATCH",
}

func classifyAutoHandler(fn *ast.FuncDecl, relPath string) string {
	if fn.Name == nil || fn.Name.Name == "main" || fn.Name.Name == "init" {
		return ""
	}
	if fn.Type != nil && fn.Type.Params != nil {
		var hasW, hasR bool
		for _, field := range fn.Type.Params.List {
			ts := autoExprString(field.Type)
			switch {
			case strings.HasSuffix(ts, "http.ResponseWriter"):
				hasW = true
			case strings.HasSuffix(ts, "http.Request"):
				hasR = true
			case strings.HasSuffix(ts, "gin.Context"), strings.HasSuffix(ts, "echo.Context"):
				return "http"
			case strings.HasSuffix(ts, "kgo.Record"), strings.HasSuffix(ts, "kafka.Message"), strings.HasSuffix(ts, "sarama.ConsumerMessage"):
				return "kafka"
			}
		}
		if hasW && hasR {
			return "http"
		}
	}
	lower := strings.ToLower(relPath)
	inPkg := strings.Contains(lower, "handler") || strings.Contains(lower, "processor") ||
		strings.Contains(lower, "consumer") || strings.Contains(lower, "service") || strings.Contains(lower, "api/")
	if inPkg && fn.Name.IsExported() {
		for _, p := range []string{"Handle", "Process", "Consume", "StartProcessing", "Serve"} {
			if strings.HasPrefix(fn.Name.Name, p) {
				if strings.Contains(strings.ToLower(fn.Name.Name), "event") || strings.Contains(strings.ToLower(fn.Name.Name), "process") {
					return "kafka"
				}
				return "handler"
			}
		}
	}
	return ""
}

func handlerScore(h autoHandler) int {
	score := 0
	if h.readsDB {
		score += 2
	}
	if h.writesDB {
		score += 3
	}
	if h.usesRedis {
		score += 2
	}
	if h.usesKafkaP {
		score += 4
	}
	if h.usesHTTP {
		score += 1
	}
	if h.kind == "kafka" || h.kind == "http" {
		score += 1
	}
	return score
}

func autoFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := strings.TrimPrefix(autoExprString(fn.Recv.List[0].Type), "*")
		if recv != "" {
			return recv + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

func autoExprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + autoExprString(x.X)
	case *ast.SelectorExpr:
		return autoExprString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + autoExprString(x.Elt)
	default:
		return ""
	}
}

func autoStringLit(e ast.Expr) (string, bool) {
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

func autoStructStringField(lit *ast.CompositeLit, field string) string {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
			if s, ok := autoStringLit(kv.Value); ok {
				return s
			}
		}
	}
	return ""
}

func autoHandlerArg(args []ast.Expr) string {
	if len(args) < 2 {
		return ""
	}
	switch fn := args[len(args)-1].(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return autoExprString(fn)
	}
	return ""
}

func autoLooksLikeTopic(s string) bool {
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

func autoSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata", "target", "dist", "build", "third_party", "coverage", "__tests__", "__mocks__":
		return true
	}
	return false
}

var (
	reAutoTSRoute       = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\.(get|post|put|patch|delete|all)\s*\(\s*['"](/[^'"]*)['"](?:\s*,\s*([A-Za-z_$][\w$.]*))?`)
	reAutoTSNextApp     = regexp.MustCompile(`\bexport\s+(?:async\s+)?(?:function|const)\s+(GET|POST|PUT|PATCH|DELETE)\b`)
	reAutoTSNextPages   = regexp.MustCompile(`\bexport\s+default\s+(?:async\s+)?function(?:\s+([A-Za-z_$][\w$]*))?`)
	reAutoTSKafkaSub    = regexp.MustCompile(`\.subscribe\s*\(`)
	reAutoTSKafkaSend   = regexp.MustCompile(`\.send\s*\(`)
	reAutoTSKafkaTopic  = regexp.MustCompile(`\btopic\s*:\s*['"]([A-Za-z0-9._-]+)['"]`)
	reAutoTSKafkaTopics = regexp.MustCompile(`\btopics\s*:\s*\[([^\]]+)\]`)
	reAutoTSStringLit   = regexp.MustCompile(`['"]([A-Za-z0-9._-]+)['"]`)
	reAutoTSKafkaRun    = regexp.MustCompile(`\b(eachMessage|eachBatch)\s*[:(]`)
	reAutoTSSQLWrite    = regexp.MustCompile(`(?i)\b(?:INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM)\b`)
	reAutoTSSQLRead     = regexp.MustCompile(`(?i)\bSELECT\b`)
	reAutoTSPGQuery     = regexp.MustCompile(`(?:\.query|\.execute)\s*\(`)
	reAutoTSPrismaRead  = regexp.MustCompile(`\.find(?:Many|First|Unique|FirstOrThrow|UniqueOrThrow)\s*\(`)
	reAutoTSPrismaWrite = regexp.MustCompile(`\.(?:create|createMany|update|updateMany|upsert|delete|deleteMany)\s*\(`)
	reAutoTSDrizzleRd   = regexp.MustCompile(`\.select\s*\(`)
	reAutoTSDrizzleWr   = regexp.MustCompile(`\.(?:insert|update|delete)\s*\(`)
	reAutoTSRedisOp     = regexp.MustCompile(`\b[A-Za-z0-9_$]*(?:redis|rdb|cache)[A-Za-z0-9_$]*\.(get|set|setnx|hget|hset|hgetall|del|incr|incrby|sadd|sismember|zadd|zrem|zrange|exists|expire|pipeline|multi|ping)\s*\(`)
	reAutoTSFetch       = regexp.MustCompile(`\bfetch\s*\(`)
	reAutoTSPGInit      = regexp.MustCompile(`\b(?:new\s+(?:pg\.)?(?:Pool|Client)|wrapPool|createReplayPool|new\s+PrismaClient|drizzle)\s*\(`)
	reAutoTSRedisInit   = regexp.MustCompile(`\b(?:new\s+(?:Redis|IORedis)|wrapRedis|createReplayRedis|createClient)\s*\(`)
	reAutoTSKafkaInit   = regexp.MustCompile(`\b(?:new\s+Kafka|wrapKafka|createReplayKafka)\s*\(`)
)

func isAutoTSFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".d.ts") || strings.Contains(lower, ".test.") || strings.Contains(lower, ".spec.") ||
		strings.HasSuffix(lower, ".config.ts") || strings.HasSuffix(lower, ".config.js") || strings.HasSuffix(lower, ".config.mjs") {
		return false
	}
	for _, ext := range []string{".ts", ".tsx", ".mts", ".js", ".mjs"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func scanAutoTSFiles(root string, sd *autoScanData) error {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if d.IsDir() {
			if path != root && autoSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isAutoTSFile(d.Name()) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return err
	}
	sort.Strings(files)
	if sd.language == "" {
		sd.language = "typescript"
	}

	routeSeen := map[string]bool{}
	for _, r := range sd.routes {
		routeSeen[r.method+" "+r.path] = true
	}
	consumeTopics := map[string]string{}
	produceTopics := map[string]string{}
	allTopics := map[string]string{}

	for _, fullPath := range files {
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		sd.files++
		rel, _ := filepath.Rel(root, fullPath)
		rel = filepath.ToSlash(rel)

		cleaned := stripAutoJSComments(string(raw))
		lines := strings.Split(cleaned, "\n")

		var fileReadsDB, fileWritesDB, fileUsesRedis, fileUsesKafkaP, fileUsesHTTP bool
		var fileTopics []string
		var fileHandlers []autoHandler

		nextAppPath, isNextApp := autoNextAppRoute(rel)
		nextPagesPath, isNextPages := autoNextPagesRoute(rel)

		// Track whether we're near a subscribe() or send() block (3-line lookahead window).
		inSubscribeWindow := 0
		inSendWindow := 0

		for idx, line := range lines {
			lineNum := idx + 1
			ref := fmt.Sprintf("%s:%d", rel, lineNum)
			lowerLine := strings.ToLower(line)

			if inSubscribeWindow > 0 {
				inSubscribeWindow--
			}
			if inSendWindow > 0 {
				inSendWindow--
			}

			if reAutoTSPGInit.MatchString(line) {
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
			}
			if reAutoTSRedisInit.MatchString(line) {
				addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache and coordination reads/writes", ref)
			}
			if reAutoTSKafkaInit.MatchString(line) {
				addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
			}

			// Routes (Express/Fastify/Hono).
			if m := reAutoTSRoute.FindStringSubmatch(line); m != nil {
				recv := strings.ToLower(m[1])
				if !strings.Contains(recv, "redis") && !strings.Contains(recv, "cache") && !strings.Contains(recv, "map") && !strings.Contains(recv, "headers") {
					method := strings.ToUpper(m[2])
					if method == "ALL" {
						method = "ANY"
					}
					path := m[3]
					key := method + " " + path
					hName := m[4]
					if hName == "" || hName == "async" || hName == "function" {
						hName = key
					}
					if !routeSeen[key] {
						routeSeen[key] = true
						sd.routes = append(sd.routes, autoRoute{method: method, path: path, handler: hName, code: ref})
					}
					addAutoSink(sd, "http:response", "http:response", "HTTP response emitted to caller", "")
					fileHandlers = append(fileHandlers, autoHandler{name: hName, kind: "http", code: ref})
				}
			}

			// Next.js App Router / Pages API.
			if isNextApp {
				if m := reAutoTSNextApp.FindStringSubmatch(line); m != nil {
					method := m[1]
					key := method + " " + nextAppPath
					if !routeSeen[key] {
						routeSeen[key] = true
						sd.routes = append(sd.routes, autoRoute{method: method, path: nextAppPath, handler: key, code: ref})
					}
					addAutoSink(sd, "http:response", "http:response", "HTTP response emitted to caller", "")
					fileHandlers = append(fileHandlers, autoHandler{name: key, kind: "http", code: ref})
				}
			}
			if isNextPages {
				if m := reAutoTSNextPages.FindStringSubmatch(line); m != nil {
					hName := m[1]
					if hName == "" {
						hName = "handler"
					}
					key := "ANY " + nextPagesPath
					if !routeSeen[key] {
						routeSeen[key] = true
						sd.routes = append(sd.routes, autoRoute{method: "ANY", path: nextPagesPath, handler: hName, code: ref})
					}
					addAutoSink(sd, "http:response", "http:response", "HTTP response emitted to caller", "")
					fileHandlers = append(fileHandlers, autoHandler{name: hName, kind: "http", code: ref})
				}
			}

			// KafkaJS consumer & producer.
			if reAutoTSKafkaSub.MatchString(line) {
				inSubscribeWindow = 4
				addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
			}
			if reAutoTSKafkaSend.MatchString(line) {
				inSendWindow = 4
				fileUsesKafkaP = true
				addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
			}
			if m := reAutoTSKafkaRun.FindStringSubmatch(line); m != nil {
				fileHandlers = append(fileHandlers, autoHandler{name: m[1], kind: "kafka", code: ref})
				addAutoGateway(sd, "gw_kafka", "external", "Kafka", "Kafka event stream", ref)
			}
			for _, m := range reAutoTSKafkaTopic.FindAllStringSubmatch(line, -1) {
				t := m[1]
				if autoLooksLikeTopic(t) {
					allTopics[t] = ref
					if inSubscribeWindow > 0 {
						consumeTopics[t] = ref
					} else if inSendWindow > 0 {
						produceTopics[t] = ref
						fileUsesKafkaP = true
						fileTopics = append(fileTopics, t)
					}
				}
			}
			for _, m := range reAutoTSKafkaTopics.FindAllStringSubmatch(line, -1) {
				for _, sm := range reAutoTSStringLit.FindAllStringSubmatch(m[1], -1) {
					t := sm[1]
					if autoLooksLikeTopic(t) {
						allTopics[t] = ref
						if inSubscribeWindow > 0 {
							consumeTopics[t] = ref
						}
					}
				}
			}

			// SQL / Prisma / Drizzle operations.
			if reAutoTSPGQuery.MatchString(line) {
				if reAutoTSSQLWrite.MatchString(line) {
					fileWritesDB = true
					addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
					addAutoSink(sd, "pg:exec", "pg:exec", "SQL write statements (INSERT/UPDATE/DELETE)", ref)
				} else {
					fileReadsDB = true
					addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
				}
			} else if reAutoTSSQLWrite.MatchString(line) && (strings.Contains(lowerLine, "pool") || strings.Contains(lowerLine, "client") || strings.Contains(lowerLine, "sql")) {
				fileWritesDB = true
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
				addAutoSink(sd, "pg:exec", "pg:exec", "SQL write statements (INSERT/UPDATE/DELETE)", ref)
			} else if reAutoTSSQLRead.MatchString(line) && (strings.Contains(lowerLine, "from ") || strings.Contains(lowerLine, "where ")) {
				fileReadsDB = true
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
			}

			if reAutoTSPrismaRead.MatchString(line) || (strings.Contains(lowerLine, "db.") && reAutoTSDrizzleRd.MatchString(line)) {
				fileReadsDB = true
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
			}
			if (strings.Contains(lowerLine, "prisma.") && reAutoTSPrismaWrite.MatchString(line)) || (strings.Contains(lowerLine, "db.") && reAutoTSDrizzleWr.MatchString(line)) {
				fileWritesDB = true
				addAutoGateway(sd, "gw_postgres", "state", "PostgreSQL / SQL", "SQL queries and transactions", ref)
				addAutoSink(sd, "pg:exec", "pg:exec", "SQL write statements (INSERT/UPDATE/DELETE)", ref)
			}

			// Redis operations.
			if reAutoTSRedisOp.MatchString(lowerLine) {
				fileUsesRedis = true
				addAutoGateway(sd, "gw_redis", "state", "Redis", "Redis cache and coordination reads/writes", ref)
			}

			// Outbound HTTP fetch.
			if reAutoTSFetch.MatchString(line) {
				fileUsesHTTP = true
				addAutoGateway(sd, "gw_http", "external", "Outbound HTTP", "External HTTP fetch calls", ref)
			}
		}

		for i := range fileHandlers {
			fileHandlers[i].readsDB = fileReadsDB
			fileHandlers[i].writesDB = fileWritesDB
			fileHandlers[i].usesRedis = fileUsesRedis
			fileHandlers[i].usesKafkaP = fileUsesKafkaP
			fileHandlers[i].usesHTTP = fileUsesHTTP
			fileHandlers[i].topics = fileTopics
			sd.handlers = append(sd.handlers, fileHandlers[i])
		}
	}

	if len(produceTopics) == 0 && len(allTopics) > 0 {
		for t, ref := range allTopics {
			if _, isConsume := consumeTopics[t]; !isConsume {
				produceTopics[t] = ref
			}
		}
	}
	for _, topic := range autoSortedMapKeys(produceTopics) {
		sinkName := "kafka:" + topic
		addAutoSink(sd, sinkName, sinkName, "Kafka produce topic "+topic, produceTopics[topic])
	}
	if _, hasKafka := sd.gateways["gw_kafka"]; hasKafka && len(produceTopics) == 0 {
		addAutoSink(sd, "kafka:produce", "kafka:produce", "Kafka producer output stream", "")
	}
	for _, topic := range autoSortedMapKeys(consumeTopics) {
		key := "KAFKA " + topic
		if !routeSeen[key] {
			routeSeen[key] = true
			sd.routes = append(sd.routes, autoRoute{method: "KAFKA", path: topic, code: consumeTopics[topic]})
		}
	}
	return nil
}

func autoNextAppRoute(relPath string) (string, bool) {
	p := filepath.ToSlash(relPath)
	if !strings.HasPrefix(filepath.Base(p), "route.") {
		return "", false
	}
	for _, marker := range []string{"src/app/", "app/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			sub := strings.TrimPrefix(p[idx+len(marker):], "/")
			dir := filepath.Dir(sub)
			if dir == "." || dir == "" {
				return "/", true
			}
			return "/" + dir, true
		}
	}
	return "", false
}

func autoNextPagesRoute(relPath string) (string, bool) {
	p := filepath.ToSlash(relPath)
	for _, marker := range []string{"src/pages/api/", "pages/api/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			sub := p[idx+len(marker):]
			sub = strings.TrimSuffix(sub, filepath.Ext(sub))
			sub = strings.TrimSuffix(sub, "/index")
			if sub == "" || sub == "index" {
				return "/api", true
			}
			return "/api/" + sub, true
		}
	}
	return "", false
}

func stripAutoJSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	n := len(src)
	var quote byte
	inLine, inBlock := false, false
	for i := 0; i < n; i++ {
		c := src[i]
		if inLine {
			if c == '\n' {
				inLine = false
				b.WriteByte('\n')
			}
			continue
		}
		if inBlock {
			if c == '\n' {
				b.WriteByte('\n')
			} else if c == '*' && i+1 < n && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < n {
				i++
				b.WriteByte(src[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < n {
			if src[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if src[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func makeAutoID(prefix, raw string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('_')
	prevUS := true
	for _, r := range strings.ToLower(raw) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUS = false
		default:
			if !prevUS {
				b.WriteByte('_')
				prevUS = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "_")
	if s == prefix {
		return prefix + "_node"
	}
	return s
}

func cleanAutoName(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimPrefix(s, "@")
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-_")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func containsStr(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

func limitStrings(slice []string, max int) []string {
	if len(slice) <= max {
		return slice
	}
	return slice[:max]
}

func autoSortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func autoSortedGatewayKeys(m map[string]*autoGateway) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func autoSortedSinkKeys(m map[string]*autoSink) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
