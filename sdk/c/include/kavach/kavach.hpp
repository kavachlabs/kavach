// Kavach C++17 wrapper: a header-only layer over the C API (kavach.h).
//
// It is not a second implementation. The recorder, the host protocol and the
// failure handling all live in libkavach; this header adds RAII, std::function
// callbacks, std::vector<uint8_t> / std::string_view buffers and exceptions.
//
// Failure model (SPEC 4.5):
//   * kavach::Panic("msg")        -> `panic` marker with exactly "msg"
//   * kavach::HandlerError("msg") -> `error` marker with exactly "msg"
//   * any other std::exception    -> `panic` marker with what()
//   * a non-std exception         -> `panic` marker "unknown exception"
// The wrapper catches at its own boundary and reports the failure through the
// C API (kavach_fail_panic / kavach_error), which return normally: nothing
// here ever longjmps across C++ frames.
//
// When the host driver aborts a step (SPEC 9.4), the next Env call throws
// kavach::detail::Aborted, which does not derive from std::exception. Do not
// swallow it with catch (...); rethrow it.
#ifndef KAVACH_KAVACH_HPP
#define KAVACH_KAVACH_HPP

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <functional>
#include <memory>
#include <optional>
#include <stdexcept>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "kavach/kavach.h"

namespace kavach {

using Bytes = std::vector<std::uint8_t>;

// A non-owning view of bytes. Converts from the usual buffer types.
class ByteView {
 public:
  ByteView() noexcept = default;
  ByteView(const void* p, std::size_t n) noexcept : p_(static_cast<const std::uint8_t*>(p)), n_(n) {}
  ByteView(std::string_view s) noexcept : p_(reinterpret_cast<const std::uint8_t*>(s.data())), n_(s.size()) {}
  ByteView(const std::string& s) noexcept : ByteView(std::string_view(s)) {}
  ByteView(const char* s) noexcept : ByteView(std::string_view(s)) {}
  ByteView(const Bytes& v) noexcept : p_(v.data()), n_(v.size()) {}

  const std::uint8_t* data() const noexcept { return p_; }
  std::size_t size() const noexcept { return n_; }
  bool empty() const noexcept { return n_ == 0; }
  std::string_view str() const noexcept {
    return std::string_view(reinterpret_cast<const char*>(p_), n_);
  }
  Bytes to_bytes() const { return Bytes(p_, p_ + n_); }

 private:
  const std::uint8_t* p_ = nullptr;
  std::size_t n_ = 0;
};

enum class Scope { Remote = KAVACH_REMOTE, Local = KAVACH_LOCAL };

// One event. The views are valid only during the step.
struct Input {
  std::string_view source;
  std::string_view position;
  ByteView data;
};

// An output of a successful step, as handed to a deliver callback.
struct Output {
  std::string sink;
  Bytes data;
  Scope scope = Scope::Remote;
};

// Thrown by the library when a recorder cannot be created (e.g. `required`).
class Error : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};
// Throw from a handler to fail the step as a `panic` with exactly this message.
class Panic : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};
// Throw from a handler to fail the step as an `error` with exactly this message.
class HandlerError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};
// Throw from a gateway callback to fail the query with this error string.
class QueryError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

namespace detail {
// Unwinds a handler when the driver aborted the step. Deliberately not a std::exception.
struct Aborted {};

inline void* dup(const void* p, std::size_t n) {
  if (n == 0) return nullptr;
  void* d = std::malloc(n);
  if (d) std::memcpy(d, p, n);
  return d;
}
inline char* dup_str(const char* s) { return static_cast<char*>(dup(s, std::strlen(s) + 1)); }
}  // namespace detail

// The result of a gateway query.
struct QueryResult {
  bool ok = false;
  Bytes response;     // when ok
  std::string error;  // when !ok
};

// What a handler may do. Valid only during the step it was passed to.
class Env {
 public:
  explicit Env(kavach_env* e) noexcept : e_(e) {}

  // Nanoseconds since the Unix epoch.
  std::int64_t now_ns() {
    std::int64_t v = kavach_now_ns(e_);
    if (kavach_aborted(e_)) throw detail::Aborted{};
    return v;
  }
  using TimePoint = std::chrono::time_point<std::chrono::system_clock, std::chrono::nanoseconds>;
  TimePoint now() { return TimePoint(std::chrono::nanoseconds(now_ns())); }

