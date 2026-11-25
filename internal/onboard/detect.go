// Package onboard provides repository detection, Go AST static diagnostics
// (kavach doctor), and Day-0 scaffolding for frictionless Kavach onboarding.
package onboard

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Supported language identifiers.
const (
	LangGo         = "go"
	LangTypeScript = "typescript"
	LangRust       = "rust"
	LangUnknown    = "unknown"
)

// Driver describes a detected I/O framework or client library.
type Driver struct {
	Name       string `json:"name"`
	Language   string `json:"language"`
	Category   string `json:"category"` // database, cache, queue, http, rpc
	ImportPath string `json:"import_path"`
	Adapter    string `json:"adapter"`
	Source     string `json:"source"` // go.mod, ast, package.json, Cargo.toml
	Version    string `json:"version,omitempty"`
}

// Detection summarizes the language, service name, manifests, and drivers
// discovered in a repository.
type Detection struct {
	Dir       string   `json:"dir"`
	Service   string   `json:"service"`
	Language  string   `json:"language"`
	Languages []string `json:"languages,omitempty"`
	Module    string   `json:"module,omitempty"`
	Manifests []string `json:"manifests,omitempty"`
	Drivers   []Driver `json:"drivers"`
}

type driverSpec struct {
	matchPrefix string
	exact       bool
	driver      Driver
}

var goManifestDrivers = []driverSpec{
	{matchPrefix: "github.com/jackc/pgx/v5", driver: Driver{Name: "pgx/v5", Language: LangGo, Category: "database", ImportPath: "github.com/jackc/pgx/v5", Adapter: "kavachpgx"}},
	{matchPrefix: "github.com/jackc/pgx", driver: Driver{Name: "pgx/v5", Language: LangGo, Category: "database", ImportPath: "github.com/jackc/pgx", Adapter: "kavachpgx"}},
	{matchPrefix: "gorm.io/gorm", driver: Driver{Name: "gorm", Language: LangGo, Category: "database", ImportPath: "gorm.io/gorm", Adapter: "kavachpgx"}},
	{matchPrefix: "gorm.io/driver/postgres", driver: Driver{Name: "gorm", Language: LangGo, Category: "database", ImportPath: "gorm.io/driver/postgres", Adapter: "kavachpgx"}},
	{matchPrefix: "github.com/redis/go-redis/v9", driver: Driver{Name: "go-redis/v9", Language: LangGo, Category: "cache", ImportPath: "github.com/redis/go-redis/v9", Adapter: "kavachredis"}},
	{matchPrefix: "github.com/redis/go-redis", driver: Driver{Name: "go-redis/v9", Language: LangGo, Category: "cache", ImportPath: "github.com/redis/go-redis", Adapter: "kavachredis"}},
	{matchPrefix: "github.com/go-redis/redis", driver: Driver{Name: "go-redis/v9", Language: LangGo, Category: "cache", ImportPath: "github.com/go-redis/redis", Adapter: "kavachredis"}},
	{matchPrefix: "github.com/twmb/franz-go", driver: Driver{Name: "franz-go", Language: LangGo, Category: "queue", ImportPath: "github.com/twmb/franz-go", Adapter: "kavachkgo"}},
	{matchPrefix: "github.com/segmentio/kafka-go", driver: Driver{Name: "kafka-go", Language: LangGo, Category: "queue", ImportPath: "github.com/segmentio/kafka-go", Adapter: "kavachkgo"}},
	{matchPrefix: "github.com/IBM/sarama", driver: Driver{Name: "sarama", Language: LangGo, Category: "queue", ImportPath: "github.com/IBM/sarama", Adapter: "kavachkgo"}},
	{matchPrefix: "github.com/Shopify/sarama", driver: Driver{Name: "sarama", Language: LangGo, Category: "queue", ImportPath: "github.com/Shopify/sarama", Adapter: "kavachkgo"}},
	{matchPrefix: "github.com/gin-gonic/gin", driver: Driver{Name: "gin", Language: LangGo, Category: "http", ImportPath: "github.com/gin-gonic/gin", Adapter: "kavachhttp"}},
	{matchPrefix: "github.com/go-chi/chi", driver: Driver{Name: "chi", Language: LangGo, Category: "http", ImportPath: "github.com/go-chi/chi", Adapter: "kavachhttp"}},
	{matchPrefix: "github.com/labstack/echo", driver: Driver{Name: "echo", Language: LangGo, Category: "http", ImportPath: "github.com/labstack/echo", Adapter: "kavachhttp"}},
	{matchPrefix: "google.golang.org/grpc", driver: Driver{Name: "grpc", Language: LangGo, Category: "rpc", ImportPath: "google.golang.org/grpc", Adapter: "kavachgrpc"}},
}

