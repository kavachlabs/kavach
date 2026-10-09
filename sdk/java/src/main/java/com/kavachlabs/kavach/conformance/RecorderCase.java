package com.kavachlabs.kavach.conformance;

import com.kavachlabs.kavach.Gateway;
import com.kavachlabs.kavach.GatewayException;
import com.kavachlabs.kavach.Gateways;
import com.kavachlabs.kavach.Input;
import com.kavachlabs.kavach.Json;
import com.kavachlabs.kavach.Recorder;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Base64;
import java.util.Comparator;
import java.util.Deque;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.stream.Stream;

/**
 * Runs SDK recorder cases (spec/recorder/sdk/README.md) through this SDK:
 *
 * <pre>
 * java -cp CLASSES com.kavachlabs.kavach.conformance.RecorderCase [--fake-recorder PATH] CASE.json...
 * </pre>
 *
 * Starts the SDK's {@link Recorder} for the conformance handler with the fake
 * recorder as its recorder command, performs the case's actions, closes it,
 * and reports the fake's verdict. Exit status 0 if every case passes.
 * {@code KAVACH_SPEC_DIR} (default: the repository's spec/, relative to sdk/java) locates the fake.
 */
public final class RecorderCase {
    private RecorderCase() {}

    static final String DEFAULT_SPEC_DIR = "../../spec";

    /** The spec directory: {@code $KAVACH_SPEC_DIR}, else the default. */
    public static Path specDir() {
        String env = System.getenv("KAVACH_SPEC_DIR");
        return Path.of(env != null && !env.isEmpty() ? env : DEFAULT_SPEC_DIR);
    }

    /** Serves one step's scripted answers, in order, per kind. */
    private static final class Answers {
        private final Map<String, Deque<Object>> queues = new HashMap<>();

        void load(Object answers) {
            queues.clear();
            if (answers instanceof Map<?, ?> m) {
                for (Map.Entry<?, ?> e : m.entrySet()) {
                    queues.put((String) e.getKey(), new ArrayDeque<>((List<?>) e.getValue()));
                }
            }
        }

        Object pop(String kind) {
            Deque<Object> q = queues.get(kind);
            if (q == null || q.isEmpty()) {
                throw new AssertionError("the handler read a " + kind + " the case has no answer for");
            }
            return q.removeFirst();
        }

        long clock() {
            return Long.parseLong((String) pop("clock"));
        }

        byte[] rand(int n) {
            byte[] data = Base64.getDecoder().decode((String) pop("rand"));
            if (data.length != n) {
                throw new AssertionError("case rand answer is " + data.length + " bytes, handler asked for " + n);
            }
            return data;
        }

        byte[] gateway(byte[] request) {
            Map<?, ?> a = (Map<?, ?>) pop("gateway");
            if (a.containsKey("error")) {
                throw new GatewayException((String) a.get("error"));
            }
            return Base64.getDecoder().decode((String) a.get("response"));
        }

        byte[] config(String key) {
            Map<?, ?> a = (Map<?, ?>) pop("config");
            return Boolean.TRUE.equals(a.get("unset")) ? null : Base64.getDecoder().decode((String) a.get("value"));
        }
    }

    /** Runs a case; returns the fake recorder's {@code result.json} contents. */
    public static Map<?, ?> run(Path casePath, Path fakeRecorder, boolean ring) throws IOException {
        Map<?, ?> kase = (Map<?, ?>) Json.parse(Files.readString(casePath));
        Map<?, ?> open = (Map<?, ?>) kase.get("open");
        ConformanceHandler handler = new ConformanceHandler();
        if (kase.get("snapshot") instanceof String s) {
            handler.restore(s.getBytes(StandardCharsets.US_ASCII));
        }
        Answers ans = new Answers();
        Map<String, byte[]> flags = new LinkedHashMap<>();
        if (kase.get("flags") instanceof Map<?, ?> f) {
            for (Map.Entry<?, ?> e : f.entrySet()) {
                flags.put((String) e.getKey(), ((String) e.getValue()).getBytes(StandardCharsets.UTF_8));
            }
        }
        Path tmp = Files.createTempDirectory("kavach-case-");
        try {
            Path resultPath = tmp.resolve("result.json");
            Recorder.Options opts = new Recorder.Options()
                    .service((String) open.get("service"))
                    .start((String) open.get("start"))
                    .snapshots(Boolean.TRUE.equals(open.get("snapshots")))
                    .recorderCommand(List.of("python3", fakeRecorder.toString(), casePath.toString(), resultPath.toString()))
                    .gateways(Gateways.any(Gateway.remote(ans::gateway)))
                    .config(ans::config)
                    .configSource("case")
                    .clockNanos(ans::clock)
                    .randomBytes(ans::rand)
                    .noRing(!ring)
                    .required(true);
            if (!flags.isEmpty()) {
                opts.flags(() -> flags);
            }
            Recorder rec = new Recorder(handler, opts);
            try {
                for (Object a : (List<?>) kase.get("actions")) {
                    Map<?, ?> action = (Map<?, ?>) a;
                    if (action.get("step") instanceof Map<?, ?> s) {
                        ans.load(action.get("answers"));
                        rec.step(new Input(
                                (String) s.get("source"),
                                (String) s.get("position"),
                                Base64.getDecoder().decode((String) s.get("data"))));
                    } else if (action.get("flush") instanceof Map<?, ?> f) {
                        rec.flush(Boolean.TRUE.equals(f.get("durable")));
                    }
                }
            } finally {
                rec.close();
            }
            if (!Files.exists(resultPath)) {
                Map<String, Object> r = new LinkedHashMap<>();
                r.put("pass", false);
                r.put("error", "the fake recorder wrote no result (did the SDK close it?)");
                return r;
            }
            return (Map<?, ?>) Json.parse(Files.readString(resultPath));
        } finally {
            try (Stream<Path> files = Files.walk(tmp)) {
                files.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete());
            }
        }
    }

    public static void main(String[] args) throws IOException {
        Path fake = specDir().resolve("recorder/sdk/fake_recorder.py");
        List<Path> cases = new ArrayList<>();
        for (int i = 0; i < args.length; i++) {
            if (args[i].equals("--fake-recorder") && i + 1 < args.length) {
                fake = Path.of(args[++i]);
            } else {
                cases.add(Path.of(args[i]));
            }
        }
        if (cases.isEmpty()) {
            System.err.println("usage: RecorderCase [--fake-recorder PATH] CASE.json...");
            System.exit(2);
        }
        int failed = 0;
        for (boolean ring : new boolean[] {false, true}) {
            for (Path c : cases) {
                boolean ok;
                Object error = null;
                try {
                    Map<?, ?> result = run(c, fake, ring);
                    ok = Boolean.TRUE.equals(result.get("pass"));
                    error = result.get("error");
                } catch (Exception e) {
                    ok = false;
                    error = e;
                    e.printStackTrace();
                }
                System.out.println((ok ? "PASS  " : "FAIL  ") + c.getFileName() + (ring ? " (ring)" : " (pipe)"));
                if (!ok) {
                    failed++;
                    System.out.println("  " + error);
                }
            }
        }
        int total = 2 * cases.size();
        System.out.println((total - failed) + "/" + total + " recorder cases passed (pipe and ring)");
        System.exit(failed == 0 ? 0 : 1);
    }
}
