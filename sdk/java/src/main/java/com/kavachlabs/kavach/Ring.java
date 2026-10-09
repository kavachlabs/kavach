package com.kavachlabs.kavach;

import java.io.IOException;
import java.lang.invoke.MethodHandles;
import java.lang.invoke.VarHandle;
import java.nio.ByteOrder;
import java.nio.MappedByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.nio.file.attribute.PosixFilePermissions;

/**
 * The SDK end of the shared-memory ring (SPEC §10.7). The file keeps its name,
 * which goes to the recorder as {@code ring_path}, since a JVM cannot pass
 * descriptor 3 to a child.
 */
final class Ring {
    static final int HEADER = 256;
    static final int MIN = 64 << 10;
    static final int DEFAULT = 8 << 20;
    private static final int OFF_CAPACITY = 8;
    private static final int OFF_WRITE = 64;
    private static final int OFF_READ = 128;
    private static final VarHandle WORD = MethodHandles.byteBufferViewVarHandle(long[].class, ByteOrder.LITTLE_ENDIAN);

    private final Path path;
    private final int capacity;
    // shortcut: the garbage collector unmaps it, since unmapping on demand needs Java 22's Arena; upgrade when the target does.
    private final MappedByteBuffer mem;
    private long write; // only this end writes the word, so it is kept here
    private long used; // bytes the recorder had not consumed after the last publish

    private Ring(Path path, int capacity, MappedByteBuffer mem) {
        this.path = path;
        this.capacity = capacity;
        this.mem = mem;
    }

    /** Creates, sizes, maps and initializes a ring file readable by its owner only. */
    static Ring create(int capacity) throws IOException {
        if (capacity < MIN || Integer.bitCount(capacity) != 1) {
            throw new IllegalArgumentException("ring capacity must be a power of two of at least 64 KiB");
        }
        Path shm = Path.of("/dev/shm");
        Path dir = Files.isDirectory(shm) ? shm : Path.of(System.getProperty("java.io.tmpdir"));
        Path path = Files.createTempFile(
                dir, "kavach-ring-", "", PosixFilePermissions.asFileAttribute(PosixFilePermissions.fromString("rw-------")));
        try (FileChannel ch = FileChannel.open(path, StandardOpenOption.READ, StandardOpenOption.WRITE)) {
            MappedByteBuffer mem = ch.map(FileChannel.MapMode.READ_WRITE, 0, HEADER + (long) capacity);
            mem.put(0, "KVRING01".getBytes(StandardCharsets.US_ASCII));
            WORD.set(mem, OFF_CAPACITY, (long) capacity);
            return new Ring(path, capacity, mem);
        } catch (IOException | RuntimeException e) {
            Files.deleteIfExists(path);
            throw e;
        }
    }

    String path() {
        return path.toString();
    }

    int capacity() {
        return capacity;
    }

    /** Bytes the recorder had not consumed after the last publish, counting it. */
    long used() {
        return used;
    }

    /**
     * Publishes {@code len} bytes of {@code p} with one release store of
     * {@code write}, so the recorder sees all or none. If they do not fit it
     * publishes nothing and returns 0, unless they exceed the whole ring, in
     * which case it publishes as many as fit.
     */
    int tryPublish(byte[] p, int off, int len) {
        long inUse = write - (long) WORD.getAcquire(mem, OFF_READ);
        long free = capacity - inUse;
        int n = len;
        if (n > free) {
            if (n <= capacity) {
                return 0;
            }
            n = (int) free;
        }
        int at = HEADER + (int) (write & (capacity - 1));
        int first = Math.min(n, HEADER + capacity - at);
        mem.put(at, p, off, first);
        mem.put(HEADER, p, off + first, n - first);
        write += n;
        WORD.setRelease(mem, OFF_WRITE, write);
        used = inUse + n;
        return n;
    }

    /** Removes the file if the recorder did not (it unlinks the file once it has mapped it). */
    void delete() {
        try {
            Files.deleteIfExists(path);
        } catch (IOException ignored) {
            // a temp file; nothing more to do
        }
    }
}
