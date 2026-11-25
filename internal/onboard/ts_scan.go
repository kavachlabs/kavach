package onboard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	reTSImportFrom = regexp.MustCompile(`(?m)\b(?:import|export)\b[^;\n]*?\bfrom\s*['"]([^'"]+)['"]`)
	reTSRequire    = regexp.MustCompile(`\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)`)

	// Driver initialization patterns.
	reTSNewPool       = regexp.MustCompile(`\b(?:new\s+(?:pg\.)?Pool|wrapPool|createReplayPool)\s*\(`)
	reTSNewPGClient   = regexp.MustCompile(`\b(?:new\s+(?:pg\.)?Client|wrapClient)\s*\(`)
	reTSNewPrisma     = regexp.MustCompile(`\bnew\s+PrismaClient\s*\(`)
	reTSDrizzleInit   = regexp.MustCompile(`\bdrizzle\s*\(`)
	reTSNewRedis      = regexp.MustCompile(`\b(?:new\s+(?:Redis|IORedis|Cluster)|wrapRedis|createReplayRedis)\s*\(`)
	reTSCreateRedis   = regexp.MustCompile(`\bcreateClient\s*\(`)
	reTSNewKafka      = regexp.MustCompile(`\b(?:new\s+Kafka|wrapKafka|createReplayKafka)\s*\(`)
	reTSKafkaProducer = regexp.MustCompile(`\.[ \t]*producer\s*\(`)
	reTSKafkaConsumer = regexp.MustCompile(`\.[ \t]*consumer\s*\(`)
	reTSExpressInit   = regexp.MustCompile(`\bexpress\s*\(\s*\)`)
	reTSFastifyInit   = regexp.MustCompile(`\b[Ff]astify\s*\(`)
	reTSHonoInit      = regexp.MustCompile(`\bnew\s+Hono\s*\(`)
	reTSHTTPCreateSrv = regexp.MustCompile(`\b(?:(?:http\.)?createServer|wrapHandler)\s*\(`)

	// Express / Fastify / Hono route registration: app.post('/v1/orders', handler)
	reTSRouteReg = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\.(get|post|put|patch|delete|all|route)\s*\(\s*['"](/[^'"]*)['"](?:\s*,\s*([A-Za-z_$][\w$.]*))?`)

	// Next.js App Router exported HTTP method handlers: export async function POST(...)
	reTSNextAppHandler  = regexp.MustCompile(`\bexport\s+(?:async\s+)?function\s+(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\b`)
	reTSNextAppConst    = regexp.MustCompile(`\bexport\s+const\s+(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s*=`)
	reTSNextPagesExport = regexp.MustCompile(`\bexport\s+default\s+(?:async\s+)?function(?:\s+([A-Za-z_$][\w$]*))?`)

	// KafkaJS subscribe/send/run handlers and topics.
	reTSKafkaRun       = regexp.MustCompile(`\b(?:eachMessage|eachBatch)\s*[:(]`)
	reTSKafkaTopicProp = regexp.MustCompile(`\btopic\s*:\s*['"]([A-Za-z0-9._-]+)['"]`)
	reTSKafkaTopicsArr = regexp.MustCompile(`\btopics\s*:\s*\[([^\]]+)\]`)
	reTSStringLiteral  = regexp.MustCompile(`['"]([A-Za-z0-9._-]+)['"]`)
	reTSEnvTopicDef    = regexp.MustCompile(`process\.env\.[A-Z0-9_]*TOPIC[A-Z0-9_]*\s*(?:\?\?|\|\|)\s*['"]([A-Za-z0-9._-]+)['"]`)

	// Bare clock & rand/UUID calls.
	reTSDateNow      = regexp.MustCompile(`\bDate\.now\s*\(\s*\)`)
	reTSNewDateEmpty = regexp.MustCompile(`\bnew\s+Date\s*\(\s*\)`)
	reTSPerfNow      = regexp.MustCompile(`\bperformance\.now\s*\(\s*\)`)
	reTSMathRandom   = regexp.MustCompile(`\bMath\.random\s*\(\s*\)`)
	reTSRandomUUID   = regexp.MustCompile(`\b(?:crypto\.)?randomUUID\s*\(\s*\)`)
	reTSRandomBytes  = regexp.MustCompile(`\b(?:crypto\.)?randomBytes\s*\(`)

	// Function context tracking for reporting enclosing function names.
	reTSFuncDecl  = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`)
	reTSArrowDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?\(`)
	reTSMethodDec = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+)?(?:async\s+)?([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::[^{]+)?\{`)
)

func isTSSourceFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".d.ts") ||
		strings.Contains(lower, ".test.") ||
		strings.Contains(lower, ".spec.") ||
		strings.HasSuffix(lower, ".config.ts") ||
		strings.HasSuffix(lower, ".config.js") ||
		strings.HasSuffix(lower, ".config.mjs") {
		return false
	}
	for _, ext := range []string{".ts", ".tsx", ".mts", ".js", ".mjs"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func collectTSSourceFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if d.IsDir() {
			if path != root && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isTSSourceFile(d.Name()) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func scanTSRepo(
	root string,
	seenDrivers map[string]Driver,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) error {
	files, err := collectTSSourceFiles(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	if rep.Language == LangUnknown {
		rep.Language = LangTypeScript
	}

	for _, fullPath := range files {
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		rep.FilesScanned++
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			rel = filepath.Base(fullPath)
		}
		rel = filepath.ToSlash(rel)

		scanTSFile(string(raw), rel, seenDrivers, rep, topicSet, routeSet)
	}
	return nil
}

func scanTSFile(
	src, relPath string,
	seenDrivers map[string]Driver,
	rep *DoctorReport,
	topicSet map[string]bool,
	routeSet map[string]bool,
) {
	cleaned := stripJSCommentsPreserveLines(src)

	imported := map[string]bool{}
	for _, m := range reTSImportFrom.FindAllStringSubmatch(cleaned, -1) {
		registerTSImport(m[1], imported, seenDrivers)
	}
	for _, m := range reTSRequire.FindAllStringSubmatch(cleaned, -1) {
		registerTSImport(m[1], imported, seenDrivers)
	}

	hasPG := imported["pg"] || imported["@kavach/pg"]
	hasPrisma := imported["@prisma/client"] || imported["prisma"]
	hasDrizzle := hasPrefixKey(imported, "drizzle-orm")
	hasIORedis := imported["ioredis"] || imported["@kavach/ioredis"]
	hasNodeRedis := imported["redis"]
	hasKafkaJS := imported["kafkajs"] || imported["@kavach/kafkajs"]
	hasExpress := imported["express"]
	hasFastify := imported["fastify"]
	hasHono := imported["hono"]
	hasHTTP := imported["http"] || imported["node:http"] || imported["@kavach/http"]
	hasCrypto := imported["crypto"] || imported["node:crypto"]

	nextAppRoute, isNextApp := nextAppRoutePath(relPath)
	nextPagesRoute, isNextPages := nextPagesAPIRoutePath(relPath)

	lines := strings.Split(cleaned, "\n")
	curFunc := ""

	for idx, line := range lines {
		lineNum := idx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Track enclosing function name heuristically for clearer diagnostics.
		if m := reTSFuncDecl.FindStringSubmatch(line); m != nil {
			curFunc = m[1]
		} else if m := reTSArrowDecl.FindStringSubmatch(line); m != nil {
			curFunc = m[1]
		} else if m := reTSMethodDec.FindStringSubmatch(line); m != nil && !isJSKeyword(m[1]) {
			curFunc = m[1]
		}

		// 1. Driver init sites.
		if (hasPG || strings.Contains(line, "pg.Pool")) && reTSNewPool.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Pool",
				Category: "pg", Adapter: "@kavach/pg",
			})
		}
		if (hasPG || strings.Contains(line, "pg.Client")) && reTSNewPGClient.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Client",
				Category: "pg", Adapter: "@kavach/pg",
			})
		}
		if (hasPrisma || strings.Contains(line, "PrismaClient")) && reTSNewPrisma.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new PrismaClient",
				Category: "prisma", Adapter: "@kavach/pg",
			})
		}
		if hasDrizzle && reTSDrizzleInit.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "drizzle",
				Category: "drizzle-orm", Adapter: "@kavach/pg",
			})
		}
		if (hasIORedis || strings.Contains(line, "IORedis")) && reTSNewRedis.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Redis",
				Category: "ioredis", Adapter: "@kavach/ioredis",
			})
		}
		if hasNodeRedis && reTSCreateRedis.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "redis.createClient",
				Category: "redis", Adapter: "@kavach/ioredis",
			})
		}
		if (hasKafkaJS || strings.Contains(line, "new Kafka")) && reTSNewKafka.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Kafka",
				Category: "kafkajs", Adapter: "@kavach/kafkajs",
			})
		}
		if hasKafkaJS && reTSKafkaProducer.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "kafka.producer",
				Category: "kafkajs", Adapter: "@kavach/kafkajs",
			})
		}
		if hasKafkaJS && reTSKafkaConsumer.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "kafka.consumer",
				Category: "kafkajs", Adapter: "@kavach/kafkajs",
			})
		}
		if hasExpress && reTSExpressInit.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "express()",
				Category: "express", Adapter: "@kavach/http",
			})
		}
		if hasFastify && reTSFastifyInit.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "fastify()",
				Category: "fastify", Adapter: "@kavach/http",
			})
		}
		if (hasHono || strings.Contains(line, "new Hono")) && reTSHonoInit.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Hono()",
				Category: "hono", Adapter: "@kavach/http",
			})
		}
		if hasHTTP && reTSHTTPCreateSrv.MatchString(line) {
			rep.InitSites = append(rep.InitSites, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "http.createServer",
				Category: "node:http", Adapter: "@kavach/http",
			})
		}

		// 2. Routes & handler entrypoints.
		if m := reTSRouteReg.FindStringSubmatch(line); m != nil {
			recv := strings.ToLower(m[1])
			if !strings.Contains(recv, "redis") && !strings.Contains(recv, "cache") && !strings.Contains(recv, "map") && !strings.Contains(recv, "headers") && !strings.Contains(recv, "params") {
				method := strings.ToUpper(m[2])
				if method == "ALL" || method == "ROUTE" {
					method = "ANY"
				}
				routePath := m[3]
				routeSet[method+" "+routePath] = true
				hName := m[4]
				if hName == "" || isJSKeyword(hName) || hName == "async" {
					hName = method + " " + routePath
				}
				rep.Handlers = append(rep.Handlers, HandlerSite{
					File: relPath, Line: lineNum, Name: hName, Kind: "http",
					Route: routePath, Method: method,
				})
			}
		}

		if isNextApp {
			if m := reTSNextAppHandler.FindStringSubmatch(line); m != nil {
				method := m[1]
				routeSet[method+" "+nextAppRoute] = true
				rep.Handlers = append(rep.Handlers, HandlerSite{
					File: relPath, Line: lineNum, Name: method + " " + nextAppRoute, Kind: "http",
					Route: nextAppRoute, Method: method,
				})
			} else if m := reTSNextAppConst.FindStringSubmatch(line); m != nil {
				method := m[1]
				routeSet[method+" "+nextAppRoute] = true
				rep.Handlers = append(rep.Handlers, HandlerSite{
					File: relPath, Line: lineNum, Name: method + " " + nextAppRoute, Kind: "http",
					Route: nextAppRoute, Method: method,
				})
			}
		}

		if isNextPages {
			if m := reTSNextPagesExport.FindStringSubmatch(line); m != nil {
				name := m[1]
				if name == "" {
					name = "handler"
				}
				routeSet["ANY "+nextPagesRoute] = true
				rep.Handlers = append(rep.Handlers, HandlerSite{
					File: relPath, Line: lineNum, Name: name, Kind: "http",
					Route: nextPagesRoute, Method: "ANY",
				})
			}
		}

		if reTSKafkaRun.MatchString(line) {
			name := "eachMessage"
			if strings.Contains(line, "eachBatch") {
				name = "eachBatch"
			}
			rep.Handlers = append(rep.Handlers, HandlerSite{
				File: relPath, Line: lineNum, Name: name, Kind: "kafka",
			})
		}

		// Extract Kafka topic literals & env defaults.
		for _, m := range reTSKafkaTopicProp.FindAllStringSubmatch(line, -1) {
			if looksLikeTopicName(m[1]) {
				topicSet[m[1]] = true
			}
		}
		for _, m := range reTSKafkaTopicsArr.FindAllStringSubmatch(line, -1) {
			for _, sm := range reTSStringLiteral.FindAllStringSubmatch(m[1], -1) {
				if looksLikeTopicName(sm[1]) {
					topicSet[sm[1]] = true
				}
			}
		}
		for _, m := range reTSEnvTopicDef.FindAllStringSubmatch(line, -1) {
			if looksLikeTopicName(m[1]) {
				topicSet[m[1]] = true
			}
		}

		// 3. Bare clock calls.
		if reTSDateNow.MatchString(line) {
			rep.ClockCalls = append(rep.ClockCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "Date.now()",
				Category: "clock", Detail: "Replace with kv.now().getTime() for deterministic replay",
			})
		}
		if reTSNewDateEmpty.MatchString(line) {
			rep.ClockCalls = append(rep.ClockCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "new Date()",
				Category: "clock", Detail: "Replace with kv.now() for deterministic replay",
			})
		}
		if reTSPerfNow.MatchString(line) {
			rep.ClockCalls = append(rep.ClockCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "performance.now()",
				Category: "clock", Detail: "Replace with kv.now() for deterministic replay",
			})
		}

		// 4. Bare rand / UUID calls.
		if reTSMathRandom.MatchString(line) {
			rep.RandCalls = append(rep.RandCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "Math.random()",
				Category: "rand", Detail: "Replace with kv.rand() for deterministic replay",
			})
		}
		if reTSRandomUUID.MatchString(line) {
			rep.RandCalls = append(rep.RandCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "crypto.randomUUID()",
				Category: "uuid", Detail: "Generate via kv.rand() for deterministic replay",
			})
		}
		if (hasCrypto || strings.Contains(line, "crypto.randomBytes")) && reTSRandomBytes.MatchString(line) {
			rep.RandCalls = append(rep.RandCalls, CallSite{
				File: relPath, Line: lineNum, Func: curFunc, Symbol: "crypto.randomBytes()",
				Category: "rand", Detail: "Replace with kv.rand() for deterministic replay",
			})
		}
	}
}

func registerTSImport(spec string, imported map[string]bool, seenDrivers map[string]Driver) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		return
	}
	imported[spec] = true
	if d, ok := tsStdlibDrivers[spec]; ok {
		mergeDriver(seenDrivers, d)
		return
	}
	pkg := normalizeTSPackageName(spec)
	imported[pkg] = true
	if d, ok := tsManifestDrivers[pkg]; ok {
		d.Source = "ast"
		mergeDriver(seenDrivers, d)
	}
}

func normalizeTSPackageName(spec string) string {
	if strings.HasPrefix(spec, "@") {
		parts := strings.Split(spec, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return spec
	}
	if idx := strings.IndexByte(spec, '/'); idx >= 0 {
		return spec[:idx]
	}
	return spec
}

func hasPrefixKey(m map[string]bool, prefix string) bool {
	for k := range m {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			return true
		}
	}
	return false
}

func nextAppRoutePath(relPath string) (string, bool) {
	p := filepath.ToSlash(relPath)
	base := filepath.Base(p)
	if !strings.HasPrefix(base, "route.") {
		return "", false
	}
	for _, marker := range []string{"src/app/", "app/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			sub := strings.TrimPrefix(p[idx+len(marker):], "/")
			dir := filepath.Dir(sub)
			if dir == "." || dir == "" {
				return "/", true
			}
			// Strip Next.js route groups like (marketing).
			parts := strings.Split(dir, "/")
			var kept []string
			for _, seg := range parts {
				if strings.HasPrefix(seg, "(") && strings.HasSuffix(seg, ")") {
					continue
				}
				kept = append(kept, seg)
			}
			if len(kept) == 0 {
				return "/", true
			}
			return "/" + strings.Join(kept, "/"), true
		}
	}
	return "", false
}

func nextPagesAPIRoutePath(relPath string) (string, bool) {
	p := filepath.ToSlash(relPath)
	for _, marker := range []string{"src/pages/api/", "pages/api/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			sub := p[idx+len(marker):]
			ext := filepath.Ext(sub)
			sub = strings.TrimSuffix(sub, ext)
			sub = strings.TrimSuffix(sub, "/index")
			if sub == "" || sub == "index" {
				return "/api", true
			}
			return "/api/" + sub, true
		}
	}
	return "", false
}

// stripJSCommentsPreserveLines removes // and /* ... */ comments while keeping
// string literals intact and preserving all newlines for exact line numbers.
func stripJSCommentsPreserveLines(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	n := len(src)
	var quote byte
	inLineComment := false
	inBlockComment := false

	for i := 0; i < n; i++ {
		c := src[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
				b.WriteByte('\n')
			}
			continue
		}
		if inBlockComment {
			if c == '\n' {
				b.WriteByte('\n')
			} else if c == '*' && i+1 < n && src[i+1] == '/' {
				inBlockComment = false
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
				inLineComment = true
				i++
				continue
			}
			if src[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isJSKeyword(s string) bool {
	switch s {
	case "if", "for", "while", "switch", "catch", "return", "new", "typeof", "delete", "void", "await", "function", "class":
		return true
	}
	return false
}

func buildTypeScriptSnippet(rep *DoctorReport) string {
	adapters := map[string]bool{}
	for _, d := range rep.Drivers {
		if d.Language == LangTypeScript && d.Adapter != "" {
			adapters[d.Adapter] = true
		}
	}

	if len(adapters) == 0 {
		return fmt.Sprintf(`import { init } from "@kavach/core";

const kv = init({
  service: %q,
  pii: "standard",
});
// No supported TS drivers detected — run 'kavach init --mode scoped' or wire kv.serveOrReplay(runner).`, rep.Service)
	}

	var imports []string
	imports = append(imports, `import { init } from "@kavach/core";`)
	if adapters["@kavach/http"] {
		imports = append(imports, `import { wrapHandler } from "@kavach/http";`)
	}
	if adapters["@kavach/pg"] {
		imports = append(imports, `import { wrapPool } from "@kavach/pg";`)
	}
	if adapters["@kavach/ioredis"] {
		imports = append(imports, `import { wrapRedis } from "@kavach/ioredis";`)
	}
	if adapters["@kavach/kafkajs"] {
		imports = append(imports, `import { wrapKafka } from "@kavach/kafkajs";`)
	}

	var wraps []string
	if adapters["@kavach/pg"] {
		wraps = append(wraps, `const pool = wrapPool(kv, rawPool);`)
	}
	if adapters["@kavach/ioredis"] {
		wraps = append(wraps, `const redis = wrapRedis(kv, rawRedis);`)
	}
	if adapters["@kavach/kafkajs"] {
		wraps = append(wraps, `const kafka = wrapKafka(kv, rawKafka);`)
	}
	if adapters["@kavach/http"] {
		wraps = append(wraps, `const app = wrapHandler(kv, expressApp);`)
	}

	serveTarget := "app"
	if !adapters["@kavach/http"] && adapters["@kavach/kafkajs"] {
		serveTarget = "consumer"
	}

	var b strings.Builder
	b.WriteString(strings.Join(imports, "\n"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "const kv = init({\n  service: %q,\n  ringPath: process.env.KAVACH_RING_PATH,\n  pii: \"standard\",\n});\n\n", rep.Service)
	b.WriteString("// 1-line driver wraps at client initialization — zero business-logic edits:\n")
	b.WriteString(strings.Join(wraps, "\n"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "await kv.serveOrReplay(%s);", serveTarget)
	return b.String()
}
