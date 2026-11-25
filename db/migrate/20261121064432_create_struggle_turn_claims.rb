class CreateStruggleTurnClaims < ActiveRecord::Migration[8.1]
  def change
    # The turns a submission claims credit for. A turn can be claimed by only
    # one live submission, so the same work cannot be credited twice in later
    # ships or updates. Rejecting or superseding a submission releases its
    # claims.
    create_table :struggle_turn_claims do |t|
      t.references :struggle_submission, null: false, foreign_key: true
      t.references :user, null: false, foreign_key: true
      t.string :turn_id, null: false
      t.bigint :interval_start_ms, null: false
      t.bigint :interval_end_ms, null: false
      t.integer :claimed_seconds, null: false, default: 0
      t.datetime :released_at
      t.timestamps
    end
    add_index :struggle_turn_claims, [ :user_id, :turn_id ], unique: true, where: "released_at IS NULL",
      name: "index_struggle_turn_claims_live_turn"
    add_index :struggle_turn_claims, [ :user_id, :interval_start_ms ]
  end
end
