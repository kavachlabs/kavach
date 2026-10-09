package com.kavachlabs.kavach;

import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.util.Map;

/** Encoding of the record stream (SPEC §10.2) and its record payloads (§4). */
final class Wire {
    private Wire() {}

    // Frame kinds (§10.2).
    static final int OPEN = 0x01;
    static final int RECORD = 0x02;
    static final int STEP_END = 0x03;
    static final int FACTS = 0x04;
    static final int SNAPSHOT = 0x05;
    static final int FLUSH = 0x06;
    static final int CLOSE = 0x07;

    // Record types (§4).
    static final int INPUT = 0x01;
    static final int CLOCK = 0x02;
    static final int RAND = 0x03;
    static final int OUTPUT = 0x04;
    static final int MARKER = 0x05;
    static final int GATEWAY = 0x07;
    static final int CONFIG = 0x09;

    private static final int CRITICAL = 0x01;

    /** A growable byte buffer with the spec's field encodings. */
    static final class Buf extends ByteArrayOutputStream {
        Buf u8(int b) {
            write(b);
            return this;
        }

        Buf uvarint(long n) {
            while ((n & ~0x7FL) != 0) {
                write((int) ((n & 0x7F) | 0x80));
                n >>>= 7;
            }
            write((int) n);
            return this;
        }

        Buf raw(byte[] b) {
            write(b, 0, b.length);
            return this;
        }

        Buf bytes(byte[] b) {
            return uvarint(b.length).raw(b);
        }

        Buf str(String s) {
            return bytes(s.getBytes(StandardCharsets.UTF_8));
        }

        Buf i64(long v) {
            for (int k = 0; k < 8; k++) {
                write((int) (v >>> (8 * k)) & 0xFF);
            }
            return this;
        }
    }

    static byte[] frame(int kind, byte[] payload) {
        Buf b = new Buf();
        b.uvarint(1 + payload.length).u8(kind).raw(payload);
        return b.toByteArray();
    }

    private static byte[] record(int type, Buf payload, boolean critical) {
        Buf b = new Buf();
        b.u8(type).u8(critical ? CRITICAL : 0).raw(payload.toByteArray());
        return frame(RECORD, b.toByteArray());
    }

    static byte[] input(String source, String position, byte[] data) {
        return record(INPUT, new Buf().str(source).str(position).bytes(data), false);
    }

    static byte[] clock(long unixNanos) {
        return record(CLOCK, new Buf().i64(unixNanos), false);
    }

    static byte[] rand(byte[] data) {
        return record(RAND, new Buf().bytes(data), false);
    }

    static byte[] output(String sink, byte[] data, boolean local) {
        return record(OUTPUT, new Buf().str(sink).bytes(data).u8(local ? 1 : 0), false);
    }

    static byte[] marker(String kind, String message, byte[] data) {
        return record(MARKER, new Buf().str(kind).str(message).bytes(data), false);
    }

    static byte[] gateway(String gateway, byte[] request, byte[] response, String error, boolean local) {
        return record(
                GATEWAY,
                new Buf().str(gateway).bytes(request).bytes(response).str(error).u8(local ? 1 : 0),
                true);
    }

    static byte[] config(String key, byte[] value, String source) {
        boolean present = value != null;
        return record(
                CONFIG,
                new Buf().str(key).u8(present ? 1 : 0).bytes(present ? value : new byte[0]).str(source),
                true);
    }

    static byte[] stepEnd() {
        return frame(STEP_END, new byte[0]);
    }

    static byte[] snapshot(byte[] data) {
        return frame(SNAPSHOT, new Buf().bytes(data).toByteArray());
    }

    static byte[] flush(boolean durable) {
        return frame(FLUSH, new byte[] {(byte) (durable ? 1 : 0)});
    }

    static byte[] close() {
        return frame(CLOSE, new byte[0]);
    }

    /** A {@code facts} frame: an environment payload whose facts all have form 0 (value). */
    static byte[] facts(Map<String, byte[]> facts) {
        Buf b = new Buf().uvarint(facts.size());
        for (Map.Entry<String, byte[]> e : facts.entrySet()) {
            b.str(e.getKey()).u8(0).bytes(e.getValue());
        }
        return frame(FACTS, b.toByteArray());
    }
}
