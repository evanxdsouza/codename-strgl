# A redacted evidence bundle exactly as strgl-collector uploads it (format
# documented in collector/bundle.go). Parsing checks that the bundle is
# internally consistent: every total is re-derived from its turns, each
# included item's commitment is in the submitted chain segment, and the chain
# reproduces any heads this device anchored earlier. None of this proves the
# client reported real work; it shows the bundle was not altered after its
# evidence was recorded and anchored.
class StruggleBundle
  class Invalid < StandardError; end

  SCHEMA = 1
  MAX_BYTES = 20.megabytes
  MAX_RANGE = 31.days
  CREDIT_WINDOW_MS = 120_000
  ITEM_STATUSES = %w[included excluded withheld purged].freeze

  Turn = Data.define(:id, :label_value, :needs_review, :backfilled, :interval_start_ms, :interval_end_ms, :productive_seconds)

  attr_reader :raw, :digest, :data

  def initialize(raw)
    raise Invalid, "bundle is empty" if raw.blank?
    raise Invalid, "bundle is larger than #{MAX_BYTES / 1.megabyte} MB" if raw.bytesize > MAX_BYTES

    @raw = raw
    @digest = Digest::SHA256.hexdigest(raw)
    @data = JSON.parse(raw)
    raise Invalid, "bundle must be a JSON object" unless @data.is_a?(Hash)
    validate!
  rescue JSON::ParserError => e
    raise Invalid, "bundle is not valid JSON: #{e.message}"
  end

  def project = data["project"]
  def range_start = Time.at(data["from"] / 1000.0).utc
  def range_end = Time.at(data["to"] / 1000.0).utc
  def device_id = data["device_id"]
  def supersedes = data["supersedes"].presence
  def totals = data["totals"]

  def turns
    @turns ||= data["turns"].map do |turn|
      label = turn["label"]
      Turn.new(
        id: turn["id"], label_value: label["value"], needs_review: label["needs_review"] == true,
        backfilled: turn["backfilled"] == true, interval_start_ms: label["interval_start"],
        interval_end_ms: label["interval_end"], productive_seconds: label["productive_seconds"]
      )
    end
  end

  # Seconds of the union of label-1 intervals, as the collector computes
  # them: overlapping turns never count twice.
  def self.union_seconds(intervals)
    covered = nil
    total = 0
    intervals.sort.each do |start, finish|
      start = [ start, covered ].max if covered
      next unless finish > start

      total += finish - start
      covered = covered ? [ covered, finish ].max : finish
    end
    total / 1000
  end

  # :verified when every anchor this device sent inside the segment matches
  # the recomputed head, :unanchored when none falls inside it and :mismatch
  # when any differs. A mismatch is kept for investigation, not rejected.
  def chain_status(anchors)
    heads = chain_heads
    return [ "empty", 0 ] if heads.empty?

    relevant = anchors.select { |anchor| heads.key?(anchor.seq) }
    return [ "unanchored", 0 ] if relevant.empty?
    return [ "mismatch", relevant.count { |anchor| heads[anchor.seq] == anchor.head } ] if relevant.any? { |anchor| heads[anchor.seq] != anchor.head }

    [ "verified", relevant.size ]
  end

  def chain_seq_range
    seqs = data.dig("chain", "entries").map { |entry| entry["seq"] }
    seqs.empty? ? nil : seqs.min..seqs.max
  end

  private

  # Head after each entry, matching chainHead in collector/store.go.
  def chain_heads
    head = data.dig("chain", "previous_head").to_s
    data.dig("chain", "entries").to_h do |entry|
      head = Digest::SHA256.hexdigest("#{head}\n#{entry["commitment"]}\n#{entry["recorded_at"]}")
      [ entry["seq"], head ]
    end
  end

  def validate!
    check data["schema"] == SCHEMA, "unsupported bundle schema #{data["schema"].inspect}"
    %w[project device_id rule_set redaction_version collector_version].each do |key|
      check data[key].is_a?(String) && data[key].present? && data[key].length <= 255, "#{key} is missing"
    end
    check data["project"] !~ /[[:cntrl:]]/, "project name has control characters"
    %w[from to created_at].each { |key| check data[key].is_a?(Integer), "#{key} must be milliseconds" }
    check data["to"] > data["from"], "range is empty"
    check (data["to"] - data["from"]) <= MAX_RANGE.in_milliseconds, "range is longer than #{MAX_RANGE.inspect}"
    check data["turns"].is_a?(Array) && data["turns"].any?, "bundle has no turns"
    check data["totals"].is_a?(Hash), "totals are missing"
    check data["chain"].is_a?(Hash) && data.dig("chain", "entries").is_a?(Array), "chain is missing"
    check data["user_context"].to_s.length <= 10_000, "context is too long"

    validate_chain!
    entries = data.dig("chain", "entries").index_by { |entry| entry["seq"] }
    ids = data["turns"].map { |turn| turn["id"] }
    check ids.all? { |id| id.is_a?(String) && id.present? } && ids.uniq.size == ids.size, "turn IDs are missing or repeated"
    data["turns"].each { |turn| validate_turn!(turn, entries) }
    validate_totals!
  end

  def validate_chain!
    seqs = data.dig("chain", "entries").map { |entry| entry["seq"] }
    check seqs.all?(Integer) && seqs == (seqs.first.to_i...(seqs.first.to_i + seqs.size)).to_a, "chain entries must be consecutive"
  end

  def validate_turn!(turn, entries)
    label = turn["label"]
    check label.is_a?(Hash) && [ 0, 1 ].include?(label["value"]), "turn #{turn["id"]} has no 0/1 label"
    check label["rule_set"] == data["rule_set"], "turn #{turn["id"]} was labelled by another rule set"
    check label["reason"].is_a?(String) && label["reason"].present?, "turn #{turn["id"]} has no label reason"
    start, finish, seconds = label.values_at("interval_start", "interval_end", "productive_seconds")
    check [ start, finish, seconds ].all?(Integer), "turn #{turn["id"]} has no interval"
    check start == turn["started_at"] && finish >= start && finish - start <= CREDIT_WINDOW_MS, "turn #{turn["id"]} interval exceeds the 2-minute estimate"
    check start >= data["from"] && start < data["to"], "turn #{turn["id"]} is outside the submitted range"
    check seconds.between?(0, (finish - start) / 1000), "turn #{turn["id"]} claims more seconds than its interval"
    check label["value"] == 1 || seconds.zero?, "turn #{turn["id"]} is labelled 0 but claims seconds"

    Array(turn["items"]).each do |item|
      check ITEM_STATUSES.include?(item["status"]), "item #{item["id"]} has an unknown status"
      check item["status"] == "included" || item["text"].blank?, "item #{item["id"]} is not included but carries text"
      entry = entries[item["chain_seq"]]
      check entry && entry["commitment"] == item["commitment"], "item #{item["id"]} is not in the submitted chain"
    end
  end

  def validate_totals!
    one = turns.select { |turn| turn.label_value == 1 }
    union = self.class.union_seconds(one.map { |turn| [ turn.interval_start_ms, turn.interval_end_ms ] })
    claimed = one.sum(&:productive_seconds)
    # Each turn's seconds are rounded down separately, so the sum may fall
    # short of the union by under a second per turn, but never exceed it.
    check claimed <= union && claimed >= union - one.size, "per-turn seconds double-count overlapping turns"
    check totals["proposed_productive_seconds"] == claimed, "proposed seconds do not match the turns"
    check totals["turns"] == turns.size, "turn count does not match"
    check totals["labelled_one"] == one.size, "label count does not match"
    check totals["estimated_human_seconds"].is_a?(Integer) && totals["estimated_human_seconds"] >= 0, "estimated seconds are missing"
    check totals["agent_seconds"].is_a?(Integer) && totals["agent_seconds"] >= 0, "agent seconds are missing"
  end

  def check(condition, message)
    raise Invalid, message unless condition
  end
end