  void random(void* buf, std::size_t n) { check(kavach_random(e_, buf, n), "random source failed"); }
  Bytes random(std::size_t n) {
    Bytes b(n);
    random(b.data(), n);
    return b;
  }

  // Never throws for a failed query: inspect QueryResult::ok.
  QueryResult query(const std::string& gateway, ByteView request) {
    std::uint8_t* resp = nullptr;
    std::size_t rl = 0;
    char* err = nullptr;
    int rc = kavach_query(e_, gateway.c_str(), request.data(), request.size(), &resp, &rl, &err);
    QueryResult r;
    if (rc == KAVACH_OK) {
      r.ok = true;
      if (resp) r.response.assign(resp, resp + rl);
    } else {
      r.error = err ? err : "";
    }
    kavach_free(resp);
    kavach_free(err);
    if (rc == KAVACH_ABORTED) throw detail::Aborted{};
    return r;
  }

  // nullopt when the value is not set.
  std::optional<Bytes> config(const std::string& key) {
    std::uint8_t* v = nullptr;
    std::size_t l = 0;
    int present = 0;
    int rc = kavach_config(e_, key.c_str(), &v, &l, &present);
    std::optional<Bytes> out;
    if (rc == KAVACH_OK && present) out = Bytes(v, v + l);
    kavach_free(v);
    check(rc, "config read failed");
    return out;
  }

  // Requests an output; delivered only after the step succeeds.
  void emit(const std::string& sink, ByteView data, Scope scope = Scope::Remote) {
    check(kavach_emit(e_, sink.c_str(), data.data(), data.size(), static_cast<kavach_scope>(scope)),
          "emit failed");
  }

 private:
  void check(int rc, const char* what) {
    if (rc == KAVACH_ABORTED) throw detail::Aborted{};
    if (rc != KAVACH_OK) throw std::runtime_error(what);
  }
  kavach_env* e_;
};

// A named property of handler state that must hold after every step.
// check returns nullopt when it holds, otherwise a description of the failure.
struct Invariant {
  std::string name;
  std::function<std::optional<std::string>()> check;
};

// Implement this (or use FunctionHandler). Exceptions: see the top of the file.
class Handler {
 public:
  virtual ~Handler() = default;
  virtual void handle(Env& env, const Input& in) = 0;
  // Snapshots let the recorder segment its journal (SPEC 3.6).
  virtual bool can_snapshot() const { return false; }
  virtual Bytes snapshot() { return {}; }
  virtual void restore(ByteView) {}
  // Called once, when the handler is registered.
  virtual std::vector<Invariant> invariants() { return {}; }
};

// Adapts a callable to Handler.
class FunctionHandler : public Handler {
 public:
  explicit FunctionHandler(std::function<void(Env&, const Input&)> fn) : fn_(std::move(fn)) {}
  void handle(Env& env, const Input& in) override { fn_(env, in); }

 private:
  std::function<void(Env&, const Input&)> fn_;
};

namespace detail {

// Bridges a Handler to the C kavach_handler. Owns the invariants' storage.
class HandlerBox {
 public:
  HandlerBox(Handler* h, std::unique_ptr<Handler> owned) : h_(h), owned_(std::move(owned)) {
    invs_ = h_->invariants();
    cinvs_.reserve(invs_.size());
    for (auto& i : invs_) cinvs_.push_back(kavach_invariant{i.name.c_str(), &check, &i});
    snapshots_ = h_->can_snapshot();
  }

  kavach_handler c_handler() {
    kavach_handler c;
    std::memset(&c, 0, sizeof c);
    c.state = this;
    c.handle = &handle;
    if (snapshots_) {
      c.snapshot = &snapshot;
      c.restore = &restore;
    }
    c.invariants = cinvs_.data();
    c.n_invariants = cinvs_.size();
    c.destroy = &destroy;
    c.flags = KAVACH_HANDLER_NOJUMP;
    return c;
  }

  static void destroy(void* state) { delete static_cast<HandlerBox*>(state); }

