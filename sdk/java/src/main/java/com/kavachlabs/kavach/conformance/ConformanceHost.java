package com.kavachlabs.kavach.conformance;

import com.kavachlabs.kavach.Gateways;
import com.kavachlabs.kavach.HostOptions;
import com.kavachlabs.kavach.Kavach;

/** The conformance host (SPEC §9.6): run with {@code kavach-host} appended, via spec/host/run.py. */
public final class ConformanceHost {
    private ConformanceHost() {}

    public static void main(String[] args) {
        Kavach.maybeHost(
                args,
                ConformanceHandler::new,
                HostOptions.defaults().gateways(Gateways.any(ConformanceHandler.refusing())));
        System.err.println("usage: ConformanceHost kavach-host   (speaks the host protocol on stdin/stdout)\n"
                + "       run it through spec/host/run.py, see the README");
        System.exit(2);
    }
}
