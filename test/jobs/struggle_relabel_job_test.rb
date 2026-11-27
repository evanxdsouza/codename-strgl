require "test_helper"
require "webmock/minitest"
WebMock.disable_net_connect!(allow_localhost: true)
require_relative "../support/struggle_bundle_helper"

class StruggleRelabelJobTest < ActiveSupport::TestCase
  include StruggleBundleHelper

  URL = "http://relabel.test/v1"
  ENV_VARS = {
    "STRGL_RELABEL_PROVIDER" => "self-hosted", "STRGL_RELABEL_URL" => URL, "STRGL_RELABEL_MODEL" => "small",
    "STRGL_RELABEL_MONTHLY_RUN_CAP" => "5", "STRGL_RELABEL_INPUT_CENTS_PER_MTOK" => "0", "STRGL_RELABEL_OUTPUT_CENTS_PER_MTOK" => "0"
  }.freeze

  setup do
    @previous = ENV_VARS.keys.to_h { |key| [ key, ENV[key] ] }
    ENV.update(ENV_VARS)
    @user = create(:user)
    @reviewer = create(:user, struggle_role: "reviewer")
    raw = struggle_bundle(turns: [ { id: "turn-a", start: 0, value: 1, seconds: 120 }, { id: "turn-b", start: 300, value: 0, seconds: 0, text: "try again" } ], consent: true)
    @submission, = StruggleSubmission.receive!(user: @user, raw:)
  end

  teardown { ENV.update(@previous) }

  def answer(turns)
    stub_request(:post, "#{URL}/chat/completions").to_return(
      headers: { "Content-Type" => "application/json" },
      body: JSON.generate(choices: [ { message: { content: JSON.generate(turns:) } } ], usage: { prompt_tokens: 900, completion_tokens: 40 })
    )
  end

  def request_run
    StruggleRelabelRun.request!(submission: @submission, requester: @reviewer, reason: "second opinion")
  end

  test "a self-hosted run stores proposals by turn, sends no identifiers and changes no hours" do
    answer([ { ref: "t1", value: 1, reason: "diagnosis" }, { ref: "t2", value: 1, reason: "maybe" } ])
    hours = @submission.attributes.slice("status", "estimated_human_seconds", "proposed_productive_seconds", "claimable_seconds", "approved_seconds")

    run = request_run
    assert GoodJob::Job.exists?(job_class: "StruggleRelabelJob")
    StruggleRelabelJob.perform_now(run.id)
    StruggleRelabelJob.perform_now(run.id) # a retry does nothing

    run.reload
    assert_equal [ "succeeded", 0 ], [ run.status, run.cost_cents ]
    assert_equal({ "turn-a" => 1, "turn-b" => 1 }, run.proposals_by_turn.transform_values { |p| p["value"] })
    assert_requested(:post, "#{URL}/chat/completions", times: 1) do |req|
      user = JSON.parse(req.body)["messages"][1]["content"]
      %w[turn-a turn-b calcs dev_test].none? { |hidden| user.include?(hidden) }
    end
    assert_equal hours, @submission.reload.attributes.slice(*hours.keys)
  end

  test "an answer that misses a turn, an endpoint error or escalation fails the run without proposals" do
    answer([ { ref: "t1", value: 1, reason: "only one" } ])
    run = request_run
    StruggleRelabelJob.perform_now(run.id)
    assert_equal [ "failed", nil ], [ run.reload.status, run.proposals ]

    stub_request(:post, "#{URL}/chat/completions").to_return(status: 500)
    run = request_run
    StruggleRelabelJob.perform_now(run.id)
    assert_equal "failed", run.reload.status

    run = request_run
    @submission.update!(status: "escalated")
    WebMock.reset_executed_requests!
    stub = answer([])
    StruggleRelabelJob.perform_now(run.id)
    assert_equal "failed", run.reload.status
    assert_not_requested stub
  end

  test "the monthly run cap bounds a self-hosted model" do
    ENV["STRGL_RELABEL_MONTHLY_RUN_CAP"] = "1"
    request_run
    error = assert_raises(StruggleRelabelRun::Refused) { request_run }
    assert_includes error.message, "re-checks are used up"
  end
end
