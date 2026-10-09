package com.kavachlabs.kavach;

/**
 * A registered gateway: the connection that makes the query, and its scope.
 * The connection is called when recording. Throw {@link GatewayException} (or
 * any exception) to report a failure; its text is recorded as the error.
 */
public record Gateway(Connection connection, Scope scope) {

    /** The application's connection to an external system. */
    @FunctionalInterface
    public interface Connection {
        byte[] call(byte[] request) throws Exception;
    }

    public Gateway {
        if (connection == null || scope == null) {
            throw new NullPointerException("connection and scope are required");
        }
    }

    public static Gateway remote(Connection connection) {
        return new Gateway(connection, Scope.REMOTE);
    }

    public static Gateway local(Connection connection) {
        return new Gateway(connection, Scope.LOCAL);
    }
}
