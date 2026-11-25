class CreateStruggleSubmissions < ActiveRecord::Migration[8.1]
  def change
    # A frozen, redacted evidence bundle a user submitted for review. The
    # bundle file itself is an Active Storage attachment; these columns are
    # what the server derived from it when it arrived, plus the current
    # review state. Estimated, proposed and approved seconds stay separate.
    create_table :struggle_submissions do |t|
      t.references :user, null: false, foreign_key: true
      t.string :project, null: false
      t.datetime :range_start, null: false
      t.datetime :range_end, null: false
      t.string :digest, null: false
      t.string :status, null: false, default: "submitted"
      t.string :rule_set, null: false
      t.string :redaction_version, null: false
      t.string :collector_version, null: false
      t.string :device_id, null: false
      t.integer :estimated_human_seconds, null: false, default: 0
      t.integer :agent_seconds, null: false, default: 0
      t.integer :proposed_productive_seconds, null: false, default: 0
      t.integer :claimable_seconds, null: false, default: 0
      t.integer :duplicate_seconds, null: false, default: 0
      t.integer :approved_seconds
      t.integer :turns_count, null: false, default: 0
      t.integer :labelled_one_count, null: false, default: 0
      t.integer :needs_review_count, null: false, default: 0
      t.integer :backfilled_count, null: false, default: 0
      t.string :chain_status, null: false
      t.integer :chain_anchors_matched, null: false, default: 0
      t.text :user_context
      t.text :reviewer_justification
      t.text :user_explanation
      t.references :supersedes, foreign_key: { to_table: :struggle_submissions }
      t.datetime :escalated_at
      t.datetime :decided_at
      t.references :decided_by, foreign_key: { to_table: :users }
      t.timestamps
    end
    add_index :struggle_submissions, [ :user_id, :digest ], unique: true
    add_index :struggle_submissions, [ :status, :created_at ]
  end
end
