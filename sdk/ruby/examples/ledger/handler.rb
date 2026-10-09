# frozen_string_literal: true

require "json"
require "kavach"

# A single-writer wallet ledger: folds events into balances.
#
# The planted bug: an event with "amount": null reaches `amount <= 0` and
# raises NoMethodError. Ledger.new(fix: true) rejects it instead.
class Ledger
  def initialize(fix: false)
    @fix = fix
    @balances = Hash.new(0)
    @net = 0 # deposits minus withdrawals
  end

  def handle(env, input)
    begin
      ev = JSON.parse(input.data.dup.force_encoding(Encoding::UTF_8))
    rescue JSON::ParserError => e
      raise Kavach::HandlerError, "decode event at #{input.position}: #{e.class}"
    end
    amount = ev["amount"]
    return reject(env, ev, "missing amount") if @fix && amount.nil?
    return reject(env, ev, "amount must be positive") if amount <= 0 # NoMethodError when nil and not fixed

    at = rfc3339(env.now)
    account = ev["account"]
    case ev["type"]
    when "deposit"
      @net += amount
      post(env, ev, account, amount, at)
    when "withdraw"
      return reject(env, ev, "insufficient funds") if @balances[account] < amount

      @net -= amount
      post(env, ev, account, -amount, at)
    when "transfer"
      return reject(env, ev, "insufficient funds") if @balances[account] < amount

      post(env, ev, account, -amount, at)
      post(env, ev, ev["to"], amount, at)
    else
      reject(env, ev, "unknown event type #{ev["type"]}")
    end
  end

  def invariants
    [
      Kavach::Invariant.new("balances_non_negative") do
        @balances.keys.sort.each do |a|
          raise "account #{a} has balance #{@balances[a]}" if @balances[a].negative?
        end
      end,
      Kavach::Invariant.new("money_conserved") do
        total = @balances.values.sum
        raise "balances sum to #{total}, deposits minus withdrawals is #{@net}" if total != @net
      end
    ]
  end

  def snapshot = JSON.generate("balances" => @balances, "net" => @net)

  def restore(data)
    state = JSON.parse(data)
    @balances = Hash.new(0).merge(state["balances"])
    @net = state["net"]
  end

  private

  # Go's time.RFC3339Nano: trailing zeros of the fraction are dropped.
  def rfc3339(time)
    frac = format("%09d", time.nsec).sub(/0+\z/, "")
    time.strftime("%Y-%m-%dT%H:%M:%S") + (frac.empty? ? "" : ".#{frac}") + "Z"
  end

  def post(env, ev, account, delta, at)
    @balances[account] += delta
    env.emit("ledger.entries", JSON.generate(
      "txn" => env.random(8).unpack1("H*"), "event" => ev["id"], "account" => account,
      "delta" => delta, "balance" => @balances[account], "at" => at
    ))
  end

  def reject(env, ev, reason)
    env.emit("ledger.rejections", JSON.generate("event" => ev["id"], "reason" => reason))
  end
end
