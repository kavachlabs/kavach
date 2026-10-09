package com.kavachlabs.kavach;

import java.nio.charset.StandardCharsets;
import java.util.List;

/**
 * The child JVM of the crash test: records a step that completes, then one
 * that kills its own process with SIGKILL, as the OOM killer would, once the
 * step's input is on record. Arguments: journal dir, "ring" or "pipe".
 */
public final class KilledService {
    private KilledService() {}

    public static void main(String[] args) throws Exception {
        Handler killer = (env, in) -> {
            if (new String(in.data(), StandardCharsets.UTF_8).equals("die")) {
                new ProcessBuilder("kill", "-9", Long.toString(ProcessHandle.current().pid())).start().waitFor();
                Thread.sleep(60_000);
            }
            env.nowNanos();
        };
        Recorder.Options o = new Recorder.Options()
                .service("killed")
                .dir(args[0])
                .recorderCommand(List.of(System.getenv("KAVACH_RECORDER")))
                .noRing(args[1].equals("pipe"));
        Recorder r = new Recorder(killer, o);
        r.step(new Input("t", "0", "fine".getBytes(StandardCharsets.UTF_8)));
        r.step(new Input("t", "1", "die".getBytes(StandardCharsets.UTF_8)));
    }
}
