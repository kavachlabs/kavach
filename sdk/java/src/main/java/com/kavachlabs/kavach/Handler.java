package com.kavachlabs.kavach;

import java.util.List;

/**
 * What a service implements: a single-writer function of its state and its
 * inputs. Everything nondeterministic goes through the {@link Env}. A handler
 * may also implement {@link Snapshotter} and override {@link #invariants()}.
 * Handlers are synchronous and called from one thread at a time.
 */
public interface Handler {
    void handle(Env env, Input input) throws Exception;

    /** Properties checked after every step that ended ok, in order. */
    default List<Invariant> invariants() {
        return List.of();
    }
}
