package com.kavachlabs.kavach;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;
import java.util.function.Supplier;

/**
 * The host side of the replay protocol (SPEC §9): runs a handler one step at a
 * time while the driver serves every read and captures every output. Most
 * programs use {@link Kavach#maybeHost}.
 */
public final class Host {
    private static final int PROTOCOL = 1;

    /** Unwinds the stack on the host's own account; handler code must not swallow these. */
    private abstract static class Signal extends Error {
        Signal(String message) {
            super(message, null, false, false);
        }
    }

    /** The driver answered a request with {@code abort} (§9.4). */
    private static final class Abort extends Signal {
        Abort() {
            super("kavach: replay of this step was aborted");
        }
    }

    /** The protocol was violated; the host sends {@code fatal} and stops. */
    private static final class Fatal extends Signal {
        Fatal(String message) {
            super(message);
        }
    }

    /** The driver closed the pipe. */
    private static final class Gone extends Signal {
        Gone() {
            super("kavach: the driver closed the pipe");
        }
    }

    private final Supplier<Handler> factory;
    private final BufferedReader in;
    private final OutputStream out;
    private final HostOptions options;
    private boolean aborted;

    public Host(Supplier<Handler> factory, BufferedReader in, OutputStream out, HostOptions options) {
        this.factory = factory;
        this.in = in;
        this.out = out;
        this.options = options;
    }

    private static String b64(byte[] b) {
        return Base64.getEncoder().encodeToString(b);
    }

    private static byte[] unb64(Object s) {
        if (!(s instanceof String str)) {
            throw new Fatal("expected a base64 string");
        }
        try {
            return Base64.getDecoder().decode(str);
        } catch (IllegalArgumentException e) {
            throw new Fatal("invalid base64");
        }
    }

    /**
     * {@code ready.environment}: the output of {@code kavach-recorder facts}
     * if it can be run, plus {@code host.runtime} (§9.2).
     */
    public static Map<String, Object> collectEnvironment(List<String> recorderCommand) {
        Map<String, Object> env = new LinkedHashMap<>();
        List<String> cmd = Recorder.findRecorder(recorderCommand);
        if (cmd != null) {
            try {
                List<String> argv = new ArrayList<>(cmd);
                argv.add("facts");
                ProcessBuilder pb = new ProcessBuilder(argv);
                pb.redirectInput(ProcessBuilder.Redirect.from(new File("/dev/null")));
                pb.redirectError(ProcessBuilder.Redirect.DISCARD);
                Process p = pb.start();
                CompletableFuture<byte[]> output = CompletableFuture.supplyAsync(() -> {
                    try {
                        return p.getInputStream().readAllBytes();
                    } catch (IOException e) {
                        return new byte[0];
                    }
                });
                if (p.waitFor(10, TimeUnit.SECONDS) && p.exitValue() == 0) {
                    Object facts = Json.parse(new String(output.get(5, TimeUnit.SECONDS), StandardCharsets.UTF_8));
                    if (facts instanceof Map<?, ?> m) {
                        for (Map.Entry<?, ?> e : m.entrySet()) {
                            if (e.getValue() instanceof Map) {
                                env.put(String.valueOf(e.getKey()), e.getValue());
                            }
                        }
                    }
                } else {
                    p.destroyForcibly();
                }
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            } catch (Exception e) {
                // no facts: the driver treats the keys as not collected
            }
        }
        env.put("host.runtime", Map.of("value", b64(Kavach.runtime().getBytes(StandardCharsets.UTF_8))));
        return env;
    }

    private void send(Map<String, Object> msg) {
        byte[] line = (Json.write(msg) + "\n").getBytes(StandardCharsets.UTF_8);
        try {
            out.write(line);
            out.flush();
        } catch (IOException e) {
            throw new Gone();
        }
    }

    @SuppressWarnings("unchecked")
    private Map<String, Object> recv() {
        String line;
        try {
            line = in.readLine();
        } catch (IOException e) {
            throw new Gone();
        }
        if (line == null) {
            throw new Gone();
        }
        Object msg;
        try {
            msg = Json.parse(line);
        } catch (RuntimeException e) {
            throw new Fatal("malformed message: not JSON");
        }
        if (!(msg instanceof Map<?, ?> m) || !(m.get("t") instanceof String)) {
            throw new Fatal("malformed message: no type");
        }
        return (Map<String, Object>) m;
    }

    /** Sends one request and waits for its answer; unwinds on {@code abort}. */
    private Map<String, Object> request(Map<String, Object> msg, String want) {
        if (aborted) {
            throw new Abort();
        }
        send(msg);
        Map<String, Object> ans = recv();
        String t = (String) ans.get("t");
        if (t.equals("abort")) {
            aborted = true;
            throw new Abort();
        }
        if (!t.equals(want)) {
            throw new Fatal("expected a '" + want + "' answer, got '" + t + "'");
        }
        return ans;
    }

