package com.kavachlabs.kavach;

import java.io.BufferedReader;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.security.SecureRandom;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.locks.LockSupport;
import java.util.concurrent.locks.ReentrantLock;
import java.util.function.Consumer;
import java.util.function.Function;
import java.util.function.IntFunction;
import java.util.function.LongSupplier;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Runs a handler step by step and writes everything to {@code kavach-recorder}
 * (SPEC §3.6, §10).
 *
 * <p>Reads are recorded live (the real clock, {@link SecureRandom}, the
 * registered gateway connections and the config provider); outputs are
 * delivered through {@link Options#deliver} only after the step succeeded. If
 * the recorder cannot be started, dies, or reports a fatal error, the problem
 * is logged on the {@code kavach} logger at SEVERE, recording stops and steps
 * carry on unrecorded; with {@link Options#required} a recorder that cannot be
 * started makes the constructor throw {@link RecorderException} instead.
 */
public final class Recorder implements AutoCloseable {
    private static final Logger LOG = Logger.getLogger("kavach");
    private static final SecureRandom RANDOM = new SecureRandom();

    /** Settings of a {@link Recorder}. */
    public static final class Options {
        String service;
        String start = "genesis";
        Boolean snapshots;
        Consumer<List<Output>> deliver;
        Gateways gateways = new Gateways();
        Function<String, byte[]> config;
        String configSource;
        Supplier<Map<String, byte[]>> flags;
        List<String> recorderCommand;
        boolean required;
        String handlerId;
        String dir;
        String compression;
        Integer level;
        Long blockBytes;
        Integer flushMs;
        Long segmentBytes;
        Long segmentSeconds;
        Integer retainSegments;
        List<String> secretKeys;
        Consumer<Map<String, Object>> onFixture;
        LongSupplier clockNanos;
        IntFunction<byte[]> randomBytes;
        boolean noRing;
        int ringBytes = Ring.DEFAULT;
        Duration readyTimeout = Duration.ofSeconds(10);
        Duration closeTimeout = Duration.ofSeconds(10);

        /** The service name, as in the journal header (required). */
        public Options service(String v) {
            service = v;
            return this;
        }

        /** {@code "genesis"} (default) or {@code "snapshot"}: the handler's current state is the journal's start. */
        public Options start(String v) {
            start = v;
            return this;
        }

        /** Whether to answer snapshot requests; default: whether the handler is a {@link Snapshotter}. */
        public Options snapshots(boolean v) {
            snapshots = v;
            return this;
        }

        /** Receives a successful step's outputs. */
        public Options deliver(Consumer<List<Output>> v) {
            deliver = v;
            return this;
        }

        public Options gateways(Gateways v) {
            gateways = v;
            return this;
        }

        /** Config provider; returns null when the key is unset. Default: environment variables. */
        public Options config(Function<String, byte[]> v) {
            config = v;
            return this;
        }

        /** Informational source recorded with each config read; default {@code "env"} or {@code "config"}. */
        public Options configSource(String v) {
            configSource = v;
            return this;
        }

        /** Feature flags, as {@code flag.*} facts, sent once at start. */
        public Options flags(Supplier<Map<String, byte[]>> v) {
            flags = v;
            return this;
        }

        /** The recorder's argument vector; else {@code $KAVACH_RECORDER}; else {@code kavach-recorder} on PATH. */
        public Options recorderCommand(List<String> v) {
            recorderCommand = v;
            return this;
        }

        /** Fail construction with {@link RecorderException} if the recorder cannot be started. */
        public Options required(boolean v) {
            required = v;
            return this;
        }

        public Options handlerId(String v) {
            handlerId = v;
            return this;
        }

        /** Directory for journal segments; fixtures go in its {@code fixtures/} subdirectory. */
        public Options dir(String v) {
            dir = v;
            return this;
        }

        public Options compression(String v) {
            compression = v;
            return this;
        }

        public Options level(int v) {
            level = v;
            return this;
        }

        public Options blockBytes(long v) {
            blockBytes = v;
            return this;
        }

        public Options flushMs(int v) {
            flushMs = v;
            return this;
        }

        public Options segmentBytes(long v) {
            segmentBytes = v;
            return this;
        }

        public Options segmentSeconds(long v) {
            segmentSeconds = v;
            return this;
        }

        public Options retainSegments(int v) {
            retainSegments = v;
            return this;
        }

        public Options secretKeys(List<String> v) {
            secretKeys = v;
            return this;
        }

        /** Called on the control thread with each {@code fixture} message. */
        public Options onFixture(Consumer<Map<String, Object>> v) {
            onFixture = v;
            return this;
        }

        /** Replaces the clock (nanoseconds since the epoch). */
        public Options clockNanos(LongSupplier v) {
            clockNanos = v;
            return this;
        }

        /** Replaces the random source. */
        public Options randomBytes(IntFunction<byte[]> v) {
            randomBytes = v;
            return this;
        }

        /** Carries the record stream over the recorder's pipe instead of a shared-memory ring (SPEC §10.7). */
        public Options noRing(boolean v) {
            noRing = v;
            return this;
        }

        /** The ring's capacity: a power of two of at least 64 KiB; default 8 MiB. */
        public Options ringBytes(int v) {
            ringBytes = v;
            return this;
        }

        public Options readyTimeout(Duration v) {
            readyTimeout = v;
            return this;
        }

        public Options closeTimeout(Duration v) {
            closeTimeout = v;
            return this;
        }
    }

    /** What happened in one {@link Recorder#step}. */
    public static final class StepResult {
        private final boolean ok;
        private final String kind;
        private final String message;
        private final String detail;
        private final Throwable exception;
        private final List<Output> outputs;

        StepResult(boolean ok, String kind, String message, String detail, Throwable exception, List<Output> outputs) {
            this.ok = ok;
            this.kind = kind;
            this.message = message;
            this.detail = detail;
            this.exception = exception;
            this.outputs = outputs;
        }

        public boolean ok() {
            return ok;
        }

        /** Empty, or {@code panic}, {@code error} or {@code invariant}. */
        public String kind() {
            return kind;
        }

        public String message() {
            return message;
        }

        /** The stack trace, or an invariant's failure. */
        public String detail() {
            return detail;
        }

        /** What the handler threw, if it did. */
        public Throwable exception() {
            return exception;
        }

        public List<Output> outputs() {
            return outputs;
        }

        /** Throws {@link StepFailedException} if the step did not end ok. */
        public void throwIfFailed() {
            if (!ok) {
                throw new StepFailedException(kind, message, exception);
            }
        }
    }

    /** A step failed; see {@link StepResult#throwIfFailed()}. */
    public static final class StepFailedException extends RuntimeException {
        private final String kind;

        StepFailedException(String kind, String message, Throwable cause) {
            super(kind + ": " + message, cause);
            this.kind = kind;
        }

        public String kind() {
            return kind;
        }
    }

    /** The recorder argument vector: {@code command}, else $KAVACH_RECORDER, else PATH; null if none. */
    public static List<String> findRecorder(List<String> command) {
        if (command != null && !command.isEmpty()) {
            return List.copyOf(command);
        }
        String env = System.getenv("KAVACH_RECORDER");
        if (env != null && !env.isEmpty()) {
            return List.of(env);
        }
        String path = System.getenv("PATH");
        if (path != null) {
            for (String dir : path.split(File.pathSeparator)) {
                if (dir.isEmpty()) {
                    continue;
                }
                File f = new File(dir, "kavach-recorder");
                if (f.isFile() && f.canExecute()) {
                    return List.of(f.getPath());
                }
            }
        }
        return null;
    }

    private final Handler handler;
    private final Options opt;
    private final boolean snapshots;
    private final Function<String, byte[]> config;
    private final String configSource;
    private final LongSupplier clock;
    private final IntFunction<byte[]> random;

    private final ReentrantLock stepLock = new ReentrantLock();
    private volatile Thread stepThread;
    private final Object durableLock = new Object();
    private long durableSent; // guarded by stepLock
    private long durableCount; // guarded by durableLock
    private volatile boolean active;
    private volatile boolean failedLogged;
    private volatile boolean closing;
    private volatile boolean closed;
    private volatile boolean snapshotRequested;
    private final CountDownLatch closedAck = new CountDownLatch(1);
    private final CountDownLatch ready = new CountDownLatch(1);
    private ByteArrayOutputStream buf; // the open step's frames, step thread only
    private List<Output> outputs = new ArrayList<>();
    private Ring ring; // null on the pipe transport
    private boolean belled; // the doorbell rang since the ring was last at most half full
    private Process proc;
    private OutputStream procIn;
    private Thread controlThread;
    private Thread hook;
    private volatile String file;
    private volatile String run;

    public Recorder(Handler handler, Options options) {
        this.opt = options;
        if (options.service == null || options.service.isEmpty()) {
            throw new IllegalArgumentException("Options.service is required");
        }
        if (!options.start.equals("genesis") && !options.start.equals("snapshot")) {
            throw new IllegalArgumentException("start must be \"genesis\" or \"snapshot\"");
        }
        boolean canSnapshot = handler instanceof Snapshotter;
        if (options.start.equals("snapshot") && !canSnapshot) {
            throw new IllegalArgumentException("start \"snapshot\" needs a handler that implements Snapshotter");
        }
        if (options.snapshots != null && options.snapshots && !canSnapshot) {
            throw new IllegalArgumentException("snapshots(true) needs a handler that implements Snapshotter");
        }
        if (options.ringBytes < Ring.MIN || Integer.bitCount(options.ringBytes) != 1) {
            throw new IllegalArgumentException("ringBytes must be a power of two of at least 64 KiB");
        }
        this.handler = handler;
        this.snapshots = options.snapshots != null ? options.snapshots : canSnapshot;
        this.config = options.config != null
                ? options.config
                : k -> {
                    String v = System.getenv(k);
                    return v == null ? null : v.getBytes(StandardCharsets.UTF_8);
                };
        this.configSource = options.configSource != null ? options.configSource : options.config != null ? "config" : "env";
        this.clock = options.clockNanos != null ? options.clockNanos : Recorder::wallClockNanos;
        this.random = options.randomBytes != null
                ? options.randomBytes
                : n -> {
                    byte[] b = new byte[n];
                    RANDOM.nextBytes(b);
                    return b;
                };

        try {
            List<String> cmd = findRecorder(options.recorderCommand);
            if (cmd == null) {
                throw new RecorderException(
                        "kavach-recorder not found (set recorderCommand, $KAVACH_RECORDER or put it on PATH)");
            }
            ring = newRing();
            spawn(cmd);
            writePipe(Wire.frame(Wire.OPEN, Json.write(openObject()).getBytes(StandardCharsets.UTF_8)));
            Map<String, byte[]> facts = new LinkedHashMap<>();
            facts.put("host.runtime", Kavach.runtime().getBytes(StandardCharsets.UTF_8));
            if (options.flags != null) {
                facts.putAll(options.flags.get());
            }
            write(Wire.facts(facts));
            if (options.start.equals("snapshot")) {
                write(Wire.snapshot(((Snapshotter) handler).snapshot()));
            }
            if (options.required) {
                if (!ready.await(options.readyTimeout.toNanos(), TimeUnit.NANOSECONDS) || !active) {
                    throw new RecorderException("kavach-recorder did not become ready");
                }
            }
        } catch (Exception e) {
            if (e instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            releaseRing();
            if (options.required) {
                kill();
                if (e instanceof RecorderException re) {
                    throw re;
                }
                throw new RecorderException("kavach-recorder could not be started: " + e, e);
            }
            fail("could not start the recorder: " + e);
        }
        hook = new Thread(this::closeFromHook, "kavach-close");
        try {
            Runtime.getRuntime().addShutdownHook(hook);
        } catch (IllegalStateException e) {
            hook = null; // the JVM is already shutting down
        }
    }

    private Map<String, Object> openObject() {
        Map<String, Object> o = new LinkedHashMap<>();
        o.put("protocol", 1L);
        o.put("service", opt.service);
        o.put("start", opt.start);
        o.put("producer", Kavach.PRODUCER);
        o.put("snapshots", snapshots);
        putIfSet(o, "handler", opt.handlerId);
        putIfSet(o, "dir", opt.dir);
        putIfSet(o, "compression", opt.compression);
        putIfSet(o, "level", opt.level);
        putIfSet(o, "block_bytes", opt.blockBytes);
        putIfSet(o, "flush_ms", opt.flushMs);
        putIfSet(o, "segment_bytes", opt.segmentBytes);
        putIfSet(o, "segment_seconds", opt.segmentSeconds);
        putIfSet(o, "retain_segments", opt.retainSegments);
        if (opt.secretKeys != null && !opt.secretKeys.isEmpty()) {
            o.put("secret_keys", opt.secretKeys);
        }
        if (ring != null) {
            o.put("ring", (long) ring.capacity());
            o.put("ring_path", ring.path());
        }
        return o;
    }

    private Ring newRing() {
        if (opt.noRing) {
            return null;
        }
        try {
            return Ring.create(opt.ringBytes);
        } catch (IOException | RuntimeException e) {
            LOG.warning("kavach: no shared-memory ring, recording over the pipe: " + e);
            return null;
        }
    }

    /** The recorder unlinks the ring file once it has mapped it; this covers one that never did. */
    private void releaseRing() {
        if (ring != null) {
            ring.delete();
        }
    }

    private static void putIfSet(Map<String, Object> o, String key, Object v) {
        if (v != null) {
            o.put(key, v);
        }
    }

    private static long wallClockNanos() {
        java.time.Instant now = java.time.Instant.now();
        return now.getEpochSecond() * 1_000_000_000L + now.getNano();
    }

    private void spawn(List<String> cmd) throws IOException {
        // The environment is left unchanged: the recorder collects env. facts from it (§10.1).
        ProcessBuilder pb = new ProcessBuilder(cmd);
        pb.redirectError(ProcessBuilder.Redirect.INHERIT);
        proc = pb.start();
        procIn = proc.getOutputStream();
        active = true;
        BufferedReader control = new BufferedReader(new InputStreamReader(proc.getInputStream(), StandardCharsets.UTF_8));
        controlThread = new Thread(() -> controlLoop(control), "kavach-control");
        controlThread.setDaemon(true);
        controlThread.start();
    }

    private void kill() {
        active = false;
        Process p = proc;
        if (p == null) {
            return;
        }
        p.destroyForcibly();
        try {
            p.waitFor(2, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    /** Stops recording, loudly, once. Never throws. */
    private void fail(String reason) {
        boolean was = active;
        active = false;
        if (!failedLogged && (was || !closing)) {
            failedLogged = true;
            LOG.severe("kavach: " + reason + "; recording has stopped and steps run unrecorded");
        }
        synchronized (durableLock) {
            durableLock.notifyAll();
        }
    }

    private void controlLoop(BufferedReader control) {
        try {
            String line;
            while ((line = control.readLine()) != null) {
                Map<?, ?> msg;
                try {
                    msg = (Map<?, ?>) Json.parse(line);
                } catch (RuntimeException e) {
                    LOG.warning("kavach: unreadable message from the recorder: " + abbreviate(line));
                    continue;
                }
                if (msg.get("t") instanceof String t) {
                    onControl(t, msg);
                }
            }
        } catch (IOException e) {
            if (!closing) {
                LOG.warning("kavach: control stream failed: " + e);
            }
        }
        if (!closing) {
            fail("the recorder exited unexpectedly");
        }
        closedAck.countDown();
        ready.countDown();
        synchronized (durableLock) {
            durableLock.notifyAll();
        }
    }

    private static String abbreviate(String s) {
        return s.length() <= 200 ? s : s.substring(0, 200) + "...";
    }

    @SuppressWarnings("unchecked")
    private void onControl(String t, Map<?, ?> msg) {
        switch (t) {
            case "ready" -> {
                file = msg.get("file") instanceof String s ? s : null;
                run = msg.get("run") instanceof String s ? s : null;
                ready.countDown();
            }
            case "snapshot_request" -> snapshotRequested = true;
            case "durable" -> {
                synchronized (durableLock) {
                    durableCount++;
                    durableLock.notifyAll();
                }
            }
            case "segment" -> LOG.fine("kavach: new journal segment " + msg.get("file"));
            case "fixture" -> {
                LOG.warning("kavach: wrote fixture " + msg.get("file") + " (input seq " + msg.get("seq") + ", "
                        + msg.get("failure") + ")");
                if (opt.onFixture != null) {
                    try {
                        opt.onFixture.accept((Map<String, Object>) msg);
                    } catch (RuntimeException e) {
                        LOG.log(Level.WARNING, "kavach: onFixture callback failed", e);
                    }
                }
            }
            case "error" -> {
                boolean fatal = Boolean.TRUE.equals(msg.get("fatal"));
                LOG.severe("kavach: recorder " + (fatal ? "fatal error" : "error") + ": " + msg.get("message"));
                if (fatal) {
                    fail("the recorder reported a fatal error: " + msg.get("message"));
                }
            }
            case "closed" -> closedAck.countDown();
            default -> LOG.fine("kavach: ignoring recorder message " + t);
        }
    }

    private void write(byte[] data) {
        if (!active || data.length == 0) {
            return;
        }
        if (ring == null) {
            writePipe(data);
            return;
        }
        int off = 0;
        while (off < data.length) {
            int n = ring.tryPublish(data, off, data.length - off);
            if (n == 0) {
                // Full: the recorder drains the ring on the doorbell. If it has
                // exited, the control thread stops recording and ends the wait.
                bell();
                while (n == 0 && active && proc.isAlive()) {
                    LockSupport.parkNanos(20_000);
                    n = ring.tryPublish(data, off, data.length - off);
                }
                if (n == 0) {
                    if (active) {
                        fail("the recorder exited unexpectedly");
                    }
                    return;
                }
            }
            off += n;
            boolean over = ring.used() > ring.capacity() / 2;
            if (over && !belled) {
                bell();
            }
            belled = over;
        }
    }

    /** Wakes a recorder that reads the ring (SPEC §10.7). */
    private void bell() {
        if (ring != null && active) {
            writePipe(new byte[] {1});
        }
    }

    private void writePipe(byte[] data) {
        try {
            procIn.write(data);
            procIn.flush();
        } catch (IOException e) {
            fail("could not write to the recorder: " + e);
        }
    }

    private void rec(byte[] frame) {
        if (buf != null && active) {
            buf.write(frame, 0, frame.length);
        }
    }

    /** The first segment's path, once the recorder is ready; else null. */
    public String file() {
        return file;
    }

    /** The recorder's run id, once ready; else null. */
    public String run() {
        return run;
    }

    /** Whether records are still reaching the recorder. */
    public boolean recording() {
        return active;
    }

    /**
     * Runs the handler on one input. A handler failure (any {@link Exception}
     * or {@link Error}) does not propagate: it is recorded and described by the
     * result; see {@link StepResult#throwIfFailed()}.
     */
    public StepResult step(Input input) {
        stepLock.lock();
        try {
            if (closed) {
                throw new IllegalStateException("kavach: step on a closed Recorder");
            }
            stepThread = Thread.currentThread();
            try {
                return doStep(input);
            } finally {
                stepThread = null;
            }
        } finally {
            stepLock.unlock();
        }
    }

    private StepResult doStep(Input input) {
        answerSnapshotRequest();
        buf = new ByteArrayOutputStream();
        outputs = new ArrayList<>();
        // The input goes in before the handler runs, so that a step that kills
        // the process still leaves it on record (§10.2).
        write(Wire.input(input.source(), input.position(), input.data()));
        Failure failure = null;
        Throwable raised = null;
        try {
            handler.handle(new RecordEnv(), input);
        } catch (Throwable t) {
            failure = Failure.classify(t);
            raised = t;
        }
        if (failure == null) {
            failure = Failure.checkInvariants(handler);
        }
        if (failure != null) {
            rec(Wire.marker(failure.kind(), failure.message(), failure.detail().getBytes(StandardCharsets.UTF_8)));
        }
        rec(Wire.stepEnd());
        ByteArrayOutputStream done = buf;
        List<Output> outs = outputs;
        buf = null;
        outputs = new ArrayList<>();
        write(done.toByteArray());
        if (failure != null) {
            return new StepResult(false, failure.kind(), failure.message(), failure.detail(), raised, outs);
        }
        if (opt.deliver != null && !outs.isEmpty()) {
            opt.deliver.accept(outs);
        }
        return new StepResult(true, "", "", "", null, outs);
    }

    private void answerSnapshotRequest() {
        if (!snapshotRequested) {
            return;
        }
        snapshotRequested = false;
        if (!active || !snapshots) {
            return;
        }
        byte[] data;
        try {
            data = ((Snapshotter) handler).snapshot();
        } catch (RuntimeException e) {
            LOG.log(Level.SEVERE, "kavach: snapshot failed; staying in the current segment", e);
            return;
        }
        write(Wire.snapshot(data));
    }

    /**
     * Asks the recorder to close its open block now. With {@code durable} also
     * waits (up to {@code timeout}) until it is on disk. Returns false if
     * recording has stopped or the wait timed out. Must be called between
     * steps, not from a handler.
     */
    public boolean flush(boolean durable, Duration timeout) {
        if (stepThread == Thread.currentThread()) {
            throw new IllegalStateException("kavach: flush() must not be called from inside a step");
        }
        long target;
        stepLock.lock();
        try {
            if (!active) {
                return false;
            }
            target = durable ? ++durableSent : 0;
            write(Wire.flush(durable));
            bell();
        } finally {
            stepLock.unlock();
        }
        if (!durable) {
            return active;
        }
        long deadline = System.nanoTime() + timeout.toNanos();
        synchronized (durableLock) {
            while (durableCount < target && active) {
                long left = deadline - System.nanoTime();
                if (left <= 0) {
                    break;
                }
                try {
                    TimeUnit.NANOSECONDS.timedWait(durableLock, left);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    break;
                }
            }
            return durableCount >= target;
        }
    }

    /** {@link #flush(boolean, Duration)} with a ten second timeout. */
    public boolean flush(boolean durable) {
        return flush(durable, Duration.ofSeconds(10));
    }

    /** Orderly shutdown: sends {@code close} and waits for {@code closed}. Idempotent. */
    @Override
    public void close() {
        if (stepThread == Thread.currentThread()) {
            throw new IllegalStateException("kavach: close() must not be called from inside a step");
        }
        stepLock.lock();
        try {
            closeLocked();
        } finally {
            stepLock.unlock();
        }
        Thread h = hook;
        if (h != null && h != Thread.currentThread()) {
            hook = null;
            try {
                Runtime.getRuntime().removeShutdownHook(h);
            } catch (IllegalStateException ignored) {
                // already shutting down
            }
        }
    }

    private void closeFromHook() {
        try {
            // A step that is still running (the JVM is exiting inside it) leaves
            // the stream ending mid-step, which the recorder marks as a crash.
            if (stepLock.tryLock(200, TimeUnit.MILLISECONDS)) {
                try {
                    closeLocked();
                } finally {
                    stepLock.unlock();
                }
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    private void closeLocked() {
        if (closed) {
            return;
        }
        closed = true;
        if (proc == null) {
            releaseRing();
            return;
        }
        if (active) {
            closing = true;
            write(Wire.close());
            bell();
            try {
                if (!closedAck.await(opt.closeTimeout.toNanos(), TimeUnit.NANOSECONDS)) {
                    LOG.warning("kavach: the recorder did not answer close within " + opt.closeTimeout.toSeconds() + "s");
                }
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }
        closing = true;
        active = false;
        try {
            procIn.close();
        } catch (IOException ignored) {
            // already gone
        }
        try {
            if (!proc.waitFor(2, TimeUnit.SECONDS)) {
                LOG.warning("kavach: the recorder is still running after close");
            }
            controlThread.join(2000);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
        releaseRing();
    }

    private final class RecordEnv implements Env {
        @Override
        public long nowNanos() {
            long ns = clock.getAsLong();
            rec(Wire.clock(ns));
            return ns;
        }

        @Override
        public byte[] random(int n) {
            if (n <= 0) {
                return new byte[0];
            }
            byte[] data = random.apply(n);
            rec(Wire.rand(data));
            return data;
        }

        @Override
        public byte[] query(String gateway, byte[] request) {
            Gateway gw = opt.gateways.lookup(gateway);
            String[] err = {""};
            byte[] resp = Gateways.call(gw, request, err);
            rec(Wire.gateway(gateway, request, resp, err[0], gw.scope() == Scope.LOCAL));
            if (!err[0].isEmpty()) {
                throw new GatewayException(err[0]);
            }
            return resp;
        }

        @Override
        public Optional<byte[]> config(String key) {
            byte[] v = config.apply(key);
            rec(Wire.config(key, v, configSource));
            return Optional.ofNullable(v);
        }

        @Override
        public void emit(String sink, byte[] data, boolean local) {
            outputs.add(new Output(sink, data, local));
            rec(Wire.output(sink, data, local));
        }
    }
}