 private:
  static int handle(void* state, kavach_env* env, const kavach_input* in) {
    auto* self = static_cast<HandlerBox*>(state);
    try {
      Env e(env);
      Input i{in->source ? in->source : "", in->position ? in->position : "",
              ByteView(in->data, in->len)};
      self->h_->handle(e, i);
      return KAVACH_OK;
    } catch (const Aborted&) {
      return KAVACH_ABORTED;
    } catch (const Panic& p) {
      return kavach_fail_panic(env, p.what());
    } catch (const HandlerError& p) {
      return kavach_error(env, p.what());
    } catch (const std::exception& p) {
      return kavach_fail_panic(env, p.what());
    } catch (...) {
      return kavach_fail_panic(env, "unknown exception");
    }
  }

  static int snapshot(void* state, std::uint8_t** data, std::size_t* len) {
    auto* self = static_cast<HandlerBox*>(state);
    try {
      Bytes b = self->h_->snapshot();
      *data = static_cast<std::uint8_t*>(dup(b.data(), b.size()));
      *len = b.size();
      return (*data || b.empty()) ? KAVACH_OK : KAVACH_ERROR;
    } catch (...) {
      return KAVACH_ERROR;
    }
  }

  static int restore(void* state, const std::uint8_t* data, std::size_t len) {
    auto* self = static_cast<HandlerBox*>(state);
    try {
      self->h_->restore(ByteView(data, len));
      return KAVACH_OK;
    } catch (...) {
      return KAVACH_ERROR;
    }
  }

  static int check(void*, void* arg, char** detail) {
    auto* inv = static_cast<Invariant*>(arg);
    try {
      auto r = inv->check();
      if (!r) return KAVACH_OK;
      *detail = dup_str(r->c_str());
    } catch (const std::exception& e) {
      *detail = dup_str(e.what());
    } catch (...) {
    }
    return KAVACH_ERROR;
  }

  Handler* h_;
  std::unique_ptr<Handler> owned_;
  std::vector<Invariant> invs_;
  std::vector<kavach_invariant> cinvs_;
  bool snapshots_ = false;
};

}  // namespace detail

// A gateway: a named connection to an external system. The callback returns the
// response bytes, or throws QueryError (or any std::exception) to fail the query.
struct Gateway {
  std::string name;  // "*" matches any gateway not registered by name
  std::function<Bytes(std::string_view gateway, ByteView request)> fn;
  Scope scope = Scope::Remote;
};

// A feature-flag fact (SPEC 4.8), e.g. {"flag.beta", "on"}.
struct Flag {
  std::string key;
  std::string value;
};

namespace detail {
struct GatewaySlot {
  Gateway g;
};
inline int gateway_cb(void* user, const char* name, const std::uint8_t* req, std::size_t rl,
                      std::uint8_t** resp, std::size_t* resp_len, char** err) {
  auto* slot = static_cast<GatewaySlot*>(user);
  try {
    Bytes r = slot->g.fn(name, ByteView(req, rl));
    *resp = static_cast<std::uint8_t*>(dup(r.data(), r.size()));
    *resp_len = r.size();
    return KAVACH_OK;
  } catch (const std::exception& e) {
    *err = dup_str(e.what());
  } catch (...) {
    *err = dup_str("unknown exception");
  }
  return KAVACH_ERROR;
}
}  // namespace detail

struct RecorderOptions {
  std::string service;  // required
  std::vector<std::string> recorder_argv;  // else $KAVACH_RECORDER, else "kavach-recorder" on PATH
  std::string dir, compression;
  int level = 0;
  std::uint64_t block_bytes = 0, flush_ms = 0, segment_bytes = 0, segment_seconds = 0, retain_segments = 0;
  std::vector<std::string> secret_keys;
  std::string handler_id;
  std::string producer;  // default "kavach-cpp/<version>"
  bool start_from_snapshot = false;
  bool no_segments = false;
  bool required = false;  // throw Error from the constructor if recording cannot start
  int startup_timeout_ms = 0, flush_timeout_ms = 0, close_timeout_ms = 0;

  std::vector<Gateway> gateways;
  std::function<std::optional<Bytes>(std::string_view key)> config;
  std::string config_source;
  std::function<std::vector<Flag>()> flags;
  // Delivers a successful step's outputs. Throw to report a delivery failure.
  std::function<void(const std::vector<Output>&)> deliver;
  std::function<void(const std::string&)> log;  // may be called from another thread
  std::function<std::int64_t()> clock_ns;       // test hook
  std::function<void(void*, std::size_t)> random;  // test hook; throw on failure
};

