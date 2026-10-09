package com.kavachlabs.kavach;

/** Where an output or a gateway lives (SPEC §4.4, §4.7). */
public enum Scope {
    /** A system on another host. */
    REMOTE,
    /** A resource of the host the process runs on. */
    LOCAL;

    /** The name used in the host protocol. */
    String wire() {
        return this == LOCAL ? "local" : "remote";
    }
}
