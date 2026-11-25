require "test_helper"
require_relative "../../support/struggle_bundle_helper"

class Review::StruggleSubmissionsControllerTest < ActionDispatch::IntegrationTest
  include StruggleBundleHelper

  setup do
    @owner = create(:user, timezone: "UTC", username: "kid")
    @submission, = StruggleSubmission.receive!(user: @owner, raw: struggle_bundle(
      turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ], context: "I learned unittest"
    ))
    @reviewer = create(:user, struggle_role: "reviewer", username: "rev")
    @fraud = create(:user, struggle_role: "fraud_squad")
  end

  test "people without a review role cannot open the queue, a submission or its bundle" do
    [ @owner, create(:user, :admin) ].each do |user|
      sign_in_as(user)
      get review_struggle_submissions_path
      assert_redirected_to root_path
      get bundle_review_struggle_submission_path(@submission)
      assert_redirected_to root_path
    end
    assert_equal 0, @submission.evidence_reads.count
  end

  test "a reviewer inspects, decides and loses access once it is escalated" do
    sign_in_as(@reviewer)

    get review_struggle_submission_path(@submission)
    assert_response :success
    assert_inertia_component "Review/StruggleSubmissions/Show"
    assert_equal "why does add() fail?", inertia.props["turns"][0]["items"][0]["text"]
    assert_includes inertia.props["actions"], "escalate"

    post decisions_review_struggle_submission_path(@submission), params: { decision_action: "escalate", justification: "Suspected copied transcript" }
    assert_redirected_to review_struggle_submissions_path

    get review_struggle_submission_path(@submission)
    assert_response :not_found
    get bundle_review_struggle_submission_path(@submission)
    assert_response :not_found
    get review_struggle_submissions_path
    assert_not_includes inertia.props["submissions"].map { |s| s["id"] }, @submission.id
    assert_equal [ [ @reviewer.id, "review" ] ], @submission.evidence_reads.pluck(:user_id, :scope)
  end

  test "the fraud squad reads escalated evidence, returns it and the export shows the decision" do
    @submission.decide!(reviewer: @reviewer, action: "escalate", justification: "Check")
    sign_in_as(@fraud)

    get bundle_review_struggle_submission_path(@submission)
    assert_response :success
    assert_equal @submission.digest, Digest::SHA256.hexdigest(response.body)
    post decisions_review_struggle_submission_path(@submission), params: { decision_action: "return_not_fraud", justification: "Not deception" }
    post decisions_review_struggle_submission_path(@submission), params: {
      decision_action: "adjust", approved_hours: "0.01", justification: "INTERNAL: reading time not credited", user_explanation: "36 seconds approved"
    }

    assert_equal [ "adjusted", 36 ], [ @submission.reload.status, @submission.approved_seconds ]
    assert_equal [ [ @fraud.id, "escalated" ] ], @submission.evidence_reads.pluck(:user_id, :scope)

    get review_struggle_submission_path(@submission)
    assert_includes inertia.props["justification"], "INTERNAL: reading time not credited"

    # The submitter's own export and Lookback show the outcome, not the
    # reviewer's internal reasons.
    sign_in_as(@owner)
    get my_lookback_path(date: "2026-11-20", justify: 1, from: "2026-11-20", to: "2026-11-20")
    text = inertia.props.dig("justification", "text")
    assert_includes text, "[STRUGGLE REVIEW]"
    assert_includes text, "decision: adjusted 0.01h"
    assert_includes text, "deflated to 0.01h"
    assert_not_includes text, "INTERNAL"
    assert_equal [ "adjusted", 36, "36 seconds approved" ],
      inertia.props["submissions"][0].values_at("status", "approved_seconds", "user_explanation")
  end

  test "reviewers cannot decide on their own submissions" do
    own, = StruggleSubmission.receive!(user: @reviewer, raw: struggle_bundle(turns: [ { id: "r1", start: 0, value: 1, seconds: 120 } ]))
    sign_in_as(@reviewer)

    get review_struggle_submission_path(own)
    assert_response :not_found
    post decisions_review_struggle_submission_path(own), params: { decision_action: "approve", justification: "mine" }
    assert_response :not_found
    assert_equal "submitted", own.reload.status
  end
end
