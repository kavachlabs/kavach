# frozen_string_literal: true

module Kavach
  # Encoding of the record stream (SPEC section 10.2) and its record payloads
  # (section 4). Every method returns a binary String.
  module Wire
    OPEN = 0x01
    RECORD = 0x02
    STEP_END = 0x03
    FACTS = 0x04
    SNAPSHOT = 0x05
    FLUSH = 0x06
    CLOSE = 0x07

    INPUT = 0x01
    CLOCK = 0x02
    RAND = 0x03
    OUTPUT = 0x04
    MARKER = 0x05
    GATEWAY = 0x07
    CONFIG = 0x09

    CRITICAL = 0x01

    module_function

    def uvarint(n)
      raise ArgumentError, "uvarint of a negative number" if n.negative?

      out = []
      while n >= 0x80
        out << ((n & 0x7F) | 0x80)
        n >>= 7
      end
      out << n
      out.pack("C*")
    end

    def bytes_field(b)
      b = b.to_s.b
      uvarint(b.bytesize) + b
    end

    def string_field(s) = bytes_field(s.to_s.dup.force_encoding(Encoding::UTF_8).b)

    def scope_byte(local) = [local ? 1 : 0].pack("C")

    def frame(kind, payload = "".b)
      uvarint(1 + payload.bytesize) + [kind].pack("C") + payload
    end

    def record_frame(rtype, payload, critical: false)
      frame(RECORD, [rtype, critical ? CRITICAL : 0].pack("CC") + payload)
    end

    def input_record(source, position, data)
      record_frame(INPUT, string_field(source) + string_field(position) + bytes_field(data))
    end

    def clock_record(unix_nanos) = record_frame(CLOCK, [unix_nanos].pack("q<"))

    def rand_record(data) = record_frame(RAND, bytes_field(data))

    def output_record(sink, data, local)
      record_frame(OUTPUT, string_field(sink) + bytes_field(data) + scope_byte(local))
    end

    def marker_record(kind, message, data = "")
      record_frame(MARKER, string_field(kind) + string_field(message) + bytes_field(data))
    end

    def gateway_record(gateway, request, response, error, local)
      record_frame(
        GATEWAY,
        string_field(gateway) + bytes_field(request) + bytes_field(response) + string_field(error) + scope_byte(local),
        critical: true
      )
    end

    def config_record(key, value, source)
      record_frame(
        CONFIG,
        string_field(key) + [value.nil? ? 0 : 1].pack("C") + bytes_field(value || "") + string_field(source),
        critical: true
      )
    end

    def step_end_frame = frame(STEP_END)

    def snapshot_frame(data) = frame(SNAPSHOT, bytes_field(data))

    def flush_frame(durable) = frame(FLUSH, [durable ? 1 : 0].pack("C"))

    def close_frame = frame(CLOSE)

    # A +facts+ frame: an environment payload whose facts all have form 0.
    def facts_frame(facts)
      body = uvarint(facts.size)
      facts.each { |key, value| body += string_field(key) + "\x00".b + bytes_field(value) }
      frame(FACTS, body)
    end
  end
end
