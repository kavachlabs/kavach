# frozen_string_literal: true

require "json"
require_relative "../kavach"

module Kavach
  # The conformance handler of SPEC section 9.6, exactly as specified.
  module Conformance
    ESCAPES = { '"' => '\\"', "\\" => "\\\\", "\n" => "\\n", "\r" => "\\r", "\t" => "\\t" }.freeze

    # Section 9.6: escape only " \ \n \r \t and other characters below U+0020
    # as \u00xx (lowercase hex); everything else is raw UTF-8. (JSON.generate
    # escapes differently in places, so the emitted JSON is built by hand.)
    def self.json_string(value)
      body = value.to_s.dup.force_encoding(Encoding::UTF_8).gsub(/["\\\x00-\x1f]/) do |c|
        ESCAPES[c] || format("\\u%04x", c.ord)
      end
      "\"#{body}\""
    end

    def self.compact(pairs)
      "{#{pairs.map { |k, v| "#{json_string(k)}:#{v == true ? "true" : json_string(v)}" }.join(",")}}".b
    end

    # A registry in which every gateway name is a remote gateway whose
    # connection is +connection+ (by default one that refuses: a host never
    # makes live queries in process mode).
    def self.any_gateway(connection = ->(_request) { raise GatewayError, "conformance host has no live connections" })
      Hash.new { |_h, _name| Gateway.new(REMOTE, connection) }
    end

    # State is a count; each step that does not fail adds 1 (plus any +count+
    # operations, which take effect immediately and are never rolled back).
    class Handler
      attr_reader :count

      def initialize
        @count = 0
      end

      def handle(env, input)
        ops = JSON.parse(input.data.dup.force_encoding(Encoding::UTF_8))
        ops.each { |op| perform(env, op) }
        @count += 1
      end

      def snapshot = @count.to_s

      def restore(data)
        @count = Integer(data.to_s, 10)
      end

      def invariants
        [Invariant.new("below_limit") { raise "count is #{@count}" if @count >= 1000 }]
      end

      private

      def utf8(str) = str.to_s.dup.force_encoding(Encoding::UTF_8).b

      def perform(env, op)
        case op["op"]
        when "clock" then env.emit("trace", Conformance.compact("clock" => env.now_ns.to_s))
        when "rand" then env.emit("trace", env.random(op["n"]))
        when "gateway"
          begin
            env.emit("trace", env.query(op["gateway"], utf8(op["request"])))
          rescue GatewayError => e
            env.emit("trace", Conformance.compact("error" => e.error))
          end
        when "config"
          value = env.config(op["key"])
          env.emit("trace", value.nil? ? Conformance.compact("unset" => true) : value)
        when "getenv"
          value = ENV.fetch(op["name"], nil)
          env.emit("trace", value.nil? ? Conformance.compact("unset" => true) : value)
        when "emit" then env.emit(op["sink"], utf8(op["data"]))
        when "panic" then raise Panic, op["message"]
        when "error" then raise HandlerError, op["message"]
        when "print" then puts op["text"]
        when "count" then @count += op["n"]
        else raise HandlerError, "unknown operation #{op["op"].inspect}"
        end
      end
    end
  end
end