struct StepResult {
  enum class Outcome { Ok, Error, Panic, Invariant, DeliverFailed };
  Outcome outcome = Outcome::Ok;
  std::string message;  // the marker message of a failure
  std::string detail;
  bool ok() const noexcept { return outcome == Outcome::Ok; }
};

// The flight recorder. Destruction closes it (waiting for the recorder).
class Recorder {
 public:
  // Does not take ownership of handler, which must outlive the Recorder.
  Recorder(Handler& handler, RecorderOptions opts) : impl_(new Impl(&handler, nullptr, std::move(opts))) {}
  Recorder(std::unique_ptr<Handler> handler, RecorderOptions opts) {
    Handler* h = handler.get();
    impl_.reset(new Impl(h, std::move(handler), std::move(opts)));
  }
  Recorder(Recorder&&) noexcept = default;
  Recorder& operator=(Recorder&&) noexcept = default;
  Recorder(const Recorder&) = delete;
  Recorder& operator=(const Recorder&) = delete;
  ~Recorder() = default;

  // Records and runs one step. Handler failures are reported in the result,
  // not thrown. Not reentrant.
  StepResult step(const Input& in) {
    std::string source(in.source), position(in.position);
    kavach_input ci{source.c_str(), position.c_str(), in.data.data(), in.data.size()};
    kavach_result r;
    std::memset(&r, 0, sizeof r);
    kavach_recorder_step(impl_->rec, &ci, &r);
    StepResult out;
    switch (r.outcome) {
      case KAVACH_OK: out.outcome = StepResult::Outcome::Ok; break;
      case KAVACH_PANIC: out.outcome = StepResult::Outcome::Panic; break;
      case KAVACH_INVARIANT: out.outcome = StepResult::Outcome::Invariant; break;
      case KAVACH_DELIVER_FAILED: out.outcome = StepResult::Outcome::DeliverFailed; break;
      default: out.outcome = StepResult::Outcome::Error; break;
    }
    if (r.message) out.message = r.message;
    if (r.detail) out.detail = r.detail;
    kavach_result_clear(&r);
    return out;
  }
  StepResult step(std::string_view source, std::string_view position, ByteView data) {
    return step(Input{source, position, data});
  }

  // Closes the open block; when durable, waits until the recorder confirms.
  // False if recording is not running or the recorder did not confirm.
  bool flush(bool durable = true) { return kavach_recorder_flush(impl_->rec, durable ? 1 : 0) == KAVACH_OK; }
  // Orderly shutdown; idempotent. False if the recorder did not confirm.
  bool close() { return kavach_recorder_close(impl_->rec) == KAVACH_OK; }
  bool active() const { return kavach_recorder_active(impl_->rec) != 0; }