    private static Map<String, Object> msg(String t, Object... kv) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("t", t);
        for (int i = 0; i < kv.length; i += 2) {
            m.put((String) kv[i], kv[i + 1]);
        }
        return m;
    }

    /** Serves the driver until {@code end} or EOF. Returns the exit status. */
    public int run() {
        Handler handler = null;
        try {
            while (true) {
                Map<String, Object> m;
                try {
                    m = recv();
                } catch (Gone g) {
                    return 0; // between steps: the driver left
                }
                String t = (String) m.get("t");
                switch (t) {
                    case "hello" -> handler = hello(m);
                    case "step" -> {
                        if (handler == null) {
                            throw new Fatal("step before hello");
                        }
                        step(handler, m);
                    }
                    case "end" -> {
                        return 0;
                    }
                    case "abort" -> { } // a stray abort between steps needs no answer
                    default -> throw new Fatal("unexpected message '" + t + "'");
                }
            }
        } catch (Fatal f) {
            try {
                send(msg("fatal", "message", f.getMessage()));
            } catch (Gone ignored) {
                // nobody to tell
            }
            return 1;
        } catch (Gone g) {
            return 1;
        }
    }

    private Handler hello(Map<String, Object> m) {
        if (!Long.valueOf(PROTOCOL).equals(m.get("protocol"))) {
            throw new Fatal("unsupported protocol " + m.get("protocol"));
        }
        if ("sandbox".equals(m.get("mode"))) {
            throw new Fatal("sandbox mode not supported");
        }
        Handler handler;
        try {
            handler = factory.get();
        } catch (Exception e) {
            throw new Fatal("could not create the handler: " + e);
        }
        if ("snapshot".equals(m.get("start"))) {
            if (!(handler instanceof Snapshotter s)) {
                throw new Fatal("the journal starts from a snapshot but the handler is not a Snapshotter");
            }
            try {
                s.restore(unb64(m.get("snapshot")));
            } catch (Exception e) {
                throw new Fatal("could not restore the snapshot: " + e);
            }
        }
        Supplier<Map<String, Object>> environment =
                options.environment != null ? options.environment : () -> collectEnvironment(options.recorderCommand);
        send(msg(
                "ready",
                "protocol", (long) PROTOCOL,
                "sdk", Kavach.PRODUCER,
                "invariants", Failure.invariantNames(handler),
                "environment", environment.get()));
        return handler;
    }

    private void step(Handler handler, Map<String, Object> m) {
        Input input = new Input(
                String.valueOf(m.getOrDefault("source", "")),
                String.valueOf(m.getOrDefault("position", "")),
                unb64(m.getOrDefault("data", "")));
        aborted = false;
        Failure failure = null;
        try {
            handler.handle(new HostEnv(), input);
        } catch (Signal s) {
            if (!(s instanceof Abort)) {
                throw s;
            }
        } catch (Throwable t) {
            failure = Failure.classify(t);
        }
        if (aborted) {
            send(msg("done", "outcome", "aborted"));
            return;
        }
        if (failure == null) {
            failure = Failure.checkInvariants(handler);
        }
        Map<String, Object> done = msg("done", "outcome", failure == null ? "ok" : failure.kind());
        if (failure != null) {
            done.put("message", failure.message());
            if (!failure.detail().isEmpty()) {
                done.put("detail", failure.detail());
            }
        }
        send(done);
    }

    private final class HostEnv implements Env {
        @Override
        public long nowNanos() {
            Object ns = request(msg("clock"), "clock").get("unix_nanos");
            try {
                return Long.parseLong(String.valueOf(ns));
            } catch (NumberFormatException e) {
                throw new Fatal("clock answer without unix_nanos");
            }
        }

        @Override
        public byte[] random(int n) {
            if (n <= 0) {
                return new byte[0];
            }
            return unb64(request(msg("rand", "n", (long) n), "rand").getOrDefault("data", ""));
        }

        @Override
        public byte[] query(String gateway, byte[] request) {
            Gateway gw = options.gateways.lookup(gateway);
            Map<String, Object> ans = request(
                    msg("gateway", "gateway", gateway, "request", b64(request), "scope", gw.scope().wire()), "gateway");
            Object err = ans.get("error");
            if (err instanceof String e && !e.isEmpty()) {
                throw new GatewayException(e);
            }
            return unb64(ans.getOrDefault("response", ""));
        }

        @Override
        public Optional<byte[]> config(String key) {
            Map<String, Object> ans = request(msg("config", "key", key), "config");
            if (!Boolean.TRUE.equals(ans.get("present"))) {
                return Optional.empty();
            }
            return Optional.of(unb64(ans.getOrDefault("value", "")));
        }

        @Override
        public void emit(String sink, byte[] data, boolean local) {
            if (aborted) {
                throw new Abort();
            }
            send(msg("emit", "sink", sink, "data", b64(data), "scope", local ? "local" : "remote"));
        }
    }
}
