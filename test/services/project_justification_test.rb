require "test_helper"

class ProjectJustificationTest < ActiveSupport::TestCase
  T0 = Time.utc(2026, 10, 19, 10).to_f
  DAY = Date.new(2026, 10, 19)

  setup do
    @user = create(:user, timezone: "UTC")
    # A ten minute agent run on "terminet" with heartbeats every minute. The
    # person only prompts at the start and checks once 30 seconds in.
    (0..600).step(60) { |s| beat("terminet", s, actor: "agent", editor: "claude-code") }
    beat("terminet", 1, actor: "human", editor: "claude-code", ai_prompt_length: 42)
    beat("terminet", 30)
    # Later, a human-only session on another project, with no actor (editor plugin).
    (3600..3900).step(30) { |s| beat("tnt-browser", s, editor: "vscode") }
  end

  teardown { Flipper.disable(User::HUMAN_AGENT_SPLIT_FLAG) }

  test "human and agent time are separate and add up to the raw per-project duration" do
    report = ProjectJustification.new(user: @user, from: DAY, to: DAY)

    raw_by_project = Heartbeat.where(user: @user).group(:project).duration_seconds
    assert_equal raw_by_project.values.sum, report.raw_seconds
    assert_equal 600 + 300, report.raw_seconds
    # Human: 1 -> 30 on terminet, plus the 300 seconds of tnt-browser.
    assert_equal 29 + 300, report.human_seconds
    assert_equal 571, report.agent_seconds

    terminet, browser = report.sessions
    assert_equal [ "terminet", 29, 571, "claude-code", 1 ],
      [ terminet.project, terminet.human_seconds.round, terminet.agent_seconds.round, terminet.harness, terminet.prompts ]
    assert_equal [ "tnt-browser", 300, 0 ], [ browser.project, browser.human_seconds.round, browser.agent_seconds.round ]
  end

  test "the block is plain text with every section and the split numbers" do
    text = ProjectJustification.new(user: @user, from: DAY, to: DAY).to_s

    %w[STRUGGLE\ TRACKER ALTERNATE\ TRACKING\ METHOD SPECIFIC\ TECHNICAL\ FEATURES PROJECT\ REVIEW
       DEFLATION\ JUSTIFICATION UPDATE\ JUSTIFICATION SESSIONS AI\ USAGE COMMITS CORRECTIONS
       USER\ HISTORY REVIEW\ INFO LINKS].each { |section| assert_includes text, "[#{section}]" }
    assert_includes text, "Projects: terminet 10/19/2026-10/19/2026, tnt-browser 10/19/2026-10/19/2026"
    assert_includes text, "Exact seconds: 900 = 329 human + 571 agent running"
    assert_includes text, "[1] 10/19/2026 terminet: 0.01h human, 0.16h agent, claude-code, 1 prompts"
    assert_includes text, "(reviewer justification)"
    assert_no_match(/[–—]/, text)
  end

  test "a project filter limits every number to that project" do
    report = ProjectJustification.new(user: @user, from: DAY, to: DAY, projects: [ "tnt-browser" ])

    assert_equal [ 300, 300, 0 ], [ report.raw_seconds, report.human_seconds, report.agent_seconds ]
    assert_not_includes report.to_s, "terminet"
  end

  test "a correction splits a session and is listed" do
    correction = @user.project_corrections.new(start_time: T0 + 300, end_time: T0 + 601, from_project: "terminet", to_project: "tnt-browser")
    correction.apply!

    assert_equal 6, correction.heartbeats_count
    assert_equal 6, Heartbeat.only_deleted.where(user: @user, project: "terminet").count
    assert_equal 6 + 11, @user.heartbeats.where(project: "tnt-browser").count

    text = ProjectJustification.new(user: @user, from: DAY, to: DAY).to_s
    assert_includes text, "User reassigned 1 time ranges between projects."
    assert_includes text, "10/19/2026 10:05-10:10 terminet -> tnt-browser, 6 heartbeats"
  end

  test "a correction must change the project and cover a valid range" do
    same = @user.project_corrections.new(start_time: T0, end_time: T0 + 60, from_project: "terminet", to_project: "terminet")
    backwards = @user.project_corrections.new(start_time: T0 + 60, end_time: T0, to_project: "x")

    assert_raises(ActiveRecord::RecordInvalid) { same.apply! }
    assert_raises(ActiveRecord::RecordInvalid) { backwards.apply! }
    assert_equal 0, ProjectCorrection.count
  end

  test "with the split flag on, the dashboard counts human time and reports agent time beside it" do
    # The dashboard total orders all projects together, so it also counts the
    # capped 120 second gap from terminet's last heartbeat to tnt-browser's first.
    raw = DashboardStats.new(user: @user).build_filterable_dashboard_data(nil)
    assert_equal 1020, raw[:total_time]
    assert_nil raw[:agent_running_time]

    Flipper.enable_actor(User::HUMAN_AGENT_SPLIT_FLAG, @user)
    user = User.find(@user.id)
    split = DashboardStats.new(user:).build_filterable_dashboard_data(nil)
    # Human: 1 -> 30, then 30 -> 3600 capped at 120, then 300 on tnt-browser.
    assert_equal 29 + 120 + 300, split[:total_time]
    assert_equal 1020, split[:total_time] + split[:agent_running_time]

    DashboardRollupRefreshService.new(user:).call
    from_rollup = DashboardStats.new(user: User.find(@user.id)).build_filterable_dashboard_data(nil)
    assert_equal [ 449, 571 ], [ from_rollup[:total_time], from_rollup[:agent_running_time] ]
  end

  private

  def beat(project, offset, **attrs)
    create(:heartbeat, user: @user, project:, time: T0 + offset, category: "coding", **attrs)
  end
end
