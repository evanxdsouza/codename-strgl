# Endpoints strgl-collector calls with the user's API key: chain-head anchors
# (hashes only) and redacted submission bundles the user previewed locally.
class Api::V1::StruggleController < ApplicationController
  skip_before_action :verify_authenticity_token
  before_action :ensure_authenticated!

  def create_anchor
    anchor = @current_user.struggle_anchors.find_or_initialize_by(device_id: params[:device_id].to_s, seq: params[:seq])
    if anchor.persisted?
      # Idempotent for a retry of the same head; a different head for the
      # same sequence number is a discrepancy worth keeping visible.
      return render json: { id: anchor.id, received_at: anchor.created_at.iso8601 }, status: :ok if anchor.head == params[:head]

      return render json: { error: "a different head was already anchored for this sequence number" }, status: :conflict
    end

    anchor.head = params[:head].to_s
    if anchor.save
      render json: { id: anchor.id, received_at: anchor.created_at.iso8601 }, status: :created
    else
      render json: { error: anchor.errors.full_messages.to_sentence }, status: :unprocessable_entity
    end
  end

  def create_submission
    submission, created = StruggleSubmission.receive!(
      user: @current_user, raw: request.raw_post, claimed_digest: request.headers["X-Bundle-Digest"]
    )
    render json: submission_json(submission), status: created ? :created : :ok
  rescue StruggleBundle::Invalid => e
    render json: { error: e.message }, status: :unprocessable_entity
  end

  private

  def ensure_authenticated!
    @current_user = api_user_from_credentials(api_key_sources: %i[bearer basic])
    render_unauthorized unless @current_user
  end

  def submission_json(submission)
    {
      id: submission.id, digest: submission.digest, status: submission.status, project: submission.project,
      estimated_human_seconds: submission.estimated_human_seconds,
      proposed_productive_seconds: submission.proposed_productive_seconds,
      claimable_seconds: submission.claimable_seconds, duplicate_seconds: submission.duplicate_seconds,
      chain_status: submission.chain_status, supersedes_digest: submission.supersedes&.digest
    }
  end
end
