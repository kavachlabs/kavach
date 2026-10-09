package com.kavachlabs.kavach;

import java.util.HashMap;
import java.util.Map;

/** A registry of gateways by name. Names must be stable across builds (SPEC §4.7). */
public final class Gateways {
    private final Map<String, Gateway> byName = new HashMap<>();
    private Gateway any;

    /** A registry with nothing registered. */
    public Gateways() {}

    /** A registry in which every name is served by {@code gateway}. */
    public static Gateways any(Gateway gateway) {
        Gateways g = new Gateways();
        g.any = gateway;
        return g;
    }

    public Gateways register(String name, Gateway gateway) {
        byName.put(name, gateway);
        return this;
    }

    /** Registers a remote gateway. */
    public Gateways register(String name, Gateway.Connection connection) {
        return register(name, Gateway.remote(connection));
    }

    Gateway lookup(String name) {
        Gateway g = byName.get(name);
        if (g == null) {
            g = any;
        }
        if (g == null) {
            throw new UnknownGatewayException("kavach: gateway '" + name + "' is not registered");
        }
        return g;
    }

    /** Runs a connection; returns the response, or sets {@code err[0]} and returns empty. */
    static byte[] call(Gateway gw, byte[] request, String[] err) {
        try {
            byte[] resp = gw.connection().call(request);
            return resp == null ? new byte[0] : resp;
        } catch (GatewayException e) {
            err[0] = e.error();
        } catch (Exception e) {
            String m = e.getMessage();
            err[0] = m == null || m.isEmpty() ? e.toString() : m;
        }
        return new byte[0];
    }
}
