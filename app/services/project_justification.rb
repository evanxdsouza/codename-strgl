# Builds the plain-text "Override Hours Spent Justification" block a program
# reviewer pastes into the YSWS Airtable flow, for some of a user's projects
# over a date range. Every number comes from heartbeats, corrections, commits
# and the user row, using the same per-project gap rule as
# Heartbeat.group(:project).duration_seconds: each heartbeat adds the gap since
# the previous heartbeat of its project, capped at the heartbeat timeout.
# Human time runs that rule over human-credited heartbeats (human ones, plus
# agent ones up to 2 minutes after the latest human one, per project);
# agent-running time is the rest, so human + agent = raw for every session,
# project and total.
class ProjectJustification
  RULE_SET = "v0".freeze
  REPO_URL = "https://github.com/evanxdsouza/codename-strgl".freeze
  COLLECTOR_PRODUCT = %r{strgl-collector/(\S+)}

  Session = Data.define(:project, :start_time, :end_time, :human_seconds, :agent_seconds, :harness, :prompts) do
    def raw_seconds = human_seconds + agent_seconds
  end

  attr_reader :user, :from, :to, :projects

  # from and to are dates in the user's time zone, both inclusive. projects
  # nil means every project with activity in the range.
  # reviewer_view includes reviewers' internal justifications, which the
  # user's own export leaves out.
  def initialize(user:, from:, to:, projects: nil, base_url: nil, reviewer_view: false)
    @user, @from, @to, @base_url, @reviewer_view = user, from, to, base_url, reviewer_view
    @projects = projects.presence
  end

  # Live struggle tracker submissions overlapping the range, oldest first.
  def submissions
    @submissions ||= begin
      scope = user.struggle_submissions.live.includes(:decided_by)
        .where("range_start < ? AND range_end > ?", Time.at(time_range.end), Time.at(time_range.begin)).order(:range_start, :id)
      scope = scope.where(project: projects) if projects
      scope.to_a
    end
  end

  def time_range
    @time_range ||= Time.use_zone(user.timezone) { from.beginning_of_day.to_f...(to + 1).beginning_of_day.to_f }
  end

  def sessions
    @sessions ||= begin
      timeout = Heartbeat.heartbeat_timeout_duration.to_i
      rows.group_by { |row| row[:project] }.flat_map do |project, beats|
        last_human_time = last_credited_time = nil
        beats.slice_before { |beat| beat[:previous_time].nil? || beat[:time] - beat[:previous_time] > timeout }.map do |session_beats|
          human = raw = 0.0
          session_beats.each do |beat|
            raw += beat[:previous_time] ? [ beat[:time] - beat[:previous_time], timeout ].min : 0
            human_beat = beat[:actor] != "agent"
            # Heartbeat.human_credited, within one project: human heartbeats and
            # agent heartbeats up to one timeout after the latest human one.
            next unless human_beat || (last_human_time && beat[:time] - last_human_time <= timeout)

            human += [ beat[:time] - last_credited_time, timeout ].min if last_credited_time
            last_credited_time = beat[:time]
            last_human_time = beat[:time] if human_beat
          end
          Session.new(
            project:, start_time: session_beats.first[:time], end_time: session_beats.last[:time],
            human_seconds: human, agent_seconds: raw - human,
            harness: harness_for(session_beats),
            prompts: session_beats.count { |beat| beat[:actor] != "agent" && beat[:ai_prompt_length] }
          )
        end
      end.sort_by(&:start_time)
    end
  end

  # Whole seconds, rounded like the SQL duration queries. Agent time is the
  # remainder, so the three always add up.
  def human_seconds = sessions.sum(&:human_seconds).round
  def raw_seconds = sessions.sum(&:raw_seconds).round
  def agent_seconds = raw_seconds - human_seconds

  def to_s
    [
      struggle_tracker_section,
      "[ALTERNATE TRACKING METHOD]\nHours come from the open source tracker codename-strgl (#{REPO_URL}), a fork of Hackatime, not from Hackatime itself. " \
        "Its collector reads Claude Code and Codex transcripts and OpenCode plugin events on the user's machine, attributes activity to a project by git root and working directory, " \
        "and marks each heartbeat as human or agent. Agent-running time is counted separately. Prompt text never leaves the user's machine.",
      "[SPECIFIC TECHNICAL FEATURES]\n(specific technical features)",
      "[PROJECT REVIEW]\n(reviewer justification)",
      deflation_section,
      "[UPDATE JUSTIFICATION]\n(only for updates: hours since the previous submission, link to the earlier one)",
      sessions_section,
      struggle_review_section,
      ai_usage_section,
      commits_section,
      corrections_section,
      user_history_section,
      "[REVIEW INFO]\nReviewed by (reviewer) at (time). Rule set #{RULE_SET}.",
      links_section
    ].join("\n\n") + "\n"
  end

  private

  def rows
    @rows ||= begin
      scope = user.heartbeats_excluding_archived_projects(include_agent: true)
        .with_valid_timestamps.where(time: time_range).where.not(project: [ nil, "" ])
      scope = scope.where(project: projects) if projects
      previous = {}
      scope.order(:project, :time, :id)
        .pluck(:project, :time, :actor, :editor, :user_agent, :operating_system, :ai_model, :ai_prompt_length)
        .map do |project, time, actor, editor, user_agent, operating_system, ai_model, ai_prompt_length|
          row = { project:, time:, previous_time: previous[project], actor:, editor:, user_agent:, operating_system:, ai_model:, ai_prompt_length: }
          previous[project] = time
          row
        end
    end
  end

  def harness_for(beats)
    agent_beats = beats.select { |beat| beat[:actor] == "agent" }
    (agent_beats.presence || beats).filter_map { |beat| beat[:editor].presence }.tally.max_by(&:last)&.first || "unknown"
  end

  def struggle_tracker_section
    return "[STRUGGLE TRACKER]\nAccount: #{account_label}\nNo tracked activity for the selected projects between #{date(from)} and #{date(to)}." if sessions.empty?

    project_ranges = sessions.group_by(&:project).map do |project, project_sessions|
      "#{project} #{date(local_date(project_sessions.first.start_time))}-#{date(local_date(project_sessions.last.end_time))}"
    end
    agent_share = raw_seconds.zero? ? 0 : (agent_seconds * 100.0 / raw_seconds).round
    harnesses = sessions.group_by(&:harness).map { |name, list| "#{name} #{hours(list.sum(&:raw_seconds))}" }.join(", ")

    <<~TEXT.chomp
      [STRUGGLE TRACKER]
      Account: #{account_label}
      Projects: #{project_ranges.join(", ")}
      Raw tracked time (before review): #{hours(raw_seconds)} = #{hours(human_seconds)} human + #{hours(agent_seconds)} agent running (#{agent_share}% agent)
      Exact seconds: #{raw_seconds} = #{human_seconds} human + #{agent_seconds} agent running
      Human time credited by rule set #{RULE_SET}: #{hours(human_seconds)}. Agent-running time is reported separately and not added.
      #{collector_line}. Tracked hours by harness or editor: #{harnesses}.
    TEXT
  end

  def collector_line
    versions = rows.filter_map { |row| row[:user_agent].to_s[COLLECTOR_PRODUCT, 1] }.uniq
    return "Collector: not used (editor plugins only)" if versions.empty?

    systems = rows.select { |row| row[:user_agent].to_s.match?(COLLECTOR_PRODUCT) }.filter_map { |row| row[:operating_system].presence }.uniq
    "Collector: #{versions.map { |v| "v#{v.delete_prefix("v")}" }.join(", ")}#{" (#{systems.join(", ")})" if systems.any?}"
  end

  def sessions_section
    lines = sessions.each_with_index.map do |session, index|
      "[#{index + 1}] #{date(local_date(session.start_time))} #{session.project}: #{hours(session.human_seconds)} human, " \
        "#{hours(session.agent_seconds)} agent, #{session.harness}, #{session.prompts} prompts"
    end
    "[SESSIONS]\nCredited #{hours(human_seconds)} human across #{sessions.length} sessions.\n#{lines.join("\n")}".chomp
  end

  def ai_usage_section
    prompts = sessions.sum(&:prompts)
    agent_only = sessions.count { |session| session.human_seconds.zero? && session.agent_seconds.positive? }
    models = rows.select { |row| row[:actor] == "agent" }.filter_map { |row| row[:ai_model].presence }.uniq.sort
    <<~TEXT.chomp
      [AI USAGE]
      #{prompts} prompts across #{sessions.length} sessions. Sessions with agent time and no human time: #{agent_only} (counted as agent time only).
      Models: #{models.presence&.join(", ") || "none reported"}.
      #{turn_label_line}
    TEXT
  end

  def commits_section
    mappings = user.project_repo_mappings.where(project_name: sessions.map(&:project).uniq).where.not(repository_id: nil)
    unlinked = sessions.map(&:project).uniq - mappings.map(&:project_name)
    count = Commit.where(user:, repository_id: mappings.map(&:repository_id), created_at: Time.at(time_range.begin)...Time.at(time_range.end)).count
    lines = [ "[COMMITS]", "#{count} commits in the credited range (read from GitHub, public repositories only)." ]
    lines << "No repository linked: #{unlinked.join(", ")}." if unlinked.any?
    lines.join("\n")
  end

  def corrections_section
    corrections = user.project_corrections.where("start_time < ? AND end_time > ?", time_range.end, time_range.begin).order(:start_time)
    if projects
      corrections = corrections.where(to_project: projects).or(corrections.where(from_project: projects)).or(corrections.where(from_project: nil))
    end
    return "[CORRECTIONS]\nNone. Every hour is attributed as the collector or editor plugin reported it." if corrections.empty?

    lines = corrections.each_with_index.map do |correction, index|
      "[#{index + 1}] #{time_label(correction.start_time)}-#{Time.at(correction.end_time).in_time_zone(user.timezone).strftime("%H:%M")} " \
        "#{correction.from_project || "any project"} -> #{correction.to_project}, #{correction.heartbeats_count} heartbeats, made #{date(correction.created_at.in_time_zone(user.timezone).to_date)}"
    end
    "[CORRECTIONS]\nUser reassigned #{corrections.size} time ranges between projects. Original heartbeats are kept (soft-deleted) and the corrected copies are counted above.\n#{lines.join("\n")}"
  end

  def user_history_section
    all_activity = user.heartbeats_excluding_archived_projects(include_agent: true)
    first_time = all_activity.with_valid_timestamps.minimum(:time)
    human = Heartbeat.duration_seconds(all_activity.human_credited)
    agent = Heartbeat.agent_running_seconds(all_activity)
    project_count = all_activity.where.not(project: [ nil, "" ]).distinct.count(:project)
    since = first_time ? date(local_date(first_time)) : "never"
    <<~TEXT.chomp
      [USER HISTORY]
      Tracking since #{since}. All-time: #{hours(human)} human + #{hours(agent)} agent running across #{project_count} projects.
      Trust level: #{user.public_trust_level}. Earlier approvals are not recorded by the tracker.
    TEXT
  end

  def links_section
    mappings = user.project_repo_mappings.where(project_name: sessions.map(&:project).uniq).index_by(&:project_name)
    lines = sessions.map(&:project).uniq.map do |project|
      mapping = mappings[project]
      page = if mapping&.public_shared_at && user.username.present? && @base_url
        "#{@base_url}/@#{user.username}/project/#{ERB::Util.url_encode(project)}"
      else
        "(project page not shared)"
      end
      "#{project}: #{page}#{" Repo: #{mapping.repo_url}" if mapping&.repo_url.present?}"
    end
    "[LINKS]\n#{lines.join("\n")}\n#{evidence_links}".sub("[LINKS]\n\n", "[LINKS]\n")
  end

  def turn_label_line
    return "Per-turn labels are not available in rule set #{RULE_SET}." if submissions.empty?

    turns = submissions.sum(&:turns_count)
    "Per-turn labels from submitted evidence (#{submissions.map(&:rule_set).uniq.join(", ")}): #{submissions.sum(&:labelled_one_count)} of #{turns} turns " \
      "labelled productive, #{submissions.sum(&:needs_review_count)} flagged for review, #{submissions.sum(&:backfilled_count)} backfilled."
  end

  def struggle_review_section
    return "[STRUGGLE REVIEW]\nNo evidence bundles were submitted for this range; the hours above are activity estimates only." if submissions.empty?

    lines = submissions.each_with_index.map do |submission, index|
      decision = case submission.status
      when "approved", "adjusted", "rejected"
        "#{submission.status} #{hours(submission.approved_seconds)} by #{reviewer_label(submission.decided_by)} at #{time_label(submission.decided_at.to_f)}"
      else
        submission.status.tr("_", " ")
      end
      "[#{index + 1}] #{submission.project} #{time_label(submission.range_start.to_f)}-#{time_label(submission.range_end.to_f)}: " \
        "estimated #{hours(submission.estimated_human_seconds)} human activity, proposed #{hours(submission.proposed_productive_seconds)} " \
        "(#{submission.labelled_one_count} of #{submission.turns_count} turns labelled 1, rule set #{submission.rule_set}), " \
        "claimable #{hours(submission.claimable_seconds)} after #{hours(submission.duplicate_seconds)} already credited, decision: #{decision}. " \
        "Evidence chain #{submission.chain_status} (#{submission.chain_anchors_matched} anchors). Bundle sha256 #{submission.digest}."
    end
    approved = submissions.select(&:decided?).sum(&:approved_seconds)
    <<~TEXT.chomp
      [STRUGGLE REVIEW]
      Estimated activity, proposed productive time and reviewer-approved time are separate numbers. Only approved hours are settled.
      #{lines.join("\n")}
      Approved by tracker reviewers: #{hours(approved)}. Awaiting a decision: #{submissions.count { |s| !s.decided? }}.
    TEXT
  end

  def deflation_section
    cut = submissions.select { |s| s.decided? && s.approved_seconds < s.claimable_seconds }
    return "[DEFLATION JUSTIFICATION]\n(only if hours were cut: hours deflated to, and why)" if cut.empty?

    lines = cut.map do |submission|
      reason = @reviewer_view ? submission.reviewer_justification : "the reviewer's reasons are in the linked review record"
      "#{submission.project}: #{hours(submission.claimable_seconds)} claimable deflated to #{hours(submission.approved_seconds)}; #{reason}"
    end
    "[DEFLATION JUSTIFICATION]\n#{lines.join("\n")}"
  end

  def evidence_links
    return "Evidence: not collected in rule set #{RULE_SET}." if submissions.empty?

    links = submissions.map do |submission|
      path = Rails.application.routes.url_helpers.review_struggle_submission_path(submission)
      "#{submission.project}: #{@base_url}#{path} (authorised reviewers only; every view is logged; sha256 #{submission.digest.first(16)})"
    end
    "Evidence:\n#{links.join("\n")}"
  end

  def reviewer_label(reviewer) = reviewer&.username.present? ? "@#{reviewer.username}" : "reviewer #{reviewer&.id}"

  def account_label = user.username.present? ? "@#{user.username} (tracker user ID #{user.id})" : "tracker user ID #{user.id}"
  def local_date(epoch) = Time.at(epoch).in_time_zone(user.timezone).to_date
  def date(value) = value.strftime("%m/%d/%Y")
  def time_label(epoch) = Time.at(epoch).in_time_zone(user.timezone).strftime("%m/%d/%Y %H:%M")
  def hours(seconds) = format("%.2fh", seconds / 3600.0)
end