 private:
  using GatewaySlot = detail::GatewaySlot;
  struct Impl {
    Impl(Handler* h, std::unique_ptr<Handler> owned, RecorderOptions o)
        : opts(std::move(o)), box(new detail::HandlerBox(h, std::move(owned))) {
      kavach_handler ch = box->c_handler();
      std::memset(&c, 0, sizeof c);
      c.service = opts.service.c_str();
      for (auto& a : opts.recorder_argv) argv.push_back(a.c_str());
      if (!argv.empty()) {
        argv.push_back(nullptr);
        c.recorder_argv = argv.data();
      }
      if (!opts.dir.empty()) c.dir = opts.dir.c_str();
      if (!opts.compression.empty()) c.compression = opts.compression.c_str();
      c.level = opts.level;
      c.block_bytes = opts.block_bytes;
      c.flush_ms = opts.flush_ms;
      c.segment_bytes = opts.segment_bytes;
      c.segment_seconds = opts.segment_seconds;
      c.retain_segments = opts.retain_segments;
      for (auto& s : opts.secret_keys) secrets.push_back(s.c_str());
      if (!secrets.empty()) {
        secrets.push_back(nullptr);
        c.secret_keys = secrets.data();
      }
      if (!opts.handler_id.empty()) c.handler_id = opts.handler_id.c_str();
      if (opts.producer.empty()) opts.producer = "kavach-cpp/" KAVACH_VERSION;
      c.producer = opts.producer.c_str();
      c.runtime = KAVACH_RUNTIME;  // evaluated in the user's translation unit: "cxx17-..."
      c.start_from_snapshot = opts.start_from_snapshot;
      c.no_segments = opts.no_segments;
      c.required = opts.required;
      c.startup_timeout_ms = opts.startup_timeout_ms;
      c.flush_timeout_ms = opts.flush_timeout_ms;
      c.close_timeout_ms = opts.close_timeout_ms;

      gslots.reserve(opts.gateways.size());
      for (auto& g : opts.gateways) {
        gslots.push_back(GatewaySlot{g});
        cgws.push_back(kavach_gateway{nullptr, &detail::gateway_cb, nullptr, static_cast<kavach_scope>(g.scope)});
      }
      for (std::size_t i = 0; i < gslots.size(); i++) {
        cgws[i].name = gslots[i].g.name.c_str();
        cgws[i].user = &gslots[i];
      }
      c.gateways = cgws.data();
      c.n_gateways = cgws.size();
      if (opts.config) {
        c.config = &config_cb;
        c.config_user = this;
      }
      if (!opts.config_source.empty()) c.config_source = opts.config_source.c_str();
      if (opts.flags) {
        c.flags = &flags_cb;
        c.flags_user = this;
      }
      if (opts.deliver) {
        c.deliver = &deliver_cb;
        c.deliver_user = this;
      }
      if (opts.log) {
        c.log = &log_cb;
        c.log_user = this;
      }
      if (opts.clock_ns) {
        c.clock_ns = &clock_cb;
        c.clock_user = this;
      }
      if (opts.random) {
        c.random_bytes = &random_cb;
        c.random_user = this;
      }

      char* err = nullptr;
      // The C recorder copies the handler struct; the box stays alive in this Impl.
      int rc = kavach_recorder_new(&ch, &c, &rec, &err);
      if (rc != KAVACH_OK) {
        std::string msg = err ? err : "kavach_recorder_new failed";
        kavach_free(err);
        throw Error(msg);
      }
    }
    ~Impl() {
      if (rec) kavach_recorder_free(rec);
    }
    Impl(const Impl&) = delete;
    Impl& operator=(const Impl&) = delete;

    static int config_cb(void* user, const char* key, std::uint8_t** val, std::size_t* len, int* present) {
      auto* self = static_cast<Impl*>(user);
      try {
        auto v = self->opts.config(key);
        if (v) {
          *val = static_cast<std::uint8_t*>(detail::dup(v->data(), v->size()));
          *len = v->size();
          *present = 1;
        } else {
          *present = 0;
        }
        return KAVACH_OK;
      } catch (...) {
        return KAVACH_ERROR;
      }
    }
    static std::size_t flags_cb(void* user, const kavach_flag** out) {
      auto* self = static_cast<Impl*>(user);
      try {
        self->flag_store = self->opts.flags();
      } catch (...) {
        self->flag_store.clear();
      }
      self->cflags.clear();
      for (auto& f : self->flag_store)
        self->cflags.push_back(kavach_flag{f.key.c_str(), reinterpret_cast<const std::uint8_t*>(f.value.data()),
                                           f.value.size()});
      *out = self->cflags.data();
      return self->cflags.size();
    }
    static int deliver_cb(void* user, const kavach_output* outs, std::size_t n) {
      auto* self = static_cast<Impl*>(user);
      try {
        std::vector<Output> v;
        v.reserve(n);
        for (std::size_t i = 0; i < n; i++)
          v.push_back(Output{outs[i].sink, Bytes(outs[i].data, outs[i].data + outs[i].len),
                             static_cast<Scope>(outs[i].scope)});
        self->opts.deliver(v);
        return KAVACH_OK;
      } catch (...) {
        return KAVACH_ERROR;
      }
    }
    static void log_cb(void* user, const char* msg) {
      auto* self = static_cast<Impl*>(user);
      try {
        self->opts.log(msg);
      } catch (...) {
      }
    }
    static std::int64_t clock_cb(void* user) {
      try {
        return static_cast<Impl*>(user)->opts.clock_ns();
      } catch (...) {
        return 0;
      }
    }
    static int random_cb(void* user, void* buf, std::size_t n) {
      try {
        static_cast<Impl*>(user)->opts.random(buf, n);
        return 0;
      } catch (...) {
        return -1;
      }
    }

