package com.kavachlabs.kavach;

import com.kavachlabs.kavach.conformance.ConformanceHandler;
import java.io.BufferedReader;
import java.io.ByteArrayOutputStream;
import java.io.StringReader;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

/** Plain {@code main}-based tests; exit status 1 if any fails. */
public final class AllTests {
    private AllTests() {}

    private static int failures;

    interface Test {
        void run() throws Exception;
    }

    static void check(boolean ok, String what) {
        if (!ok) {
            throw new AssertionError(what);
        }
    }

    static void eq(Object want, Object got, String what) {
        if (!java.util.Objects.equals(want, got)) {
            throw new AssertionError(what + ": want " + want + ", got " + got);
        }
    }

    static void test(String name, Test t) {
        try {
            t.run();
            System.out.println("PASS  " + name);
        } catch (Throwable e) {
            failures++;
            System.out.println("FAIL  " + name);
            e.printStackTrace(System.out);
        }
    }

    public static void main(String[] args) {
        test("json parse and write", AllTests::jsonRoundTrip);
        test("json escaping follows SPEC 9.6", AllTests::jsonEscaping);
        test("wire encoding", AllTests::wire);
        test("failure mapping", AllTests::failureMapping);
        test("conformance handler counts without rollback", AllTests::countNoRollback);
        test("host: a full session", AllTests::hostSession);
        test("host: abort cannot be swallowed by catch (Exception)", AllTests::hostAbort);
        test("host: sandbox mode is refused", AllTests::hostSandbox);
        test("host: protocol output is not polluted by System.out", AllTests::hostStdout);
        test("recorder: missing recorder does not fail the step", AllTests::recorderMissing);
        test("recorder: required makes construction fail", AllTests::recorderRequired);
        test("recorder: a recorder that dies does not fail the step", AllTests::recorderDies);
        test("ring: a service killed after its input leaves a crash fixture", AllTests::killedService);
        test("ring: frames larger than the ring and a ring that keeps filling lose nothing", AllTests::smallRing);
        test("recorder: a silent recorder delays construction once", AllTests::silentRecorder);
        test("ring: a recorder that stops reading a full ring does not hang the service", AllTests::deadRecorderFullRing);
        test("ring: the file is gone once the recorder is", AllTests::ringFileRemoved);
        System.out.println(failures == 0 ? "all tests passed" : failures + " test(s) failed");
        System.exit(failures == 0 ? 0 : 1);
    }

    static void jsonRoundTrip() {
        Object v = Json.parse(" {\"a\":[1,-2,3.5,true,null,\"x\\u00e9\\n\"],\"b\":{\"c\":9007199254740993}} ");
        eq("{\"a\":[1,-2,3.5,true,null,\"x\u00e9\\n\"],\"b\":{\"c\":9007199254740993}}", Json.write(v), "round trip");
        for (String bad : new String[] {"", "{", "[1,]", "{\"a\"}", "tru", "1 2", "\"\\x\"", "\"a\nb\""}) {
            try {
                Json.parse(bad);
                throw new AssertionError("accepted " + bad);
            } catch (Json.ParseException expected) {
                // ok
            }
        }
    }

    static void jsonEscaping() {
        eq("\"a\\\"b\\\\c\\n\\r\\t\\u0001\\u001f<>&\u00e9\u2028\"", Json.quote("a\"b\\c\n\r\t\u0001\u001f<>&\u00e9\u2028"), "quote");
    }

    static void wire() {
        eq(List.of(0xAC, 0x02), toInts(new Wire.Buf().uvarint(300).toByteArray()), "uvarint 300");
        eq(List.of(0), toInts(new Wire.Buf().uvarint(0).toByteArray()), "uvarint 0");
        // len 11 = frame kind, record type, flags, 8 bytes little-endian
        eq(List.of(11, 2, 2, 0, 1, 0, 0, 0, 0, 0, 0, 0), toInts(Wire.clock(1)), "clock frame");
        eq(List.of(2, 6, 1), toInts(Wire.flush(true)), "flush frame");
        eq(List.of(1, 3), toInts(Wire.stepEnd()), "step_end frame");
        Map<String, byte[]> facts = new LinkedHashMap<>();
        facts.put("k", new byte[] {7});
        eq(List.of(7, 4, 1, 1, (int) 'k', 0, 1, 7), toInts(Wire.facts(facts)), "facts frame");
        // gateway records are critical, others are not
        eq(1, Wire.gateway("g", new byte[0], new byte[0], "", false)[3] & 0xFF, "gateway critical flag");
        eq(0, Wire.output("s", new byte[0], false)[3] & 0xFF, "output flags");
    }

