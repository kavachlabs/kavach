package com.kavachlabs.kavach;

import java.io.BufferedReader;
import java.io.FileDescriptor;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.function.Supplier;

/** Entry points of the SDK. */
public final class Kavach {
    private Kavach() {}

    public static final String VERSION = "0.1.0";

    /** Recorded in the journal header ({@code producer}) and sent in {@code ready.sdk}. */
    public static final String PRODUCER = "kavach-java/" + VERSION;

    /** The last argument that makes a program act as a replay host (SPEC §9.1). */
    public static final String HOST_ARG = "kavach-host";

    /** The {@code host.runtime} fact, e.g. {@code java-25.0.1}. */
    public static String runtime() {
        StringBuilder sb = new StringBuilder("java-");
        boolean first = true;
        for (int part : Runtime.version().version()) {
            if (!first) {
                sb.append('.');
            }
            first = false;
            sb.append(part);
        }
        return sb.toString();
    }

    /**
     * If this process was started as a replay host (its last argument is
     * {@code kavach-host}), serves the driver and exits the JVM; otherwise
     * returns at once. Call it first thing in {@code main}, before consuming
     * input or starting servers.
     */
    public static void maybeHost(String[] args, Supplier<Handler> factory) {
        maybeHost(args, factory, HostOptions.defaults());
    }

    /** As {@link #maybeHost(String[], Supplier)}, with gateways and other options. */
    public static void maybeHost(String[] args, Supplier<Handler> factory, HostOptions options) {
        if (args == null || args.length == 0 || !HOST_ARG.equals(args[args.length - 1])) {
            return;
        }
        // Take the protocol stream for ourselves, then point System.out at
        // standard error so that a handler that prints cannot corrupt it (§9.1).
        OutputStream proto = new FileOutputStream(FileDescriptor.out);
        System.out.flush();
        System.setOut(System.err);
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
        int code = new Host(factory, in, proto, options).run();
        try {
            proto.close();
        } catch (IOException ignored) {
            // the driver is gone
        }
        System.err.flush();
        System.exit(code);
    }
}
