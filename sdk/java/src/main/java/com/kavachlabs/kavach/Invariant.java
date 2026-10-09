package com.kavachlabs.kavach;

import java.util.function.BooleanSupplier;

/**
 * A named property of handler state that must hold after every step. It holds
 * when {@code check} returns normally and is violated when it throws.
 */
public record Invariant(String name, Check check) {

    /** The check; throw anything to report a violation. */
    @FunctionalInterface
    public interface Check {
        void run() throws Exception;
    }

    /** An invariant that is violated when {@code holds} returns false. */
    public static Invariant of(String name, BooleanSupplier holds) {
        return new Invariant(name, () -> {
            if (!holds.getAsBoolean()) {
                throw new AssertionError("check returned false");
            }
        });
    }
}
