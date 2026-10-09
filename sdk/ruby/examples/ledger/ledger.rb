#!/usr/bin/env ruby
# frozen_string_literal: true

# Kavach's demo service, in Ruby.
#
#   ruby -I../../lib ledger.rb          # the buggy build: crashes on "amount": null
#   ruby -I../../lib ledger.rb --fix    # the fixed build
#
# Both take --in FILE (default events.jsonl, next to this file) and
# --fixtures DIR.
#
# The build is chosen by a command-line flag and not an environment variable
# on purpose: environment variables are served from the journal on replay, so
# a replay of the buggy build's fixture would otherwise run the buggy code.
# Replay hosts are therefore `ruby ledger.rb` (old) and `ruby ledger.rb --fix`
# (new).
#
# If kavach-recorder is available ($KAVACH_RECORDER or PATH) the service
# records through it; otherwise it says so loudly and runs unrecorded.
require "optparse"
require "kavach"
require_relative "handler"

# The host command has `kavach-host` appended; it must be able to see --fix.
FIX = ARGV.include?("--fix")
Kavach.maybe_host { Ledger.new(fix: FIX) }

path = File.join(__dir__, "events.jsonl")
fixtures = "fixtures"
OptionParser.new do |o|
  o.on("--fix", "run the fixed handler")
  o.on("--in FILE", "JSON-lines file of events") { |v| path = v }
  o.on("--fixtures DIR", "directory for journals and crash fixtures") { |v| fixtures = v }
end.parse!

rec = Kavach::Recorder.new(
  Ledger.new(fix: FIX), service: "ledger", dir: fixtures,
  deliver: ->(outs) { outs.each { |o| puts format("%-18s %s", o.sink, o.data) } }
)
begin
  File.foreach(path).with_index(1) do |line, line_no|
    line = line.strip
    next if line.empty?

    result = rec.step(Kavach::Input.new(source: "file:#{File.basename(path)}", position: line_no.to_s, data: line))
    next if result.ok?

    warn "ledger: line #{line_no}: #{result.kind}: #{result.message}"
    # Stop the service, as the Go demo does. The recorder has already marked
    # the failure and cut a fixture.
    exit 1 if result.kind == "panic"
  end
ensure
  rec.close
end
