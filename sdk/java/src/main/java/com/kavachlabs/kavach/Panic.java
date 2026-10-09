package com.kavachlabs.kavach;

/** Fails the step as a {@code panic} whose marker message is exactly the message. */
public class Panic extends RuntimeException {
    public Panic(String message) {
        super(message);
    }
}
