package com.kavachlabs.kavach;

/** The recorder could not be started and recording was required. */
public class RecorderException extends RuntimeException {
    public RecorderException(String message) {
        super(message);
    }

    public RecorderException(String message, Throwable cause) {
        super(message, cause);
    }
}
