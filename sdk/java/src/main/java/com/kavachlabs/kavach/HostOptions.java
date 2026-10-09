package com.kavachlabs.kavach;

import java.util.List;
import java.util.Map;
import java.util.function.Supplier;

/** Options of a replay host. Sandbox mode (SPEC §6.3) is not supported. */
public final class HostOptions {
    Gateways gateways = new Gateways();
    List<String> recorderCommand;
    Supplier<Map<String, Object>> environment;

    public static HostOptions defaults() {
        return new HostOptions();
    }

    /** The gateways the handler may query; names must match those used when recording. */
    public HostOptions gateways(Gateways gateways) {
        this.gateways = gateways;
        return this;
    }

    /** The {@code kavach-recorder} argument vector used to collect {@code ready.environment}. */
    public HostOptions recorderCommand(List<String> command) {
        this.recorderCommand = command;
        return this;
    }

    /** Replaces the collection of {@code ready.environment} (for tests). */
    public HostOptions environment(Supplier<Map<String, Object>> environment) {
        this.environment = environment;
        return this;
    }
}
