package com.kavachlabs.kavach;

import java.time.Instant;
import java.util.Optional;

/** Passed to a handler for each input. Everything nondeterministic must go through it. */
public interface Env {
    /** Nanoseconds since the Unix epoch, UTC. */
    long nowNanos();

    /** The clock as an {@link Instant}. */
    default Instant now() {
        long ns = nowNanos();
        return Instant.ofEpochSecond(Math.floorDiv(ns, 1_000_000_000L), Math.floorMod(ns, 1_000_000_000L));
    }

    /** {@code n} random bytes. */
    byte[] random(int n);

    /** Queries a registered gateway; throws {@link GatewayException} if it failed. */
    byte[] query(String gateway, byte[] request);

    /** Reads a config value that can change what the handler does. */
    Optional<byte[]> config(String key);

    /** Requests an effect. Delivered only after the step succeeds. */
    void emit(String sink, byte[] data, boolean local);

    /** Requests a remote effect. */
    default void emit(String sink, byte[] data) {
        emit(sink, data, false);
    }
}
