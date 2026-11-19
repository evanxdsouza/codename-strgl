require "test_helper"

class My::LookbackControllerTest < ActionDispatch::IntegrationTest
  T0 = Time.utc(2026, 10, 19, 10).to_i

  test "redirects guests" do
    get my_lookback_path

    assert_redirected_to root_path
  end

  test "shows the day's sessions and generates a justification" do
    user = create(:user, timezone: "UTC")
    [ 0, 60, 120 ].each { |s| create(:heartbeat, user:, project: "alpha", time: T0 + s, category: "coding") }
    sign_in_as(user)

    get my_lookback_path(date: "2026-10-19", justify: 1, from: "2026-10-19", to: "2026-10-19")

    assert_response :success
    assert_inertia_component "Lookback/Show"
    assert_equal [ [ "alpha", "2026-10-19T10:00", 120 ] ],
      inertia.props["sessions"].map { |s| [ s["project"], s["start_time"], s["human_seconds"] ] }
    assert_includes inertia.props.dig("justification", "text"), "[STRUGGLE TRACKER]"
  end

  test "moves a time range to another project" do
    user = create(:user, timezone: "UTC")
    [ 0, 60, 120 ].each { |s| create(:heartbeat, user:, project: "alpha", time: T0 + s, category: "coding") }
    sign_in_as(user)

    post my_lookback_corrections_path, params: {
      date: "2026-10-19", start_time: "2026-10-19T10:01", end_time: "2026-10-19T10:02", from_project: "alpha", to_project: "beta"
    }

    assert_redirected_to my_lookback_path(date: "2026-10-19")
    # "To" 10:02 includes the heartbeat at exactly 10:02:00.
    assert_equal({ "alpha" => 1, "beta" => 2 }, user.heartbeats.group(:project).count)
  end
end
