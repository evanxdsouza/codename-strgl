require "test_helper"
require_relative "../../../support/struggle_bundle_helper"

class Api::V1::StruggleControllerTest < ActionDispatch::IntegrationTest
  include StruggleBundleHelper

  setup do
    @user = create(:user)
    @headers = { "Authorization" => "Bearer #{create(:api_key, user: @user).token}", "Content-Type" => "application/json" }
  end

  test "accepts a bundle once, returns the same submission on retry and rejects invalid ones" do
    raw = struggle_bundle(turns: [ { id: "t1", start: 0, value: 1, seconds: 120 } ])
    digest = Digest::SHA256.hexdigest(raw)

    post api_v1_struggle_submissions_path, params: raw, headers: @headers.merge("X-Bundle-Digest" => digest)
    assert_response :created
    assert_equal [ digest, "submitted", 120 ], response.parsed_body.values_at("digest", "status", "claimable_seconds")

    post api_v1_struggle_submissions_path, params: raw, headers: @headers.merge("X-Bundle-Digest" => digest)
    assert_response :ok
    assert_equal 1, @user.struggle_submissions.count

    post api_v1_struggle_submissions_path, params: raw.sub("why does", "WHY DOES"), headers: @headers.merge("X-Bundle-Digest" => digest)
    assert_response :unprocessable_entity
    post api_v1_struggle_submissions_path, params: raw, headers: { "Content-Type" => "application/json" }
    assert_response :unauthorized
  end

  test "anchors are idempotent and a conflicting head is refused" do
    body = { device_id: "dev_a", seq: 4, head: "a" * 64 }

    post api_v1_struggle_anchors_path, params: body.to_json, headers: @headers
    assert_response :created
    post api_v1_struggle_anchors_path, params: body.to_json, headers: @headers
    assert_response :ok
    post api_v1_struggle_anchors_path, params: body.merge(head: "b" * 64).to_json, headers: @headers
    assert_response :conflict
    assert_equal [ "a" * 64 ], @user.struggle_anchors.pluck(:head)
  end
end
