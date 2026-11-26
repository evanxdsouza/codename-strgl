# A frozen struggle tracker submission: one redacted bundle for a project and
# time range, as the user previewed and uploaded it, and its review state.
#
# Three numbers stay separate: estimated_human_seconds (the 2-minute activity
# estimate), proposed_productive_seconds (turns the rules labelled 1) and
# approved_seconds (what a reviewer decided). claimable_seconds is the
# proposal minus seconds an earlier live submission already claimed.
class StruggleSubmission < ApplicationRecord
  STATUSES = %w[submitted approved adjusted rejected relabel_requested escalated superseded].freeze
  # Actions, the states they apply to and the permission they need.
  ACTIONS = {
    "approve" => { to: "approved", escalated_ok: true },
    "adjust" => { to: "adjusted", escalated_ok: true },
    "reject" => { to: "rejected", escalated_ok: true },
    "request_relabel" => { to: "relabel_requested", escalated_ok: true },
    "escalate" => { to: "escalated", escalated_ok: false },
    "return_not_fraud" => { to: "submitted", escalated_only: true }
  }.freeze

  belongs_to :user
  belongs_to :supersedes, class_name: "StruggleSubmission", optional: true
  belongs_to :decided_by, class_name: "User", optional: true
  has_many :turn_claims, class_name: "StruggleTurnClaim", dependent: :destroy
  has_many :review_decisions, -> { order(:created_at, :id) }, class_name: "StruggleReviewDecision", dependent: :destroy
  has_many :evidence_reads, class_name: "StruggleEvidenceRead", dependent: :destroy
  has_many :relabel_runs, class_name: "StruggleRelabelRun", dependent: :destroy
  has_one_attached :bundle

  validates :status, inclusion: { in: STATUSES }
  validates :project, :digest, :rule_set, presence: true

  scope :live, -> { where.not(status: "superseded") }
  scope :awaiting_review, -> { where(status: "submitted") }

  def escalated? = status == "escalated"
  def decided? = status.in?(%w[approved adjusted rejected])

  # Stores a bundle. Retrying the same bytes returns the existing submission
  # (created is false). A bundle naming an earlier submission in
  # `supersedes` replaces it and releases its turns.
  def self.receive!(user:, raw:, claimed_digest: nil)
    bundle = StruggleBundle.new(raw)
    if claimed_digest.present? && claimed_digest != bundle.digest
      raise StruggleBundle::Invalid, "the bundle's SHA-256 does not match X-Bundle-Digest; it changed in transit"
    end

    existing = user.struggle_submissions.find_by(digest: bundle.digest)
    return [ existing, false ] if existing

    submission = transaction do
      user.lock! # one submission at a time per user, so claims cannot race
      existing = user.struggle_submissions.find_by(digest: bundle.digest)
      next existing if existing

      superseded = supersede!(user, bundle.supersedes) if bundle.supersedes
      status, matched = bundle.chain_status(user.struggle_anchors.where(device_id: bundle.device_id, seq: bundle.chain_seq_range || []).to_a)
      submission = user.struggle_submissions.create!(
        project: bundle.project, range_start: bundle.range_start, range_end: bundle.range_end, digest: bundle.digest,
        rule_set: bundle.data["rule_set"], redaction_version: bundle.data["redaction_version"],
        collector_version: bundle.data["collector_version"], device_id: bundle.device_id,
        estimated_human_seconds: bundle.totals["estimated_human_seconds"], agent_seconds: bundle.totals["agent_seconds"],
        proposed_productive_seconds: bundle.totals["proposed_productive_seconds"],
        turns_count: bundle.turns.size, labelled_one_count: bundle.turns.count { |turn| turn.label_value == 1 },
        needs_review_count: bundle.turns.count(&:needs_review), backfilled_count: bundle.turns.count(&:backfilled),
        chain_status: status, chain_anchors_matched: matched, user_context: bundle.data["user_context"].presence,
        supersedes: superseded
      )
      submission.claim_turns!(bundle)
      submission.bundle.attach(io: StringIO.new(raw), filename: "struggle-#{bundle.digest}.json", content_type: "application/json")
      submission
    end
    [ submission, true ]
  end

  def self.supersede!(user, digest)
    previous = user.struggle_submissions.find_by(digest:)
    raise StruggleBundle::Invalid, "supersedes names no submission of yours" unless previous
    unless previous.status.in?(%w[submitted relabel_requested])
      raise StruggleBundle::Invalid, "only a submission awaiting review or returned for relabelling can be replaced"
    end

    previous.update!(status: "superseded")
    previous.turn_claims.where(released_at: nil).update_all(released_at: Time.current)
    previous
  end

  # Claims each turn of the bundle. A turn already claimed by a live
  # submission is a duplicate and earns nothing here; seconds that overlap
  # another live claim in time (another device or session) are not claimable
  # either.
  def claim_turns!(bundle)
    ids = bundle.turns.map(&:id)
    taken = StruggleTurnClaim.where(user_id:, turn_id: ids, released_at: nil).pluck(:turn_id).to_set
    starts = bundle.turns.map(&:interval_start_ms)
    others = StruggleTurnClaim.where(user_id:, released_at: nil).where("claimed_seconds > 0")
      .where("interval_end_ms > ? AND interval_start_ms < ?", starts.min, bundle.turns.map(&:interval_end_ms).max)
      .pluck(:interval_start_ms, :interval_end_ms)

    duplicate = 0
    claimable_intervals = []
    now = Time.current
    rows = bundle.turns.filter_map do |turn|
      if taken.include?(turn.id)
        duplicate += turn.productive_seconds
        next
      end
      claimable_intervals << [ turn.interval_start_ms, turn.interval_end_ms ] if turn.label_value == 1 && turn.productive_seconds.positive?
      {
        struggle_submission_id: id, user_id:, turn_id: turn.id, interval_start_ms: turn.interval_start_ms,
        interval_end_ms: turn.interval_end_ms, claimed_seconds: turn.productive_seconds, created_at: now, updated_at: now
      }
    end
    StruggleTurnClaim.insert_all!(rows) if rows.any?

    own = StruggleBundle.union_seconds(claimable_intervals)
    overlap = own + StruggleBundle.union_seconds(others) - StruggleBundle.union_seconds(claimable_intervals + others)
    claimable = [ [ own - overlap, proposed_productive_seconds - duplicate ].min, 0 ].max
    update!(claimable_seconds: claimable, duplicate_seconds: proposed_productive_seconds - claimable)
  end

  # Who may read this submission's evidence, and under which permission.
  def evidence_access_for(viewer)
    return nil unless viewer && viewer != user
    return (viewer.can_view_escalated_struggle? ? "escalated" : nil) if escalated?

    viewer.can_review_struggle? ? "review" : nil
  end

  def allowed_actions_for(viewer)
    access = evidence_access_for(viewer)
    return [] if access.nil? || status == "superseded"

    ACTIONS.select do |_, rule|
      if escalated?
        rule[:escalated_ok] || rule[:escalated_only]
      else
        !rule[:escalated_only]
      end
    end.keys
  end

  # Records a review action and moves the submission to its new state.
  def decide!(reviewer:, action:, justification:, approved_seconds: nil, user_explanation: nil)
    action = action.to_s
    unless allowed_actions_for(reviewer).include?(action)
      errors.add(:base, "You cannot #{action.humanize(capitalize: false)} this submission")
      raise ActiveRecord::RecordInvalid, self
    end
    if justification.to_s.strip.empty?
      errors.add(:base, "A justification is required")
      raise ActiveRecord::RecordInvalid, self
    end

    seconds = case action
    when "approve" then claimable_seconds
    when "adjust" then Integer(approved_seconds.to_s, exception: false)
    when "reject" then 0
    end
    if action == "adjust" && (seconds.nil? || seconds.negative? || seconds > (range_end - range_start).to_i)
      errors.add(:approved_seconds, "must be between 0 and the length of the range")
      raise ActiveRecord::RecordInvalid, self
    end

    transaction do
      before = status
      after = ACTIONS.fetch(action)[:to]
      attrs = { status: after }
      attrs[:escalated_at] = Time.current if action == "escalate"
      if seconds
        attrs.merge!(approved_seconds: seconds, decided_at: Time.current, decided_by: reviewer,
          reviewer_justification: justification.strip, user_explanation: user_explanation.presence)
      elsif action == "request_relabel"
        attrs[:user_explanation] = user_explanation.presence
      end
      update!(attrs)
      review_decisions.create!(
        reviewer:, action:, status_before: before, status_after: after, approved_seconds: seconds,
        justification: justification.strip, user_explanation: user_explanation.presence, bundle_digest: digest, rule_set:
      )
    end
  end

  # Records a read of the evidence and returns the parsed bundle.
  def read_evidence!(viewer)
    access = evidence_access_for(viewer)
    raise ActiveRecord::RecordNotFound unless access

    evidence_reads.create!(user: viewer, scope: escalated? ? "escalated" : "review", access:)
    JSON.parse(bundle.download)
  end
end
