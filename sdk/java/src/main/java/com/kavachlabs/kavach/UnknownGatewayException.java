package com.kavachlabs.kavach;

/** The handler queried a gateway that was not registered. */
public class UnknownGatewayException extends RuntimeException {
    public UnknownGatewayException(String message) {
        super(message);
    }
}
