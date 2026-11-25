require "test_helper"
require_relative "../support/struggle_bundle_helper"

class StruggleSubmissionTest < ActiveSupport::TestCase
  include StruggleBundleHelper

  setup { @user = create(:user) }

  test "stores a frozen bundle once and treats a retry of the same bytes as the same submission" do
    raw = struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 }, { id: "t2", start: 60, value: 0, seconds: 0 } ])

    first, created = StruggleSubmission.receive!(user: @user, raw:, claimed_digest: Digest::SHA256.hexdigest(raw))
    again, created_again = StruggleSubmission.receive!(user: @user, raw:)

    assert created
    assert_not created_again
    assert_equal first, again
    assert_equal raw, first.bundle.download
    assert_equal [ 240, 120, 120, nil ], [ first.estimated_human_seconds, first.proposed_productive_seconds, first.claimable_seconds, first.approved_seconds ]
    assert_equal "unanchored", first.chain_status
  end

  test "rejects bundles whose totals or intervals do not match their turns, or that changed in transit" do
    tampered = struggle_bundle_data(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ])
    tampered["totals"]["proposed_productive_seconds"] = 7200
    overlapping = struggle_bundle(turns: [ { id: "a", start: 0, value: 1, seconds: 120 }, { id: "b", start: 60, value: 1, seconds: 120 } ])
    long = struggle_bundle(turns: [ { id: "a", start: 0, value: 1, seconds: 600, interval: 600 } ])
    raw = struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ])

    { JSON.generate(tampered) => "proposed seconds", overlapping => "double-count", long => "2-minute" }.each do |bundle, message|
      error = assert_raises(StruggleBundle::Invalid) { StruggleSubmission.receive!(user: @user, raw: bundle) }
      assert_includes error.message, message
    end
    assert_raises(StruggleBundle::Invalid) { StruggleSubmission.receive!(user: @user, raw:, claimed_digest: "0" * 64) }
    assert_equal 0, @user.struggle_submissions.count
  end

  test "does not credit the same turn or overlapping time twice" do
    StruggleSubmission.receive!(user: @user, raw: struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ]))

    # The same turn again (a later ship), plus a new turn from another device
    # whose interval overlaps t1 by 60 seconds.
    later, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(
      device_id: "dev_other",
      turns: [ { id: "t1", start: 0, value: 1, seconds: 120 }, { id: "t9", start: 60, value: 0, seconds: 0 }, { id: "t3", start: 180, value: 1, seconds: 120 } ]
    ))
    other_device, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(device_id: "dev_x", turns: [ { id: "x1", start: 60, value: 1, seconds: 120 } ]))

    assert_equal [ 240, 120, 120 ], [ later.proposed_productive_seconds, later.claimable_seconds, later.duplicate_seconds ]
    # x1 overlaps t1 (0-120s) for 60s and t3 (180-300s) for 0s.
    assert_equal [ 60, 60 ], [ other_device.claimable_seconds, other_device.duplicate_seconds ]
  end

  test "a relabelled resubmission replaces the original and releases its turns" do
    reviewer = create(:user, struggle_role: "reviewer")
    original, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ]))
    original.decide!(reviewer:, action: "request_relabel", justification: "Please add context", user_explanation: "Say what you tried")

    resubmitted, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(supersedes: original.digest, context: "I tried two fixes",
      turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ]))

    assert_equal "superseded", original.reload.status
    assert_equal original, resubmitted.supersedes
    assert_equal 120, resubmitted.claimable_seconds
    assert_raises(StruggleBundle::Invalid) do
      StruggleSubmission.receive!(user: @user, raw: struggle_bundle(supersedes: original.digest, turns: [ { id: "t5", start: 500, value: 0, seconds: 0 } ]))
    end
  end

  test "verifies the chain against anchors and keeps a mismatch for investigation" do
    data = struggle_bundle_data(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 }, { id: "t2", start: 200, value: 1, seconds: 120 } ])
    heads = struggle_chain_heads(data)
    @user.struggle_anchors.create!(device_id: "dev_test", seq: 2, head: heads[2])
    verified, = StruggleSubmission.receive!(user: @user, raw: JSON.generate(data))

    other = create(:user)
    other.struggle_anchors.create!(device_id: "dev_test", seq: 1, head: "f" * 64)
    mismatch, = StruggleSubmission.receive!(user: other, raw: JSON.generate(data))

    assert_equal [ "verified", 1 ], [ verified.chain_status, verified.chain_anchors_matched ]
    assert_equal "mismatch", mismatch.chain_status

    tampered = struggle_bundle_data(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ])
    tampered["turns"][0]["items"][0]["commitment"] = "0" * 64
    assert_raises(StruggleBundle::Invalid) { StruggleSubmission.receive!(user: @user, raw: JSON.generate(tampered)) }
  end

  test "a later project correction leaves a frozen submission unchanged" do
    submission, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(project: "calcs", turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ]))
    before = [ submission.project, submission.digest, submission.proposed_productive_seconds, submission.claimable_seconds, submission.bundle.download ]
    t0 = StruggleBundleHelper::T0_MS / 1000
    [ 0, 60, 120 ].each { |s| create(:heartbeat, user: @user, project: "calcs", time: t0 + s, category: "coding") }

    @user.project_corrections.new(start_time: t0, end_time: t0 + 121, from_project: "calcs", to_project: "other").apply!

    submission.reload
    assert_equal before, [ submission.project, submission.digest, submission.proposed_productive_seconds, submission.claimable_seconds, submission.bundle.download ]
    assert_equal 0, @user.heartbeats.where(project: "calcs").count
    again, created = StruggleSubmission.receive!(user: @user, raw: before.last)
    assert_not created
    assert_equal submission, again
  end

  test "separates review and escalation permissions and records every decision" do
    submission, = StruggleSubmission.receive!(user: @user, raw: struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ]))
    reviewer = create(:user, struggle_role: "reviewer")
    fraud = create(:user, struggle_role: "fraud_squad")
    supa = create(:user, :superadmin)
    admin = create(:user, :admin)

    assert_equal [ "review", "review", "review", nil, nil ], [ reviewer, fraud, supa, admin, @user ].map { |u| submission.evidence_access_for(u) }

    submission.decide!(reviewer:, action: "escalate", justification: "Identical prompts across three accounts")
    assert_equal [ nil, "escalated", "escalated" ], [ reviewer, fraud, supa ].map { |u| submission.evidence_access_for(u) }
    assert_raises(ActiveRecord::RecordInvalid) { submission.decide!(reviewer:, action: "approve", justification: "x") }
    assert_raises(ActiveRecord::RecordNotFound) { submission.read_evidence!(reviewer) }

    submission.decide!(reviewer: fraud, action: "return_not_fraud", justification: "Shared template, not deception")
    submission.decide!(reviewer:, action: "adjust", approved_seconds: 90, justification: "Second half was reading output", user_explanation: "Approved 90 seconds")
    submission.read_evidence!(reviewer)

    assert_equal [ "adjusted", 90, reviewer ], [ submission.status, submission.approved_seconds, submission.decided_by ]
    assert_equal %w[escalate return_not_fraud adjust], submission.review_decisions.map(&:action)
    assert_equal [ submission.digest ], submission.review_decisions.map(&:bundle_digest).uniq
    assert_equal [ [ reviewer.id, "review" ] ], submission.evidence_reads.pluck(:user_id, :scope)
    assert_raises(ActiveRecord::RecordInvalid) { submission.decide!(reviewer:, action: "approve", justification: " ") }
  end

  test "only SupaAdmins grant review roles" do
    supa = create(:user, :superadmin)
    admin = create(:user, :admin)

    assert_not @user.set_struggle_role("fraud_squad", changed_by_user: admin)
    assert_not supa.set_struggle_role("fraud_squad", changed_by_user: supa)
    assert @user.set_struggle_role("fraud_squad", changed_by_user: supa)
    assert @user.reload.can_view_escalated_struggle?
  end
end