    RecorderOptions opts;
    std::unique_ptr<detail::HandlerBox> box;  // must outlive rec
    kavach_recorder_options c;
    std::vector<const char*> argv, secrets;
    std::vector<GatewaySlot> gslots;
    std::vector<kavach_gateway> cgws;
    std::vector<Flag> flag_store;
    std::vector<kavach_flag> cflags;
    kavach_recorder* rec = nullptr;
  };

  std::unique_ptr<Impl> impl_;
};

// Host options (SPEC 9, 6.3).
struct HostOptions {
  std::string sdk;  // default "kavach-cpp/<version>"
  std::vector<Gateway> gateways;  // answers to `live` queries in sandbox mode
  std::function<void()> setup;    // local setup in sandbox mode; throw to fail
  std::function<void(const std::vector<Output>&)> deliver;  // local outputs after a successful step
};

// If the last argument is "kavach-host", runs the host protocol and exits the
// process. Otherwise returns at once. Call it first in main().
inline void maybe_host(int argc, char** argv, std::function<std::unique_ptr<Handler>()> factory,
                       HostOptions opts = {}) {
  if (argc < 2 || !argv[argc - 1] || std::strcmp(argv[argc - 1], "kavach-host") != 0) return;

  // kavach_maybe_host never returns, so these stay alive for the host's lifetime.
  struct Ctx {
    std::function<std::unique_ptr<Handler>()> factory;
    HostOptions opts;
    std::vector<detail::GatewaySlot> slots;
    std::vector<kavach_gateway> cgws;

    static int make(void* user, kavach_handler* out) {
      auto* self = static_cast<Ctx*>(user);
      try {
        std::unique_ptr<Handler> h = self->factory();
        if (!h) return KAVACH_ERROR;
        Handler* raw = h.get();
        auto* box = new detail::HandlerBox(raw, std::move(h));
        *out = box->c_handler();
        return KAVACH_OK;
      } catch (...) {
        return KAVACH_ERROR;
      }
    }
    static int setup(void* user, char** err) {
      auto* self = static_cast<Ctx*>(user);
      try {
        self->opts.setup();
        return KAVACH_OK;
      } catch (const std::exception& e) {
        *err = detail::dup_str(e.what());
      } catch (...) {
        *err = detail::dup_str("unknown exception");
      }
      return KAVACH_ERROR;
    }
    static int deliver(void* user, const kavach_output* outs, std::size_t n) {
      auto* self = static_cast<Ctx*>(user);
      try {
        std::vector<Output> v;
        for (std::size_t i = 0; i < n; i++)
          v.push_back(Output{outs[i].sink, Bytes(outs[i].data, outs[i].data + outs[i].len),
                             static_cast<Scope>(outs[i].scope)});
        self->opts.deliver(v);
        return KAVACH_OK;
      } catch (...) {
        return KAVACH_ERROR;
      }
    }
  } ctx{std::move(factory), std::move(opts), {}, {}};

  if (ctx.opts.sdk.empty()) ctx.opts.sdk = "kavach-cpp/" KAVACH_VERSION;
  ctx.slots.reserve(ctx.opts.gateways.size());
  for (auto& g : ctx.opts.gateways) ctx.slots.push_back(detail::GatewaySlot{g});
  for (auto& s : ctx.slots)
    ctx.cgws.push_back(kavach_gateway{s.g.name.c_str(), &detail::gateway_cb, &s, static_cast<kavach_scope>(s.g.scope)});

  kavach_host_options o;
  std::memset(&o, 0, sizeof o);
  o.user = &ctx;
  o.sdk = ctx.opts.sdk.c_str();
  o.runtime = KAVACH_RUNTIME;
  o.gateways = ctx.cgws.data();
  o.n_gateways = ctx.cgws.size();
  if (ctx.opts.setup) {
    o.setup = &Ctx::setup;
    o.setup_user = &ctx;
  }
  if (ctx.opts.deliver) {
    o.deliver = &Ctx::deliver;
    o.deliver_user = &ctx;
  }
  kavach_maybe_host(argc, argv, &Ctx::make, &o);
}

}  // namespace kavach

#endif  // KAVACH_KAVACH_HPP
