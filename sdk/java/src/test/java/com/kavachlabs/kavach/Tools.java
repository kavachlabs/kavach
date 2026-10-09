package com.kavachlabs.kavach;

import com.kavachlabs.kavach.conformance.RecorderCase;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;

/** Builds the real {@code kavach-recorder} and {@code kavach} from the repository, once per JVM. */
final class Tools {
    private Tools() {}

    private static Path dir;

    private static synchronized Path dir() throws IOException, InterruptedException {
        if (dir == null) {
            Path root = RecorderCase.specDir().toAbsolutePath().normalize().getParent();
            Path out = Files.createTempDirectory("kavach-tools-");
            Process p = new ProcessBuilder("go", "build", "-o", out.toString(), "./cmd/kavach-recorder", "./cmd/kavach")
                    .directory(root.toFile())
                    .inheritIO()
                    .start();
            if (p.waitFor() != 0) {
                throw new IOException("go build of the recorder failed");
            }
            dir = out;
        }
        return dir;
    }

    static String recorder() throws IOException, InterruptedException {
        return dir().resolve("kavach-recorder").toString();
    }

    static String kavach() throws IOException, InterruptedException {
        return dir().resolve("kavach").toString();
    }
}
