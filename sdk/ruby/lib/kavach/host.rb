# frozen_string_literal: true

require "json"
require "timeout"
require_relative "version"
require_relative "core"
require_relative "recorder"

module Kavach
  HOST_ARG = "kavach-host"
  PROTOCOL = 1

  # Unwinds a step after the driver answered a request with +abort+ (section
  # 9.4). Deliberately not a StandardError, so a bare +rescue+ in handler code
  # does not swallow it.
  class AbortStep < Exception; end # rubocop:disable Lint/InheritException

  # The protocol was violated; the host sends +fatal+ and stops.
  class HostFatal < Exception; end # rubocop:disable Lint/InheritException

  # The driver closed the pipe.
  class DriverGone < Exception; end # rubocop:disable Lint/InheritException

  def self.b64(bytes) = [bytes.to_s.b].pack("m0")

  def self.unb64(str)
    raise HostFatal, "expected a base64 string" unless str.is_a?(String)

    str.unpack1("m0")
  rescue ArgumentError
    raise HostFatal, "invalid base64"
  end

  # +ready.environment+: the output of <tt>kavach-recorder facts</tt> if it can
  # be run, plus +host.runtime+ (section 9.2).
  def self.collect_environment(recorder_command = nil)
    env = {}
    cmd = find_recorder(recorder_command)
    if cmd
      begin
        out = Timeout.timeout(10) { IO.popen(cmd + ["facts"], in: File::NULL, err: File::NULL, &:read) }
        facts = JSON.parse(out) if $?.success?
        facts.each { |k, v| env[k] = v if v.is_a?(Hash) } if facts.is_a?(Hash)
      rescue StandardError
        nil
      end
    end
    env["host.runtime"] = { "value" => b64(runtime) }
    env
  end

  # One host session over a pair of binary streams (section 9).
  class Host
    attr_reader :aborted, :mode, :gateways, :local_outputs

    # handler_factory:: callable returning a fresh handler.
    # gateways:: registry of gateways (see Kavach.resolve_gateway).
    def initialize(handler_factory, reader, writer, gateways: nil, environment: nil)
      @factory = handler_factory
      @rd = reader
      @wr = writer
      @gateways = gateways
      @environment = environment || -> { Kavach.collect_environment }
      @mode = "process"
      @aborted = false
      @local_outputs = []
    end

    def send_msg(msg)
      @wr.write("#{JSON.generate(msg)}\n")
      @wr.flush
    rescue IOError, SystemCallError
      raise DriverGone
    end

    def recv
      line = begin
        @rd.gets
      rescue IOError
        nil
      end
      raise DriverGone if line.nil?

      begin
        msg = JSON.parse(line)
      rescue JSON::ParserError
        raise HostFatal, "malformed message: not JSON"
      end
      raise HostFatal, "malformed message: no type" unless msg.is_a?(Hash) && msg["t"].is_a?(String)

      msg
    end

    # Sends one request and waits for its answer; unwinds on +abort+.
    def request(msg, want)
      raise AbortStep if @aborted

      send_msg(msg)
      ans = recv
      if ans["t"] == "abort"
        @aborted = true
        raise AbortStep
      end
      raise HostFatal, "expected a #{want.inspect} answer, got #{ans["t"].inspect}" unless ans["t"] == want

      ans
    end

    # Serves the driver until +end+ or EOF. Returns the exit status.
    def run
      handler = nil
      loop do
        msg = begin
          recv
        rescue DriverGone
          return 0 # between steps: the driver left
        end
        case msg["t"]
        when "hello" then handler = hello(msg)
        when "step"
          raise HostFatal, "step before hello" if handler.nil?

          step(handler, msg)
        when "end" then return 0
        when "abort" then next # a stray abort between steps needs no answer
        else raise HostFatal, "unexpected message #{msg["t"].inspect}"
        end
      end
    rescue HostFatal => e
      begin
        send_msg("t" => "fatal", "message" => e.message)
      rescue DriverGone
        nil
      end
      1
    rescue DriverGone
      1
    end

    private

    def hello(msg)
      raise HostFatal, "unsupported protocol #{msg["protocol"].inspect}" unless msg["protocol"] == PROTOCOL

      @mode = msg["mode"] || "process"
      # SPEC: sandbox mode (section 6.3) is not implemented by this SDK.
      raise HostFatal, "sandbox mode not supported" if @mode == "sandbox"

      handler = begin
        @factory.call
      rescue StandardError => e
        raise HostFatal, "could not create the handler: #{e.class}: #{e.message}"
      end
      if msg["start"] == "snapshot"
        raise HostFatal, "the journal starts from a snapshot but the handler has no restore" unless handler.respond_to?(:restore)

        begin
          handler.restore(Kavach.unb64(msg["snapshot"] || ""))
        rescue StandardError => e
          raise HostFatal, "could not restore the snapshot: #{e.class}: #{e.message}"
        end
      end
      send_msg("t" => "ready", "protocol" => PROTOCOL, "sdk" => PRODUCER,
               "invariants" => Kavach.invariant_list(handler).map(&:name), "environment" => @environment.call)
      handler
    end

    def step(handler, msg)
      input = Input.new(source: msg["source"].to_s, position: msg["position"].to_s,
                        data: Kavach.unb64(msg["data"] || ""))
      @aborted = false
      @local_outputs = []
      failure = nil
      begin
        handler.handle(HostEnv.new(self), input)
      rescue AbortStep
        nil
      rescue HostFatal, DriverGone, SystemExit, SignalException
        raise
      rescue Exception => e # rubocop:disable Lint/RescueException
        failure = Kavach.classify(e)
      end
      return send_msg("t" => "done", "outcome" => "aborted") if @aborted

      failure ||= Kavach.check_invariants(handler)
      done = { "t" => "done", "outcome" => failure ? failure.kind : "ok" }
      if failure
        done["message"] = failure.message
        done["detail"] = failure.detail unless failure.detail.empty?
      end
      send_msg(done)
    end

    # The Env handed to the handler while hosting.
    class HostEnv < Env
      def initialize(host)
        super()
        @h = host
      end

      def now_ns = Integer(@h.request({ "t" => "clock" }, "clock")["unix_nanos"])

      def random(n)
        return "".b if n <= 0

        Kavach.unb64(@h.request({ "t" => "rand", "n" => n }, "rand")["data"] || "")
      end

      def query(gateway, request)
        request = Kavach.binary(request)
        gw = Kavach.resolve_gateway(@h.gateways, gateway)
        ans = @h.request({ "t" => "gateway", "gateway" => gateway, "request" => Kavach.b64(request),
                           "scope" => gw.scope.to_s }, "gateway")
        if ans["live"]
          resp, err = Kavach.call_gateway(gw, request)
          @h.send_msg(err.empty? ? { "t" => "observed", "response" => Kavach.b64(resp) } : { "t" => "observed", "error" => err })
        elsif ans["error"] && !ans["error"].to_s.empty?
          resp, err = "".b, ans["error"].to_s
        else
          resp, err = Kavach.unb64(ans["response"] || ""), ""
        end
        raise GatewayError, err unless err.empty?

        resp
      end

      def config(key)
        ans = @h.request({ "t" => "config", "key" => key }, "config")
        ans["present"] ? Kavach.unb64(ans["value"] || "") : nil
      end

      def emit(sink, data, local: false)
        raise AbortStep if @h.aborted

        data = Kavach.binary(data)
        @h.send_msg("t" => "emit", "sink" => sink, "data" => Kavach.b64(data), "scope" => local ? "local" : "remote")
        nil
      end
    end
  end

  # If this process was started as a replay host (its last argument is
  # +kavach-host+), serves the driver and exits; otherwise returns at once.
  #
  # Call it first thing in the program, before consuming any input or starting
  # any servers. The block returns a fresh handler. +gateways+ maps names to
  # Kavach::Gateway objects (or connection callables, taken as remote).
  def self.maybe_host(argv = ARGV, gateways: nil, &handler_factory)
    raise ArgumentError, "maybe_host needs a block that creates the handler" unless handler_factory
    return unless argv.last == HOST_ARG

    # Take the protocol stream for ourselves, then point fd 1 and $stdout at
    # standard error so that a handler that prints cannot corrupt it (section 9.1).
    proto_out = STDOUT.dup
    proto_out.binmode
    proto_out.sync = true
    proto_in = STDIN.dup
    proto_in.binmode
    $stdout.flush
    STDOUT.reopen(STDERR)
    $stdout = STDERR
    STDIN.reopen(File::NULL)
    code = Host.new(handler_factory, proto_in, proto_out, gateways: gateways).run
    proto_out.close
    $stderr.flush
    exit(code)
  end
end