    static List<Integer> toInts(byte[] b) {
        Integer[] out = new Integer[b.length];
        for (int i = 0; i < b.length; i++) {
            out[i] = b[i] & 0xFF;
        }
        return Arrays.asList(out);
    }

    static void failureMapping() {
        Failure p = Failure.classify(new Panic("boom"));
        eq("panic", p.kind(), "Panic kind");
        eq("boom", p.message(), "Panic message");
        Failure e = Failure.classify(new HandlerError("bad input"));
        eq("error", e.kind(), "HandlerError kind");
        eq("bad input", e.message(), "HandlerError message");
        Failure npe = Failure.classify(new IllegalStateException("oops"));
        eq("panic", npe.kind(), "exception kind");
        eq("java.lang.IllegalStateException: oops", npe.message(), "exception message is toString()");
        check(npe.detail().contains("AllTests.failureMapping"), "detail holds the stack trace");
        eq("java.lang.StackOverflowError", Failure.classify(new StackOverflowError()).message(), "Error is a panic");
    }

    static void countNoRollback() throws Exception {
        ConformanceHandler h = new ConformanceHandler();
        Env env = new NullEnv();
        try {
            h.handle(env, input("[{\"op\":\"count\",\"n\":5},{\"op\":\"panic\",\"message\":\"x\"}]"));
            throw new AssertionError("expected a panic");
        } catch (Panic expected) {
            eq("5", new String(h.snapshot(), StandardCharsets.US_ASCII), "count applied, no +1");
        }
        h.handle(env, input("[]"));
        eq("6", new String(h.snapshot(), StandardCharsets.US_ASCII), "+1 on success");
    }

    static Input input(String json) {
        return new Input("test", "0", json.getBytes(StandardCharsets.UTF_8));
    }

    static final class NullEnv implements Env {
        public long nowNanos() {
            return 0;
        }

        public byte[] random(int n) {
            return new byte[n];
        }

        public byte[] query(String gateway, byte[] request) {
            return new byte[0];
        }

        public Optional<byte[]> config(String key) {
            return Optional.empty();
        }

        public void emit(String sink, byte[] data, boolean local) {}
    }

    static String b64(String s) {
        return Base64.getEncoder().encodeToString(s.getBytes(StandardCharsets.UTF_8));
    }

    static String hello(String mode) {
        return "{\"t\":\"hello\",\"protocol\":1,\"service\":\"conformance\",\"start\":\"genesis\",\"mode\":\"" + mode + "\"}\n";
    }

    /** Plays the driver's lines to a host and returns what it wrote. */
    static List<String> play(Handler h, String driver) {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        HostOptions opts = HostOptions.defaults().environment(() -> Map.of());
        int code = new Host(() -> h, new BufferedReader(new StringReader(driver)), out, opts).run();
        List<String> lines = Arrays.asList(out.toString(StandardCharsets.UTF_8).split("\n"));
        if (code != 0 && lines.stream().noneMatch(l -> l.contains("\"fatal\""))) {
            throw new AssertionError("host exited " + code);
        }
        return lines;
    }

