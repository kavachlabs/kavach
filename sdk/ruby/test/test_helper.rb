# frozen_string_literal: true

$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "minitest/autorun"
require "stringio"
require "kavach"
require "kavach/conformance/recorder_case"

module TestHelper
  SPEC_DIR = Kavach::Conformance.spec_dir

  def self.python? = system("python3", "--version", out: File::NULL, err: File::NULL)

  # Runs the block with SDK log lines collected instead of printed.
  def quiet_logs
    lines = []
    old = Kavach.logger
    Kavach.logger = ->(level, msg) { lines << "#{level}: #{msg}" }
    yield lines
  ensure
    Kavach.logger = old
  end
end
