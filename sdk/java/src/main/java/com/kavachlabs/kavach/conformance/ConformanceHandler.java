package com.kavachlabs.kavach.conformance;

import com.kavachlabs.kavach.Env;
import com.kavachlabs.kavach.Gateway;
import com.kavachlabs.kavach.GatewayException;
import com.kavachlabs.kavach.Handler;
import com.kavachlabs.kavach.HandlerError;
import com.kavachlabs.kavach.Input;
import com.kavachlabs.kavach.Invariant;
import com.kavachlabs.kavach.Json;
import com.kavachlabs.kavach.Panic;
import com.kavachlabs.kavach.Snapshotter;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.Optional;

/**
 * The conformance handler of SPEC §9.6, exactly as specified. Its state is a
 * count; each step that does not fail adds 1, plus any {@code count} operations
 * (which take effect immediately, with no rollback).
 */
public final class ConformanceHandler implements Handler, Snapshotter {
    private long count;

    /** A gateway for every name that refuses to connect, for hosts (nothing is live there). */
    public static Gateway refusing() {
        return Gateway.remote(request -> {
            throw new GatewayException("conformance host has no live connections");
        });
    }

    @Override
    public void handle(Env env, Input input) throws Exception {
        Object parsed = Json.parse(new String(input.data(), StandardCharsets.UTF_8));
        for (Object o : (List<?>) parsed) {
            Map<?, ?> op = (Map<?, ?>) o;
            String kind = (String) op.get("op");
            switch (kind) {
                case "clock" -> env.emit("trace", compact("clock", Long.toString(env.nowNanos())));
                case "rand" -> env.emit("trace", env.random(((Number) op.get("n")).intValue()));
                case "gateway" -> {
                    byte[] request = ((String) op.get("request")).getBytes(StandardCharsets.UTF_8);
                    try {
                        env.emit("trace", env.query((String) op.get("gateway"), request));
                    } catch (GatewayException e) {
                        env.emit("trace", compact("error", e.error()));
                    }
                }
                case "config" -> {
                    Optional<byte[]> value = env.config((String) op.get("key"));
                    env.emit("trace", value.isPresent() ? value.get() : unset());
                }
                case "getenv" -> {
                    String value = System.getenv((String) op.get("name"));
                    env.emit("trace", value != null ? value.getBytes(StandardCharsets.UTF_8) : unset());
                }
                case "emit" -> env.emit((String) op.get("sink"), ((String) op.get("data")).getBytes(StandardCharsets.UTF_8));
                case "panic" -> throw new Panic((String) op.get("message"));
                case "error" -> throw new HandlerError((String) op.get("message"));
                case "print" -> System.out.println((String) op.get("text"));
                case "count" -> count += ((Number) op.get("n")).longValue();
                default -> throw new HandlerError("unknown operation '" + kind + "'");
            }
        }
        count++;
    }

    // {"key":"value"} with the spec's escaping and no whitespace.
    private static byte[] compact(String key, String value) {
        return ("{" + Json.quote(key) + ":" + Json.quote(value) + "}").getBytes(StandardCharsets.UTF_8);
    }

    private static byte[] unset() {
        return "{\"unset\":true}".getBytes(StandardCharsets.UTF_8);
    }

    @Override
    public byte[] snapshot() {
        return Long.toString(count).getBytes(StandardCharsets.US_ASCII);
    }

    @Override
    public void restore(byte[] data) {
        count = Long.parseLong(new String(data, StandardCharsets.US_ASCII));
    }

    @Override
    public List<Invariant> invariants() {
        return List.of(new Invariant("below_limit", () -> {
            if (count >= 1000) {
                throw new AssertionError("count is " + count);
            }
        }));
    }
}
