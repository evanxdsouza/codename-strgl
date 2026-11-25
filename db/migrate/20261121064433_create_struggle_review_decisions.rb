class CreateStruggleReviewDecisions < ActiveRecord::Migration[8.1]
  def change
    # Append-only history of review actions: who did what, when, why, and
    # against which bundle digest and rule set.
    create_table :struggle_review_decisions do |t|
      t.references :struggle_submission, null: false, foreign_key: true
      t.references :reviewer, null: false, foreign_key: { to_table: :users }
      t.string :action, null: false
      t.string :status_before, null: false
      t.string :status_after, null: false
      t.integer :approved_seconds
      t.text :justification, null: false
      t.text :user_explanation
      t.string :bundle_digest, null: false
      t.string :rule_set, null: false
      t.datetime :created_at, null: false
    end
  end
end