var goStdlibDrivers = map[string]Driver{
	"database/sql": {Name: "database/sql", Language: LangGo, Category: "database", ImportPath: "database/sql", Adapter: "kavachpgx", Source: "ast"},
	"net/http":     {Name: "net/http", Language: LangGo, Category: "http", ImportPath: "net/http", Adapter: "kavachhttp", Source: "ast"},
}

var tsManifestDrivers = map[string]Driver{
	"pg":              {Name: "pg", Language: LangTypeScript, Category: "database", ImportPath: "pg", Adapter: "@kavach/pg"},
	"@kavach/pg":      {Name: "pg", Language: LangTypeScript, Category: "database", ImportPath: "@kavach/pg", Adapter: "@kavach/pg"},
	"prisma":          {Name: "prisma", Language: LangTypeScript, Category: "database", ImportPath: "prisma", Adapter: "@kavach/pg"},
	"@prisma/client":  {Name: "prisma", Language: LangTypeScript, Category: "database", ImportPath: "@prisma/client", Adapter: "@kavach/pg"},
	"drizzle-orm":     {Name: "drizzle-orm", Language: LangTypeScript, Category: "database", ImportPath: "drizzle-orm", Adapter: "@kavach/pg"},
	"ioredis":         {Name: "ioredis", Language: LangTypeScript, Category: "cache", ImportPath: "ioredis", Adapter: "@kavach/ioredis"},
	"@kavach/ioredis": {Name: "ioredis", Language: LangTypeScript, Category: "cache", ImportPath: "@kavach/ioredis", Adapter: "@kavach/ioredis"},
	"redis":           {Name: "redis", Language: LangTypeScript, Category: "cache", ImportPath: "redis", Adapter: "@kavach/ioredis"},
	"kafkajs":         {Name: "kafkajs", Language: LangTypeScript, Category: "queue", ImportPath: "kafkajs", Adapter: "@kavach/kafkajs"},
	"@kavach/kafkajs": {Name: "kafkajs", Language: LangTypeScript, Category: "queue", ImportPath: "@kavach/kafkajs", Adapter: "@kavach/kafkajs"},
	"express":         {Name: "express", Language: LangTypeScript, Category: "http", ImportPath: "express", Adapter: "@kavach/http"},
	"fastify":         {Name: "fastify", Language: LangTypeScript, Category: "http", ImportPath: "fastify", Adapter: "@kavach/http"},
	"hono":            {Name: "hono", Language: LangTypeScript, Category: "http", ImportPath: "hono", Adapter: "@kavach/http"},
	"next":            {Name: "next", Language: LangTypeScript, Category: "http", ImportPath: "next", Adapter: "@kavach/http"},
	"@kavach/http":    {Name: "node:http", Language: LangTypeScript, Category: "http", ImportPath: "@kavach/http", Adapter: "@kavach/http"},
}

