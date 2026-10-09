// The Kavach demo service in C++: a single-writer wallet ledger that folds a
// JSON-lines stream of events into balances. A port of examples/ledger (Go).
//
// One upstream event has "amount": null. The buggy build dereferences the
// missing amount; std::optional::value() throws, the wrapper turns the
// exception into a `panic` marker and the recorder writes a fixture.
//
//   ledger-buggy --in events.jsonl --fixtures fixtures   # panics at evt-008
//   ledger-fixed --in events.jsonl --fixtures fixtures   # rejects it instead
//
// The fix is selected at build time (two executables from one source), never
// from the environment: environment variables are served from the journal on
// replay, so a handler that read one would not behave the same.
#include <cstdio>
#include <ctime>
#include <fstream>
#include <iostream>
#include <map>
#include <optional>
#include <sstream>
#include <string>

#include "kavach/kavach.hpp"

#ifdef LEDGER_FIX
constexpr bool kFixNullAmount = true;
#else
constexpr bool kFixNullAmount = false;
#endif

namespace {

// ---- a tiny parser for the flat events the ledger consumes ----

struct Event {
  std::string id, type, account, to;
  std::optional<std::int64_t> amount;  // null or absent -> nullopt
};

class Scanner {
 public:
  explicit Scanner(std::string_view s) : s_(s) {}

  Event event() {
    Event ev;
    ws();
    expect('{');
    ws();
    if (peek() == '}') return (get(), ev);
    for (;;) {
      ws();
      std::string key = string();
      ws();
      expect(':');
      ws();
      if (peek() == '"') {
        std::string v = string();
        if (key == "id") ev.id = v;
        else if (key == "type") ev.type = v;
        else if (key == "account") ev.account = v;
        else if (key == "to") ev.to = v;
        else if (key == "amount") fail("amount must be a number or null");
      } else if (word("null") || word("true") || word("false")) {
        if (key == "amount" && s_.substr(pos_ - 4, 4) != "null") fail("amount must be a number or null");
      } else {
        std::int64_t n = number();
        if (key == "amount") ev.amount = n;
      }
      ws();
      if (peek() == ',') {
        get();
        continue;
      }
      expect('}');
      break;
    }
    ws();
    if (pos_ != s_.size()) fail("trailing characters");
    return ev;
  }

 private:
  [[noreturn]] void fail(const char* m) { throw std::runtime_error(std::string(m) + " at offset " + std::to_string(pos_)); }
  char peek() const { return pos_ < s_.size() ? s_[pos_] : '\0'; }
  char get() { return pos_ < s_.size() ? s_[pos_++] : '\0'; }
  void ws() {
    while (peek() == ' ' || peek() == '\t' || peek() == '\r' || peek() == '\n') pos_++;
  }
  void expect(char c) {
    if (get() != c) fail("unexpected character");
  }
  bool word(const char* w) {
    std::string_view v(w);
    if (s_.substr(pos_, v.size()) != v) return false;
    pos_ += v.size();
    return true;
  }
  std::string string() {
    expect('"');
    std::string out;
    for (;;) {
      char c = get();
      if (c == '\0') fail("unterminated string");
      if (c == '"') return out;
      if (c == '\\') {
        char e = get();
        switch (e) {
          case 'n': out += '\n'; break;
          case 't': out += '\t'; break;
          case '"': case '\\': case '/': out += e; break;
          default: fail("unsupported escape");
        }
      } else {
        out += c;
      }
    }
  }
  std::int64_t number() {
    std::size_t start = pos_;
    if (peek() == '-') pos_++;
    while (peek() >= '0' && peek() <= '9') pos_++;
    if (pos_ == start || (pos_ == start + 1 && s_[start] == '-')) fail("expected a value");
    return std::stoll(std::string(s_.substr(start, pos_ - start)));
  }
  std::string_view s_;
  std::size_t pos_ = 0;
};

std::string quote(const std::string& s) {
  std::string o = "\"";
  for (char c : s) {
    if (c == '"' || c == '\\') o += '\\';
    o += c;
  }
  return o + "\"";
}

std::string rfc3339_nano(std::int64_t ns) {
  std::time_t secs = static_cast<std::time_t>(ns / 1000000000);
  long frac = static_cast<long>(ns % 1000000000);
  std::tm tm{};
  gmtime_r(&secs, &tm);
  char buf[32];
  std::strftime(buf, sizeof buf, "%Y-%m-%dT%H:%M:%S", &tm);
  std::string out = buf;
  if (frac) {
    char f[16];
    std::snprintf(f, sizeof f, ".%09ld", frac);
    std::string fs = f;
    while (fs.back() == '0') fs.pop_back();
    out += fs;
  }
  return out + "Z";
}

std::string hex(const kavach::Bytes& b) {
  static const char d[] = "0123456789abcdef";
  std::string o;
  for (auto c : b) {
    o += d[c >> 4];
    o += d[c & 15];
  }
  return o;
}

}  // namespace

