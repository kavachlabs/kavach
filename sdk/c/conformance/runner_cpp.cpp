// Recorder-case runner (spec/recorder/sdk/README.md) for the C++ wrapper.
//   runner-cpp <case.json> <fake_recorder.py> <result.json>
#include <cstdio>
#include <cstring>
#include <iostream>

#include "conformance.hpp"

extern "C" {
#include "caseio.h"
}

namespace {
ans* pop(ans_list& l) { return l.next < l.n ? &l.v[l.next++] : nullptr; }
}  // namespace

int main(int argc, char** argv) {
  if (argc != 4) {
    std::fprintf(stderr, "usage: %s case.json fake_recorder.py result.json\n", argv[0]);
    return 2;
  }
  char err[256];
  case_t* c = case_load(argv[1], err, sizeof err);
  if (!c) {
    std::fprintf(stderr, "%s\n", err);
    return 2;
  }
  ConfHandler handler;
  if (c->snapshot) handler.restore(c->snapshot);

  case_action* cur = nullptr;
  kavach::RecorderOptions o;
  o.service = c->service;
  o.recorder_argv = {"python3", argv[2], argv[1], argv[3]};
  o.start_from_snapshot = c->start_snapshot != 0;
  o.no_segments = !c->snapshots;
  o.required = true;
  o.config_source = "test";
  o.clock_ns = [&]() -> std::int64_t {
    ans* a = pop(cur->clock);
    return a && a->text ? std::strtoll(a->text, nullptr, 10) : 0;
  };
  o.random = [&](void* buf, std::size_t n) {
    ans* a = pop(cur->rand);
    if (!a || a->len != n) throw std::runtime_error("no scripted random bytes of that length");
    std::memcpy(buf, a->data, n);
  };
  o.gateways.push_back(kavach::Gateway{"*",
                                       [&](std::string_view, kavach::ByteView) -> kavach::Bytes {
                                         ans* a = pop(cur->gateway);
                                         if (!a) throw kavach::QueryError("no answer scripted");
                                         if (a->kind == 1) throw kavach::QueryError(a->text);
                                         return kavach::Bytes(a->data, a->data + a->len);
                                       },
                                       kavach::Scope::Remote});
  o.config = [&](std::string_view) -> std::optional<kavach::Bytes> {
    ans* a = pop(cur->config);
    if (!a || a->kind == 2) return std::nullopt;
    return kavach::Bytes(a->data, a->data + a->len);
  };
  if (c->n_flags) {
    o.flags = [&] {
      std::vector<kavach::Flag> v;
      for (std::size_t i = 0; i < c->n_flags; i++) v.push_back({c->flags[i].key, c->flags[i].value});
      return v;
    };
  }

  int bad = 0;
  try {
    kavach::Recorder rec(handler, std::move(o));
    for (std::size_t i = 0; i < c->n_actions; i++) {
      case_action* a = &c->actions[i];
      if (a->is_flush) {
        if (!rec.flush(a->durable != 0)) {
          std::fprintf(stderr, "flush failed\n");
          bad = 1;
        }
      } else {
        cur = a;
        rec.step(a->source, a->position, kavach::ByteView(a->data, a->len));
      }
    }
    if (!rec.close()) {
      std::fprintf(stderr, "close failed\n");
      bad = 1;
    }
  } catch (const std::exception& e) {
    std::fprintf(stderr, "recorder: %s\n", e.what());
    return 1;
  }
  int rc = case_check_result(argv[3]);
  case_free(c);
  return bad || rc;
}
