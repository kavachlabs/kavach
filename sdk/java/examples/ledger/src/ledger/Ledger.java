package ledger;

import com.kavachlabs.kavach.Env;
import com.kavachlabs.kavach.Handler;
import com.kavachlabs.kavach.HandlerError;
import com.kavachlabs.kavach.Input;
import com.kavachlabs.kavach.Invariant;
import com.kavachlabs.kavach.Json;
import com.kavachlabs.kavach.Snapshotter;
import java.nio.charset.StandardCharsets;
import java.time.format.DateTimeFormatter;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * A single-writer wallet ledger: folds events into balances.
 *
 * <p>The planted bug: an event with {@code "amount": null} is unboxed into a
 * {@code long} and throws {@link NullPointerException}. With {@code fix} set
 * it is rejected instead.
 */
public final class Ledger implements Handler, Snapshotter {
    private final boolean fix;
    private Map<String, Long> balances = new TreeMap<>();
    private long net; // deposits minus withdrawals

    public Ledger(boolean fix) {
        this.fix = fix;
    }

    @Override
    public void handle(Env env, Input input) {
        Map<?, ?> ev;
        try {
            ev = (Map<?, ?>) Json.parse(new String(input.data(), StandardCharsets.UTF_8));
        } catch (RuntimeException e) {
            throw new HandlerError("decode event at " + input.position() + ": " + e.getMessage());
        }
        if (fix && ev.get("amount") == null) {
            reject(env, ev, "missing amount");
            return;
        }
        long amount = (Long) ev.get("amount"); // NullPointerException when amount is null and fix is off
        if (amount <= 0) {
            reject(env, ev, "amount must be positive");
            return;
        }
        String at = DateTimeFormatter.ISO_INSTANT.format(env.now());
        String account = (String) ev.get("account");

        String kind = (String) ev.get("type");
        switch (kind == null ? "" : kind) {
            case "deposit" -> {
                net += amount;
                post(env, ev, account, amount, at);
            }
            case "withdraw" -> {
                if (balances.getOrDefault(account, 0L) < amount) {
                    reject(env, ev, "insufficient funds");
                    return;
                }
                net -= amount;
                post(env, ev, account, -amount, at);
            }
            case "transfer" -> {
                if (balances.getOrDefault(account, 0L) < amount) {
                    reject(env, ev, "insufficient funds");
                    return;
                }
                post(env, ev, account, -amount, at);
                post(env, ev, (String) ev.get("to"), amount, at);
            }
            default -> reject(env, ev, "unknown event type " + kind);
        }
    }

    private void post(Env env, Map<?, ?> ev, String account, long delta, String at) {
        long balance = balances.merge(account, delta, Long::sum);
        StringBuilder txn = new StringBuilder();
        for (byte b : env.random(8)) {
            txn.append(Character.forDigit((b >> 4) & 15, 16)).append(Character.forDigit(b & 15, 16));
        }
        Map<String, Object> entry = new LinkedHashMap<>();
        entry.put("txn", txn.toString());
        entry.put("event", ev.get("id"));
        entry.put("account", account);
        entry.put("delta", delta);
        entry.put("balance", balance);
        entry.put("at", at);
        env.emit("ledger.entries", Json.write(entry).getBytes(StandardCharsets.UTF_8));
    }

    private static void reject(Env env, Map<?, ?> ev, String reason) {
        Map<String, Object> rejection = new LinkedHashMap<>();
        rejection.put("event", ev.get("id"));
        rejection.put("reason", reason);
        env.emit("ledger.rejections", Json.write(rejection).getBytes(StandardCharsets.UTF_8));
    }

    @Override
    public List<Invariant> invariants() {
        return List.of(
                new Invariant("balances_non_negative", () -> {
                    for (Map.Entry<String, Long> e : balances.entrySet()) {
                        if (e.getValue() < 0) {
                            throw new AssertionError("account " + e.getKey() + " has balance " + e.getValue());
                        }
                    }
                }),
                new Invariant("money_conserved", () -> {
                    long sum = balances.values().stream().mapToLong(Long::longValue).sum();
                    if (sum != net) {
                        throw new AssertionError("balances sum to " + sum + ", deposits minus withdrawals is " + net);
                    }
                }));
    }

    @Override
    public byte[] snapshot() {
        Map<String, Object> state = new LinkedHashMap<>();
        state.put("balances", balances);
        state.put("net", net);
        return Json.write(state).getBytes(StandardCharsets.UTF_8);
    }

    @Override
    public void restore(byte[] data) {
        Map<?, ?> state = (Map<?, ?>) Json.parse(new String(data, StandardCharsets.UTF_8));
        Map<String, Long> restored = new TreeMap<>();
        for (Map.Entry<?, ?> e : ((Map<?, ?>) state.get("balances")).entrySet()) {
            restored.put((String) e.getKey(), (Long) e.getValue());
        }
        balances = restored;
        net = (Long) state.get("net");
    }
}
