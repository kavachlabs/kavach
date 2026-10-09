# frozen_string_literal: true

require "json"
require "tmpdir"
require_relative "../conformance"

module Kavach
  module Conformance
    # The spec directory: $KAVACH_SPEC_DIR, else the repository's own.
    def self.spec_dir
      ENV.fetch("KAVACH_SPEC_DIR", File.expand_path("../../../../../spec", __dir__))
    end

    # Serves one step's scripted answers, in order, per kind.
    class Answers
      def initialize = @queue = {}

      def load(answers) = @queue = answers.transform_values(&:dup)

      def pop(kind)
        @queue.fetch(kind).shift or raise "the case has no more #{kind} answers"
      rescue KeyError
        raise "the handler read a #{kind} the case has no answer for"
      end

      def clock = Integer(pop("clock"), 10)

      def random(n)
        data = pop("rand").unpack1("m")
        raise "case rand answer is #{data.bytesize} bytes, handler asked for #{n}" unless data.bytesize == n

        data
      end

      def gateway(_request)
        a = pop("gateway")
        raise GatewayError, a["error"] if a.key?("error")

        a["response"].unpack1("m")
      end

      def config(_key)
        a = pop("config")
        a["unset"] ? nil : a["value"].unpack1("m")
      end
    end

    # Runs one SDK recorder case (spec/recorder/sdk/README.md) through this SDK
    # against the fake recorder; returns the fake's result.json contents.
    def self.run_recorder_case(case_path, fake_recorder: nil, python: "python3")
      kase = JSON.parse(File.read(case_path))
      fake = fake_recorder || File.join(spec_dir, "recorder", "sdk", "fake_recorder.py")
      handler = Handler.new
      handler.restore(kase["snapshot"]) if kase.key?("snapshot")
      answers = Answers.new
      flags = (kase["flags"] || {}).transform_values { |v| v.b }
      Dir.mktmpdir("kavach-case-") do |tmp|
        result_path = File.join(tmp, "result.json")
        rec = Recorder.new(
          handler,
          service: kase["open"]["service"], start: kase["open"]["start"], snapshots: kase["open"]["snapshots"],
          recorder_command: [python, fake, File.expand_path(case_path), result_path],
          gateways: any_gateway(answers.method(:gateway)),
          config: answers.method(:config), config_source: "case",
          flags: flags.empty? ? nil : -> { flags },
          clock_ns: answers.method(:clock), random_bytes: answers.method(:random),
          required: true
        )
        kase["actions"].each do |action|
          if (s = action["step"])
            answers.load(action["answers"] || {})
            rec.step(Input.new(source: s["source"], position: s["position"], data: s["data"].unpack1("m")))
          elsif (f = action["flush"])
            rec.flush(durable: f["durable"] == true)
          end
        end
        rec.close
        unless File.exist?(result_path)
          return { "pass" => false, "error" => "the fake recorder wrote no result (did the SDK close it?)", "frames" => [] }
        end

        JSON.parse(File.read(result_path))
      end
    end
  end
end
