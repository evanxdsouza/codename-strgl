# Sends one queued StruggleRelabelRun to the configured model and stores its
# proposed labels. Safe to retry: only a queued run is claimed, and a run is
# claimed once. Proposals never change any hours on the submission.
class StruggleRelabelJob < ApplicationJob
  queue_as :literally_whenever

  SYSTEM_PROMPT = <<~PROMPT.freeze
    You give a second opinion on labels for a student's coding session with an AI agent. Each turn is one
    prompt the student typed, then what the agent did. Placeholders such as [PERSON:1a2b3c4d] are redacted values.

    Judge each turn only by its own prompt text:
    - value 1 when the student shows their own thinking: they describe an error or a cause, state a hypothesis,
      add a constraint, question or review the agent's work, or report their own edit.
    - value 0 when the prompt shows no thinking of their own, for example only asking to retry or continue.
    A failed attempt can still be 1. Low effort is not cheating. Never accuse anyone.
    Write a reason of under 20 words in your own words about that turn's prompt.
  PROMPT

  def perform(run_id)
    # Claim atomically, so a retried or duplicated job does nothing.
    return unless StruggleRelabelRun.where(id: run_id, status: "queued").update_all(status: "running", updated_at: Time.current) == 1

    run = StruggleRelabelRun.find(run_id)
    config = StruggleRelabelRun::Config.from_env
    submission = run.struggle_submission
    raw = submission.bundle.download
    unless config.enabled? && Digest::SHA256.hexdigest(raw) == run.bundle_digest && !submission.escalated?
      return fail_run(run, "not sent: relabelling is off, the bundle changed or the submission was escalated")
    end

    payload = StruggleRelabelPayload.new(JSON.parse(raw), prompt_version: run.prompt_version)
    result = StruggleRelabelClient.new(url: config.url, model: run.model, api_key: config.api_key)
      .complete(system: SYSTEM_PROMPT, user: payload.to_json, max_tokens: config.max_output_tokens, schema: answer_schema(payload.turn_ids.keys))
    cost = config.cost_cents(result.input_tokens, result.output_tokens)
    proposals = parse_proposals(result.content, payload.turn_ids)
    return fail_run(run, "the model's answer did not label every turn exactly once", cost:) unless proposals

    run.update!(status: "succeeded", proposals:, cost_cents: cost, finished_at: Time.current)
  rescue StruggleRelabelClient::Error, StruggleRelabelPayload::Leak => e
    fail_run(run, e.message)
  end

  private

  # One entry per turn, in order, so a small model cannot skip or repeat one.
  # The answer is still checked in parse_proposals for servers that ignore it.
  def answer_schema(refs)
    turn = ->(ref) do
      { type: "object", required: %w[ref value reason],
        properties: { ref: { const: ref }, value: { enum: [ 0, 1 ] }, reason: { type: "string" } } }
    end
    { type: "object", required: [ "turns" ],
      properties: { turns: { type: "array", prefixItems: refs.map(&turn), minItems: refs.size, maxItems: refs.size } } }
  end

  # Maps the model's refs back to turn IDs, or nil when the answer does not
  # give exactly one 0/1 label per turn.
  def parse_proposals(content, turn_ids)
    turns = JSON.parse(content)["turns"]
    return nil unless turns.is_a?(Array)

    proposals = turns.map do |turn|
      return nil unless turn.is_a?(Hash) && turn_ids.key?(turn["ref"]) && [ 0, 1 ].include?(turn["value"])

      { "turn_id" => turn_ids[turn["ref"]], "value" => turn["value"], "reason" => turn["reason"].to_s.first(500) }
    end
    proposals.map { |p| p["turn_id"] }.sort == turn_ids.values.sort ? proposals : nil
  rescue JSON::ParserError
    nil
  end

  # The reservation stays counted when the cost is unknown.
  def fail_run(run, message, cost: nil)
    run.update!(status: "failed", error: message, cost_cents: cost, finished_at: Time.current)
  end
end
