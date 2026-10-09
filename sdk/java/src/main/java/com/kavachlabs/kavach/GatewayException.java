package com.kavachlabs.kavach;

/** A gateway query failed. {@link #error()} is the text the connection reported. */
public class GatewayException extends RuntimeException {
    private final String error;

    public GatewayException(String error) {
        super(error);
        this.error = error;
    }

    public String error() {
        return error;
    }
}
