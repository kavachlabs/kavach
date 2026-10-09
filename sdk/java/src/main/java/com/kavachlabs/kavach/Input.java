package com.kavachlabs.kavach;

/**
 * One event consumed by a handler.
 *
 * @param source where the event came from, e.g. {@code "kafka:wallet-events"}
 * @param position its position in that source, e.g. {@code "3:1042"}
 * @param data the event exactly as received
 */
public record Input(String source, String position, byte[] data) {}
