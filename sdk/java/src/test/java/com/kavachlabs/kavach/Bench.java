package com.kavachlabs.kavach;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.List;
import java.util.Random;

/**
 * Step overhead of the recorder, like Go's BenchmarkRecorderStep: one clock
 * read, one 8-byte random read and one emit per step, a 33-byte input, so 4
 * events a step. Run with {@code ./build.sh bench}.
 */
public final class Bench {
    private Bench() {}

    private static final int WARMUP_STEPS = 200_000;
    private static final int STEPS = 500_000;
    private static final int RUNS = 5;

    /** Snapshots, as a production handler would. */
    private static final class BenchHandler implements Handler, Snapshotter {
        @Override
        public void handle(Env env, Input in) {
            env.nowNanos();
            env.random(8);
            env.emit("entries", in.data());
        }

        @Override
        public byte[] snapshot() {
            return "{}".getBytes(StandardCharsets.UTF_8);
        }

        @Override
        public void restore(byte[] data) {}
    }

    private static double run(boolean ring) throws Exception {
        Path dir = Files.createTempDirectory("kavach-bench-");
        Random rnd = new Random(1);
        Recorder.Options o = new Recorder.Options()
                .service("bench")
                .dir(dir.toString())
                .recorderCommand(List.of(Tools.recorder()))
                .noRing(!ring)
                .randomBytes(n -> {
                    byte[] b = new byte[n];
                    rnd.nextBytes(b);
                    return b;
                })
                .required(true);
        Input in = new Input("bench", "0", "{\"account\":\"alice\",\"amount\":1000}".getBytes(StandardCharsets.UTF_8));
        try (Recorder r = new Recorder(new BenchHandler(), o)) {
            for (int i = 0; i < WARMUP_STEPS; i++) {
                r.step(in);
            }
            long t0 = System.nanoTime();
            for (int i = 0; i < STEPS; i++) {
                r.step(in);
            }
            return (System.nanoTime() - t0) / (double) ((long) STEPS * 4);
        }
    }

    public static void main(String[] args) throws Exception {
        for (String transport : new String[] {"pipe", "ring"}) {
            double[] ns = new double[RUNS];
            for (int i = 0; i < RUNS; i++) {
                ns[i] = run(transport.equals("ring"));
            }
            double[] sorted = ns.clone();
            Arrays.sort(sorted);
            System.out.printf("%-4s median %.1f ns/event of %d runs of %d steps: %s%n",
                    transport, sorted[RUNS / 2], RUNS, STEPS, Arrays.toString(ns));
        }
    }
}
