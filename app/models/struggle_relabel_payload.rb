# What a model relabelling run may send: a further-minimised copy of a frozen,
# already redacted submission bundle. It keeps the included items' redacted
# text, command and exit code. The rule labels, reasons and signals are left
# out, so the model's opinion is independent of them (a small model copies a
# label it can see). It
# drops the device ID, session hashes, commitments, chain, branch, project
# name, file names and absolute times, and leaves out excluded, withheld and
# purged items entirely. Turns are renamed t1, t2, ... so turn IDs stay here.
#
# The collector is open source and can be modified, so the server checks the
# payload again for text that should have been redacted. A match raises
# Leak and nothing is sent.
class StruggleRelabelPayload
  class Leak < StandardError; end

  LEAK_PATTERNS = {
    "email address" => /\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b/,
    "private key" => /-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----/,
    "API key" => /\b(?:sk|pk|rk|whsec)[-_](?:live|test|proj|ant)?[-_]?[A-Za-z0-9_-]{16,}\b/,
    "GitHub token" => /\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b/,
    "AWS key" => /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/,
    "Slack token" => /\bxox[abposr]-[A-Za-z0-9-]{10,}\b/,
    "JWT" => /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b/,
    "home directory" => %r{(?:/home/|/Users/|[A-Za-z]:\\Users\\)(?!\[)[^/\\\s"']+}
  }.freeze

  attr_reader :turn_ids

  def initialize(bundle_data, prompt_version:)
    @data = bundle_data
    @prompt_version = prompt_version
    @turn_ids = {}
  end

  def to_json(*)
    @json ||= begin
      json = JSON.generate(build)
      check_for_leaks!(json)
      json
    end
  end

  private

  def build
    from = @data["from"]
    {
      "prompt_version" => @prompt_version,
      "rule_set" => @data["rule_set"],
      "redaction_version" => @data["redaction_version"],
      "user_context" => @data["user_context"].to_s,
      "turns" => @data["turns"].each_with_index.map do |turn, index|
        ref = "t#{index + 1}"
        @turn_ids[ref] = turn["id"]
        {
          "ref" => ref,
          "seconds_from_start" => (turn["started_at"] - from) / 1000,
          "end_reason" => turn["end_reason"],
          "tool_calls" => turn["tool_calls"],
          "items" => Array(turn["items"]).select { |item| item["status"] == "included" }.map do |item|
            item.slice("kind", "text", "command", "exit", "is_error").compact
          end
        }
      end
    }
  end

  def check_for_leaks!(json)
    # Inspect the decoded strings, not the JSON, so escapes cannot hide a match.
    text = strings(JSON.parse(json)).join("\n")
    LEAK_PATTERNS.each do |kind, pattern|
      raise Leak, "the redacted bundle still contains what looks like a #{kind}" if text.match?(pattern)
    end
  end

  def strings(value)
    case value
    when String then [ value ]
    when Array then value.flat_map { |v| strings(v) }
    when Hash then value.values.flat_map { |v| strings(v) }
    else []
    end
  end
end
