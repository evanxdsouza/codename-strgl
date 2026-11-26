# A reviewer's request to have an external model re-check a submission's
# labels. The model's proposals are a second opinion shown beside the rule
# labels; they never change estimated, proposed, claimable or approved time.
#
# Privacy: only submissions awaiting ordinary review whose user consented in
# the frozen bundle are eligible. Escalated evidence is only for the fraud
# squad and SupaAdmins, so escalated submissions are never sent. The payload
# is a minimised copy of the redacted bundle (StruggleRelabelPayload), and
# each run is logged as an evidence read.
#
# Spend: a conservative estimate is reserved under an advisory lock before
# anything is sent, and the monthly total counts reservations until the
# actual cost replaces them, so concurrent requests cannot pass the cap.
# Both caps default to 0, which keeps the feature off.
class StruggleRelabelRun < ApplicationRecord
  class Refused < StandardError; end

  PROMPT_VERSION = "relabel-v0.1"
  STATUSES = %w[queued running succeeded failed].freeze
  # Instructions and a JSON frame added around the payload, in tokens.
  PROMPT_OVERHEAD_TOKENS = 1_500
  LOCK_KEY = 0x5354_524c # "STRL" in ASCII

  Config = Data.define(:provider, :model, :monthly_cap_cents, :run_cap_cents,
    :input_cents_per_mtok, :output_cents_per_mtok, :max_output_tokens) do
    def self.from_env(env = ENV)
      new(
        provider: env["STRGL_RELABEL_PROVIDER"].to_s, model: env["STRGL_RELABEL_MODEL"].to_s,
        monthly_cap_cents: env["STRGL_RELABEL_MONTHLY_CAP_CENTS"].to_i, run_cap_cents: env["STRGL_RELABEL_RUN_CAP_CENTS"].to_i,
        input_cents_per_mtok: env["STRGL_RELABEL_INPUT_CENTS_PER_MTOK"].to_i,
        output_cents_per_mtok: env["STRGL_RELABEL_OUTPUT_CENTS_PER_MTOK"].to_i,
        max_output_tokens: (env["STRGL_RELABEL_MAX_OUTPUT_TOKENS"].presence || 4_000).to_i
      )
    end

    def enabled? = provider.present? && model.present? && monthly_cap_cents.positive? && run_cap_cents.positive? &&
      input_cents_per_mtok.positive? && output_cents_per_mtok.positive?

    # Bytes over 3 is a generous token count for redacted English and code.
    def estimate_cents(payload_bytes)
      input_tokens = (payload_bytes / 3.0).ceil + PROMPT_OVERHEAD_TOKENS
      ((input_tokens * input_cents_per_mtok + max_output_tokens * output_cents_per_mtok) / 1_000_000.0).ceil
    end
  end

  belongs_to :struggle_submission
  belongs_to :requested_by, class_name: "User"

  validates :status, inclusion: { in: STATUSES }
  validates :reason, presence: true

  # Spend this calendar month (UTC): actual cost where known, otherwise the
  # reservation, so unfinished and failed runs are counted conservatively.
  def self.month_spend_cents(now = Time.current)
    where(created_at: now.utc.beginning_of_month..).sum("COALESCE(cost_cents, reserved_cents)")
  end

  # Records an eligible, affordable run, or raises Refused and records
  # nothing. Sending it is a separate step.
  def self.request!(submission:, requester:, reason:, config: Config.from_env)
    raise Refused, "model relabelling is not configured" unless config.enabled?
    raise Refused, "a reason is required" if reason.to_s.strip.empty?
    # Not found rather than refused for people who cannot see the submission.
    raise ActiveRecord::RecordNotFound unless submission.evidence_access_for(requester)
    raise Refused, "escalated submissions are never sent to a model" if submission.escalated?
    raise Refused, "only submissions awaiting review can be re-checked" unless submission.status == "submitted"

    data = JSON.parse(submission.bundle.download)
    raise Refused, "the user did not allow a model re-check of this submission" unless data["model_relabel_consent"] == true

    payload = StruggleRelabelPayload.new(data, prompt_version: PROMPT_VERSION)
    begin
      bytes = payload.to_json.bytesize
    rescue StruggleRelabelPayload::Leak => e
      raise Refused, "#{e.message}; nothing was sent"
    end
    estimate = config.estimate_cents(bytes)
    raise Refused, "this run could cost up to #{estimate}¢, over the #{config.run_cap_cents}¢ per-run cap" if estimate > config.run_cap_cents

    transaction do
      connection.execute("SELECT pg_advisory_xact_lock(#{LOCK_KEY})")
      spent = month_spend_cents
      if spent + estimate > config.monthly_cap_cents
        raise Refused, "this month's model budget is used up (#{spent}¢ of #{config.monthly_cap_cents}¢); request relabelling from the user instead"
      end

      submission.evidence_reads.create!(user: requester, scope: "model_relabel", access: "review")
      create!(
        struggle_submission: submission, requested_by: requester, reason: reason.strip,
        provider: config.provider, model: config.model, prompt_version: PROMPT_VERSION,
        bundle_digest: submission.digest, payload_bytes: bytes, reserved_cents: estimate
      )
    end
  end
end
