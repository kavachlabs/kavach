package com.kavachlabs.kavach;

/** Fails the step as an {@code error} whose marker message is exactly the message. */
public class HandlerError extends RuntimeException {
    public HandlerError(String message) {
        super(message);
    }
}
