require 'swagger_helper'
require Rails.root.join('test/support/struggle_bundle_helper').to_s

RSpec.describe 'Api::V1::Struggle', type: :request do
  include StruggleBundleHelper

  path '/api/v1/struggle/anchors' do
    post('Anchor an evidence chain head') do
      tags 'Struggle Tracker'
      description 'Sent by strgl-collector to timestamp its local evidence chain. Carries a SHA-256 head only, never content. ' \
                  'Repeating the same head is idempotent; a different head for the same device and sequence number is refused. ' \
                  'Authenticate with an API key as a Bearer token (HTTP Basic auth with the API key is also accepted).'
      consumes 'application/json'
      produces 'application/json'
      security [ { Bearer: [] } ]
      parameter name: :anchor, in: :body, schema: {
        type: :object,
        properties: {
          device_id: { type: :string, example: 'dev_3f1a9c0b2e4d6f8a1b2c3d4e' },
          seq: { type: :integer, format: :int64, example: 42, description: 'Sequence number of the newest chain entry the head covers.' },
          head: { type: :string, example: 'a' * 64, description: 'Hex SHA-256 chain head after entry `seq`.' }
        },
        required: %w[device_id seq head]
      }

      response(201, 'anchored') do
        let(:Authorization) { 'Bearer dev-api-key-12345' }
        let(:anchor) { { device_id: 'dev_spec', seq: 1, head: 'a' * 64 } }
        schema type: :object,
          properties: {
            id: { type: :integer, example: 7 },
            received_at: { type: :string, format: :date_time, example: '2026-11-21T06:00:00Z' }
          },
          required: %w[id received_at]
        run_test!
      end

      response(401, 'unauthorized') do
        let(:Authorization) { 'Bearer invalid' }
        let(:anchor) { { device_id: 'dev_spec', seq: 1, head: 'a' * 64 } }
        schema '$ref' => '#/components/schemas/Error'
        run_test!
      end
    end
  end

  path '/api/v1/struggle/submissions' do
    post('Submit a redacted evidence bundle') do
      tags 'Struggle Tracker'
      description 'Uploads the exact bytes the user previewed locally with `strgl-collector preview`. The server re-derives every total ' \
                  'from the turns, checks each included item against the submitted evidence chain and any earlier anchors, freezes the ' \
                  'bundle and records which turns it claims, so the same turn is never credited twice. Retrying the same bytes returns the ' \
                  'existing submission with 200. Authenticate with an API key as a Bearer token.'
      consumes 'application/json'
      produces 'application/json'
      security [ { Bearer: [] } ]
      parameter name: 'X-Bundle-Digest', in: :header, type: :string, required: false,
        description: 'Hex SHA-256 of the body as the collector computed it. A mismatch is rejected.'
      parameter name: :bundle, in: :body, schema: {
        type: :object,
        description: 'Bundle format version 1, documented in collector/bundle.go.',
        properties: {
          schema: { type: :integer, example: 1 },
          project: { type: :string, example: 'calcs' },
          from: { type: :integer, format: :int64, description: 'Range start, Unix milliseconds' },
          to: { type: :integer, format: :int64, description: 'Range end (exclusive), Unix milliseconds' },
          totals: { type: :object },
          turns: { type: :array, items: { type: :object } },
          chain: { type: :object }
        },
        required: %w[schema project from to totals turns chain]
      }

      submission_schema = {
        type: :object,
        properties: {
          id: { type: :integer, example: 12 },
          digest: { type: :string, example: 'b' * 64 },
          status: { type: :string, example: 'submitted' },
          project: { type: :string, example: 'calcs' },
          estimated_human_seconds: { type: :integer, example: 240 },
          proposed_productive_seconds: { type: :integer, example: 120 },
          claimable_seconds: { type: :integer, example: 120 },
          duplicate_seconds: { type: :integer, example: 0 },
          chain_status: { type: :string, example: 'verified', enum: %w[verified unanchored mismatch empty] },
          supersedes_digest: { type: :string, nullable: true }
        },
        required: %w[id digest status project estimated_human_seconds proposed_productive_seconds claimable_seconds duplicate_seconds chain_status]
      }

      response(201, 'submitted') do
        let(:Authorization) { 'Bearer dev-api-key-12345' }
        let(:bundle) { struggle_bundle_data(turns: [ { id: 'spec-t1', start: 0, value: 1, seconds: 120 } ]) }
        let(:'X-Bundle-Digest') { nil }
        schema submission_schema
        run_test!
      end

      response(422, 'invalid bundle') do
        let(:Authorization) { 'Bearer dev-api-key-12345' }
        let(:bundle) { struggle_bundle_data(turns: [ { id: 'spec-t2', start: 0, value: 1, seconds: 120 } ]).merge('totals' => { 'proposed_productive_seconds' => 9999 }) }
        let(:'X-Bundle-Digest') { nil }
        schema '$ref' => '#/components/schemas/Error'
        run_test!
      end

      response(401, 'unauthorized') do
        let(:Authorization) { 'Bearer invalid' }
        let(:bundle) { {} }
        let(:'X-Bundle-Digest') { nil }
        schema '$ref' => '#/components/schemas/Error'
        run_test!
      end
    end
  end
end
