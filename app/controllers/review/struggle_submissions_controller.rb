# Review queue for struggle tracker submissions. Reviewers see submissions
# awaiting review; escalated submissions and their evidence are only for the
# fraud squad and SupaAdmins. Every evidence view and download is logged, and
# authorisation is checked on each action, not only in navigation.
class Review::StruggleSubmissionsController < InertiaController
  layout "inertia", only: [ :index, :show ]

  before_action :require_reviewer
  before_action :set_submission, only: [ :show, :bundle, :decide, :request_model_relabel ]

  def index
    scope = StruggleSubmission.live.where.not(user_id: current_user.id).includes(:user).order(created_at: :desc)
    scope = scope.where.not(status: "escalated") unless current_user.can_view_escalated_struggle?
    status = params[:status].presence_in(StruggleSubmission::STATUSES)
    scope = scope.where(status:) if status

    render inertia: "Review/StruggleSubmissions/Index", props: {
      page_title: "Struggle review | Hackatime",
      status: status,
      statuses: StruggleSubmission::STATUSES - [ "superseded" ],
      can_view_escalated: current_user.can_view_escalated_struggle?,
      submissions: scope.limit(100).map { |submission| summary(submission) }
    }
  end

  def show
    bundle = @submission.read_evidence!(current_user)
    render inertia: "Review/StruggleSubmissions/Show", props: {
      page_title: "Struggle review | Hackatime",
      submission: summary(@submission).merge(
        user_context: @submission.user_context,
        reviewer_justification: @submission.reviewer_justification,
        user_explanation: @submission.user_explanation,
        supersedes_id: @submission.supersedes_id,
        rule_set: @submission.rule_set,
        redaction_version: @submission.redaction_version,
        collector_version: @submission.collector_version,
        chain_anchors_matched: @submission.chain_anchors_matched,
        duplicate_seconds: @submission.duplicate_seconds,
        agent_seconds: @submission.agent_seconds,
        digest: @submission.digest
      ),
      turns: bundle["turns"],
      notes: bundle["notes"],
      justification: ProjectJustification.new(
        user: @submission.user, projects: [ @submission.project ], base_url: request.base_url, reviewer_view: true,
        from: @submission.range_start.in_time_zone(@submission.user.timezone).to_date,
        to: (@submission.range_end - 1).in_time_zone(@submission.user.timezone).to_date
      ).to_s,
      actions: @submission.allowed_actions_for(current_user),
      model_relabel: model_relabel_props(bundle),
      decisions: @submission.review_decisions.includes(:reviewer).map { |decision| decision_props(decision) },
      evidence_reads: @submission.evidence_reads.includes(:user).order(created_at: :desc).limit(20).map do |read|
        { user: read.user.display_name, scope: read.scope, at: read.created_at.iso8601 }
      end
    }
  end

  def bundle
    @submission.read_evidence!(current_user)
    send_data @submission.bundle.download, filename: "struggle-#{@submission.digest}.json", type: "application/json", disposition: "attachment"
  end

  def decide
    @submission.decide!(
      reviewer: current_user, action: params[:decision_action], justification: params[:justification],
      approved_seconds: params[:approved_hours].presence && (params[:approved_hours].to_f * 3600).round,
      user_explanation: params[:user_explanation]
    )
    notice = "Recorded: #{params[:decision_action].to_s.humanize}."
    # After escalating, an ordinary reviewer can no longer open it.
    return redirect_to review_struggle_submissions_path, notice: notice unless @submission.evidence_access_for(current_user)

    redirect_to review_struggle_submission_path(@submission), notice: notice
  rescue ActiveRecord::RecordInvalid
    redirect_to review_struggle_submission_path(@submission), inertia: { errors: { decision: @submission.errors.full_messages.to_sentence } }
  end

  # Records a model re-check and queues it. It changes no hours.
  def request_model_relabel
    StruggleRelabelRun.request!(submission: @submission, requester: current_user, reason: params[:reason])
    redirect_to review_struggle_submission_path(@submission), notice: "Model re-check queued. It will not change any hours."
  rescue StruggleRelabelRun::Refused => e
    redirect_to review_struggle_submission_path(@submission), inertia: { errors: { relabel: e.message } }
  end

  private

  def model_relabel_props(bundle)
    runs = @submission.relabel_runs.includes(:requested_by).order(created_at: :desc).limit(10)
    latest = runs.find { |run| run.status == "succeeded" }
    {
      available: StruggleRelabelRun::Config.from_env.enabled? && @submission.status == "submitted" &&
        bundle["model_relabel_consent"] == true,
      consent: bundle["model_relabel_consent"] == true,
      proposals: latest&.proposals_by_turn || {},
      runs: runs.map do |run|
        {
          id: run.id, status: run.status, model: run.model, prompt_version: run.prompt_version,
          requested_by: run.requested_by.display_name, reason: run.reason, error: run.error,
          cost_cents: run.cost_cents, at: run.created_at.iso8601
        }
      end
    }
  end

  def require_reviewer
    redirect_to root_path, alert: "You are not authorized to access this page." unless current_user&.can_review_struggle?
  end

  # Not found, rather than forbidden, so escalation is not revealed.
  def set_submission
    @submission = StruggleSubmission.live.find(params[:id])
    raise ActiveRecord::RecordNotFound unless @submission.evidence_access_for(current_user)
  end

  def summary(submission)
    {
      id: submission.id,
      user: submission.user.display_name,
      project: submission.project,
      range_start: submission.range_start.iso8601,
      range_end: submission.range_end.iso8601,
      status: submission.status,
      estimated_human_seconds: submission.estimated_human_seconds,
      proposed_productive_seconds: submission.proposed_productive_seconds,
      claimable_seconds: submission.claimable_seconds,
      approved_seconds: submission.approved_seconds,
      turns_count: submission.turns_count,
      labelled_one_count: submission.labelled_one_count,
      needs_review_count: submission.needs_review_count,
      backfilled_count: submission.backfilled_count,
      chain_status: submission.chain_status,
      submitted_at: submission.created_at.iso8601
    }
  end

  def decision_props(decision)
    {
      id: decision.id, reviewer: decision.reviewer.display_name, action: decision.action,
      status_before: decision.status_before, status_after: decision.status_after,
      approved_seconds: decision.approved_seconds, justification: decision.justification,
      user_explanation: decision.user_explanation, rule_set: decision.rule_set,
      bundle_digest: decision.bundle_digest, at: decision.created_at.iso8601
    }
  end
end
