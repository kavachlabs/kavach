package com.kavachlabs.kavach;

/**
 * Implemented by a handler that can save and restore its state. A recorder
 * answers the recorder's snapshot requests with it (SPEC §10.4); a host
 * restores a journal that starts from a snapshot (SPEC §9.2).
 */
public interface Snapshotter {
    byte[] snapshot();

    void restore(byte[] data);
}