var tsStdlibDrivers = map[string]Driver{
	"http":      {Name: "node:http", Language: LangTypeScript, Category: "http", ImportPath: "node:http", Adapter: "@kavach/http", Source: "ast"},
	"node:http": {Name: "node:http", Language: LangTypeScript, Category: "http", ImportPath: "node:http", Adapter: "@kavach/http", Source: "ast"},
}

var rustManifestDrivers = map[string]Driver{
	"axum":    {Name: "axum", Language: LangRust, Category: "http", ImportPath: "axum", Adapter: "kavach-axum"},
	"sqlx":    {Name: "sqlx", Language: LangRust, Category: "database", ImportPath: "sqlx", Adapter: "kavach-sqlx"},
	"redis":   {Name: "redis", Language: LangRust, Category: "cache", ImportPath: "redis", Adapter: "kavach-redis"},
	"rdkafka": {Name: "rdkafka", Language: LangRust, Category: "queue", ImportPath: "rdkafka", Adapter: "kavach-rdkafka"},
}

// DetectRepo inspects go.mod, package.json, and Cargo.toml under dir to detect
// the repository's primary language, service name, and I/O drivers.
func DetectRepo(dir string) (*Detection, error) {
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	det := &Detection{
		Dir:      absDir,
		Language: LangUnknown,
		Drivers:  []Driver{},
	}

	seenDrivers := map[string]Driver{}
	langSet := map[string]bool{}

	manifests := findManifests(absDir)
	for _, m := range manifests {
		rel, err := filepath.Rel(absDir, m)
		if err != nil {
			rel = filepath.Base(m)
		}
		rel = filepath.ToSlash(rel)
		det.Manifests = append(det.Manifests, rel)

		switch filepath.Base(m) {
		case "go.mod":
			langSet[LangGo] = true
			mod, drv := parseGoMod(m, rel)
			if det.Module == "" && mod != "" {
				det.Module = mod
			}
			if det.Service == "" && mod != "" {
				det.Service = serviceNameFromModule(mod)
			}
			for _, d := range drv {
				mergeDriver(seenDrivers, d)
			}
		case "package.json":
			langSet[LangTypeScript] = true
			pkgName, drv := parsePackageJSON(m, rel)
			if det.Service == "" && pkgName != "" {
				det.Service = sanitizeServiceName(pkgName)
			}
			for _, d := range drv {
				mergeDriver(seenDrivers, d)
			}
		case "Cargo.toml":
			langSet[LangRust] = true
			crateName, drv := parseCargoToml(m, rel)
			if det.Service == "" && crateName != "" {
				det.Service = sanitizeServiceName(crateName)
			}
			for _, d := range drv {
				mergeDriver(seenDrivers, d)
			}
		}
	}

	// Deterministic primary language priority: Go -> TypeScript -> Rust.
	for _, lang := range []string{LangGo, LangTypeScript, LangRust} {
		if langSet[lang] {
			det.Languages = append(det.Languages, lang)
			if det.Language == LangUnknown {
				det.Language = lang
			}
		}
	}

	if det.Service == "" {
		det.Service = sanitizeServiceName(filepath.Base(absDir))
	}
	if det.Service == "" {
		det.Service = "service"
	}

	det.Drivers = sortedDrivers(seenDrivers)
	return det, nil
}

func findManifests(root string) []string {
	names := []string{"go.mod", "package.json", "Cargo.toml"}
	var found []string
	for _, name := range names {
		p := filepath.Join(root, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return found
	}
	// Fallback: check up to depth 2 for monorepo roots.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || shouldSkipDir(e.Name()) {
			continue
		}
		sub := filepath.Join(root, e.Name())
		for _, name := range names {
			p := filepath.Join(sub, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				found = append(found, p)
			}
		}
	}
	sort.Strings(found)
	return found
}