    static void hostSession() {
        String ops = b64("[{\"op\":\"clock\"},{\"op\":\"emit\",\"sink\":\"s\",\"data\":\"d\"}]");
        List<String> got = play(new ConformanceHandler(),
                hello("process")
                        + "{\"t\":\"step\",\"seq\":\"0\",\"source\":\"test\",\"position\":\"0\",\"data\":\"" + ops + "\"}\n"
                        + "{\"t\":\"clock\",\"unix_nanos\":\"42\"}\n"
                        + "{\"t\":\"end\"}\n");
        eq(5, got.size(), "lines: " + got);
        eq("{\"t\":\"ready\",\"protocol\":1,\"sdk\":\"" + Kavach.PRODUCER + "\",\"invariants\":[\"below_limit\"],\"environment\":{}}",
                got.get(0), "ready");
        eq("{\"t\":\"clock\"}", got.get(1), "clock request");
        eq("{\"t\":\"emit\",\"sink\":\"trace\",\"data\":\"" + b64("{\"clock\":\"42\"}") + "\",\"scope\":\"remote\"}",
                got.get(2), "emit of the clock");
        eq("{\"t\":\"emit\",\"sink\":\"s\",\"data\":\"" + b64("d") + "\",\"scope\":\"remote\"}", got.get(3), "emit");
        eq("{\"t\":\"done\",\"outcome\":\"ok\"}", got.get(4), "done");
    }

    static void hostAbort() {
        Handler swallowing = (env, in) -> {
            try {
                env.nowNanos();
            } catch (Exception e) {
                // would swallow a RuntimeException; must not swallow the abort
            }
            env.emit("after", new byte[0]);
        };
        List<String> got = play(swallowing,
                hello("process")
                        + "{\"t\":\"step\",\"seq\":\"0\",\"source\":\"test\",\"position\":\"0\",\"data\":\"\"}\n"
                        + "{\"t\":\"abort\",\"detail\":\"x\"}\n"
                        + "{\"t\":\"end\"}\n");
        eq(3, got.size(), "lines: " + got);
        eq("{\"t\":\"clock\"}", got.get(1), "clock request");
        eq("{\"t\":\"done\",\"outcome\":\"aborted\"}", got.get(2), "aborted, no emit");

        // A handler that catches Throwable and returns still reports aborted.
        Handler greedy = (env, in) -> {
            try {
                env.nowNanos();
            } catch (Throwable t) {
                // swallowed
            }
        };
        got = play(greedy,
                hello("process")
                        + "{\"t\":\"step\",\"seq\":\"0\",\"source\":\"test\",\"position\":\"0\",\"data\":\"\"}\n"
                        + "{\"t\":\"abort\",\"detail\":\"x\"}\n"
                        + "{\"t\":\"end\"}\n");
        eq("{\"t\":\"done\",\"outcome\":\"aborted\"}", got.get(2), "aborted even if swallowed");
    }

    static void hostSandbox() {
        List<String> got = play(new ConformanceHandler(), hello("sandbox"));
        eq(List.of("{\"t\":\"fatal\",\"message\":\"sandbox mode not supported\"}"), got, "fatal");
    }

    static void hostStdout() throws Exception {
        // Kavach.maybeHost points System.out at System.err; here we check the
        // handler-visible effect through the real conformance host in a subprocess.
        String ops = b64("[{\"op\":\"print\",\"text\":\"hello\"}]");
        String java = Path_of(System.getProperty("java.home"), "bin", "java");
        ProcessBuilder pb = new ProcessBuilder(java, "-cp", System.getProperty("java.class.path"),
                "com.kavachlabs.kavach.conformance.ConformanceHost", "kavach-host");
        Process p = pb.start();
        p.getOutputStream().write((hello("process")
                + "{\"t\":\"step\",\"seq\":\"0\",\"source\":\"test\",\"position\":\"0\",\"data\":\"" + ops + "\"}\n"
                + "{\"t\":\"end\"}\n").getBytes(StandardCharsets.UTF_8));
        p.getOutputStream().close();
        String stdout = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        String stderr = new String(p.getErrorStream().readAllBytes(), StandardCharsets.UTF_8);
        eq(0, p.waitFor(), "exit status");
        String[] lines = stdout.split("\n");
        eq(2, lines.length, "stdout: " + stdout);
        check(lines[1].equals("{\"t\":\"done\",\"outcome\":\"ok\"}"), "done: " + lines[1]);
        check(stderr.contains("hello"), "the print went to stderr: " + stderr);
    }

    static String Path_of(String first, String... more) {
        return java.nio.file.Path.of(first, more).toString();
    }

