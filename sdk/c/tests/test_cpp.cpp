// Failure mapping and RAII of the C++ wrapper, with no recorder process.
#include <cstdio>
#include <stdexcept>

#include "kavach/kavach.hpp"

static int failures;
#define CHECK(c)                                                              \
  do {                                                                        \
    if (!(c)) {                                                               \
      std::fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, #c); \
      failures++;                                                             \
    }                                                                         \
  } while (0)

int main() {
  using Outcome = kavach::StepResult::Outcome;
  int delivered = 0;
  kavach::FunctionHandler h([](kavach::Env& env, const kavach::Input& in) {
    std::string mode(in.data.str());
    env.emit("out", "x");
    if (mode == "panic") throw kavach::Panic("boom");
    if (mode == "error") throw kavach::HandlerError("amount is null");
    if (mode == "std") throw std::out_of_range("vector::at");
    if (mode == "int") throw 42;
    if (mode == "reads") {
      CHECK(env.now_ns() > 0);
      CHECK(env.random(4).size() == 4);
      auto q = env.query("nowhere", "q");
      CHECK(!q.ok && !q.error.empty());
      CHECK(!env.config("k").has_value());
    }
  });

  kavach::RecorderOptions o;
  o.service = "t";
  o.recorder_argv = {"/nonexistent/kavach-recorder"};
  o.deliver = [&](const std::vector<kavach::Output>& outs) {
    CHECK(outs.size() == 1 && outs[0].sink == "out");
    delivered++;
  };
  std::string last_log;
  o.log = [&](const std::string& m) { last_log = m; };
  kavach::Recorder rec(h, o);
  CHECK(!rec.active());
  CHECK(last_log.find("unavailable") != std::string::npos);

  auto run = [&](const char* mode) { return rec.step("src", "0", mode); };
  auto r = run("ok");
  CHECK(r.ok() && delivered == 1);
  r = run("reads");
  CHECK(r.ok() && delivered == 2);
  r = run("panic");
  CHECK(r.outcome == Outcome::Panic && r.message == "boom");
  r = run("error");
  CHECK(r.outcome == Outcome::Error && r.message == "amount is null");
  r = run("std");
  CHECK(r.outcome == Outcome::Panic && r.message == "vector::at");
  r = run("int");
  CHECK(r.outcome == Outcome::Panic && r.message == "unknown exception");
  CHECK(delivered == 2);  // failed steps deliver nothing

  o.required = true;
  bool threw = false;
  try {
    kavach::Recorder bad(h, o);
  } catch (const kavach::Error& e) {
    threw = std::string(e.what()).find("required") != std::string::npos;
  }
  CHECK(threw);

  if (failures) return 1;
  std::puts("ok");
  return 0;
}
