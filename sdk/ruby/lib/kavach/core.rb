# frozen_string_literal: true

# Kavach records what a handler did before it failed, and replays it.
# This file holds the handler-facing API shared by the recorder and the host.
module Kavach
  REMOTE = :remote
  LOCAL = :local

  # One event consumed by a handler. +data+ is a binary String.
  Input = Struct.new(:source, :position, :data, keyword_init: true)

  # One effect a handler requested with +env.emit+.
  Output = Struct.new(:sink, :data, :local, keyword_init: true)

  # A named property of handler state that must hold after every step. The
  # check holds when it returns normally (or returns anything but +false+); it
  # is violated when it raises or returns +false+.
  class Invariant
    attr_reader :name, :check

    def initialize(name, check = nil, &block)
      @name = name.to_s
      @check = check || block or raise ArgumentError, "an invariant needs a check"
    end
  end

  # A registered gateway: the connection that makes the query, and its scope.
  # The connection is called with the request bytes when recording and returns
  # the response. Raise any
  # exception, or Kavach::GatewayError, to report a failure.
  class Gateway
    attr_reader :scope, :connection

    def initialize(scope = REMOTE, connection = nil, &block)
      @scope = scope.to_sym
      raise ArgumentError, "gateway scope must be :remote or :local" unless [REMOTE, LOCAL].include?(@scope)

      @connection = connection || block or raise ArgumentError, "a gateway needs a connection"
    end
  end

  # Base class of errors raised by the SDK itself.
  class Error < StandardError; end

  # The recorder could not be started and recording was required.
  class RecorderError < Error; end

  # The handler queried a gateway that was not registered.
  class UnknownGatewayError < Error; end

  # A gateway query failed. +error+ is the text the connection reported.
  class GatewayError < Error
    attr_reader :error

    def initialize(error)
      @error = error.to_s
      super(@error)
    end
  end

  # Fails the step as a +panic+ whose marker message is exactly the message.
  class Panic < StandardError; end

  # Fails the step as an +error+ whose marker message is exactly the message.
  class HandlerError < StandardError; end

  # A step failure as it is recorded (kind is "panic", "error" or "invariant").
  Failure = Struct.new(:kind, :message, :detail)

  PATH_RE = %r{(?<![\w.])(?:[A-Za-z]:)?(?:/[\w.\-+@~]+)+}
  ADDR_OBJ_RE = /#<([A-Za-z_][\w:]*):0x\h+[^>]*>/
  ADDR_RE = /\b0x\h{6,}\b/
  LINE_RE = /(?<=\.rb):\d+(?::\d+)?/

  # Removes what a Ruby runtime puts into exception messages that depends on
  # where or how the build runs (SPEC section 4.5): object identities and
  # addresses, file paths (kept as base names) and line numbers.
  def self.scrub(text)
    text.to_s
        .gsub(ADDR_OBJ_RE) { "#<#{Regexp.last_match(1)}>" }
        .gsub(ADDR_RE, "0x")
        .gsub(PATH_RE) { |p| File.basename(p) }
        .gsub(LINE_RE, "")
  end

  # The failure mapping of SPEC section 4.5, used both live and in the host.
  #
  # * Kavach::Panic.new(msg)        -> panic, message exactly msg
  # * Kavach::HandlerError.new(msg) -> error, message exactly msg
  # * anything else                 -> panic, message "ClassName: message"
  #   with paths, line numbers and addresses removed (Kavach.scrub); the
  #   unscrubbed message and the backtrace are the marker data.
  def self.classify(exc)
    detail = "#{exc.class}: #{exc.message}\n#{(exc.backtrace || []).join("\n")}"
    case exc
    when Panic then Failure.new("panic", exc.message.to_s, detail)
    when HandlerError then Failure.new("error", exc.message.to_s, detail)
    else Failure.new("panic", scrub("#{exc.class}: #{exc.message}"), detail)
    end
  end

  # Passed to a handler for each input. Everything nondeterministic a handler
  # does must go through it.
  class Env
    # Nanoseconds since the Unix epoch, UTC.
    def now_ns = raise(NotImplementedError)

    # The clock as a UTC Time (nanosecond precision).
    def now
      ns = now_ns
      Time.at(ns / 1_000_000_000, ns % 1_000_000_000, :nsec).utc
    end

    # +n+ random bytes (a binary String).
    def random(_n) = raise(NotImplementedError)

    # Query a registered gateway; raises Kavach::GatewayError if it failed.
    def query(_gateway, _request) = raise(NotImplementedError)

    # Read a config value that can change what the handler does; nil if unset.
    def config(_key) = raise(NotImplementedError)

    # Request an effect. Delivered only after the step succeeds.
    def emit(_sink, _data, local: false) = raise(NotImplementedError)
  end

  @logger = ->(level, msg) { $stderr.puts("kavach: #{level}: #{msg}") }

  class << self
    # A callable +(level, message)+ that receives the SDK's log lines
    # (default: standard error). Set it to redirect them.
    attr_accessor :logger

    def log(level, msg)
      @logger&.call(level, msg)
    rescue StandardError
      nil
    end

    def runtime = "#{RUBY_ENGINE}-#{RUBY_VERSION}"

    def binary(value) = value.to_s.b

    def invariant_list(handler)
      handler.respond_to?(:invariants) ? Array(handler.invariants) : []
    end

    # The first violated invariant, in declaration order, or nil.
    def check_invariants(handler)
      invariant_list(handler).each do |inv|
        begin
          return Failure.new("invariant", inv.name, "check returned false") if inv.check.call == false
        rescue StandardError => e
          return Failure.new("invariant", inv.name, e.message.to_s.empty? ? e.class.to_s : e.message)
        end
      end
      nil
    end

    # Looks +name+ up in a registry (anything with +[]+) of Gateway objects,
    # bare connection callables (remote) or [scope, callable] pairs.
    def resolve_gateway(gateways, name)
      g = gateways && gateways[name]
      raise UnknownGatewayError, "gateway #{name.inspect} is not registered" if g.nil?

      case g
      when Gateway then g
      when Array then Gateway.new(*g)
      else Gateway.new(REMOTE, g)
      end
    end

    # Runs a connection; returns [response, error]; exactly one is meaningful.
    def call_gateway(gw, request)
      [binary(gw.connection.call(request)), ""]
    rescue GatewayError => e
      ["".b, e.error]
    rescue StandardError => e
      ["".b, e.message.to_s.empty? ? e.class.to_s : e.message]
    end
  end
end
