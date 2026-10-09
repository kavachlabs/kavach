package com.kavachlabs.kavach;

/** One effect a handler requested with {@link Env#emit}. */
public record Output(String sink, byte[] data, boolean local) {}
