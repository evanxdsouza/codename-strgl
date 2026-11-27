require "test_helper"
require_relative "../support/struggle_bundle_helper"

class StruggleRelabelRunTest < ActiveSupport::TestCase
  include StruggleBundleHelper

  CONFIG = StruggleRelabelRun::Config.new(
    provider: "test", url: "http://relabel.test/v1", api_key: "", model: "test-model", monthly_run_cap: 100,
    monthly_cap_cents: 1_000, run_cap_cents: 100,
    input_cents_per_mtok: 300, output_cents_per_mtok: 1_500, max_output_tokens: 4_000
  )

  setup do
    @user = create(:user)
    @reviewer = create(:user, struggle_role: "reviewer")
  end

  def submit(consent: true, text: "why does add() fail?")
    # A different context per call, so each bundle is a separate submission.
    @count = @count.to_i + 1
    raw = struggle_bundle(turns: [ { id: "turn-#{@count}", start: @count * 600, value: 1, seconds: 120, text: } ], consent:, context: "case #{@count}")
    StruggleSubmission.receive!(user: @user, raw:).first
  end

  def request(submission, requester: @reviewer, config: CONFIG)
    StruggleRelabelRun.request!(submission:, requester:, reason: "Labels look generous for a bare fix request", config:)
  end

  test "records a consented run with a reservation and a logged read, and changes no hours" do
    submission = submit
    before = submission.attributes.slice("status", "estimated_human_seconds", "proposed_productive_seconds", "claimable_seconds", "approved_seconds")

    run = request(submission)

    assert_equal [ "queued", "test-model", StruggleRelabelRun::PROMPT_VERSION, submission.digest ], [ run.status, run.model, run.prompt_version, run.bundle_digest ]
    request_bytes = StruggleRelabelPayload.new(JSON.parse(submission.bundle.download), prompt_version: run.prompt_version).requests.values.map(&:bytesize)
    assert_equal CONFIG.estimate_cents(request_bytes), run.reserved_cents
    assert run.reserved_cents.positive?
    assert_equal [ [ @reviewer.id, "model_relabel" ] ], submission.evidence_reads.pluck(:user_id, :scope)
    assert_equal before, submission.reload.attributes.slice(*before.keys)
  end

  test "refuses without consent, configuration, an eligible state or budget, and records nothing" do
    no_consent = submit(consent: false)
    leaky = submit(text: "mail me at kid@example.com")
    escalated = submit.tap { |s| s.update!(status: "escalated") }
    decided = submit.tap { |s| s.update!(status: "approved") }
    eligible = submit

    cases = {
      [ no_consent, CONFIG ] => "did not allow",
      [ leaky, CONFIG ] => "email address",
      [ decided, CONFIG ] => "awaiting review",
      [ eligible, CONFIG.with(monthly_cap_cents: 0) ] => "not configured",
      [ eligible, CONFIG.with(run_cap_cents: 1) ] => "per-run cap"
    }
    cases.each do |(submission, config), message|
      error = assert_raises(StruggleRelabelRun::Refused) { request(submission, config:) }
      assert_includes error.message, message
    end
    # Escalated evidence never goes to a model, even when the fraud squad asks.
    error = assert_raises(StruggleRelabelRun::Refused) { request(escalated, requester: create(:user, struggle_role: "fraud_squad")) }
    assert_includes error.message, "escalated"
    # Someone who cannot review it, including its own author, learns nothing.
    [ create(:user), @user ].each do |outsider|
      assert_raises(ActiveRecord::RecordNotFound) { request(eligible, requester: outsider) }
    end

    assert_equal 0, StruggleRelabelRun.count
    assert_equal 0, StruggleEvidenceRead.where(scope: "model_relabel").count
  end

  test "counts reservations and actual costs against the monthly cap" do
    first = request(submit)
    config = CONFIG.with(monthly_cap_cents: first.reserved_cents * 2)
    first.update!(status: "succeeded", cost_cents: 1)

    second = request(submit, config:)
    error = assert_raises(StruggleRelabelRun::Refused) { request(submit, config:) }
    assert_includes error.message, "budget is used up"
    assert_equal 1 + second.reserved_cents, StruggleRelabelRun.month_spend_cents
  end

  test "the payload keeps redacted text but no identifiers or rule labels" do
    data = struggle_bundle_data(turns: [ { id: "turn-a", start: 0, value: 1, seconds: 120 }, { id: "turn-b", start: 300, value: 0, seconds: 0 } ], project: "secret-project", device_id: "dev_42", consent: true)
    data["turns"][1]["items"][0].merge!("status" => "excluded", "text" => nil)
    payload = StruggleRelabelPayload.new(data, prompt_version: "p1")
    json = payload.to_json

    %w[secret-project dev_42 turn-a turn-b abc commitment chain_seq main label diagnosis].each { |hidden| assert_not_includes json, hidden }
    parsed = JSON.parse(json)
    assert_equal [ [ "t1", 0 ], [ "t2", 300 ] ], parsed["turns"].map { |t| [ t["ref"], t["seconds_from_start"] - 3600 ] }
    assert_equal [ 1, 0 ], parsed["turns"].map { |t| t["items"].size }
    assert_equal({ "t1" => "turn-a", "t2" => "turn-b" }, payload.turn_ids)
  end
end
