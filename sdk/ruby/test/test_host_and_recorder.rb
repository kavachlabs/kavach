# frozen_string_literal: true

require_relative "test_helper"
require "json"
require "open3"
require_relative "../examples/ledger/handler"

class TestHostAndRecorder < Minitest::Test
  include TestHelper

  def b64(s) = [s].pack("m0")

  def host_session(handler_factory, messages)
    input = StringIO.new(messages.map { |m| "#{JSON.generate(m)}\n" }.join)
    output = StringIO.new
    code = Kavach::Host.new(handler_factory, input, output, environment: -> { {} }).run
    [code, output.string.lines.map { |l| JSON.parse(l) }]
  end

  def hello(extra = {}) = { "t" => "hello", "protocol" => 1, "service" => "x", "start" => "genesis", "mode" => "process" }.merge(extra)

  def step(ops) = { "t" => "step", "seq" => "0", "source" => "s", "position" => "0", "data" => b64(JSON.generate(ops)) }

  def test_sandbox_mode_is_refused
    code, out = host_session(-> { Kavach::Conformance::Handler.new }, [hello("mode" => "sandbox")])
    assert_equal 1, code
    assert_equal [{ "t" => "fatal", "message" => "sandbox mode not supported" }], out
  end

  def test_abort_is_not_swallowed_by_a_bare_rescue
    handler = Class.new do
      def handle(env, _input)
        env.now_ns
      rescue Exception # rubocop:disable Lint/RescueException
        env.emit("after", "swallowed")
      end
    end
    _, out = host_session(-> { handler.new }, [hello, step([]), { "t" => "abort", "detail" => "x" }, { "t" => "end" }])
    assert_equal({ "t" => "clock" }, out[1])
    assert_equal({ "t" => "done", "outcome" => "aborted" }, out.last)
    refute(out.any? { |m| m["t"] == "emit" })
  end

  def test_non_standard_error_in_handler_is_a_panic_in_host
    handler = Class.new do
      def handle(_env, _input) = raise(NotImplementedError, "nope")
    end
    _, out = host_session(-> { handler.new }, [hello, step([]), { "t" => "end" }])
    assert_equal "panic", out.last["outcome"]
    assert_equal "NotImplementedError: nope", out.last["message"]
  end

  def test_recorder_failure_never_fails_the_step
    quiet_logs do |logs|
      rec = Kavach::Recorder.new(Kavach::Conformance::Handler.new, service: "x",
                                 recorder_command: ["/nonexistent/kavach-recorder"])
      refute rec.recording?
      assert(logs.any? { |l| l.include?("recording has stopped") })
      input = Kavach::Input.new(source: "s", position: "0", data: '[{"op":"error","message":"m"}]')
      r = rec.step(input)
      assert_equal ["error", "m"], [r.kind, r.message]
      rec.close
    end
  end

  def test_required_recorder_fails_construction
    assert_raises(Kavach::RecorderError) do
      Kavach::Recorder.new(Kavach::Conformance::Handler.new, service: "x", required: true,
                           recorder_command: ["/nonexistent/kavach-recorder"])
    end
  end

  def test_ledger_bug_and_fix
    quiet_logs do
      input = Kavach::Input.new(source: "s", position: "1", data: '{"id":"e","type":"deposit","account":"a","amount":null}')
      rec = Kavach::Recorder.new(Ledger.new, service: "ledger", recorder_command: ["/nonexistent"])
      r = rec.step(input)
      assert_equal ["panic", "NoMethodError: undefined method '<=' for nil"], [r.kind, r.message] if RUBY_VERSION >= "3.4"
      assert_equal "panic", r.kind
      fixed = Kavach::Recorder.new(Ledger.new(fix: true), service: "ledger", recorder_command: ["/nonexistent"])
      r = fixed.step(input)
      assert r.ok?
      assert_equal "ledger.rejections", r.outputs.first.sink
    end
  end

  def test_exit_exception_is_recorded_and_reraised
    quiet_logs do
      handler = Class.new do
        def handle(_env, _input) = raise(Interrupt)
      end
      rec = Kavach::Recorder.new(handler.new, service: "x", recorder_command: ["/nonexistent"])
      assert_raises(Interrupt) { rec.step(Kavach::Input.new(source: "s", position: "0", data: "")) }
    end
  end

  def test_records_through_the_real_recorder_if_available
    recorder = Kavach.find_recorder
    skip "kavach-recorder not on PATH or $KAVACH_RECORDER" unless recorder
    Dir.mktmpdir("kavach-rb-") do |dir|
      rec = Kavach::Recorder.new(Ledger.new, service: "ledger", dir: dir, required: true)
      rec.step(Kavach::Input.new(source: "s", position: "1", data: '{"id":"e","type":"deposit","account":"a","amount":null}'))
      assert rec.flush(durable: true)
      rec.close
      assert_includes Dir[File.join(dir, "fixtures", "*.kavach")].map { |f| File.basename(f) }.join, ".kavach"
    end
  end
end
