# Lookback: review one day's sessions afterwards, move a time range to another
# project, and generate the justification block for program reviewers.
class My::LookbackController < InertiaController
  layout "inertia", only: [ :show ]

  before_action :ensure_current_user

  def show
    date = parse_date(params[:date]) || Date.current
    day = ProjectJustification.new(user: current_user, from: date, to: date)

    render inertia: "Lookback/Show", props: {
      page_title: "Lookback | Hackatime",
      date: date.iso8601,
      timezone: current_user.timezone,
      sessions: day.sessions.map { |session| session_props(session) },
      projects: current_user.heartbeats.distinct_values(:project).compact_blank,
      corrections: current_user.project_corrections.order(created_at: :desc).limit(20).map { |correction| correction_props(correction) },
      justification: justification_props(date),
      submissions: current_user.struggle_submissions.order(created_at: :desc).limit(20).map { |submission| submission_props(submission) }
    }
  end

  def create_correction
    correction = current_user.project_corrections.new(
      start_time: parse_local_time(params[:start_time]),
      # The form has minute precision, so "To" includes the whole chosen minute.
      end_time: parse_local_time(params[:end_time])&.+(60),
      from_project: params[:from_project],
      to_project: params[:to_project]
    )
    correction.apply!
    redirect_to my_lookback_path(date: params[:date]), notice: "Moved #{correction.heartbeats_count} heartbeats to #{correction.to_project}."
  rescue ActiveRecord::RecordInvalid
    redirect_to my_lookback_path(date: params[:date]), inertia: { errors: { correction: correction.errors.full_messages.to_sentence } }
  end

  private

  def ensure_current_user
    redirect_to root_path, alert: "You must be logged in to view this page" unless current_user
  end

  def justification_props(date)
    from = parse_date(params[:from]) || date - 6
    to = parse_date(params[:to]) || date
    projects = params[:projects].to_s.split(",").map(&:strip).compact_blank
    text = if params[:justify].present?
      ProjectJustification.new(user: current_user, from:, to:, projects:, base_url: request.base_url).to_s
    end
    { from: from.iso8601, to: to.iso8601, projects:, text: }
  end

  def session_props(session)
    {
      project: session.project,
      start_time: local_time(session.start_time),
      end_time: local_time(session.end_time),
      human_seconds: session.human_seconds.round,
      agent_seconds: session.agent_seconds.round,
      harness: session.harness,
      prompts: session.prompts
    }
  end

  # What the submitter sees: numbers, status and the explanation written for
  # them. Reviewers' internal justifications are not shown.
  def submission_props(submission)
    {
      id: submission.id,
      project: submission.project,
      range_start: local_time(submission.range_start.to_f),
      range_end: local_time(submission.range_end.to_f),
      status: submission.status,
      estimated_human_seconds: submission.estimated_human_seconds,
      proposed_productive_seconds: submission.proposed_productive_seconds,
      claimable_seconds: submission.claimable_seconds,
      approved_seconds: submission.decided? ? submission.approved_seconds : nil,
      user_explanation: submission.user_explanation,
      digest: submission.digest
    }
  end

  def correction_props(correction)
    {
      id: correction.id,
      start_time: local_time(correction.start_time),
      end_time: local_time(correction.end_time),
      from_project: correction.from_project,
      to_project: correction.to_project,
      heartbeats_count: correction.heartbeats_count
    }
  end

  # "YYYY-MM-DDTHH:MM" in the user's zone, the format of datetime-local inputs.
  def local_time(epoch) = Time.at(epoch).in_time_zone(current_user.timezone).strftime("%Y-%m-%dT%H:%M")

  def parse_local_time(value)
    Time.use_zone(current_user.timezone) { Time.zone.parse(value.to_s) }&.to_f
  rescue ArgumentError
    nil
  end

  def parse_date(value)
    Date.iso8601(value.to_s) if value.present?
  rescue Date::Error
    nil
  end
end
