// The conformance handler of SPEC 9.6, written against the C++ wrapper.
#ifndef KAVACH_CONFORMANCE_HPP
#define KAVACH_CONFORMANCE_HPP

#include <cstdio>
#include <cstdlib>
#include <iostream>
#include <string>

#include "kavach/kavach.hpp"

extern "C" {
#include "json.h"
}

class ConfHandler : public kavach::Handler {
 public:
  long count = 0;

  void handle(kavach::Env& env, const kavach::Input& in) override {
    char err[96];
    kj* ops = kj_parse(reinterpret_cast<const char*>(in.data.data()), in.data.size(), err, sizeof err);
    if (!ops || ops->type != KJ_ARR) {
      kj_free(ops);
      throw kavach::HandlerError(std::string("invalid operations: ") + err);
    }
    struct Guard {
      kj* p;
      ~Guard() { kj_free(p); }
    } guard{ops};

    for (std::size_t i = 0; i < ops->n; i++) {
      const kj* op = &ops->v[i];
      std::string name = str(op, "op");
      if (name == "clock") {
        trace(env, "{\"clock\":\"" + std::to_string(env.now_ns()) + "\"}");
      } else if (name == "rand") {
        const kj* n = kj_get(op, "n");
        kavach::Bytes b = env.random(kj_is_num(n) ? static_cast<std::size_t>(n->num) : 0);
        env.emit("trace", b);
      } else if (name == "gateway") {
        kavach::QueryResult r = env.query(str(op, "gateway"), str(op, "request"));
        if (r.ok)
          env.emit("trace", r.response);
        else
          trace(env, "{\"error\":" + quote(r.error) + "}");
      } else if (name == "config") {
        auto v = env.config(str(op, "key"));
        if (v)
          env.emit("trace", *v);
        else
          trace(env, "{\"unset\":true}");
      } else if (name == "getenv") {
        const char* v = std::getenv(str(op, "name").c_str());
        trace(env, v ? std::string(v) : std::string("{\"unset\":true}"));
      } else if (name == "emit") {
        env.emit(str(op, "sink"), str(op, "data"));
      } else if (name == "panic") {
        throw kavach::Panic(str(op, "message"));
      } else if (name == "error") {
        throw kavach::HandlerError(str(op, "message"));
      } else if (name == "print") {
        std::cout << str(op, "text") << std::endl;
      } else if (name == "count") {
        const kj* n = kj_get(op, "n");
        count += kj_is_num(n) ? static_cast<long>(n->num) : 0;  // no rollback (SPEC 9.6)
      } else {
        throw kavach::HandlerError("unknown operation " + name);
      }
    }
    count += 1;
  }

  bool can_snapshot() const override { return true; }
  kavach::Bytes snapshot() override {
    std::string s = std::to_string(count);
    return kavach::Bytes(s.begin(), s.end());
  }
  void restore(kavach::ByteView b) override { count = std::stol(std::string(b.str())); }

  std::vector<kavach::Invariant> invariants() override {
    return {{"below_limit", [this]() -> std::optional<std::string> {
               if (count < 1000) return std::nullopt;
               return "count is " + std::to_string(count);
             }}};
  }

 private:
  static std::string str(const kj* op, const char* key) {
    const kj* v = kj_get(op, key);
    return kj_is_str(v) ? std::string(v->s, v->slen) : std::string();
  }
  static void trace(kavach::Env& env, const std::string& s) { env.emit("trace", s); }
  static std::string quote(const std::string& s) {
    static const char hex[] = "0123456789abcdef";
    std::string o = "\"";
    for (unsigned char c : s) {
      switch (c) {
        case '"': o += "\\\""; break;
        case '\\': o += "\\\\"; break;
        case '\n': o += "\\n"; break;
        case '\r': o += "\\r"; break;
        case '\t': o += "\\t"; break;
        default:
          if (c < 0x20) {
            o += "\\u00";
            o += hex[c >> 4];
            o += hex[c & 15];
          } else {
            o += static_cast<char>(c);
          }
      }
    }
    return o + "\"";
  }
};

#endif
