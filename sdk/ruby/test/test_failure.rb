# frozen_string_literal: true

require_relative "test_helper"

# The failure mapping of SPEC section 4.5, including location independence.
class TestFailure < Minitest::Test
  class Widget; end

  def test_panic_and_error_messages_are_exact
    f = Kavach.classify(Kavach::Panic.new("boom at /a/b.rb:3 0x00007f00"))
    assert_equal %w[panic boom\ at\ /a/b.rb:3\ 0x00007f00], [f.kind, f.message]
    f = Kavach.classify(Kavach::HandlerError.new("amount is null"))
    assert_equal ["error", "amount is null"], [f.kind, f.message]
  end

  def test_standard_error_is_a_panic_with_class_and_message
    f = Kavach.classify(KeyError.new("key not found: :amount"))
    assert_equal ["panic", "KeyError: key not found: :amount"], [f.kind, f.message]
  end

  def test_backtrace_goes_in_the_detail
    e = begin
      raise ArgumentError, "bad"
    rescue ArgumentError => err
      err
    end
    f = Kavach.classify(e)
    assert_includes f.detail, "ArgumentError: bad"
    assert_includes f.detail, "test_failure.rb"
    refute_includes f.message, "test_failure.rb"
  end

  def test_object_identities_and_addresses_are_removed
    msg = Kavach.classify(RuntimeError.new("bad #<#{Widget}:0x000071a2b3c4d5e6 @a=1>: 0x000071a2b3c4d5e6 at 0x0000dead00")).message
    refute_match(/0x\h{6}/, msg)
    assert_equal "RuntimeError: bad #<TestFailure::Widget>: 0x at 0x", msg
    assert_equal "NoMethodError: undefined method 'x' for #<TestFailure::Widget>",
                 Kavach.classify(NoMethodError.new("undefined method 'x' for #<TestFailure::Widget:0x000071a2b3c4d5e6>")).message
  end

  def test_paths_and_line_numbers_are_removed
    msg = Kavach.classify(LoadError.new("cannot load /home/alice/app/lib/ledger.rb:42:in 'handle' and C:/x/y.rb:7")).message
    assert_equal "LoadError: cannot load ledger.rb:in 'handle' and y.rb", msg
  end

  def test_same_failure_from_another_checkout_compares_equal
    a = Kavach.classify(RuntimeError.new("/srv/a/app.rb:10 failed #<Foo:0x0000aaaa1111>")).message
    b = Kavach.classify(RuntimeError.new("/home/b/app.rb:99 failed #<Foo:0x0000bbbb2222>")).message
    assert_equal a, b
  end

  def test_nil_method_message_is_stable
    err = begin
      nil <= 0
    rescue NoMethodError => e
      e
    end
    assert_match(/\ANoMethodError: undefined method .<=. for nil\z/, Kavach.classify(err).message)
  end
end
