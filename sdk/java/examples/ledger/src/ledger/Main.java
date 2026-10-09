package ledger;

import com.kavachlabs.kavach.Input;
import com.kavachlabs.kavach.Kavach;
import com.kavachlabs.kavach.Output;
import com.kavachlabs.kavach.Recorder;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

/**
 * Kavach's demo service, in Java.
 *
 * <pre>
 * java -cp build/classes:build/examples ledger.Main --in events.jsonl              # the buggy build
 * java -cp build/classes:build/examples ledger.Main --in events.jsonl --fix        # the fixed build
 * </pre>
 *
 * Options: {@code --in FILE} (default {@code events.jsonl} next to this
 * example), {@code --fixtures DIR} (default {@code fixtures}), {@code --fix}.
 *
 * <p>The build is chosen by a command-line flag and not an environment variable
 * on purpose: environment variables are served from the journal on replay, so a
 * replay of the buggy build's fixture would otherwise run the buggy code.
 * Replay hosts are therefore {@code ... ledger.Main} (old) and
 * {@code ... ledger.Main --fix} (new); the driver appends {@code kavach-host}.
 *
 * <p>If {@code kavach-recorder} is available ({@code $KAVACH_RECORDER} or PATH)
 * the service records through it; otherwise it says so loudly and runs
 * unrecorded.
 */
public final class Main {
    private Main() {}

    public static void main(String[] args) throws IOException {
        String in = "examples/ledger/events.jsonl";
        String fixtures = "fixtures";
        boolean fix = false;
        for (int i = 0; i < args.length; i++) {
            switch (args[i]) {
                case "--fix" -> fix = true;
                case "--in" -> in = args[++i];
                case "--fixtures" -> fixtures = args[++i];
                case Kavach.HOST_ARG -> { }
                default -> {
                    System.err.println("usage: ledger.Main [--fix] [--in FILE] [--fixtures DIR]");
                    System.exit(2);
                }
            }
        }
        final boolean fixed = fix;
        // Lets the kavach CLI use this program to replay fixtures.
        Kavach.maybeHost(args, () -> new Ledger(fixed));

        Recorder rec = new Recorder(
                new Ledger(fixed),
                new Recorder.Options().service("ledger").dir(fixtures).deliver(Main::deliver));
        int status = 0;
        try {
            List<String> lines = Files.readAllLines(Path.of(in), StandardCharsets.UTF_8);
            String source = "file:" + Path.of(in).getFileName();
            for (int n = 1; n <= lines.size(); n++) {
                String line = lines.get(n - 1).strip();
                if (line.isEmpty()) {
                    continue;
                }
                Recorder.StepResult result = rec.step(new Input(source, Integer.toString(n), line.getBytes(StandardCharsets.UTF_8)));
                if (result.kind().equals("panic")) {
                    // Stop the service, as the Go demo does. The recorder has
                    // already marked the failure and cut a fixture.
                    System.err.println("ledger: line " + n + ": panic: " + result.message());
                    status = 1;
                    break;
                } else if (!result.ok()) {
                    System.err.println("ledger: line " + n + ": " + result.kind() + ": " + result.message());
                }
            }
        } finally {
            rec.close();
        }
        System.exit(status);
    }

    private static void deliver(List<Output> outs) {
        for (Output o : outs) {
            System.out.printf("%-18s %s%n", o.sink(), new String(o.data(), StandardCharsets.UTF_8));
        }
    }
}
