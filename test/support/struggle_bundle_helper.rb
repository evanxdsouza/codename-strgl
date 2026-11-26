# Builds redacted bundles in strgl-collector's format (collector/bundle.go)
# with real commitments and chain entries, for tests.
module StruggleBundleHelper
  T0_MS = Time.utc(2026, 11, 20, 10).to_i * 1000

  # turns: [{ id:, start: seconds after T0, value: 0/1, seconds:, interval: seconds }]
  def struggle_bundle_data(turns:, project: "calcs", device_id: "dev_test", supersedes: nil, chain_start: 1, previous_head: "", context: "", consent: false)
    seq = chain_start
    entries = []
    built = turns.map do |turn|
      start = T0_MS + turn[:start] * 1000
      commitment = Digest::SHA256.hexdigest("item-#{turn[:id]}")
      entries << { "seq" => seq, "commitment" => commitment, "recorded_at" => start + 1 }
      item = {
        "id" => "#{turn[:id]}:0", "kind" => "prompt", "occurred_at" => start, "recorded_at" => start + 1,
        "commitment" => commitment, "chain_seq" => seq, "status" => "included", "text" => turn.fetch(:text, "why does add() fail?")
      }
      seq += 1
      {
        "id" => turn[:id], "harness" => "opencode", "session" => "abc", "branch" => "main", "model" => "m",
        "started_at" => start, "ended_at" => start + 30_000, "end_reason" => "completed", "recorded_at" => start + 1,
        "backfilled" => false, "prompt_length" => 20, "agent_events" => 3, "tool_calls" => 1, "child_sessions" => 0,
        "input_tokens" => 10, "output_tokens" => 5, "files_changed" => [], "diff_observed" => true,
        "label" => {
          "turn_id" => turn[:id], "value" => turn[:value], "reason" => "test reason", "signals" => [ "diagnosis" ],
          "evidence" => [ item["id"] ], "coverage" => "full", "needs_review" => false, "rule_set" => "struggle-v1.0",
          "interval_start" => start, "interval_end" => start + turn.fetch(:interval, 120) * 1000,
          "productive_seconds" => turn[:seconds]
        },
        "items" => [ item ]
      }
    end
    {
      "schema" => 1, "collector_version" => "0.1.0", "device_id" => device_id, "key_protection" => "file-0600",
      "rule_set" => "struggle-v1.0", "redaction_version" => "redact-v1.0", "project" => project,
      "from" => T0_MS - 3_600_000, "to" => T0_MS + 86_400_000, "created_at" => T0_MS + 90_000_000,
      "user_context" => context, "supersedes" => supersedes, "model_relabel_consent" => (true if consent),
      "totals" => {
        "estimated_human_seconds" => built.size * 120, "agent_seconds" => 0,
        "proposed_productive_seconds" => built.sum { |t| t["label"]["productive_seconds"] },
        "turns" => built.size, "labelled_one" => built.count { |t| t["label"]["value"] == 1 },
        "needs_review" => 0, "backfilled" => 0, "withheld_items" => 0, "excluded_items" => 0
      },
      "turns" => built,
      "chain" => { "previous_head" => previous_head, "entries" => entries },
      "notes" => [ "test bundle" ]
    }.compact
  end

  def struggle_bundle(**options) = JSON.generate(struggle_bundle_data(**options))

  # The chain head after each entry, as the collector computes it.
  def struggle_chain_heads(data)
    head = data.dig("chain", "previous_head")
    data.dig("chain", "entries").to_h do |entry|
      head = Digest::SHA256.hexdigest("#{head}\n#{entry["commitment"]}\n#{entry["recorded_at"]}")
      [ entry["seq"], head ]
    end
  end
end
