# frozen_string_literal: true

require_relative "test_helper"
require "rbconfig"

class TestConformance < Minitest::Test
  ROOT = File.expand_path("..", __dir__)
  C = Kavach::Conformance

  def test_json_string_escapes_only_what_the_spec_says
    assert_equal "\"a \\\"q\\\" \\\\ <b>&amp; café\\nl\\tt\\r\\u0001\\u001f\u007f\"".b,
                 C.json_string("a \"q\" \\ <b>&amp; café\nl\tt\r\u0001\u001f\u007f").b
  end

  def test_compact_keeps_key_order
    assert_equal '{"error":"x","unset":true}'.b, C.compact("error" => "x", "unset" => true)
  end

  def test_transcripts
    skip "python3 or #{TestHelper::SPEC_DIR}/host missing" unless TestHelper.python? && File.directory?("#{TestHelper::SPEC_DIR}/host")
    host = "#{RbConfig.ruby} -I#{ROOT}/lib #{ROOT}/bin/conformance-host"
    out = IO.popen(["python3", "#{TestHelper::SPEC_DIR}/host/run.py", "--host", host], err: %i[child out], &:read)
    assert $?.success?, out
    assert_match(%r{18/18 transcripts passed}, out)
  end

  Dir[File.join(TestHelper::SPEC_DIR, "recorder", "sdk", "*.json")].sort.each do |path|
    define_method("test_recorder_case_#{File.basename(path, ".json").tr("-", "_")}") do
      skip "python3 missing" unless TestHelper.python?
      result = C.run_recorder_case(path)
      assert result["pass"], result["error"].to_s
    end
  end
end