class Ledger : public kavach::Handler {
 public:
  void handle(kavach::Env& env, const kavach::Input& in) override {
    Event ev;
    try {
      ev = Scanner(in.data.str()).event();
    } catch (const std::exception& e) {
      throw kavach::HandlerError("decode event at " + std::string(in.position) + ": " + e.what());
    }
    if (kFixNullAmount && !ev.amount) return reject(env, ev, "missing amount");
    // THE PLANTED BUG: with "amount": null the optional is empty; value() throws.
    std::int64_t amount = ev.amount.value();
    if (amount <= 0) return reject(env, ev, "amount must be positive");
    auto at = env.now_ns();

    if (ev.type == "deposit") {
      net_ += amount;
      post(env, ev, ev.account, amount, at);
    } else if (ev.type == "withdraw") {
      if (balances_[ev.account] < amount) return reject(env, ev, "insufficient funds");
      net_ -= amount;
      post(env, ev, ev.account, -amount, at);
    } else if (ev.type == "transfer") {
      if (balances_[ev.account] < amount) return reject(env, ev, "insufficient funds");
      post(env, ev, ev.account, -amount, at);
      post(env, ev, ev.to, amount, at);
    } else {
      reject(env, ev, "unknown event type " + ev.type);
    }
  }

  // Invariants are checked after every step, live and in replay.
  std::vector<kavach::Invariant> invariants() override {
    return {
        {"balances_non_negative",
         [this]() -> std::optional<std::string> {
           for (auto& [a, b] : balances_)
             if (b < 0) return "account " + a + " has balance " + std::to_string(b);
           return std::nullopt;
         }},
        {"money_conserved",
         [this]() -> std::optional<std::string> {
           std::int64_t sum = 0;
           for (auto& [a, b] : balances_) sum += b;
           if (sum != net_)
             return "balances sum to " + std::to_string(sum) + ", deposits minus withdrawals is " +
                    std::to_string(net_);
           return std::nullopt;
         }},
    };
  }

  // The state's own encoding: the net, then one "account=balance" per line.
  bool can_snapshot() const override { return true; }
  kavach::Bytes snapshot() override {
    std::string s = std::to_string(net_) + "\n";
    for (auto& [a, b] : balances_) s += a + "=" + std::to_string(b) + "\n";
    return kavach::Bytes(s.begin(), s.end());
  }
  void restore(kavach::ByteView b) override {
    std::istringstream in{std::string(b.str())};
    std::string line;
    balances_.clear();
    std::getline(in, line);
    net_ = std::stoll(line);
    while (std::getline(in, line)) {
      auto eq = line.rfind('=');
      if (eq != std::string::npos) balances_[line.substr(0, eq)] = std::stoll(line.substr(eq + 1));
    }
  }

 private:
  void post(kavach::Env& env, const Event& ev, const std::string& account, std::int64_t delta,
            std::int64_t at) {
    balances_[account] += delta;
    std::string txn = hex(env.random(8));
    env.emit("ledger.entries",
             "{\"txn\":" + quote(txn) + ",\"event\":" + quote(ev.id) + ",\"account\":" + quote(account) +
                 ",\"delta\":" + std::to_string(delta) + ",\"balance\":" + std::to_string(balances_[account]) +
                 ",\"at\":" + quote(rfc3339_nano(at)) + "}");
  }
  void reject(kavach::Env& env, const Event& ev, const std::string& reason) {
    env.emit("ledger.rejections", "{\"event\":" + quote(ev.id) + ",\"reason\":" + quote(reason) + "}");
  }

  std::map<std::string, std::int64_t> balances_;
  std::int64_t net_ = 0;
};

int main(int argc, char** argv) {
  // Lets the kavach CLI use this binary as a replay host.
  kavach::maybe_host(argc, argv, [] { return std::make_unique<Ledger>(); });

  std::string in, dir = "fixtures";
  for (int i = 1; i < argc; i++) {
    std::string a = argv[i];
    if (a == "--in" && i + 1 < argc) in = argv[++i];
    else if (a == "--fixtures" && i + 1 < argc) dir = argv[++i];
    else {
      std::cerr << "usage: " << argv[0] << " --in FILE [--fixtures DIR]\n";
      return 2;
    }
  }
  if (in.empty()) {
    std::cerr << "ledger: pass --in FILE\n";
    return 2;
  }
  std::ifstream f(in);
  if (!f) {
    std::cerr << "ledger: cannot open " << in << "\n";
    return 1;
  }

  Ledger ledger;
  kavach::RecorderOptions o;
  o.service = "ledger";
  o.dir = dir;
  o.deliver = [](const std::vector<kavach::Output>& outs) {
    for (auto& out : outs) {
      std::string line(out.data.begin(), out.data.end());
      std::printf("%-18s %s\n", out.sink.c_str(), line.c_str());
    }
  };
  kavach::Recorder rec(ledger, o);

  std::string line;
  for (int n = 0; std::getline(f, line); n++) {
    auto r = rec.step("file:" + in, "0:" + std::to_string(n), line);
    if (!r.ok()) {
      std::cerr << "ledger: step " << n << " failed (" << (r.outcome == kavach::StepResult::Outcome::Panic ? "panic" : "error")
                << "): " << r.message << "\n";
      rec.flush(true);
      return 1;
    }
  }
  return 0;
}
