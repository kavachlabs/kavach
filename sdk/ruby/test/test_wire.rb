# frozen_string_literal: true

require_relative "test_helper"

class TestWire < Minitest::Test
  W = Kavach::Wire

  def test_uvarint
    assert_equal "\x00".b, W.uvarint(0)
    assert_equal "\x7f".b, W.uvarint(127)
    assert_equal "\x80\x01".b, W.uvarint(128)
    assert_equal "\xac\x02".b, W.uvarint(300)
  end

  def test_frames_are_length_kind_payload
    assert_equal "\x01\x03".b, W.step_end_frame
    assert_equal "\x02\x06\x01".b, W.flush_frame(true)
  end

  def test_input_record_is_binary_safe
    f = W.input_record("s", "é", "\xff\x00".b)
    assert_equal Encoding::BINARY, f.encoding
    assert_equal [0x01, 0x00], f.bytes[2, 2] # type input, flags 0
    assert_equal "\x01s\x02\xc3\xa9\x02\xff\x00".b, f.byteslice(4..)
  end

  def test_critical_flag
    assert_equal 1, W.gateway_record("g", "r", "", "e", false).getbyte(3)
    assert_equal 1, W.config_record("k", nil, "env").getbyte(3)
    assert_equal 0, W.output_record("s", "d", true).getbyte(3)
  end

  def test_clock_is_little_endian_i64
    assert_equal [1, 0, 0, 0, 0, 0, 0, 0], W.clock_record(1).bytes[4, 8]
    assert_equal [0xff] * 8, W.clock_record(-1).bytes[4, 8]
  end
end
