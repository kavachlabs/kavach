# frozen_string_literal: true

require "json"
require "monitor"
require "rbconfig"
require_relative "version"
require_relative "core"
require_relative "wire"

module Kavach
  F_SETPIPE_SZ = 1031 # Linux
  PIPE_SIZE = 1 << 20

  # The recorder argument vector: +command+, else $KAVACH_RECORDER, else
  # "kavach-recorder" on PATH. nil if there is none.
  def self.find_recorder(command = nil)
    return command.map(&:to_s) if command && !command.empty?

    env = ENV["KAVACH_RECORDER"]
    return [env] if env && !env.empty?

    ENV.fetch("PATH", "").split(File::PATH_SEPARATOR).each do |dir|
      path = File.join(dir, "kavach-recorder")
      return [path] if File.file?(path) && File.executable?(path)
    end
    nil
  end

  # What happened in one Recorder#step.
  class StepResult
    attr_reader :kind, :message, :detail, :exception, :outputs

    def initialize(kind: "", message: "", detail: "", exception: nil, outputs: [])
      @kind, @message, @detail, @exception, @outputs = kind, message, detail, exception, outputs
    end

    def ok? = kind.empty?

    # Re-raises the handler's exception, or a RuntimeError for an invariant.
    def raise_for_failure
      return if ok?
      raise exception if exception

      raise "kavach: invariant #{message} violated: #{detail}"
    end
  end

  # Runs a handler step by step and writes everything to kavach-recorder.
  #
  # Reads are recorded live (the real clock, SecureRandom-quality bytes, the
  # registered gateway connections and the config provider); outputs are
  # delivered through +deliver+ only after the step succeeded. If the recorder
  # cannot be started, dies, or reports a fatal error, the problem is logged
  # (Kavach.logger), recording stops and steps carry on unrecorded; with
  # <tt>required: true</tt> a recorder that cannot be started raises
  # Kavach::RecorderError from the constructor instead.
  class Recorder
    attr_reader :file, :run

    # handler:: responds to +handle(env, input)+ and optionally +snapshot+,
    #           +restore(data)+ and +invariants+.
    # recorder_command:: argument vector for the recorder (else $KAVACH_RECORDER,
    #                    else PATH).
    # config:: callable key -> String or nil (default: ENV).
    # flags:: callable returning a Hash of "flag.*" keys to values.
    # clock_ns / random_bytes:: replace the real clock and random source.
    def initialize(handler, service:, start: "genesis", snapshots: nil, deliver: nil, gateways: nil,
                   config: nil, config_source: nil, flags: nil, recorder_command: nil, required: false,
                   handler_id: nil, dir: nil, compression: nil, level: nil, block_bytes: nil, flush_ms: nil,
                   segment_bytes: nil, segment_seconds: nil, retain_segments: nil, secret_keys: nil,
                   on_fixture: nil, clock_ns: nil, random_bytes: nil, ready_timeout: 10.0, close_timeout: 10.0)
      start = start.to_s
      raise ArgumentError, "start must be 'genesis' or 'snapshot'" unless %w[genesis snapshot].include?(start)

      can_snapshot = handler.respond_to?(:snapshot) && handler.respond_to?(:restore)
      raise ArgumentError, "start: 'snapshot' needs a handler with snapshot and restore" if start == "snapshot" && !can_snapshot

      @snapshots = snapshots.nil? ? can_snapshot : snapshots
      raise ArgumentError, "snapshots: true needs a handler with snapshot and restore" if @snapshots && !can_snapshot

      @handler = handler
      @deliver = deliver
      @gateways = gateways
      @config = config || ->(k) { ENV.fetch(k, nil) }
      @config_source = config_source || (config ? "config" : "env")
      @clock_ns = clock_ns || -> { Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond) }
      @random = random_bytes || ->(n) { Random.urandom(n) }
      @on_fixture = on_fixture
      @close_timeout = close_timeout

      @step_lock = Monitor.new
      @step_thread = nil
      @mutex = Mutex.new
      @cond = ConditionVariable.new
      @active = false
      @failed_logged = false
      @closing = false
      @closed = false
      @closed_ack = false
      @ready = false
      @durable_count = 0
      @snapshot_requested = false
      @buf = nil
      @outputs = []
      @io = nil
      @thread = nil

      open_obj = { "protocol" => 1, "service" => service, "start" => start, "producer" => PRODUCER,
                   "snapshots" => @snapshots }
      { "handler" => handler_id, "dir" => dir, "compression" => compression, "level" => level,
        "block_bytes" => block_bytes, "flush_ms" => flush_ms, "segment_bytes" => segment_bytes,
        "segment_seconds" => segment_seconds, "retain_segments" => retain_segments,
        "secret_keys" => (secret_keys && !secret_keys.empty? ? secret_keys.to_a : nil) }.each do |k, v|
        open_obj[k] = v unless v.nil?
      end

      cmd = Kavach.find_recorder(recorder_command)
      begin
        raise RecorderError, "kavach-recorder not found (set recorder_command, $KAVACH_RECORDER or put it on PATH)" if cmd.nil?

        spawn_recorder(cmd)
        write(Wire.frame(Wire::OPEN, JSON.generate(open_obj).b))
        facts = { "host.runtime" => Kavach.runtime }
        flags&.call&.each { |k, v| facts[k.to_s] = Kavach.binary(v) }
        write(Wire.facts_frame(facts))
        write(Wire.snapshot_frame(Kavach.binary(handler.snapshot))) if start == "snapshot"
        wait_ready(ready_timeout) if required
      rescue StandardError => e
        if required
          kill
          raise if e.is_a?(RecorderError)

          raise RecorderError, "kavach-recorder could not be started: #{e.message}"
        end
        fail_recording("could not start the recorder: #{e.message}")
      end
      at_exit { close }
    end

    # Runs the handler on one input and returns a StepResult. A handler
    # failure never raises (see StepResult#raise_for_failure); an exception
    # that is not a StandardError (SignalException, SystemExit, NoMemoryError)
    # is recorded as a panic and then re-raised.
    def step(input)
      @step_lock.synchronize do
        raise "kavach: step on a closed Recorder" if @closed

        @step_thread = Thread.current
        begin
          run_step(input)
        ensure
          @step_thread = nil
        end
      end
    end

    # Asks the recorder to close its open block now. With <tt>durable: true</tt>
    # also waits (up to +timeout+ seconds) until it is on disk. Returns false if
    # recording has stopped or the wait timed out. Call it between steps.
    def flush(durable: false, timeout: 10.0)
      raise "kavach: flush must not be called from inside a step" if @step_thread == Thread.current

      before = nil
      @step_lock.synchronize do
        return false unless @active

        before = @mutex.synchronize { @durable_count }
        write(Wire.flush_frame(durable))
      end
      return @active unless durable

      wait_until(timeout) { @durable_count > before || !@active }
      @mutex.synchronize { @durable_count > before }
    end

    # Orderly shutdown: sends +close+ and waits for +closed+.
    def close
      raise "kavach: close must not be called from inside a step" if @step_thread == Thread.current

      @step_lock.synchronize do
        return if @closed

        @closed = true
        return if @io.nil?

        if @active
          @closing = true
          write(Wire.close_frame)
          unless wait_until(@close_timeout) { @closed_ack }
            Kavach.log("warning", "the recorder did not answer close within #{@close_timeout}s")
          end
        end
        @closing = true
        @active = false
        begin
          @io.close_write
        rescue IOError, SystemCallError
          nil
        end
        if @thread&.join(2)
          begin
            @io.close
          rescue IOError, SystemCallError
            nil
          end
        else
          Kavach.log("warning", "the recorder is still running after close")
        end
      end
    end

    # True while records are being written to the recorder.
    def recording? = @active

    # Reads made by the handler through RecordEnv; they are recorded in program
    # order. Not part of the public API.
    def record_clock # :nodoc:
      ns = @clock_ns.call
      rec(Wire.clock_record(ns))
      ns
    end

    def record_random(n) # :nodoc:
      data = Kavach.binary(@random.call(n))
      rec(Wire.rand_record(data))
      data
    end

    def record_query(gateway, request) # :nodoc:
      gw = Kavach.resolve_gateway(@gateways, gateway)
      resp, err = Kavach.call_gateway(gw, request)
      rec(Wire.gateway_record(gateway, request, resp, err, gw.scope == LOCAL))
      raise GatewayError, err unless err.empty?

      resp
    end

    def record_config(key) # :nodoc:
      v = @config.call(key)
      v = Kavach.binary(v) unless v.nil?
      rec(Wire.config_record(key, v, @config_source))
      v
    end

    def record_emit(sink, data, local) # :nodoc:
      @outputs << Output.new(sink: sink, data: data, local: local)
      rec(Wire.output_record(sink, data, local))
      nil
    end

    private

    def spawn_recorder(cmd)
      @io = IO.popen(cmd, "r+") # environment unchanged (section 10.1)
      @io.binmode
      @io.sync = true
      @active = true
      if RUBY_PLATFORM.include?("linux")
        begin
          @io.fcntl(F_SETPIPE_SZ, PIPE_SIZE)
        rescue StandardError
          nil
        end
      end
      @thread = Thread.new { control_loop }
      @thread.name = "kavach-control"
      @thread.report_on_exception = false
    end

    def kill
      @active = false
      return unless @io

      begin
        Process.kill("KILL", @io.pid)
      rescue SystemCallError
        nil
      end
      begin
        @io.close
      rescue IOError, SystemCallError
        nil
      end
    end

    def fail_recording(reason)
      was = @active
      @active = false
      if !@failed_logged && (was || !@closing)
        @failed_logged = true
        Kavach.log("error", "#{reason}; recording has stopped and steps run unrecorded")
      end
      @mutex.synchronize { @cond.broadcast }
    end

    def wait_ready(timeout)
      raise RecorderError, "kavach-recorder did not become ready" unless wait_until(timeout) { @ready || !@active } && @active
    end

    # Blocks until the block is true or +timeout+ seconds pass; true if it held.
    def wait_until(timeout)
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
      @mutex.synchronize do
        until yield
          left = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
          return false if left <= 0

          @cond.wait(@mutex, left)
        end
        true
      end
    end

    def control_loop
      while (raw = @io.gets)
        begin
          msg = JSON.parse(raw)
          t = msg.fetch("t")
        rescue JSON::ParserError, KeyError, TypeError, NoMethodError
          Kavach.log("warning", "unreadable message from the recorder: #{raw[0, 200].inspect}")
          next
        end
        on_control(t, msg)
      end
    rescue StandardError, IOError => e
      Kavach.log("warning", "control stream failed: #{e.message}")
    ensure
      fail_recording("the recorder exited unexpectedly") unless @closing
      @mutex.synchronize do
        @closed_ack = true
        @ready = true
        @cond.broadcast
      end
    end

    def on_control(type, msg)
      case type
      when "ready"
        @file, @run = msg["file"], msg["run"]
        @mutex.synchronize { @ready = true; @cond.broadcast }
      when "snapshot_request"
        @snapshot_requested = true
      when "durable"
        @mutex.synchronize { @durable_count += 1; @cond.broadcast }
      when "fixture"
        Kavach.log("warning", "wrote fixture #{msg["file"]} (input seq #{msg["seq"]}, #{msg["failure"]})")
        begin
          @on_fixture&.call(msg)
        rescue StandardError => e
          Kavach.log("error", "on_fixture callback failed: #{e.message}")
        end
      when "error"
        fatal = msg["fatal"] == true
        Kavach.log("error", "recorder #{fatal ? "fatal error" : "error"}: #{msg["message"]}")
        fail_recording("the recorder reported a fatal error: #{msg["message"]}") if fatal
      when "closed"
        @mutex.synchronize { @closed_ack = true; @cond.broadcast }
      end
    end

    def write(data)
      return unless @active && !data.empty?

      @io.write(data)
    rescue IOError, SystemCallError => e
      fail_recording("could not write to the recorder: #{e.message}")
    end

    def rec(frame)
      @buf << frame if @buf && @active
    end

    def run_step(input)
      answer_snapshot_request
      @buf = "".b
      @outputs = []
      # The input goes in before the handler runs, so that a step that kills
      # the process still leaves it on record (section 10.2).
      write(Wire.input_record(input.source, input.position, input.data))
      failure = raised = nil
      begin
        @handler.handle(RecordEnv.new(self), input)
      rescue Exception => e # rubocop:disable Lint/RescueException
        failure = Kavach.classify(e)
        raised = e
      end
      failure ||= Kavach.check_invariants(@handler)
      rec(Wire.marker_record(failure.kind, failure.message, failure.detail)) if failure
      rec(Wire.step_end_frame)
      buf, outputs = @buf, @outputs
      @buf, @outputs = nil, []
      write(buf)
      result = if failure
                 StepResult.new(kind: failure.kind, message: failure.message, detail: failure.detail,
                                exception: raised, outputs: outputs)
               else
                 StepResult.new(outputs: outputs)
               end
      @deliver&.call(outputs) if failure.nil? && !outputs.empty?
      raise raised if raised && !raised.is_a?(StandardError)

      result
    end

    def answer_snapshot_request
      return unless @snapshot_requested

      @snapshot_requested = false
      return unless @active && @snapshots

      begin
        data = Kavach.binary(@handler.snapshot)
      rescue StandardError => e
        Kavach.log("error", "snapshot failed (#{e.message}); staying in the current segment")
        return
      end
      write(Wire.snapshot_frame(data))
    end

    # The Env handed to the handler while recording.
    class RecordEnv < Env
      def initialize(recorder)
        super()
        @r = recorder
      end

      def now_ns = @r.record_clock
      def random(n) = n <= 0 ? "".b : @r.record_random(n)
      def query(gateway, request) = @r.record_query(gateway, Kavach.binary(request))
      def config(key) = @r.record_config(key)
      def emit(sink, data, local: false) = @r.record_emit(sink, Kavach.binary(data), local)
    end
  end
end