    static void recorderMissing() throws Exception {
        Recorder.Options o = new Recorder.Options().service("t").recorderCommand(List.of("/nonexistent/kavach-recorder"));
        java.util.logging.Logger.getLogger("kavach").setLevel(java.util.logging.Level.OFF);
        try (Recorder r = new Recorder(new ConformanceHandler(), o)) {
            check(!r.recording(), "not recording");
            Recorder.StepResult res = r.step(input("[{\"op\":\"emit\",\"sink\":\"a\",\"data\":\"b\"}]"));
            check(res.ok(), "the step still ran");
            eq(1, res.outputs().size(), "outputs");
            Recorder.StepResult bad = r.step(input("[{\"op\":\"panic\",\"message\":\"m\"}]"));
            eq("panic", bad.kind(), "panic still classified");
            check(!r.flush(true), "flush reports not durable");
        }
    }

    static void recorderRequired() {
        Recorder.Options o = new Recorder.Options()
                .service("t").required(true).recorderCommand(List.of("/nonexistent/kavach-recorder"));
        try {
            new Recorder(new ConformanceHandler(), o).close();
            throw new AssertionError("expected RecorderException");
        } catch (RecorderException expected) {
            // ok
        }
    }

    static void recorderDies() {
        // `false` exits at once: the control stream ends and writes fail.
        Recorder.Options o = new Recorder.Options().service("t").recorderCommand(List.of("false"));
        java.util.logging.Logger.getLogger("kavach").setLevel(java.util.logging.Level.OFF);
        try (Recorder r = new Recorder(new ConformanceHandler(), o)) {
            for (int i = 0; i < 3; i++) {
                check(r.step(input("[]")).ok(), "step " + i);
                try {
                    Thread.sleep(50);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
            }
            check(!r.recording(), "recording stopped");
        }
    }

    /** The records of a journal or fixture, as {@code kavach inspect --json} prints them. */
    static List<?> inspect(String file) throws Exception {
        Process p = new ProcessBuilder(Tools.kavach(), "inspect", "--json", file).redirectError(ProcessBuilder.Redirect.INHERIT).start();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        eq(0, p.waitFor(), "kavach inspect");
        return (List<?>) ((Map<?, ?>) Json.parse(out)).get("records");
    }

    static void killedService() throws Exception {
        for (String transport : new String[] {"ring", "pipe"}) {
            java.nio.file.Path dir = java.nio.file.Files.createTempDirectory("kavach-killed-");
            ProcessBuilder pb = new ProcessBuilder(Path_of(System.getProperty("java.home"), "bin", "java"),
                    "-cp", System.getProperty("java.class.path"), "com.kavachlabs.kavach.KilledService", dir.toString(), transport)
                    .inheritIO();
            pb.environment().put("KAVACH_RECORDER", Tools.recorder());
            Process child = pb.start();
            eq(137, child.waitFor(), transport + ": the child should die of SIGKILL");
            // The recorder is no child of this process: it finishes on its own.
            java.nio.file.Path fixture = null;
            for (long end = System.nanoTime() + 10_000_000_000L; fixture == null && System.nanoTime() < end; Thread.sleep(20)) {
                java.io.File[] found = dir.resolve("fixtures").toFile().listFiles((d, n) -> n.endsWith(".kavach"));
                if (found != null && found.length == 1) {
                    fixture = found[0].toPath();
                }
            }
            check(fixture != null, transport + ": no fixture was written");
            List<?> records = inspect(fixture.toString());
            Map<?, ?> last = (Map<?, ?>) records.get(records.size() - 1);
            eq("marker", last.get("type"), transport + ": last record");
            eq("crash", last.get("kind"), transport + ": marker kind");
            Map<?, ?> in = null;
            for (Object r : records) {
                if (r instanceof Map<?, ?> m && "input".equals(m.get("type"))) {
                    in = m;
                }
            }
            eq(b64("die"), in.get("data"), transport + ": the failing input");
        }
    }

    static Recorder.Options realRecorder(java.nio.file.Path dir) throws Exception {
        return new Recorder.Options().service("small").dir(dir.toString()).recorderCommand(List.of(Tools.recorder())).required(true);
    }

    static void smallRing() throws Exception {
        java.nio.file.Path dir = java.nio.file.Files.createTempDirectory("kavach-small-");
        byte[] big = new byte[200 << 10];
        Arrays.fill(big, (byte) 'b');
        Handler sink = (env, in) -> env.emit("out", in.data());
        int steps = 400;
        String file;
        try (Recorder r = new Recorder(sink, realRecorder(dir).ringBytes(64 << 10))) {
            for (int i = 0; i < steps; i++) {
                r.step(new Input("t", "p", i % 100 == 7 ? big : "small".getBytes(StandardCharsets.UTF_8)));
            }
            check(r.flush(true), "flush");
            file = r.file();
        }
        int inputs = 0;
        int bigs = 0;
        for (Object o : inspect(file)) {
            if (o instanceof Map<?, ?> m && "input".equals(m.get("type"))) {
                inputs++;
                if (java.util.Base64.getDecoder().decode((String) m.get("data")).length == big.length) {
                    bigs++;
                }
            }
        }
        eq(steps, inputs, "inputs in the journal");
        eq(4, bigs, "large inputs in the journal");
    }

    static void silentRecorder() throws Exception {
        java.util.logging.Logger.getLogger("kavach").setLevel(java.util.logging.Level.OFF);
        Recorder.Options o = new Recorder.Options().service("silent")
                .dir(java.nio.file.Files.createTempDirectory("kavach-silent-").toString()).recorderCommand(List.of("sleep", "4"));
        long start = System.nanoTime();
        try (Recorder r = new Recorder(new ConformanceHandler(), o)) {
            long ms = (System.nanoTime() - start) / 1_000_000;
            check(ms >= 1000 && ms <= 3000, "construction took " + ms + " ms");
            start = System.nanoTime();
            for (int i = 0; i < 100; i++) {
                r.step(input("[]"));
            }
            ms = (System.nanoTime() - start) / 1_000_000;
            check(ms <= 500, "100 steps took " + ms + " ms");
        }
    }

    static void deadRecorderFullRing() throws Exception {
        java.util.logging.Logger.getLogger("kavach").setLevel(java.util.logging.Level.OFF);
        Recorder.Options o = new Recorder.Options().service("dead").dir(java.nio.file.Files.createTempDirectory("kavach-dead-").toString())
                .recorderCommand(List.of("sleep", "1")).ringBytes(64 << 10);
        Handler sink = (env, in) -> env.emit("out", in.data());
        byte[] data = new byte[30 << 10];
        Thread t = new Thread(() -> {
            try (Recorder r = new Recorder(sink, o)) {
                for (int i = 0; i < 20; i++) {
                    r.step(new Input("t", "p", data));
                }
            }
        });
        t.start();
        t.join(15_000);
        check(!t.isAlive(), "the service is stuck waiting for a recorder that is gone");
    }

    static void ringFileRemoved() throws Exception {
        java.nio.file.Path dir = java.nio.file.Files.createTempDirectory("kavach-gone-");
        java.util.Set<String> before = ringFiles();
        try (Recorder r = new Recorder(new ConformanceHandler(), realRecorder(dir))) {
            check(r.step(input("[]")).ok(), "step");
        }
        eq(before, ringFiles(), "ring files left behind");
        // A recorder that never starts leaves nothing either.
        java.util.logging.Logger.getLogger("kavach").setLevel(java.util.logging.Level.OFF);
        new Recorder(new ConformanceHandler(), new Recorder.Options().service("t").recorderCommand(List.of("/nonexistent/kavach-recorder"))).close();
        eq(before, ringFiles(), "ring files left behind by a recorder that never started");
    }

    static java.util.Set<String> ringFiles() throws Exception {
        java.nio.file.Path shm = java.nio.file.Path.of("/dev/shm");
        java.nio.file.Path dir = java.nio.file.Files.isDirectory(shm) ? shm : java.nio.file.Path.of(System.getProperty("java.io.tmpdir"));
        try (var files = java.nio.file.Files.list(dir)) {
            return files.map(f -> f.getFileName().toString()).filter(n -> n.startsWith("kavach-ring-")).collect(java.util.stream.Collectors.toSet());
        }
    }
}