func parseGoMod(path, source string) (module string, drivers []Driver) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	inRequire := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "module ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				module = strings.Trim(fields[1], `"`)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "require (") || trimmed == "require(" {
			inRequire = true
			continue
		}
		if inRequire && trimmed == ")" {
			inRequire = false
			continue
		}

		// Ignore indirect dependencies so transitive packages do not pollute driver detection.
		if strings.Contains(raw, "// indirect") {
			continue
		}

		line := trimmed
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if strings.HasPrefix(line, "require ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		} else if !inRequire {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		modPath := strings.Trim(fields[0], `"`)
		ver := ""
		if len(fields) >= 2 {
			ver = fields[1]
		}
		if d, ok := matchGoDriver(modPath, ver, source); ok {
			drivers = append(drivers, d)
		}
	}
	return module, drivers
}

func matchGoDriver(importPath, ver, source string) (Driver, bool) {
	for _, spec := range goManifestDrivers {
		if importPath == spec.matchPrefix || strings.HasPrefix(importPath, spec.matchPrefix+"/") {
			d := spec.driver
			d.Source = source
			d.Version = ver
			return d, true
		}
	}
	return Driver{}, false
}

func parsePackageJSON(path, source string) (name string, drivers []Driver) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	var pkg struct {
		Name                 string            `json:"name"`
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", nil
	}
	check := func(deps map[string]string) {
		for dep, ver := range deps {
			if d, ok := tsManifestDrivers[dep]; ok {
				d.Source = source
				d.Version = ver
				drivers = append(drivers, d)
			}
		}
	}
	// Check real driver packages before @kavach/* aliases so peer/optional real driver versions win cleanly.
	check(pkg.PeerDependencies)
	check(pkg.OptionalDependencies)
	check(pkg.Dependencies)
	check(pkg.DevDependencies)
	return pkg.Name, drivers
}

func parseCargoToml(path, source string) (name string, drivers []Driver) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		if section == "package" && key == "name" && name == "" {
			name = strings.Trim(val, `"'`)
			continue
		}
		if section == "dependencies" || section == "workspace.dependencies" || strings.HasSuffix(section, ".dependencies") {
			if d, ok := rustManifestDrivers[key]; ok {
				d.Source = source
				d.Version = extractCargoVersion(val)
				drivers = append(drivers, d)
			}
		}
	}
	return name, drivers
}

func extractCargoVersion(val string) string {
	val = strings.TrimSpace(val)
	if strings.HasPrefix(val, `"`) || strings.HasPrefix(val, `'`) {
		return strings.Trim(val, `"'`)
	}
	// Inline table: { version = "0.7", features = [...] }
	if idx := strings.Index(val, "version"); idx >= 0 {
		rest := val[idx+len("version"):]
		if eq := strings.IndexByte(rest, '='); eq >= 0 {
			rest = strings.TrimSpace(rest[eq+1:])
			if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
				q := rest[0]
				if end := strings.IndexByte(rest[1:], q); end >= 0 {
					return rest[1 : 1+end]
				}
			}
		}
	}
	return ""
}

func mergeDriver(seen map[string]Driver, d Driver) {
	key := d.Language + ":" + d.Name
	if prev, exists := seen[key]; exists {
		if prev.Version == "" && d.Version != "" {
			prev.Version = d.Version
			seen[key] = prev
		}
		return
	}
	seen[key] = d
}

func sortedDrivers(seen map[string]Driver) []Driver {
	out := make([]Driver, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func serviceNameFromModule(mod string) string {
	mod = strings.TrimSpace(mod)
	mod = strings.TrimSuffix(mod, "/")
	parts := strings.Split(mod, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		// Skip version suffixes like v2, v3.
		if len(p) >= 2 && p[0] == 'v' && isAllDigits(p[1:]) {
			continue
		}
		if s := sanitizeServiceName(p); s != "" {
			return s
		}
	}
	return ""
}

func sanitizeServiceName(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimPrefix(s, "@")
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func shouldSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata", "target", "dist", "build", "third_party", "coverage", "__tests__", "__mocks__":
		return true
	}
	return false
}
