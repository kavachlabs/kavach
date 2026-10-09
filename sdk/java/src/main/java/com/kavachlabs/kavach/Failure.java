package com.kavachlabs.kavach;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.util.List;

/** How a step failed: {@code kind} is {@code panic}, {@code error} or {@code invariant}. */
record Failure(String kind, String message, String detail) {

    /**
     * The failure mapping of SPEC §4.5, used both live and in the host.
     * {@link Panic} is a panic with exactly its message, {@link HandlerError}
     * an error with exactly its message, and any other {@link Throwable} a
     * panic with message {@code toString()}; the stack trace is the detail.
     */
    static Failure classify(Throwable t) {
        StringWriter sw = new StringWriter();
        t.printStackTrace(new PrintWriter(sw));
        String detail = sw.toString();
        if (t instanceof Panic p) {
            return new Failure("panic", String.valueOf(p.getMessage()), detail);
        }
        if (t instanceof HandlerError e) {
            return new Failure("error", String.valueOf(e.getMessage()), detail);
        }
        return new Failure("panic", t.toString(), detail);
    }

    /** The first violated invariant, in declaration order, or null. */
    static Failure checkInvariants(Handler handler) {
        for (Invariant inv : handler.invariants()) {
            try {
                inv.check().run();
            } catch (Throwable t) {
                String msg = t.getMessage();
                return new Failure("invariant", inv.name(), msg == null || msg.isEmpty() ? t.toString() : msg);
            }
        }
        return null;
    }

    static List<String> invariantNames(Handler handler) {
        return handler.invariants().stream().map(Invariant::name).toList();
    }
}
